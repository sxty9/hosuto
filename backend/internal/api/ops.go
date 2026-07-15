// This file holds hosuto's server operations as transport-agnostic methods: the REST handlers in
// api.go call them, and so do the MCP tools in mcp.go. Neither the HTTP request nor the JSON-RPC
// envelope reaches here — the logic that changes a server lives in exactly one place, so the two
// surfaces can never drift apart (the Single Source of Truth maxim, applied inside hosuto).
package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"hosuto/internal/access"
	"hosuto/internal/auth"
	"hosuto/internal/runtime"
	"hosuto/internal/store"
)

// The access level a caller must hold for an operation — the three gates the REST surface enforces.
// The rule itself lives in package access (shared with the in-game CLI); these are local aliases so
// every call site here reads accessVisible/Control/Owned unchanged.
type accessLevel = access.Level

const (
	accessVisible = access.Visible // owner, admin, or any resolved member
	accessControl = access.Control // owner, admin, or an op-level member
	accessOwned   = access.Owned   // owner or admin only
)

// findServer resolves a server by id (srv-…) or by slug. A slug is accepted because it is what a
// person — or a model operating the server by name — naturally has; the id is an internal handle.
func (s *Server) findServer(ref string) (store.Server, bool) {
	if ref == "" {
		return store.Server{}, false
	}
	if srv, ok := s.st.Server(ref); ok {
		return srv, true
	}
	for _, srv := range s.st.Servers() {
		if srv.Slug == ref {
			return srv, true
		}
	}
	return store.Server{}, false
}

// hasAccess / resolve delegate to the shared resolver so the api handlers and the in-game CLI run one
// implementation.
func (s *Server) hasAccess(ctx context.Context, srv store.Server, u *auth.User, level accessLevel) bool {
	return s.acc.HasAccess(ctx, srv, u, level)
}

// installMod resolves a Modrinth project against the server's version/loader, downloads the jar into
// the server's mods/ dir, and records it. A mod the server cannot run is refused before anything is
// written (the mirror of that rule — a client-only mod must never be exported — lives in export).
func (s *Server) installMod(ctx context.Context, srv store.Server, projectID string) (store.Mod, error) {
	if !store.LoaderHasClientMods(srv.Loader) {
		return store.Mod{}, errors.New("this server's loader does not run mods")
	}
	ver, hit, err := s.mr.Resolve(ctx, projectID, srv.MCVersion, srv.Loader)
	if err != nil {
		return store.Mod{}, errors.New("no build of that mod for this version and loader")
	}
	if hit.ServerSide == "unsupported" {
		return store.Mod{}, errors.New("that mod is client-only — it does not belong on the server")
	}
	file := primary(ver)
	if file.URL == "" {
		return store.Mod{}, errors.New("that mod has no downloadable file")
	}
	dir := filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return store.Mod{}, errors.New("could not create the mods folder")
	}
	if err := s.mr.Download(ctx, file, filepath.Join(dir, file.Filename)); err != nil {
		return store.Mod{}, errors.New("could not download the mod")
	}
	return s.st.AddMod(srv.ID, store.Mod{
		Source: "modrinth", ProjectID: hit.ProjectID, VersionID: ver.ID,
		Name: hit.Title, Filename: file.Filename, URL: file.URL,
		SHA1: file.SHA1, SHA512: file.SHA512, Size: file.Size,
		ClientSide: orUnknown(hit.ClientSide), ServerSide: orUnknown(hit.ServerSide),
	})
}

// uninstallMod drops a mod record and deletes its jar. Returns store.ErrNotFound for an unknown mod.
func (s *Server) uninstallMod(_ context.Context, srv store.Server, modID string) error {
	m, err := s.st.RemoveMod(srv.ID, modID)
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods", m.Filename))
	return nil
}

// applyPolicy sets the join policy (open|whitelist) and re-writes server.properties. white-list is
// read at startup, so a running server needs a restart to pick it up — the caller is told, and the
// server is told over chat, rather than pretending it took effect now.
func (s *Server) applyPolicy(ctx context.Context, srv store.Server, policy string) (store.Server, error) {
	if !store.ValidPolicy(policy) {
		return srv, errors.New("unknown join policy")
	}
	srv.JoinPolicy = policy
	if err := s.st.UpdateServer(srv); err != nil {
		return srv, errors.New("could not save the policy")
	}
	if err := s.rt.SyncConfig(ctx, srv); err != nil {
		return srv, err
	}
	s.rt.Say(ctx, srv, "hosuto: join policy is now "+policy+" (restart to apply)")
	return srv, nil
}

// revokeUser removes a user from every ad-hoc grant on a server and drops any grant left empty, then
// re-applies the whitelist. It cannot remove someone who is on the server via a contax or OS group —
// that membership belongs to the group, not to hosuto — so it reports that rather than doing nothing.
func (s *Server) revokeUser(ctx context.Context, srv store.Server, target string) error {
	fresh, ok := s.st.Server(srv.ID)
	if !ok {
		return store.ErrNotFound
	}
	changed := false
	var kept []store.Grant
	for _, g := range fresh.Grants {
		if g.Kind == "adhoc" {
			var members []string
			for _, m := range g.Members {
				if m != target {
					members = append(members, m)
				}
			}
			if len(members) != len(g.Members) {
				changed = true
			}
			if len(members) == 0 {
				continue // an ad-hoc grant with nobody left is just noise
			}
			g.Members = members
		}
		kept = append(kept, g)
	}
	if !changed {
		return errors.New("that member is not on this server, or was added through a group")
	}
	fresh.Grants = kept
	if err := s.st.UpdateServer(fresh); err != nil {
		return err
	}
	fresh, _ = s.st.Server(srv.ID)
	return s.applyMembers(ctx, fresh)
}
