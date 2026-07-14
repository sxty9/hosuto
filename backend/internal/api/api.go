// Package api is hosuto's HTTP surface under /api/services/hosuto/.
//
// Authorisation has two independent layers, and both are enforced here, never in the browser:
//
//  1. Rights (hp_hosuto_play|host|admin) — the standard holistic rule: isAdmin || group ∈ groups.
//  2. Ownership and the contax relation — WHO a member may add to a server. The browser sends a
//     username; the daemon decides whether that pairing is legitimate. See canAdd().
//
// The contax relation is the reason hosuto exists in this shape. hosuto owns the Linux-user →
// Minecraft-account mapping, so once A and B are established as acquainted, hosuto is the only
// service that can turn "add Bob" into a whitelist entry.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"hosuto/internal/auth"
	"hosuto/internal/contax"
	"hosuto/internal/directory"
	"hosuto/internal/export"
	"hosuto/internal/hconfig"
	"hosuto/internal/mcapi"
	"hosuto/internal/mcfiles"
	"hosuto/internal/modrinth"
	"hosuto/internal/notify"
	"hosuto/internal/rights"
	"hosuto/internal/runtime"
	"hosuto/internal/skin"
	"hosuto/internal/store"
	"hosuto/internal/versions"
)

const (
	base    = "/api/services/hosuto/"
	service = "hosuto"
	version = "0.1.0"
)

// Server is the HTTP layer.
type Server struct {
	v    *auth.Verifier
	st   *store.Store
	cfg  *hconfig.Config
	rt   *runtime.Manager
	dir  *directory.Directory
	cx   *contax.Client
	nt   *notify.Client
	mc   *mcapi.Client
	mr   *modrinth.Client
	vc   *versions.Client
	skin *skin.Renderer
	http *http.Client
}

// New wires the HTTP layer.
func New(v *auth.Verifier, st *store.Store, cfg *hconfig.Config, rt *runtime.Manager,
	dir *directory.Directory, cx *contax.Client, nt *notify.Client, mc *mcapi.Client,
	mr *modrinth.Client, vc *versions.Client, sk *skin.Renderer) *Server {
	return &Server{v: v, st: st, cfg: cfg, rt: rt, dir: dir, cx: cx, nt: nt, mc: mc, mr: mr, vc: vc,
		skin: sk, http: &http.Client{Timeout: 60 * time.Second}}
}

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User)

// Handler returns the routed http.Handler (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+base+"info", s.guard("", false, s.info))
	mux.HandleFunc("GET "+base+"health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// The game account. Every member with hp_hosuto_play may link one; without it they cannot be
	// whitelisted anywhere, so the UI asks for it first.
	mux.HandleFunc("GET "+base+"account", s.guard(rights.GroupPlay, false, s.getAccount))
	mux.HandleFunc("PUT "+base+"account", s.guard(rights.GroupPlay, true, s.linkAccount))
	mux.HandleFunc("DELETE "+base+"account", s.guard(rights.GroupPlay, true, s.unlinkAccount))

	// A member's face, rendered from their Minecraft skin. hosuto owns the account mapping, so hosuto
	// is the only service that can serve this.
	mux.HandleFunc("GET "+base+"avatar/{user}", s.guard(rights.GroupPlay, false, s.avatar))

	mux.HandleFunc("GET "+base+"servers", s.guard(rights.GroupPlay, false, s.listServers))
	mux.HandleFunc("POST "+base+"servers", s.guard(rights.GroupHost, true, s.createServer))
	mux.HandleFunc("GET "+base+"servers/{id}", s.guard(rights.GroupPlay, false, s.getServer))
	mux.HandleFunc("DELETE "+base+"servers/{id}", s.guard(rights.GroupHost, true, s.deleteServer))

	mux.HandleFunc("POST "+base+"servers/{id}/start", s.guard(rights.GroupPlay, true, s.lifecycle("start")))
	mux.HandleFunc("POST "+base+"servers/{id}/stop", s.guard(rights.GroupPlay, true, s.lifecycle("stop")))
	mux.HandleFunc("POST "+base+"servers/{id}/restart", s.guard(rights.GroupPlay, true, s.lifecycle("restart")))
	mux.HandleFunc("GET "+base+"servers/{id}/status", s.guard(rights.GroupPlay, false, s.status))

	mux.HandleFunc("GET "+base+"servers/{id}/members", s.guard(rights.GroupPlay, false, s.listMembers))
	mux.HandleFunc("POST "+base+"servers/{id}/members", s.guard(rights.GroupHost, true, s.addMembers))
	mux.HandleFunc("DELETE "+base+"servers/{id}/members/{grantId}", s.guard(rights.GroupHost, true, s.removeMember))
	mux.HandleFunc("PUT "+base+"servers/{id}/policy", s.guard(rights.GroupHost, true, s.setPolicy))

	mux.HandleFunc("GET "+base+"servers/{id}/mods", s.guard(rights.GroupPlay, false, s.listMods))
	mux.HandleFunc("POST "+base+"servers/{id}/mods", s.guard(rights.GroupHost, true, s.addMod))
	mux.HandleFunc("DELETE "+base+"servers/{id}/mods/{modId}", s.guard(rights.GroupHost, true, s.removeMod))
	mux.HandleFunc("PUT "+base+"servers/{id}/version", s.guard(rights.GroupHost, true, s.setVersion))

	mux.HandleFunc("GET "+base+"catalog/versions", s.guard(rights.GroupPlay, false, s.catalogVersions))
	mux.HandleFunc("GET "+base+"catalog/loaders", s.guard(rights.GroupPlay, false, s.catalogLoaders))
	mux.HandleFunc("GET "+base+"catalog/mods", s.guard(rights.GroupPlay, false, s.catalogMods))

	mux.HandleFunc("GET "+base+"servers/{id}/export/mods", s.guard(rights.GroupPlay, false, s.exportMods))
	mux.HandleFunc("GET "+base+"servers/{id}/export/mrpack", s.guard(rights.GroupPlay, false, s.exportMrpack))
	mux.HandleFunc("GET "+base+"servers/{id}/export/prism", s.guard(rights.GroupPlay, false, s.exportPrism))

	return mux
}

// guard authenticates, optionally requires a right, and optionally enforces the CSRF double-submit.
func (s *Server) guard(perm string, csrf bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.v.User(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission for this action")
			return
		}
		if csrf && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": service,
		"version": version,
		"user":    u.Username,
		"zone":    s.cfg.String("zone", "mc.henrysoase.org"),
		"canHost": u.Can(rights.GroupHost),
	})
}

// ── accounts ──────────────────────────────────────────────────────────────────────────

func (s *Server) getAccount(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	if a, ok := s.st.Account(u.Username); ok {
		writeJSON(w, http.StatusOK, a)
		return
	}
	writeJSON(w, http.StatusOK, nil)
}

// linkAccount resolves the claimed in-game name against Mojang and records the mapping.
//
// This is a CLAIM, not a proof: the user could type someone else's name. That is a deliberate,
// accepted trade — the alternative needs an Azure app that Microsoft/Mojang must approve for
// third-party use. The blast radius is small: the worst a liar achieves is putting a stranger's name
// on a server they were already allowed to join. Account.Verified stays false so a real ownership
// proof can be layered on later without a migration.
func (s *Server) linkAccount(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	p, err := s.mc.Lookup(r.Context(), strings.TrimSpace(body.Name))
	if errors.Is(err, mcapi.ErrNoSuchPlayer) {
		writeErr(w, http.StatusNotFound, "No Minecraft account with that name")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Could not reach the Minecraft account service")
		return
	}
	a, err := s.st.LinkAccount(u.Username, p.UUID, p.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the account")
		return
	}
	// A newly linked account changes who can be whitelisted, so re-apply every server they are on.
	s.resyncFor(r.Context(), u.Username)
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) unlinkAccount(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if err := s.st.UnlinkAccount(u.Username); err != nil {
		writeErr(w, http.StatusNotFound, "No linked account")
		return
	}
	s.resyncFor(r.Context(), u.Username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// resyncFor re-applies the whitelist of every server the user belongs to, after their account
// mapping changed. Best-effort: a server that is down just gets its files rewritten.
func (s *Server) resyncFor(ctx context.Context, user string) {
	for _, srv := range s.st.Servers() {
		if srv.Owner != user {
			if _, ok := s.resolve(ctx, srv)[user]; !ok {
				continue
			}
		}
		_ = s.applyMembers(ctx, srv)
	}
}

// avatar serves a member's Minecraft face as a PNG.
//
// The path carries a Linux username, not a UUID: the UUID is hosuto's to know, and putting it in a
// URL the browser holds would leak every member's Mojang identity into logs and history for no gain.
//
// A member with no linked account, or one whose profile has no skin, gets a 404 — and the SDK's
// <Avatar> falls back to their initials on an image error. That fallback is the honest outcome: a
// made-up Steve face would claim a skin they do not have.
func (s *Server) avatar(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	user := strings.TrimSuffix(r.PathValue("user"), ".png")
	acc, ok := s.st.Account(user)
	if !ok {
		writeErr(w, http.StatusNotFound, "No linked account")
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	png, err := s.skin.Face(r.Context(), acc.UUID, size)
	if err != nil {
		writeErr(w, http.StatusNotFound, "No skin")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// The face is derived from a skin the member can change at will, but it changes rarely. An hour of
	// browser cache keeps a member list from re-fetching a dozen PNGs on every render, while a skin
	// change still lands the same day. The renderer holds its own six-hour cache behind this.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(png)
}

// ── servers ───────────────────────────────────────────────────────────────────────────

// view is a server as the UI sees it, with the caller's relationship to it attached.
type view struct {
	store.Server
	Owned  bool            `json:"owned"`
	Level  string          `json:"level,omitempty"` // the caller's level on a joinable server
	Status *runtime.Status `json:"status,omitempty"`
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	ctx := r.Context()
	owned, joinable := []view{}, []view{}
	for _, srv := range s.st.Servers() {
		st := s.rt.Status(ctx, srv)
		switch {
		case srv.Owner == u.Username:
			owned = append(owned, view{Server: redact(srv), Owned: true, Status: &st})
		case u.Can(rights.GroupAdmin):
			joinable = append(joinable, view{Server: redact(srv), Status: &st})
		default:
			if lvl, ok := s.resolve(ctx, srv)[u.Username]; ok {
				joinable = append(joinable, view{Server: redact(srv), Level: lvl, Status: &st})
			}
		}
	}
	acc, has := s.st.Account(u.Username)
	var account any
	if has {
		account = acc
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owned": owned, "joinable": joinable, "account": account,
		"canHost": u.Can(rights.GroupHost),
	})
}

// redact strips what must never leave the daemon. RconPass is json:"-" already; the backend ports
// are internal detail too — a player needs the domain, not the loopback port behind mc-router.
func redact(srv store.Server) store.Server {
	srv.RconPass = ""
	srv.RconPort = 0
	return srv
}

func (s *Server) createServer(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		Name          string `json:"name"`
		Slug          string `json:"slug"`
		MCVersion     string `json:"mcVersion"`
		Loader        string `json:"loader"`
		LoaderVersion string `json:"loaderVersion"`
		HeapMB        int    `json:"heapMB"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	body.Slug = strings.ToLower(strings.TrimSpace(body.Slug))
	if !store.SlugRe.MatchString(body.Slug) {
		writeErr(w, http.StatusBadRequest, "The address may use lowercase letters, digits and dashes")
		return
	}
	if !store.ValidLoader(body.Loader) {
		writeErr(w, http.StatusBadRequest, "Unknown loader")
		return
	}
	if body.MCVersion == "" {
		writeErr(w, http.StatusBadRequest, "Pick a Minecraft version")
		return
	}
	// The slug is a public DNS label, so it is globally unique, not per-user.
	if s.st.SlugTaken(body.Slug) {
		writeErr(w, http.StatusConflict, "That address is already taken")
		return
	}
	if err := s.supported(r.Context(), body.Loader, body.MCVersion); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The per-user cap is not a nicety: each server is gigabytes of heap on a box that also runs the
	// rest of the landscape. devlab caps previews for exactly this reason.
	max := s.cfg.Int("maxServersPerUser", 3)
	if !u.Can(rights.GroupAdmin) && s.st.CountOwnedBy(u.Username) >= max {
		writeErr(w, http.StatusForbidden,
			fmt.Sprintf("You already have %d servers — remove one first", max))
		return
	}

	heap := body.HeapMB
	if heap <= 0 {
		heap = s.cfg.Int("defaultHeapMB", 2048)
	}
	if lim := s.cfg.Int("maxHeapMB", 4096); heap > lim {
		heap = lim
	}
	port, rconPort, err := s.rt.AllocatePorts()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "No free port — the server pool is full")
		return
	}
	pass, err := mcfiles.GenRconPassword()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not generate a control password")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = body.Slug
	}

	srv := store.Server{
		Slug: body.Slug, Name: name, Owner: u.Username,
		MCVersion: body.MCVersion, Loader: body.Loader, LoaderVersion: body.LoaderVersion,
		HeapMB: heap, Port: port, RconPort: rconPort, RconPass: pass,
		Host: s.rt.Host(body.Slug), JoinPolicy: "whitelist",
	}
	// Record it first: a failure part-way through Create then leaves a row the user can retry or
	// delete, rather than an orphaned directory nobody knows about.
	srv, err = s.st.CreateServer(srv)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the server")
		return
	}
	if srv, err = s.rt.Create(r.Context(), srv); err != nil {
		_ = s.st.DeleteServer(srv.ID)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.st.UpdateServer(srv); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the server")
		return
	}
	writeJSON(w, http.StatusOK, redact(srv))
}

// supported reports whether a loader actually has a build for a Minecraft version.
//
// It exists because the failure without it is awful: mod loaders lag behind Minecraft by weeks, so
// "newest Minecraft + NeoForge" — the most natural thing to pick, and the default the form offers —
// silently has no builds at all. The version list simply comes back empty, hosuto happily creates the
// server, and it dies minutes later on a 404 while downloading an installer that was never published.
// Paper is worse: its API answers 410 Gone for a version it does not know.
//
// So ask BEFORE committing anything, and say the true thing.
func (s *Server) supported(ctx context.Context, loader, mcVersion string) error {
	if loader == "vanilla" {
		return nil // every Minecraft version has a vanilla server by definition
	}
	vs, err := s.vc.LoaderVersions(ctx, loader, mcVersion)
	if err != nil || len(vs) == 0 {
		return fmt.Errorf("%s has no build for Minecraft %s yet — pick an older Minecraft version, or a different loader",
			loaderName(loader), mcVersion)
	}
	return nil
}

func loaderName(l string) string {
	switch l {
	case "fabric":
		return "Fabric"
	case "neoforge":
		return "NeoForge"
	case "paper":
		return "Paper"
	}
	return l
}

// owned resolves the server and checks the caller may CONTROL it (owner or admin).
func (s *Server) owned(r *http.Request, u *auth.User) (store.Server, bool) {
	srv, ok := s.st.Server(r.PathValue("id"))
	if !ok {
		return store.Server{}, false
	}
	return srv, srv.Owner == u.Username || u.Can(rights.GroupAdmin)
}

// visible resolves the server and checks the caller may SEE it (owner, admin, or a member).
func (s *Server) visible(r *http.Request, u *auth.User) (store.Server, bool) {
	srv, ok := s.st.Server(r.PathValue("id"))
	if !ok {
		return store.Server{}, false
	}
	if srv.Owner == u.Username || u.Can(rights.GroupAdmin) {
		return srv, true
	}
	_, member := s.resolve(r.Context(), srv)[u.Username]
	return srv, member
}

func (s *Server) getServer(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	st := s.rt.Status(r.Context(), srv)
	writeJSON(w, http.StatusOK, view{Server: redact(srv), Owned: srv.Owner == u.Username, Status: &st})
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if err := s.rt.Destroy(r.Context(), srv); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.st.DeleteServer(srv.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not remove the server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// lifecycle drives start/stop/restart. An op-level member may control a server, not only its owner —
// that is what "op" means to the people using it.
func (s *Server) lifecycle(action string) handler {
	return func(w http.ResponseWriter, r *http.Request, u *auth.User) {
		srv, ok := s.st.Server(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "No such server")
			return
		}
		if srv.Owner != u.Username && !u.Can(rights.GroupAdmin) {
			if s.resolve(r.Context(), srv)[u.Username] != "op" {
				writeErr(w, http.StatusForbidden, "You may not control this server")
				return
			}
		}
		var err error
		switch action {
		case "start":
			err = s.rt.Start(r.Context(), srv)
		case "stop":
			err = s.rt.Stop(r.Context(), srv)
		case "restart":
			err = s.rt.Restart(r.Context(), srv)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	writeJSON(w, http.StatusOK, s.rt.Status(r.Context(), srv))
}

// ── members: the contax mapping ───────────────────────────────────────────────────────

// resolve expands a server's grants into the Linux usernames that may join, and at what level.
//
// Membership is resolved LIVE on every call. A contax group's members and an OS group's members are
// never copied into hosuto's store: contax owns its groups, the OS owns its groups, and hosuto owns
// only the grant that points at them. Copying would be exactly the parallel data path the Single
// Source of Truth maxim forbids — and it would go stale the moment someone leaves a group.
//
// "op" wins over "play" when a user is reachable through more than one grant.
func (s *Server) resolve(ctx context.Context, srv store.Server) map[string]string {
	out := map[string]string{}
	set := func(user, level string) {
		if user == "" || user == srv.Owner {
			return
		}
		if out[user] == "op" {
			return
		}
		out[user] = level
	}
	for _, g := range srv.Grants {
		switch g.Kind {
		case "adhoc":
			for _, m := range g.Members {
				set(m, g.Level)
			}
		case "contax":
			// A contax lookup that fails (contax down, secret unset) resolves to NO members rather
			// than to stale ones. Failing closed here means a member briefly loses access; failing
			// open would mean someone removed from a group keeps it. The former is recoverable.
			members, _ := s.cx.Members(g.Ref)
			for _, m := range members {
				set(m, g.Level)
			}
		case "holistic":
			for _, m := range s.dir.GroupMembers(g.Ref) {
				set(m, g.Level)
			}
		}
	}
	return out
}

// canAdd decides whether `actor` may add `target` to a server.
//
// This is the heart of the feature, and it is enforced ONLY here — the browser's contact picker is a
// convenience, not a control. The rule mirrors contax's own visibility rule, because it is the same
// question: two members are acquainted iff they share an hc_* contact group (privleg materialises
// those from a group flagged contactVisibility). hosuto reads the Linux groups directly rather than
// asking contax, exactly as contax reads them rather than asking privleg — the kernel can answer
// this, so putting a service in the request path would be a parallel data path.
//
// A contax PERSONAL group is a separate case, handled in addMembers: there the actor must be a
// member of the group they are granting, which only contax can answer.
func (s *Server) canAdd(actor, target string, actorIsAdmin bool) error {
	if target == actor {
		return errors.New("that is you")
	}
	if !s.dir.IsManaged(target) {
		return fmt.Errorf("%s is not a member here", target)
	}
	if !actorIsAdmin && !s.dir.Knows(actor, target) {
		return fmt.Errorf("you and %s are not contacts", target)
	}
	if _, ok := s.st.Account(target); !ok {
		return fmt.Errorf("%s has not linked a Minecraft account yet", target)
	}
	return nil
}

type playerView struct {
	User       string `json:"user"`
	Name       string `json:"name,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Level      string `json:"level"`
	HasAccount bool   `json:"hasAccount"`
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	players := []playerView{}
	add := func(user, level string) {
		acc, has := s.st.Account(user)
		players = append(players, playerView{
			User: user, Name: acc.Name, UUID: acc.UUID, Level: level, HasAccount: has,
		})
	}
	add(srv.Owner, "op")
	for user, level := range s.resolve(r.Context(), srv) {
		add(user, level)
	}
	sort.Slice(players, func(i, j int) bool { return players[i].User < players[j].User })
	grants := srv.Grants
	if grants == nil {
		grants = []store.Grant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy": srv.JoinPolicy, "grants": grants, "players": players,
	})
}

func (s *Server) addMembers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		Kind    string   `json:"kind"`
		Ref     string   `json:"ref"`
		Label   string   `json:"label"`
		Level   string   `json:"level"`
		Members []string `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	if body.Level == "" {
		body.Level = "play"
	}
	if !store.ValidKind(body.Kind) || !store.ValidLevel(body.Level) {
		writeErr(w, http.StatusBadRequest, "Unknown member kind or level")
		return
	}
	isAdmin := u.Can(rights.GroupAdmin)

	switch body.Kind {
	case "adhoc":
		if len(body.Members) == 0 {
			writeErr(w, http.StatusBadRequest, "Nobody selected")
			return
		}
		for _, m := range body.Members {
			if err := s.canAdd(u.Username, m, isAdmin); err != nil {
				writeErr(w, http.StatusForbidden, err.Error())
				return
			}
		}
	case "contax":
		// The actor may grant a contax group only if they are IN it. contax's machine-to-machine
		// endpoint is the authority — an actor who is not a member, or a group that does not exist,
		// is refused. This is the "oder in einer contax Gruppe" half of the rule.
		//
		// `ok` distinguishes "contax says the group is empty" from "contax could not be reached".
		// Granting on an unreachable contax would write an access rule nobody can audit.
		members, ok := s.cx.Members(body.Ref)
		if !ok || len(members) == 0 {
			writeErr(w, http.StatusBadGateway, "Could not resolve that group")
			return
		}
		if !isAdmin && !contains(members, u.Username) {
			writeErr(w, http.StatusForbidden, "You are not a member of that group")
			return
		}
	case "holistic":
		// A raw OS group is not a relationship a member established, so only an admin may grant one.
		if !isAdmin {
			writeErr(w, http.StatusForbidden, "Only an administrator may add a system group")
			return
		}
	}

	g, err := s.st.AddGrant(srv.ID, store.Grant{
		Kind: body.Kind, Ref: body.Ref, Label: body.Label, Level: body.Level, Members: body.Members,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not add the member")
		return
	}
	srv, _ = s.st.Server(srv.ID)
	if err := s.applyMembers(r.Context(), srv); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyAdded(r.Context(), srv, u.Username)
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if err := s.st.RemoveGrant(srv.ID, r.PathValue("grantId")); err != nil {
		writeErr(w, http.StatusNotFound, "No such member")
		return
	}
	srv, _ = s.st.Server(srv.ID)
	if err := s.applyMembers(r.Context(), srv); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyMembers turns the grant set into whitelist.json + ops.json and reloads a running server.
//
// A member with no linked Minecraft account is skipped: there is nothing to write. canAdd() refuses
// to add such a member in the first place — but a member can UNLINK later, and the whitelist must
// then stop naming them rather than keep a stale entry that no longer belongs to anyone.
func (s *Server) applyMembers(ctx context.Context, srv store.Server) error {
	var entries []mcfiles.Entry
	var ops []mcfiles.Op
	add := func(user, level string) {
		acc, ok := s.st.Account(user)
		if !ok {
			return
		}
		entries = append(entries, mcfiles.Entry{UUID: acc.UUID, Name: acc.Name})
		if level == "op" {
			ops = append(ops, mcfiles.Op{UUID: acc.UUID, Name: acc.Name, Level: 4})
		}
	}
	add(srv.Owner, "op")
	for user, level := range s.resolve(ctx, srv) {
		add(user, level)
	}
	return s.rt.ApplyWhitelist(ctx, srv, entries, ops)
}

// notifyAdded tells the people who just gained access. Best-effort and always deduped, per notify's
// contract — a failure to notify must never fail the operation that succeeded.
func (s *Server) notifyAdded(ctx context.Context, srv store.Server, actor string) {
	for user := range s.resolve(ctx, srv) {
		_ = s.nt.Emit(notify.EmitInput{
			User:    user,
			Service: service,
			Title:   "Server freigegeben",
			Body:    fmt.Sprintf("%s hat dich zu %q hinzugefügt.", actor, srv.Name),
			URL:     "/app/hosuto",
			Level:   "info",
			Dedupe:  "hosuto-member-" + srv.ID + "-" + user,
		})
	}
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		JoinPolicy string `json:"joinPolicy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !store.ValidPolicy(body.JoinPolicy) {
		writeErr(w, http.StatusBadRequest, "Unknown join policy")
		return
	}
	srv.JoinPolicy = body.JoinPolicy
	if err := s.st.UpdateServer(srv); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the policy")
		return
	}
	if err := s.rt.SyncConfig(r.Context(), srv); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// white-list is read at startup, so a running server needs a restart to pick the change up. Say
	// so on the server rather than pretending it took effect.
	s.rt.Say(r.Context(), srv, "hosuto: join policy is now "+body.JoinPolicy+" (restart to apply)")
	writeJSON(w, http.StatusOK, map[string]any{
		"joinPolicy": srv.JoinPolicy, "restartRequired": true,
	})
}

// ── mods & versions ───────────────────────────────────────────────────────────────────

func (s *Server) listMods(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	mods := srv.Mods
	if mods == nil {
		mods = []store.Mod{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mods": mods, "loader": srv.Loader,
		"hasClientMods": store.LoaderHasClientMods(srv.Loader),
	})
}

func (s *Server) addMod(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if !store.LoaderHasClientMods(srv.Loader) {
		writeErr(w, http.StatusBadRequest, "This server's loader does not run mods")
		return
	}
	var body struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProjectID == "" {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	ver, hit, err := s.mr.Resolve(r.Context(), body.ProjectID, srv.MCVersion, srv.Loader)
	if err != nil {
		writeErr(w, http.StatusNotFound, "No build of that mod for this version and loader")
		return
	}
	// A mod the server cannot run must never be installed on it. (The mirror rule — a mod the client
	// cannot run must never be exported — lives in the export package.)
	if hit.ServerSide == "unsupported" {
		writeErr(w, http.StatusBadRequest, "That mod is client-only — it does not belong on the server")
		return
	}
	file := primary(ver)
	if file.URL == "" {
		writeErr(w, http.StatusBadGateway, "That mod has no downloadable file")
		return
	}
	dir := filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the mods folder")
		return
	}
	if err := s.mr.Download(r.Context(), file, filepath.Join(dir, file.Filename)); err != nil {
		writeErr(w, http.StatusBadGateway, "Could not download the mod")
		return
	}
	m, err := s.st.AddMod(srv.ID, store.Mod{
		Source: "modrinth", ProjectID: hit.ProjectID, VersionID: ver.ID,
		Name: hit.Title, Filename: file.Filename, URL: file.URL,
		SHA1: file.SHA1, SHA512: file.SHA512, Size: file.Size,
		ClientSide: orUnknown(hit.ClientSide), ServerSide: orUnknown(hit.ServerSide),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not record the mod")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// orUnknown records a missing environment field honestly rather than guessing. A guess here would
// silently put a client-only mod on a server, or withhold a required mod from a player.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func primary(v modrinth.Version) modrinth.File {
	for _, f := range v.Files {
		if f.Primary {
			return f
		}
	}
	for _, f := range v.Files {
		if strings.HasSuffix(f.Filename, ".jar") {
			return f
		}
	}
	return modrinth.File{}
}

func (s *Server) removeMod(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	m, err := s.st.RemoveMod(srv.ID, r.PathValue("modId"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "No such mod")
		return
	}
	_ = os.Remove(filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods", m.Filename))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setVersion changes the Minecraft version and/or the loader.
//
// The mod set is NOT silently carried over. A mod jar is built for one (version, loader) pair, and
// leaving an incompatible jar in mods/ is the most common way to get a server that dies on boot with
// an unreadable stack trace. Every Modrinth mod is re-resolved against the new pair; the ones with
// no matching build are removed AND reported back, so the user learns what they lost instead of
// discovering it from a crash.
func (s *Server) setVersion(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		MCVersion     string `json:"mcVersion"`
		Loader        string `json:"loader"`
		LoaderVersion string `json:"loaderVersion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !store.ValidLoader(body.Loader) {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	ctx := r.Context()
	dir := runtime.Dir(srv.Owner, srv.Slug)

	// Refuse a combination that has no builds BEFORE touching the installed server. Without this, a
	// version change to "newest Minecraft + NeoForge" would delete the working jar, fail to fetch a
	// replacement that does not exist, and leave the server unbootable.
	if err := s.supported(ctx, body.Loader, body.MCVersion); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	argv, err := s.vc.Install(ctx, dir, body.Loader, body.MCVersion, body.LoaderVersion, srv.HeapMB)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "exec.argv"),
		[]byte(strings.Join(argv, "\n")+"\n"), 0o640); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not update the launch command")
		return
	}

	kept := []store.Mod{}
	dropped := []string{}
	for _, m := range srv.Mods {
		if m.Source != "modrinth" {
			kept = append(kept, m) // an uploaded jar is the user's business; we cannot re-resolve it
			continue
		}
		ver, hit, err := s.mr.Resolve(ctx, m.ProjectID, body.MCVersion, body.Loader)
		if err != nil {
			_ = os.Remove(filepath.Join(dir, "mods", m.Filename))
			dropped = append(dropped, m.Name)
			continue
		}
		f := primary(ver)
		_ = os.Remove(filepath.Join(dir, "mods", m.Filename))
		if err := s.mr.Download(ctx, f, filepath.Join(dir, "mods", f.Filename)); err != nil {
			dropped = append(dropped, m.Name)
			continue
		}
		m.VersionID, m.Filename, m.URL = ver.ID, f.Filename, f.URL
		m.SHA1, m.SHA512, m.Size = f.SHA1, f.SHA512, f.Size
		m.ClientSide, m.ServerSide = orUnknown(hit.ClientSide), orUnknown(hit.ServerSide)
		kept = append(kept, m)
	}

	srv.MCVersion, srv.Loader, srv.LoaderVersion = body.MCVersion, body.Loader, body.LoaderVersion
	srv.Mods = kept
	if err := s.st.UpdateServer(srv); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the version")
		return
	}
	if err := s.rt.SyncConfig(ctx, srv); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": redact(srv), "dropped": dropped})
}

func (s *Server) catalogVersions(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	v, err := s.vc.MinecraftVersions(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Could not reach the Minecraft version service")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": v})
}

func (s *Server) catalogLoaders(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	q := r.URL.Query()
	v, err := s.vc.LoaderVersions(r.Context(), q.Get("loader"), q.Get("mcVersion"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Could not reach the loader service")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": v})
}

func (s *Server) catalogMods(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	hits, err := s.mr.Search(r.Context(), q.Get("q"), q.Get("mcVersion"), q.Get("loader"), limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Could not reach Modrinth")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mods": hits})
}

// ── client export ─────────────────────────────────────────────────────────────────────

// fetcher hands the export package a downloader bound to this request. The export package does no
// HTTP of its own: it decides WHAT belongs in a bundle, not how bytes arrive.
func (s *Server) fetcher(ctx context.Context) export.Fetcher {
	return func(_ context.Context, url, _ string) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("download %s: status %d", url, resp.StatusCode)
		}
		return resp.Body, nil
	}
}

// sendExport streams one of the three client bundles.
//
// Nothing is materialised on disk or held in memory — the ZIP is written straight to the socket. The
// cost is that once the first byte is out, the status line is gone: a mid-stream failure can only be
// signalled by cutting the connection, which the browser sees as a truncated (invalid) archive. That
// is the honest failure, and it is strictly better than a silently incomplete modpack that fails at
// the player's end with a cryptic crash.
func (s *Server) sendExport(w http.ResponseWriter, r *http.Request, u *auth.User, suffix string,
	write func(srv store.Server, jarDir string) error) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if !store.LoaderHasClientMods(srv.Loader) {
		writeErr(w, http.StatusBadRequest, "This server has no client mods to export")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+srv.Slug+"-"+suffix+`"`)
	w.Header().Set("Cache-Control", "no-store")
	jarDir := filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods")
	if err := write(srv, jarDir); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) exportMods(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.sendExport(w, r, u, "mods.zip", func(srv store.Server, jarDir string) error {
		return export.WriteModsZip(w, srv.Mods, jarDir, s.fetcher(r.Context()))
	})
}

func (s *Server) exportMrpack(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.sendExport(w, r, u, "ez2go.mrpack", func(srv store.Server, jarDir string) error {
		return export.WriteMrpack(w, srv, jarDir, s.fetcher(r.Context()))
	})
}

func (s *Server) exportPrism(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.sendExport(w, r, u, "prism.zip", func(srv store.Server, jarDir string) error {
		return export.WritePrismZip(w, srv, jarDir, s.fetcher(r.Context()))
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────────────

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
