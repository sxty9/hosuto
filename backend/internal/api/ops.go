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
	"strings"

	"hosuto/internal/access"
	"hosuto/internal/auth"
	"hosuto/internal/modrinth"
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

// installMod installs a Modrinth project on the server together with its REQUIRED dependencies (the
// loader API like fabric-api, a shared library like balm, …), so a mod can never land on the server
// missing something it needs to load — the failure that otherwise only surfaces as an aborted start.
// It returns the requested mod and every dependency it had to add alongside it.
func (s *Server) installMod(ctx context.Context, srv store.Server, projectID string) (store.Mod, []store.Mod, error) {
	if !store.LoaderHasClientMods(srv.Loader) {
		return store.Mod{}, nil, errors.New("this server's loader does not run mods")
	}
	// Project ids we already have (or add along the way), so a shared or cyclic dependency is fetched
	// exactly once. Modrinth ids are opaque; lower-case for a stable set key.
	have := map[string]bool{}
	for _, m := range srv.Mods {
		if m.ProjectID != "" {
			have[strings.ToLower(m.ProjectID)] = true
		}
	}
	main, ver, err := s.installOne(ctx, srv, projectID)
	if err != nil {
		return store.Mod{}, nil, err
	}
	have[strings.ToLower(main.ProjectID)] = true

	// Breadth-first over required dependencies. A dep we cannot resolve, or one that is client-only, is
	// skipped rather than failing the whole install — the primary mod is in, and a genuinely missing
	// hard dependency still surfaces in the start-failure diagnosis.
	var added []store.Mod
	queue := requiredDepIDs(ver, have)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if have[strings.ToLower(id)] {
			continue
		}
		have[strings.ToLower(id)] = true // mark before install so a cycle cannot re-queue it
		depMod, depVer, derr := s.installOne(ctx, srv, id)
		if derr != nil {
			continue
		}
		added = append(added, depMod)
		queue = append(queue, requiredDepIDs(depVer, have)...)
	}
	return main, added, nil
}

// installOne resolves one Modrinth project against the server's version/loader, downloads the jar into
// the server's mods/ dir and records it, returning the resolved version so the caller can walk its
// dependencies. A mod the server cannot run is refused before anything is written (the mirror of that
// rule — a client-only mod must never be exported — lives in export).
func (s *Server) installOne(ctx context.Context, srv store.Server, projectID string) (store.Mod, modrinth.Version, error) {
	ver, hit, err := s.mr.Resolve(ctx, projectID, srv.MCVersion, srv.Loader)
	if err != nil {
		return store.Mod{}, modrinth.Version{}, errors.New("no build of that mod for this version and loader")
	}
	if hit.ServerSide == "unsupported" {
		return store.Mod{}, modrinth.Version{}, errors.New("that mod is client-only — it does not belong on the server")
	}
	file := primary(ver)
	if file.URL == "" {
		return store.Mod{}, modrinth.Version{}, errors.New("that mod has no downloadable file")
	}
	dir := filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return store.Mod{}, modrinth.Version{}, errors.New("could not create the mods folder")
	}
	if err := s.mr.Download(ctx, file, filepath.Join(dir, file.Filename)); err != nil {
		return store.Mod{}, modrinth.Version{}, errors.New("could not download the mod")
	}
	m, err := s.st.AddMod(srv.ID, store.Mod{
		Source: "modrinth", ProjectID: hit.ProjectID, VersionID: ver.ID,
		Name: hit.Title, Filename: file.Filename, URL: file.URL,
		SHA1: file.SHA1, SHA512: file.SHA512, Size: file.Size,
		ClientSide: orUnknown(hit.ClientSide), ServerSide: orUnknown(hit.ServerSide),
	})
	return m, ver, err
}

// requiredDepIDs returns the project ids of a version's REQUIRED dependencies that are not already
// present. Version-pinned dependencies with no project id (rare) are skipped.
func requiredDepIDs(ver modrinth.Version, have map[string]bool) []string {
	var out []string
	for _, d := range ver.Dependencies {
		if d.Type != "required" || d.ProjectID == "" {
			continue
		}
		if have[strings.ToLower(d.ProjectID)] {
			continue
		}
		out = append(out, d.ProjectID)
	}
	return out
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
