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
	"strconv"
	"strings"
	"sync"
	"time"

	"hosuto/internal/access"
	"hosuto/internal/aigentic"
	"hosuto/internal/auth"
	"hosuto/internal/chathub"
	"hosuto/internal/chatstore"
	"hosuto/internal/contax"
	"hosuto/internal/directory"
	"hosuto/internal/export"
	"hosuto/internal/files"
	"hosuto/internal/hconfig"
	"hosuto/internal/jobs"
	"hosuto/internal/mcapi"
	"hosuto/internal/mcfiles"
	"hosuto/internal/mcp"
	"hosuto/internal/modrinth"
	"hosuto/internal/notify"
	"hosuto/internal/pairing"
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
	v     *auth.Verifier
	st    *store.Store
	cfg   *hconfig.Config
	rt    *runtime.Manager
	dir   *directory.Directory
	cx    *contax.Client
	nt    *notify.Client
	mc    *mcapi.Client
	mr    *modrinth.Client
	vc    *versions.Client
	skin  *skin.Renderer
	tok   *mcp.TokenStore
	pair  *pairing.Codes
	chats *chatstore.Store
	hub   *chathub.Hub
	acc   *access.Resolver
	ai    *aigentic.Client
	seen  *seenUUIDs
	http  *http.Client
	// jobs tracks the work that outlives its request — a migration moves gigabytes, which no HTTP
	// round trip should be asked to hold open. dataDir is where hosuto keeps its own files (template
	// payloads, staged uploads), beside the state file rather than under the servers root.
	jobs    *jobs.Registry
	dataDir string
}

// New wires the HTTP layer.
func New(v *auth.Verifier, st *store.Store, cfg *hconfig.Config, rt *runtime.Manager,
	dir *directory.Directory, cx *contax.Client, nt *notify.Client, mc *mcapi.Client,
	mr *modrinth.Client, vc *versions.Client, sk *skin.Renderer, tok *mcp.TokenStore,
	chats *chatstore.Store, hub *chathub.Hub, acc *access.Resolver, ai *aigentic.Client,
	jobsReg *jobs.Registry, dataDir string) *Server {
	return &Server{v: v, st: st, cfg: cfg, rt: rt, dir: dir, cx: cx, nt: nt, mc: mc, mr: mr, vc: vc,
		skin: sk, tok: tok, pair: pairing.New(pairingTTL), chats: chats, hub: hub, acc: acc, ai: ai,
		seen: newSeenUUIDs(searchFaceTTL),
		http: &http.Client{Timeout: 60 * time.Second}, jobs: jobsReg, dataDir: dataDir}
}

// pairingTTL is how long a desktop pairing code stays claimable. Long enough to walk from the browser
// to the app, short enough that a code read over someone's shoulder is worthless by the time it is
// tried. It takes no config: a knob here would only ever be turned the wrong way.
const pairingTTL = 5 * time.Minute

// searchFaceTTL is how long a searched-but-not-yet-admitted account stays renderable. It only has to
// outlive the owner reading the dropdown and deciding; once they admit the account, the grant itself
// makes the face renderable and this no longer matters.
const searchFaceTTL = 15 * time.Minute

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

	// Finding a game account by name, without linking or granting anything. Both flows that need one
	// — linking your own, admitting someone else's — ask here first, so neither has to guess whether
	// the name exists before it acts on it.
	mux.HandleFunc("GET "+base+"minecraft/search", s.guard(rights.GroupPlay, false, s.searchMinecraft))

	// A member's face, rendered from their Minecraft skin. hosuto owns the account mapping, so hosuto
	// is the only service that can serve this.
	mux.HandleFunc("GET "+base+"avatar/{user}", s.guard(rights.GroupPlay, false, s.avatar))

	mux.HandleFunc("GET "+base+"servers", s.guard(rights.GroupPlay, false, s.listServers))
	mux.HandleFunc("POST "+base+"servers", s.guard(rights.GroupHost, true, s.createServer))
	// Migration: the same act as a create, but it moves gigabytes off an upload or a foreign host, so
	// it answers with a job to watch instead of holding the request open for minutes. See migrate.go.
	mux.HandleFunc("POST "+base+"servers/import", s.guard(rights.GroupHost, true, s.importServer))
	mux.HandleFunc("GET "+base+"servers/{id}", s.guard(rights.GroupPlay, false, s.getServer))
	mux.HandleFunc("DELETE "+base+"servers/{id}", s.guard(rights.GroupHost, true, s.deleteServer))

	mux.HandleFunc("POST "+base+"servers/{id}/start", s.guard(rights.GroupPlay, true, s.lifecycle("start")))
	mux.HandleFunc("POST "+base+"servers/{id}/stop", s.guard(rights.GroupPlay, true, s.lifecycle("stop")))
	mux.HandleFunc("POST "+base+"servers/{id}/restart", s.guard(rights.GroupPlay, true, s.lifecycle("restart")))
	mux.HandleFunc("GET "+base+"servers/{id}/status", s.guard(rights.GroupPlay, false, s.status))
	mux.HandleFunc("GET "+base+"servers/{id}/diagnose", s.guard(rights.GroupPlay, false, s.diagnose))
	mux.HandleFunc("GET "+base+"servers/{id}/players/online", s.guard(rights.GroupPlay, false, s.onlinePlayers))
	mux.HandleFunc("PUT "+base+"servers/{id}/autostart", s.guard(rights.GroupHost, true, s.setAutostart))

	// Spieledateien: the server's own on-disk tree, browsed through the holistic Files UI. Owner-only
	// throughout — the tree holds configs, worlds and the rcon password, not a merely-a-member surface.
	// The coarse right gates the route; treeFor() enforces ownership inside every handler.
	mux.HandleFunc("GET "+base+"servers/{id}/fs/roots", s.guard(rights.GroupPlay, false, s.fsRoots))
	mux.HandleFunc("GET "+base+"servers/{id}/fs/list", s.guard(rights.GroupPlay, false, s.fsList))
	mux.HandleFunc("GET "+base+"servers/{id}/fs/download", s.guard(rights.GroupPlay, false, s.fsServe(true)))
	mux.HandleFunc("GET "+base+"servers/{id}/fs/raw", s.guard(rights.GroupPlay, false, s.fsServe(false)))
	mux.HandleFunc("GET "+base+"servers/{id}/fs/text", s.guard(rights.GroupPlay, false, s.fsText))
	mux.HandleFunc("POST "+base+"servers/{id}/fs/mkdir", s.guard(rights.GroupHost, true, s.fsMkdir))
	mux.HandleFunc("POST "+base+"servers/{id}/fs/rename", s.guard(rights.GroupHost, true, s.fsRename))
	mux.HandleFunc("POST "+base+"servers/{id}/fs/move", s.guard(rights.GroupHost, true, s.fsMove))
	mux.HandleFunc("POST "+base+"servers/{id}/fs/copy", s.guard(rights.GroupHost, true, s.fsCopy))
	mux.HandleFunc("POST "+base+"servers/{id}/fs/delete", s.guard(rights.GroupHost, true, s.fsDelete))
	mux.HandleFunc("POST "+base+"servers/{id}/fs/upload", s.guard(rights.GroupHost, true, s.fsUpload))

	mux.HandleFunc("GET "+base+"servers/{id}/members", s.guard(rights.GroupPlay, false, s.listMembers))
	mux.HandleFunc("POST "+base+"servers/{id}/members", s.guard(rights.GroupHost, true, s.addMembers))
	mux.HandleFunc("DELETE "+base+"servers/{id}/members/{grantId}", s.guard(rights.GroupHost, true, s.removeMember))
	mux.HandleFunc("PUT "+base+"servers/{id}/policy", s.guard(rights.GroupHost, true, s.setPolicy))

	mux.HandleFunc("GET "+base+"servers/{id}/mods", s.guard(rights.GroupPlay, false, s.listMods))
	mux.HandleFunc("POST "+base+"servers/{id}/mods", s.guard(rights.GroupHost, true, s.addMod))
	mux.HandleFunc("DELETE "+base+"servers/{id}/mods/{modId}", s.guard(rights.GroupHost, true, s.removeMod))
	mux.HandleFunc("PUT "+base+"servers/{id}/version", s.guard(rights.GroupHost, true, s.setVersion))

	// Templates: a server saved as a recipe, and instantiated back into a new one. Owned like a
	// server (creator + admin), because a payload carries the source server's config files.
	mux.HandleFunc("GET "+base+"templates", s.guard(rights.GroupHost, false, s.listTemplates))
	mux.HandleFunc("POST "+base+"templates", s.guard(rights.GroupHost, true, s.createTemplate))
	mux.HandleFunc("DELETE "+base+"templates/{tid}", s.guard(rights.GroupHost, true, s.deleteTemplate))

	// Background work (a migration, packing a template). The UI polls these.
	mux.HandleFunc("GET "+base+"jobs", s.guard(rights.GroupHost, false, s.listJobs))
	mux.HandleFunc("GET "+base+"jobs/{jid}", s.guard(rights.GroupHost, false, s.job))
	mux.HandleFunc("DELETE "+base+"jobs/{jid}", s.guard(rights.GroupHost, true, s.cancelJob))

	mux.HandleFunc("GET "+base+"catalog/versions", s.guard(rights.GroupPlay, false, s.catalogVersions))
	mux.HandleFunc("GET "+base+"catalog/loaders", s.guard(rights.GroupPlay, false, s.catalogLoaders))
	mux.HandleFunc("GET "+base+"catalog/mods", s.guard(rights.GroupPlay, false, s.catalogMods))

	mux.HandleFunc("GET "+base+"servers/{id}/export/mods", s.guard(rights.GroupPlay, false, s.exportMods))
	mux.HandleFunc("GET "+base+"servers/{id}/export/mrpack", s.guard(rights.GroupPlay, false, s.exportMrpack))
	mux.HandleFunc("GET "+base+"servers/{id}/export/prism", s.guard(rights.GroupPlay, false, s.exportPrism))

	// The MCP surface. The endpoint itself speaks JSON-RPC and authenticates a bearer token or the
	// session cookie inside the handler (see authenticateMCP), so it is mounted raw, not behind guard.
	// The token routes that mint/list/revoke those bearer tokens ARE behind guard, like any other API.
	mux.Handle(base+"mcp", s.mcp().Handler(s.authenticateMCP))
	mux.HandleFunc("POST "+base+"mcp/token", s.guard(rights.GroupPlay, true, s.mintMCPToken))
	mux.HandleFunc("GET "+base+"mcp/token", s.guard(rights.GroupPlay, false, s.mcpTokenStatus))
	mux.HandleFunc("DELETE "+base+"mcp/token", s.guard(rights.GroupPlay, true, s.revokeMCPToken))

	// Device pairing for the desktop client. start runs in the browser, where the user already has a
	// session; claim is the one deliberately UNAUTHENTICATED route on this surface, because the caller
	// is a freshly installed app that has no session yet — the code it presents IS the credential, and
	// that is the entire point of pairing. See pairing.go.
	mux.HandleFunc("POST "+base+"pair/start", s.guard(rights.GroupPlay, true, s.startPairing))
	mux.HandleFunc("POST "+base+"pair/claim", s.claimPairing)

	// The "Ask AI" chats are SHARED per server, persisted here and visible to every operator of the
	// server — many conversations, managed like aigentic's own chat. The coarse right gates the route;
	// controlled() enforces operator access (owner, admin, or an op-level member) inside every handler.
	mux.HandleFunc("GET "+base+"servers/{id}/chats", s.guard(rights.GroupPlay, false, s.listChats))
	mux.HandleFunc("POST "+base+"servers/{id}/chats", s.guard(rights.GroupPlay, true, s.createChat))
	mux.HandleFunc("GET "+base+"servers/{id}/chats/{cid}", s.guard(rights.GroupPlay, false, s.getChat))
	mux.HandleFunc("POST "+base+"servers/{id}/chats/{cid}", s.guard(rights.GroupPlay, true, s.appendChat))
	mux.HandleFunc("DELETE "+base+"servers/{id}/chats/{cid}", s.guard(rights.GroupPlay, true, s.deleteChat))
	// Real-time: a live event stream per conversation (new turns + who is typing/asking) and the
	// presence heartbeat operators post while they type or wait for the AI.
	mux.HandleFunc("GET "+base+"servers/{id}/chats/{cid}/events", s.guard(rights.GroupPlay, false, s.chatEvents))
	mux.HandleFunc("POST "+base+"servers/{id}/chats/{cid}/presence", s.guard(rights.GroupPlay, true, s.chatPresence))

	return mux
}

// resolveCaller establishes WHO is calling, from either of the two credentials this surface accepts: a
// same-origin browser presents the session cookie; a client with no browser to speak of — the Windows
// desktop app, or an external MCP client — presents a bearer token it was minted. Either way the
// identity is resolved to live OS groups: the credential names WHO, the kernel decides WHAT.
//
// It returns the server a bearer token is bound to (empty for a cookie, and for an account-wide token),
// and whether a bearer was used at all — which is the CSRF decision. The double-submit exists because a
// browser attaches its cookie to a cross-site request of its own accord; a bearer token, sent by
// nothing but the client holding it, cannot suffer that, so demanding a CSRF header of it would be
// cargo cult.
func (s *Server) resolveCaller(r *http.Request) (*auth.User, string, bool, error) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		subject, scope, ok := s.tok.Lookup(strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
		if !ok {
			return nil, "", true, errors.New("invalid or expired token")
		}
		u, ok := s.v.Resolve(subject)
		if !ok {
			return nil, "", true, errors.New("unknown account")
		}
		return u, scope, true, nil
	}
	u, err := s.v.User(r)
	if err != nil {
		return nil, "", false, err
	}
	return u, "", false, nil
}

// guard authenticates, optionally requires a right, and optionally enforces the CSRF double-submit.
func (s *Server) guard(perm string, csrf bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, scope, viaBearer, err := s.resolveCaller(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission for this action")
			return
		}
		if csrf && !viaBearer && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		if scope != "" && !s.boundTo(r, scope) {
			writeErr(w, http.StatusForbidden, "This token is bound to a different server")
			return
		}
		h(w, r, u)
	}
}

// boundTo reports whether the request targets the one server a bearer token is bound to.
//
// A server-scoped token is a narrow credential and must stay narrow on this surface too, or minting
// "a token for just this server" would quietly hand over every server the user can reach. Every route
// that acts on a server names it in {id}; a route that names none is account-wide and therefore out of
// a bound token's reach. Fail closed — the desktop client pairs an account-wide token, which is the
// only kind that drives this surface freely.
func (s *Server) boundTo(r *http.Request, scope string) bool {
	ref := r.PathValue("id")
	if ref == "" {
		return false
	}
	srv, ok := s.findServer(ref)
	return ok && srv.ID == scope
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": service,
		"version": version,
		"user":    u.Username,
		"zone":    s.cfg.String("zone", ""),
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

// searchMinecraft finds game accounts by name, from the two sources that can answer.
//
// Mojang has NO name-search: its API answers an exact name and nothing else — there is no prefix or
// fuzzy endpoint to call, at any price. So a search box over "all Minecraft accounts" cannot exist,
// and pretending otherwise would mean scraping a third-party site, which is both an availability
// dependency and someone else's data.
//
// What can be answered honestly is the union of:
//
//	LOCAL  — accounts this owner has admitted before, matched by prefix. Instant, free, and the
//	         common case: the same friends get added to one server after another.
//	MOJANG — the exact name, if the query names an account not already known here. This is the
//	         only way to reach somebody entirely new.
//
// The Mojang half is rate-limited for the whole host (~200 requests per two minutes, shared), so it
// is skipped whenever a local match already answers the query exactly, mcapi refuses an impossible
// name without spending a request, and the UI only asks once the user stops typing.
func (s *Server) searchMinecraft(w http.ResponseWriter, r *http.Request, u *auth.User) {
	q := strings.TrimSpace(r.URL.Query().Get("name"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"matches": []mcapi.Profile{}})
		return
	}

	matches := []mcapi.Profile{}
	seen := map[string]bool{}
	exact := false
	for _, p := range s.knownAccounts(u, q) {
		matches = append(matches, p)
		seen[strings.ToLower(p.UUID)] = true
		exact = exact || strings.EqualFold(p.Name, q)
	}

	// Only spend Mojang's budget on a name nothing here already answers.
	if !exact {
		if p, err := s.mc.Lookup(r.Context(), q); err == nil && !seen[strings.ToLower(p.UUID)] {
			matches = append(matches, p)
		}
	}
	// A face is fetched by UUID for an account nobody has been admitted under yet, so the renderer
	// has to accept these until the owner decides. See seenUUIDs.
	for _, p := range matches {
		s.seen.mark(p.UUID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

// maxSuggestions bounds the local half of a search. A dropdown is a shortlist; past a handful the
// owner is better served by typing the name out.
const maxSuggestions = 8

// knownAccounts is the local half of searchMinecraft: Minecraft accounts already admitted on the
// caller's own servers, whose name starts with the query.
//
// It suggests only what the caller put there themselves (an admin sees all, as everywhere). Linked
// MEMBERS are deliberately absent: they are added through the contact picker, which enforces contax
// visibility, and offering them here too would both duplicate that path and leak who else exists.
func (s *Server) knownAccounts(u *auth.User, q string) []mcapi.Profile {
	prefix := strings.ToLower(q)
	isAdmin := u.Can(rights.GroupAdmin)
	var out []mcapi.Profile
	seen := map[string]bool{}
	for _, srv := range s.st.Servers() {
		if srv.Owner != u.Username && !isAdmin {
			continue
		}
		for _, g := range srv.Grants {
			if g.Kind != "minecraft" || !strings.HasPrefix(strings.ToLower(g.Label), prefix) {
				continue
			}
			key := strings.ToLower(g.Ref)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, mcapi.Profile{UUID: g.Ref, Name: g.Label})
			if len(out) == maxSuggestions {
				return out
			}
		}
	}
	return out
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

// seenUUIDs remembers accounts this daemon resolved for a search, so their face can be rendered
// while the owner is still deciding whether to admit them.
//
// It is what keeps the face route from being an open Mojang proxy without making the search useless:
// a UUID gets in only by coming back from a lookup an authenticated caller already paid for, and it
// ages out shortly after, so the set stays small and cannot be grown into a general permit.
type seenUUIDs struct {
	mu  sync.Mutex
	at  map[string]time.Time
	ttl time.Duration
	now func() time.Time
}

func newSeenUUIDs(ttl time.Duration) *seenUUIDs {
	return &seenUUIDs{at: map[string]time.Time{}, ttl: ttl, now: time.Now}
}

// mark records a UUID as freshly resolved, dropping any that have aged out. Pruning on write keeps
// the map bounded by what one search burst can produce, with no goroutine to own.
func (s *seenUUIDs) mark(uuid string) {
	key := strings.ToLower(strings.TrimSpace(uuid))
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, t := range s.at {
		if now.Sub(t) > s.ttl {
			delete(s.at, k)
		}
	}
	s.at[key] = now
}

func (s *seenUUIDs) has(uuid string) bool {
	key := strings.ToLower(strings.TrimSpace(uuid))
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[key]
	return ok && s.now().Sub(t) <= s.ttl
}

// avatar serves a player's Minecraft face as a PNG.
//
// For a MEMBER the path carries their Linux username, never their UUID: the UUID is hosuto's to
// know, and putting it in a URL the browser holds would leak every member's Mojang identity into
// logs and history for no gain.
//
// A directly-admitted Minecraft account has no username to carry, so for those the path carries the
// dashed UUID — which gives nothing away, because it is the identity the owner typed in to admit
// them and belongs to no member here. The two cannot be confused: the account lookup runs first, so
// a real member always wins, and only the strict 8-4-4-4-12 form is read as a UUID.
//
// A member with no linked account, or a profile with no skin, gets a 404 — and the SDK's <Avatar>
// falls back to their initials on an image error. That fallback is the honest outcome: a made-up
// Steve face would claim a skin they do not have.
func (s *Server) avatar(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	ref := strings.TrimSuffix(r.PathValue("user"), ".png")
	uuid := ""
	if acc, ok := s.st.Account(ref); ok {
		uuid = acc.UUID
	} else if len(ref) == 36 {
		// Only a UUID this landscape actually plays with, or one it just resolved for a search, or
		// the route would render faces for any UUID a member cares to name — on a Mojang budget the
		// whole host shares.
		if u, err := mcapi.Dash(ref); err == nil && (s.acc.AdmittedUUID(u) || s.seen.has(u)) {
			uuid = u
		}
	}
	if uuid == "" {
		writeErr(w, http.StatusNotFound, "No linked account")
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	png, err := s.skin.Face(r.Context(), uuid, size)
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
	// Drift is reported on Status, where it is masked by the real run state — one truth, one place.
	// Repeating the raw flag on the record would only let the two disagree.
	srv.RestartRequired = false
	return srv
}

// createServer makes a server either blank or from a template. The third way in — migrating one from
// an archive or a foreign host — is importServer in migrate.go, which answers with a job because it
// moves gigabytes; everything the three share (slug rules, the quota, ports, the record) lives in
// reserve() so the rules that protect the host cannot drift between them.
func (s *Server) createServer(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		Name          string `json:"name"`
		Slug          string `json:"slug"`
		MCVersion     string `json:"mcVersion"`
		Loader        string `json:"loader"`
		LoaderVersion string `json:"loaderVersion"`
		HeapMB        int    `json:"heapMB"`
		TemplateID    string `json:"templateId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}

	// A template already carries the version, the loader and the heap, so the form does not ask for
	// them again and they are not validated here — they were validated when the source server was
	// made, and the payload on disk was built for exactly that pair.
	var tpl store.Template
	fromTpl := strings.TrimSpace(body.TemplateID) != ""
	if fromTpl {
		t, ok := s.visibleTemplate(body.TemplateID, u)
		if !ok {
			writeErr(w, http.StatusNotFound, "No such template")
			return
		}
		if _, err := os.Stat(s.templatePath(t.ID)); err != nil {
			writeErr(w, http.StatusConflict, "That template's files are missing")
			return
		}
		tpl = t
		if body.HeapMB <= 0 {
			body.HeapMB = t.HeapMB
		}
	} else {
		if !store.ValidLoader(body.Loader) {
			writeErr(w, http.StatusBadRequest, "Unknown loader")
			return
		}
		if body.MCVersion == "" {
			writeErr(w, http.StatusBadRequest, "Pick a Minecraft version")
			return
		}
		if err := s.supported(r.Context(), body.Loader, body.MCVersion); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// A template supplies the version/loader/heap it was built for; a blank create takes them from
	// the form. Either way they are settled BEFORE the record is registered — the store validates the
	// loader on insert, so a record cannot be completed after the fact.
	proto := store.Server{
		Name: body.Name, Slug: body.Slug, HeapMB: body.HeapMB,
		MCVersion: body.MCVersion, Loader: body.Loader, LoaderVersion: body.LoaderVersion,
	}
	if fromTpl {
		proto.MCVersion, proto.Loader, proto.LoaderVersion = tpl.MCVersion, tpl.Loader, tpl.LoaderVersion
	}
	srv, code, err := s.reserve(u, proto)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}

	if fromTpl {
		srv, err = s.fromTemplate(r.Context(), srv, tpl)
	} else {
		srv, err = s.rt.Create(r.Context(), srv)
	}
	if err != nil {
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
	_ = s.chats.DeleteAll(srv.ID) // the shared chats go with the server they belonged to
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

// onlinePlayers reports who is currently connected, read from the server console (authoritative,
// unlike the SLP sample). Each name is mapped back to its holistic user when it belongs to a linked
// account, so the UI can render that member's face; an open server's guests appear by name alone.
func (s *Server) onlinePlayers(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	names, ok := s.rt.OnlinePlayers(r.Context(), srv)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"reachable": false, "online": []any{}})
		return
	}
	type onlinePlayer struct {
		Name string `json:"name"`
		User string `json:"user,omitempty"`
	}
	out := make([]onlinePlayer, 0, len(names))
	for _, n := range names {
		e := onlinePlayer{Name: n}
		if user, ok := s.acc.UserByName(n); ok {
			e.User = user
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"reachable": true, "online": out})
}

// diagnose answers "the start failed and I don't know why": it returns the tail of the server's
// console log plus a short, plain explanation from the AI. The AI runs on the CALLER's own aigentic
// credential (server-to-server) and degrades silently — the log is always returned, the diagnosis is
// best-effort, so an operator without an AI credential still gets the log. Gated like start/restart
// (owner, admin, or an op-level member): if you can start the server you may see why it would not.
func (s *Server) diagnose(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	state := s.rt.State(r.Context(), srv)
	logTail, _ := s.rt.LogTail(srv, 160)
	resp := map[string]any{"state": state, "log": logTail}
	if strings.TrimSpace(logTail) != "" && s.ai != nil && s.ai.Enabled() {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res, err := s.ai.Run(ctx, u.Username, aigentic.Req{
			Kind: "choose",
			System: "You are diagnosing why a Minecraft server failed to start, from its console log. " +
				"Answer in at most two short, plain sentences: what concretely went wrong and how to fix it. " +
				"If a mod is missing a required dependency, name the mod and the dependency. No markdown, no preamble.",
			Prompt: "Console log tail:\n\n" + logTail,
		})
		switch {
		case err == nil:
			resp["diagnosis"] = strings.TrimSpace(res.Output)
			resp["engine"] = res.Engine
			resp["model"] = res.Model
		case errors.Is(err, aigentic.ErrNoCredential):
			resp["diagnosisError"] = "no-credential"
		default:
			resp["diagnosisError"] = "failed"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// setAutostart toggles whether the server comes up with the OS. It never starts or stops the server
// now — that is the point of a separate control from the start/stop buttons.
func (s *Server) setAutostart(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.owned(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	if err := s.rt.SetAutostart(r.Context(), srv, body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"autostart": body.Enabled})
}

// ── Spieledateien: the server's on-disk tree ──────────────────────────────────────────
//
// The files package is confined to the one server directory (symlink escapes and traversal are
// refused there); this layer only adds the ownership gate and maps its errors to HTTP. Every handler
// requires the caller to OWN the server (or be admin) — the file tree holds configs, worlds and rcon
// passwords, so it is not a merely-a-member surface.

// treeFor resolves the owned server and opens its confined tree.
func (s *Server) treeFor(r *http.Request, u *auth.User) (store.Server, *files.Tree, bool) {
	srv, ok := s.owned(r, u)
	if !ok {
		return store.Server{}, nil, false
	}
	tr, err := files.Open(runtime.Dir(srv.Owner, srv.Slug))
	if err != nil {
		return srv, nil, false
	}
	return srv, tr, true
}

// fsError maps a files-package error to an HTTP status + message.
func fsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrNotFound):
		writeErr(w, http.StatusNotFound, "No such file")
	case errors.Is(err, files.ErrDenied):
		writeErr(w, http.StatusForbidden, "Not allowed")
	case errors.Is(err, files.ErrExists):
		writeErr(w, http.StatusConflict, "A file with that name already exists")
	case errors.Is(err, files.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "Invalid request")
	default:
		writeErr(w, http.StatusInternalServerError, "File operation failed")
	}
}

func (s *Server) fsRoots(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	writeJSON(w, http.StatusOK, tr.Roots())
}

func (s *Server) fsList(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	path := r.URL.Query().Get("path")
	entries, err := tr.List(path)
	if err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": entries})
}

// fsServe streams a file, either as a download (attachment) or inline (for image/media/pdf preview
// and thumbnails).
func (s *Server) fsServe(download bool) handler {
	return func(w http.ResponseWriter, r *http.Request, u *auth.User) {
		_, tr, ok := s.treeFor(r, u)
		if !ok {
			writeErr(w, http.StatusNotFound, "No such server")
			return
		}
		f, e, err := tr.OpenFile(r.URL.Query().Get("path"))
		if err != nil {
			fsError(w, err)
			return
		}
		defer f.Close()
		if e.Mime != "" {
			w.Header().Set("Content-Type", e.Mime)
		}
		if download {
			w.Header().Set("Content-Disposition", `attachment; filename="`+files.DownloadName(e)+`"`)
		}
		w.Header().Set("Cache-Control", "private, no-store")
		http.ServeContent(w, r, e.Name, time.UnixMilli(e.MTime), f)
	}
}

func (s *Server) fsText(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	// 256 KiB is plenty for a config or the tail a person reads; a 200 MB latest.log is not something
	// to shovel into the browser. Truncation is reported so the viewer can say so.
	content, truncated, err := tr.ReadText(r.URL.Query().Get("path"), 256<<10)
	if err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": content, "truncated": truncated})
}

func (s *Server) fsMkdir(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct{ Path, Name string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	if err := tr.Mkdir(body.Path, body.Name); err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) fsRename(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct{ Path, NewName string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	if err := tr.Rename(body.Path, body.NewName); err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) fsMove(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.fsRelocate(w, r, u, false)
}
func (s *Server) fsCopy(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.fsRelocate(w, r, u, true)
}

func (s *Server) fsRelocate(w http.ResponseWriter, r *http.Request, u *auth.User, cp bool) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct{ Src, DstDir string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	var err error
	if cp {
		err = tr.Copy(body.Src, body.DstDir)
	} else {
		err = tr.Move(body.Src, body.DstDir)
	}
	if err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) fsDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	if err := tr.Delete(body.Path, body.Recursive); err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) fsUpload(w http.ResponseWriter, r *http.Request, u *auth.User) {
	_, tr, ok := s.treeFor(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	// The upload is streamed to a temp file, so it is never buffered in memory; 32 KiB is only the
	// multipart part-header budget, not the file size.
	if err := r.ParseMultipartForm(32 << 10); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed upload")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "No file")
		return
	}
	defer f.Close()
	if err := tr.Save(r.FormValue("path"), hdr.Filename, f); err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── members: the contax mapping ───────────────────────────────────────────────────────

// resolve expands a server's grants into the Linux usernames that may join, and at what level.
//
// Membership is resolved LIVE on every call. A contax group's members and an OS group's members are
// never copied into hosuto's store: contax owns its groups, the OS owns its groups, and hosuto owns
// only the grant that points at them. Copying would be exactly the parallel data path the Single
// Source of Truth maxim forbids — and it would go stale the moment someone leaves a group.
//
// "op" wins over "play" when a user is reachable through more than one grant. The rule lives in
// package access (shared with the in-game CLI); this is a shim so every call site here is unchanged.
func (s *Server) resolve(ctx context.Context, srv store.Server) map[string]string {
	return s.acc.Resolve(ctx, srv)
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

// Why admitting a game account can fail. They are sentinels rather than strings because the HTTP
// surface has to turn them back into status codes, and the MCP surface must not have to.
var (
	errMcUnknown     = errors.New("no Minecraft account with that name")
	errMcUnreachable = errors.New("could not reach the Minecraft account service")
	errMcAlreadyOn   = errors.New("already on this server")
)

// admitMinecraft resolves an in-game name into the grant that would admit it, WITHOUT saving it.
//
// It is the one place a bare game account becomes a player, shared by the dashboard and the MCP
// tool: what a name resolves to, and when admitting it is refused, is one rule and must not be
// written twice. Saving is left to the caller, because the dashboard adds this grant through the
// same path as every other kind.
//
// The name is resolved to a UUID once, here, and the UUID is what gets stored: it survives a Mojang
// rename, and it is the key whitelist.json is actually read by. The label keeps Mojang's spelling
// so the player list reads the way the player writes their own name.
func (s *Server) admitMinecraft(ctx context.Context, srv store.Server, name, level string) (store.Grant, error) {
	p, err := s.mc.Lookup(ctx, strings.TrimSpace(name))
	if errors.Is(err, mcapi.ErrNoSuchPlayer) {
		return store.Grant{}, errMcUnknown
	}
	if err != nil {
		return store.Grant{}, errMcUnreachable
	}
	for _, g := range srv.Grants {
		if g.Kind == "minecraft" && strings.EqualFold(g.Ref, p.UUID) {
			return store.Grant{}, fmt.Errorf("%s is %w", p.Name, errMcAlreadyOn)
		}
	}
	return store.Grant{Kind: "minecraft", Ref: p.UUID, Label: p.Name, Level: level}, nil
}

// admitStatus maps an admission failure to the status the browser should see.
func admitStatus(err error) int {
	switch {
	case errors.Is(err, errMcUnknown):
		return http.StatusNotFound
	case errors.Is(err, errMcAlreadyOn):
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
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
	for _, p := range s.acc.Players(r.Context(), srv) {
		players = append(players, playerView{
			User: p.User, Name: p.Name, UUID: p.UUID, Level: p.Level,
			// "has an account to write to the whitelist", which is what the UI warns about. A
			// directly-admitted Minecraft account is nothing but such an account, so it is never
			// the thing being warned about even though no holistic user stands behind it.
			HasAccount: p.UUID != "",
		})
	}
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
	case "minecraft":
		// Admitting a bare game account is the owner's call alone — and this handler already ran
		// s.owned(), so no further check belongs here. It deliberately skips canAdd: that gate asks
		// whether two MEMBERS of this landscape are acquainted, and there is no member on the other
		// side of this grant to be acquainted with.
		g, err := s.admitMinecraft(r.Context(), srv, body.Label, body.Level)
		if err != nil {
			writeErr(w, admitStatus(err), err.Error())
			return
		}
		body.Ref, body.Label, body.Members = g.Ref, g.Label, nil
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
	for _, p := range s.acc.Players(ctx, srv) {
		if p.UUID == "" {
			continue
		}
		entries = append(entries, mcfiles.Entry{UUID: p.UUID, Name: p.Name})
		if p.Level == "op" {
			ops = append(ops, mcfiles.Op{UUID: p.UUID, Name: p.Name, Level: 4})
		}
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
	srv, err := s.applyPolicy(r.Context(), srv, body.JoinPolicy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	var body struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProjectID == "" {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	m, deps, err := s.installMod(r.Context(), srv, body.ProjectID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mod": m, "dependencies": deps})
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
	if err := s.uninstallMod(r.Context(), srv, r.PathValue("modId")); err != nil {
		writeErr(w, http.StatusNotFound, "No such mod")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setVersion changes the Minecraft version and/or the loader.
//
// The mod set is NOT silently carried over ACROSS A PAIR CHANGE. A mod jar is built for one
// (Minecraft version, loader) pair, and leaving an incompatible jar in mods/ is the most common way
// to get a server that dies on boot with an unreadable stack trace. So when that pair changes, every
// Modrinth mod is re-resolved against the new one; the ones with no matching build are removed AND
// reported back, so the user learns what they lost instead of discovering it from a crash.
//
// Changing only the loader BUILD is a different thing and must not touch the mods. A jar is not built
// against NeoForge 21.1.236 as opposed to 21.1.240 — it is built against Minecraft 1.21.1 on NeoForge,
// and both builds run it. Re-resolving anyway would quietly upgrade every mod to whatever is newest
// today, which is actively harmful in the case that matters most: a server migrated from another host,
// whose players' clients are already synced to the exact versions it came with. They would be locked
// out by a version mismatch they never asked for.
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

	argv, resolvedLoader, err := s.vc.Install(ctx, dir, body.Loader, body.MCVersion, body.LoaderVersion, srv.HeapMB)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "exec.argv"),
		[]byte(strings.Join(argv, "\n")+"\n"), 0o640); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not update the launch command")
		return
	}

	// The pair is what a mod jar is built against. If it did not change, the installed jars are still
	// exactly right and are left alone — see the note on this function.
	pairChanged := body.MCVersion != srv.MCVersion || body.Loader != srv.Loader

	kept := []store.Mod{}
	dropped := []string{}
	for _, m := range srv.Mods {
		if !pairChanged || m.Source != "modrinth" {
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

	// resolvedLoader, not body.LoaderVersion: an empty request means "newest", and Install is the only
	// thing that knows which one that turned out to be.
	srv.MCVersion, srv.Loader, srv.LoaderVersion = body.MCVersion, body.Loader, resolvedLoader
	srv.Mods = kept
	// exec.argv now names a different loader build (and possibly a different Minecraft), and on a pair
	// change every jar under mods/ was replaced too. Either way a live server is running none of it
	// until it is bounced — no exceptions worth carving out here.
	srv.RestartRequired = true
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
// write receives the io.Writer to pack into — the counting wrapper below, never the raw
// ResponseWriter. Handing it over rather than letting each caller close over w is what keeps the byte
// count honest, and the count is the whole basis on which a failure is reported or aborted.
func (s *Server) sendExport(w http.ResponseWriter, r *http.Request, u *auth.User, suffix string,
	write func(dst io.Writer, srv store.Server, jarDir string) error) {
	srv, ok := s.visible(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if !store.LoaderHasClientMods(srv.Loader) {
		writeErr(w, http.StatusBadRequest, "This server has no client mods to export")
		return
	}
	jarDir := filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "mods")

	// Count what reaches the client, because that decides how a failure may be reported.
	//
	// Once the first byte is out the status line is already sent, and the only honest way to end a
	// broken stream is to break the connection: ErrAbortHandler does exactly that, without the stack
	// trace a real panic would log. But an export can also fail with nothing written at all — a
	// server whose record names no loader version cannot be packed — and aborting THAT looks to the
	// browser like the download simply evaporated. It is reported as what it is instead.
	cw := &countingWriter{ResponseWriter: w}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+srv.Slug+"-"+suffix+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := write(cw, srv, jarDir); err != nil {
		if cw.n > 0 {
			panic(http.ErrAbortHandler)
		}
		// Nothing was committed, so this is an ordinary error response after all — and it must stop
		// looking like a download, or the browser saves the error as a file called "<slug>-prism.zip".
		w.Header().Del("Content-Disposition")
		writeErr(w, http.StatusConflict, err.Error())
	}
}

// countingWriter reports whether a handler has already committed bytes to the client.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) exportMods(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.sendExport(w, r, u, "mods.zip", func(dst io.Writer, srv store.Server, jarDir string) error {
		return export.WriteModsZip(dst, srv.Mods, jarDir, s.fetcher(r.Context()))
	})
}

func (s *Server) exportMrpack(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.sendExport(w, r, u, "ez2go.mrpack", func(dst io.Writer, srv store.Server, jarDir string) error {
		return export.WriteMrpack(dst, srv, jarDir, s.fetcher(r.Context()))
	})
}

func (s *Server) exportPrism(w http.ResponseWriter, r *http.Request, u *auth.User) {
	s.sendExport(w, r, u, "prism.zip", func(dst io.Writer, srv store.Server, jarDir string) error {
		return export.WritePrismZip(dst, srv, jarDir, s.fetcher(r.Context()))
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
