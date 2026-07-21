// Package store is hosuto's own persistence: the game-account mapping and the server registry.
//
// It follows the contax store shape — one flat JSON file, an atomic temp→fsync→rename write, one
// mutex, and the daemon as the sole writer. It deliberately holds only what hosuto OWNS:
//
//   - Accounts: the Linux user → game account mapping. This is the entity hosuto exists to own;
//     no other holistic service knows a user's Minecraft identity. Keyed by Linux username, which
//     is the landscape's single source of truth for identity.
//   - Servers: hosuto's metadata about a server (slug, version, loader, allocated ports, grants).
//
// It is NOT the truth for anything the filesystem already knows. Worlds, mods and server.properties
// live on disk under the owner's account and are read from there; the store never mirrors them.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPath is where the daemon keeps its state (owned by the hosuto system user).
const DefaultPath = "/var/lib/hosuto/state.json"

var (
	// ErrNotFound is returned for an unknown server, grant or account.
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when a slug is already taken.
	ErrExists = errors.New("already exists")
	// ErrInvalid is returned for a malformed slug, kind or level.
	ErrInvalid = errors.New("invalid")

	// errUnchanged lets a mutate closure report that it changed nothing, so the shared write path
	// returns the current record without a disk write. It never leaves the store: it is how a
	// conditional writer (SetRestartRequired) shares the one atomic path without a spurious save.
	errUnchanged = errors.New("unchanged")
)

// SlugRe bounds a server slug. It has to be safe in three namespaces at once, and the shortest of
// them sets the limit:
//
//   - a DNS label — it becomes <slug>.<zone>
//   - a systemd instance name — hosuto-mc@<owner>-<slug>.service
//   - a Linux username — the server's own run account is "hs-<slug>", and useradd caps a name at 32
//     characters, so the slug itself may not exceed 28.
//
// Hence 2–28 characters, lowercase, no dots and no underscores.
var SlugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,27}$`)

// Account is one Linux user's identity in one game. hosuto owns this mapping.
//
// Verified records whether the user PROVED ownership of the account (a future Microsoft OAuth
// flow) as opposed to merely claiming a name that resolved against Mojang. Nothing enforces
// Verified today; it exists so that turning verification on later is a policy change, not a
// migration.
type Account struct {
	User     string `json:"user"`     // Linux username — the join key to the whole landscape
	Game     string `json:"game"`     // "minecraft"
	UUID     string `json:"uuid"`     // dashed; whitelist.json rejects an entry without it
	Name     string `json:"name"`     // the in-game name at link time
	Verified bool   `json:"verified"` // ownership proven (always false for a name lookup)
	Linked   int64  `json:"linked"`
}

// Grant is one membership entry on a server. The shape is icaly's calendar-sharing model, which
// solved exactly this problem: let an owner share a resource with a person, a contax personal
// group, or an OS group, and resolve membership live at request time.
//
//	adhoc     — the owner picked specific people; Members holds their Linux usernames.
//	contax    — Ref is a contax personal group id (grp-xxxxxxxx); membership is resolved live.
//	holistic  — Ref is an OS group name (e.g. an hc_* contact group); membership is the Linux group.
//	minecraft — Ref is a dashed Mojang UUID: a game account admitted directly, with nobody in this
//	            landscape behind it. It is the one kind that resolves to no Linux user at all, so it
//	            grants the right to JOIN and nothing else — never dashboard access. Should that
//	            player later link the same account to a holistic user, access.Resolve finds them by
//	            UUID and the grant starts covering the person too, with no second grant to add.
type Grant struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`  // adhoc | contax | holistic | minecraft
	Ref     string   `json:"ref"`   // contax: grp-id · holistic: group name · minecraft: uuid · adhoc: ""
	Label   string   `json:"label"` // display label
	Level   string   `json:"level"` // play | op
	Members []string `json:"members,omitempty"`
	Created int64    `json:"created"`
}

// Mod is one mod installed on a server.
//
// ClientSide/ServerSide carry Modrinth's environment fields verbatim (required|optional|
// unsupported). They are what splits the server's mod set from the client's: a mod with
// ClientSide == "unsupported" must never reach the client export, and one with
// ServerSide == "unsupported" must never be installed on the server.
//
// Source distinguishes a Modrinth-resolved mod (which the export may reference by CDN URL + hash,
// and therefore need not redistribute) from a raw upload (which it can only ship as bytes).
type Mod struct {
	ID         string `json:"id"`
	Source     string `json:"source"` // modrinth | upload
	ProjectID  string `json:"projectId,omitempty"`
	VersionID  string `json:"versionId,omitempty"`
	Name       string `json:"name"`
	Filename   string `json:"filename"`
	URL        string `json:"url,omitempty"` // modrinth CDN url
	SHA1       string `json:"sha1,omitempty"`
	SHA512     string `json:"sha512,omitempty"`
	Size       int64  `json:"size,omitempty"`
	ClientSide string `json:"clientSide"` // required | optional | unsupported | unknown
	ServerSide string `json:"serverSide"`
	Added      int64  `json:"added"`
}

// Server is hosuto's metadata for one game server.
//
// RconPass is json:"-" on purpose: it is generated by the privileged wrapper and lives in the
// on-disk server.properties too, but it must never leave the daemon over the API.
type Server struct {
	ID            string  `json:"id"`   // srv-xxxxxxxx
	Slug          string  `json:"slug"` // DNS label + systemd instance name; globally unique
	Name          string  `json:"name"`
	Owner         string  `json:"owner"` // Linux username
	Game          string  `json:"game"`  // "minecraft"
	MCVersion     string  `json:"mcVersion"`
	Loader        string  `json:"loader"` // vanilla | fabric | neoforge | paper
	LoaderVersion string  `json:"loaderVersion,omitempty"`
	HeapMB        int     `json:"heapMB"`
	Port          int     `json:"port"`
	RconPort      int     `json:"rconPort"`
	RconPass      string  `json:"-"`
	Host          string  `json:"host"`       // <slug>.<zone>
	JoinPolicy    string  `json:"joinPolicy"` // whitelist | open
	Grants        []Grant `json:"grants,omitempty"`
	Mods          []Mod   `json:"mods,omitempty"`
	// RestartRequired records that a RUNNING server no longer matches this record's mod set — set
	// when the set changes under a live server, cleared by Start/Restart. It is persisted because a
	// daemon restart must not forget that the world is still running yesterday's mods, but it never
	// leaves the daemon on the server object: Status reports it, masked by the actual run state.
	RestartRequired bool  `json:"restartRequired,omitempty"`
	Created         int64 `json:"created"`
}

// Loader / policy / grant vocabularies. Kept as functions rather than maps so the compiler sees
// every legal value at the call site.
func ValidLoader(l string) bool {
	return l == "vanilla" || l == "fabric" || l == "neoforge" || l == "paper"
}
func ValidPolicy(p string) bool { return p == "whitelist" || p == "open" }
func ValidKind(k string) bool {
	return k == "adhoc" || k == "contax" || k == "holistic" || k == "minecraft"
}
func ValidLevel(l string) bool { return l == "play" || l == "op" }

// ModsOnly reports whether this loader runs client-side mods at all. Paper runs Bukkit PLUGINS,
// which are server-only: there is nothing to hand a Paper player. The UI must say so rather than
// offer an export that would always be empty.
func LoaderHasClientMods(l string) bool { return l == "fabric" || l == "neoforge" }

// Template is a saved server RECIPE: what a server was made of, so another one can be made the same
// way. It is created from an existing server and instantiated into a new one.
//
// The recipe lives here; the FILES it carries (config/, mods/, and the world when the creator asked
// for it) live in a payload zip beside the state file, named by ID. The split is the store's usual
// rule applied to a new entity — the store holds what hosuto knows, never a mirror of what is on
// disk — and it is what keeps state.json small when a template is four gigabytes of world.
//
// Mods is copied from the source server rather than re-derived from the payload: a Modrinth-resolved
// mod records its project and version, so instantiating a template can restore that provenance
// instead of degrading every mod to an anonymous jar the Modding tab cannot act on.
type Template struct {
	ID            string `json:"id"` // tpl-xxxxxxxx
	Name          string `json:"name"`
	Owner         string `json:"owner"` // Linux username; templates are owned like servers are
	Game          string `json:"game"`
	MCVersion     string `json:"mcVersion"`
	Loader        string `json:"loader"`
	LoaderVersion string `json:"loaderVersion,omitempty"`
	HeapMB        int    `json:"heapMB"`
	JoinPolicy    string `json:"joinPolicy"`
	Mods          []Mod  `json:"mods,omitempty"`
	// IncludeWorld records what the creator chose. A template without a world starts every server
	// from a fresh one; a template with it is a clone, and the UI must be able to say which it is
	// before someone instantiates four gigabytes by accident.
	IncludeWorld bool   `json:"includeWorld"`
	Size         int64  `json:"size"` // payload bytes, for the same reason
	SourceSlug   string `json:"sourceSlug,omitempty"`
	Created      int64  `json:"created"`
}

type state struct {
	Accounts  map[string]Account  `json:"accounts"`            // keyed by Linux username
	Servers   map[string]Server   `json:"servers"`             // keyed by server id
	Templates map[string]Template `json:"templates,omitempty"` // keyed by template id
}

// Store is the daemon's sole writer of hosuto state.
type Store struct {
	path string
	mu   sync.Mutex
	st   state
}

// Open loads the state file, creating an empty one if it does not exist.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath
	}
	s := &Store{path: path, st: state{
		Accounts: map[string]Account{}, Servers: map[string]Server{}, Templates: map[string]Template{},
	}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, err
	}
	if s.st.Accounts == nil {
		s.st.Accounts = map[string]Account{}
	}
	if s.st.Servers == nil {
		s.st.Servers = map[string]Server{}
	}
	// Absent in every state file written before templates existed, so it is created rather than
	// assumed — the same reason Accounts and Servers are checked above.
	if s.st.Templates == nil {
		s.st.Templates = map[string]Template{}
	}
	return s, nil
}

// save writes the state atomically: temp file in the same dir → fsync → rename. A crash mid-write
// therefore leaves the previous good state, never a truncated one. Caller holds the mutex.
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func genID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// mutate is the store's single read-modify-write path over one server. It takes the lock, reads the
// CURRENT record, applies fn, and persists the result — read, change and write share one critical
// section, so no concurrent writer can interleave (Atomare Zugriffe). fn must change only the fields
// it owns and must not call back into the store, which already holds the lock. Returning an error from
// fn aborts the write and propagates the error; returning errUnchanged aborts the write silently and
// yields the current record. Every server write in this package goes through here, so the atomicity
// guarantee holds in exactly one place rather than being re-derived at each call site.
func (s *Store) mutate(id string, fn func(*Server) error) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv, ok := s.st.Servers[id]
	if !ok {
		return Server{}, ErrNotFound
	}
	switch err := fn(&srv); err {
	case nil:
		// fall through to the write
	case errUnchanged:
		return srv, nil // no change, no observable write
	default:
		return Server{}, err
	}
	srv.Mods = identifyMods(srv.Mods)
	s.st.Servers[id] = srv
	if err := s.save(); err != nil {
		return Server{}, err
	}
	return srv, nil
}

// MutateServer applies fn to a server as one atomic read-modify-write and returns the saved record.
//
// This is the correct way to change a LIVE server: fn receives the store's CURRENT record (never a
// snapshot the caller read earlier and has been holding across other work), so a field a concurrent
// operation set in the meantime is preserved, not clobbered. Any slow work a change needs — resolving
// or downloading mods, talking to another service — must run BEFORE the call and be handed in through
// the closure, because fn runs under the store lock. If fn returns an error nothing is written and the
// error is returned; ErrNotFound comes back for an unknown id.
func (s *Store) MutateServer(id string, fn func(*Server) error) (Server, error) {
	return s.mutate(id, fn)
}

// ── accounts ──────────────────────────────────────────────────────────────────────────

// Account returns a user's linked game account.
func (s *Store) Account(user string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.st.Accounts[user]
	return a, ok
}

// Accounts returns every linked account, keyed by Linux username.
func (s *Store) Accounts() map[string]Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Account, len(s.st.Accounts))
	for k, v := range s.st.Accounts {
		out[k] = v
	}
	return out
}

// LinkAccount records (or replaces) a user's game account. The caller resolved uuid/name against
// Mojang; the store does not talk to the network.
func (s *Store) LinkAccount(user, uuid, name string) (Account, error) {
	if user == "" || uuid == "" || name == "" {
		return Account{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := Account{
		User: user, Game: "minecraft", UUID: uuid, Name: name,
		Verified: false, Linked: time.Now().Unix(),
	}
	s.st.Accounts[user] = a
	return a, s.save()
}

// UnlinkAccount drops a user's game account.
func (s *Store) UnlinkAccount(user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Accounts[user]; !ok {
		return ErrNotFound
	}
	delete(s.st.Accounts, user)
	return s.save()
}

// ── servers ───────────────────────────────────────────────────────────────────────────

// Server returns one server by id.
func (s *Store) Server(id string) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv, ok := s.st.Servers[id]
	return srv, ok
}

// Servers returns every server, sorted by slug (stable order for the UI).
func (s *Store) Servers() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Server, 0, len(s.st.Servers))
	for _, v := range s.st.Servers {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// OwnedBy returns the servers a user owns.
func (s *Store) OwnedBy(user string) []Server {
	var out []Server
	for _, srv := range s.Servers() {
		if srv.Owner == user {
			out = append(out, srv)
		}
	}
	return out
}

// CountOwnedBy is the per-user cap check. A server is a real resource commitment, so hosuto
// enforces a ceiling exactly as devlab does for previews.
func (s *Store) CountOwnedBy(user string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, srv := range s.st.Servers {
		if srv.Owner == user {
			n++
		}
	}
	return n
}

// SlugTaken reports whether a slug is in use. Slugs are globally unique because they become public
// DNS labels, not per-user names.
func (s *Store) SlugTaken(slug string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.st.Servers {
		if srv.Slug == slug {
			return true
		}
	}
	return false
}

// UsedPorts returns every port the store has already handed out (game + rcon), so the allocator
// can skip them before it even test-binds.
func (s *Store) UsedPorts() map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := map[int]bool{}
	for _, srv := range s.st.Servers {
		used[srv.Port] = true
		used[srv.RconPort] = true
	}
	return used
}

// CreateServer registers a new server. The caller has already validated the slug, allocated the
// ports and created the on-disk tree through the privileged wrapper.
func (s *Store) CreateServer(srv Server) (Server, error) {
	if !SlugRe.MatchString(srv.Slug) || !ValidLoader(srv.Loader) || !ValidPolicy(srv.JoinPolicy) {
		return Server{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.st.Servers {
		if e.Slug == srv.Slug {
			return Server{}, ErrExists
		}
	}
	srv.ID = genID("srv-")
	srv.Game = "minecraft"
	srv.Created = time.Now().Unix()
	srv.Mods = identifyMods(srv.Mods)
	s.st.Servers[srv.ID] = srv
	return srv, s.save()
}

// UpdateServer replaces a server record wholesale from a snapshot the caller owns exclusively. Use it
// only to FINALIZE a record the caller alone is provisioning — server creation and import — where no
// other writer holds the id yet, so replacing the whole record cannot lose a concurrent change. For an
// in-place change to a live server, use MutateServer: a wholesale write of a snapshot read earlier
// would silently drop a grant, mod or member added in between (the read-modify-write is not atomic).
func (s *Store) UpdateServer(srv Server) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Servers[srv.ID]; !ok {
		return ErrNotFound
	}
	srv.Mods = identifyMods(srv.Mods)
	s.st.Servers[srv.ID] = srv
	return s.save()
}

// DeleteServer drops a server record. The caller has already destroyed the unit and the on-disk
// tree through the privileged wrapper, and removed the mc-router route.
func (s *Store) DeleteServer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Servers[id]; !ok {
		return ErrNotFound
	}
	delete(s.st.Servers, id)
	return s.save()
}

// ── grants ────────────────────────────────────────────────────────────────────────────

// AddGrant records a membership entry on a server.
func (s *Store) AddGrant(serverID string, g Grant) (Grant, error) {
	if !ValidKind(g.Kind) || !ValidLevel(g.Level) {
		return Grant{}, ErrInvalid
	}
	_, err := s.mutate(serverID, func(srv *Server) error {
		g.ID = genID("grn-")
		g.Created = time.Now().Unix()
		srv.Grants = append(srv.Grants, g)
		return nil
	})
	return g, err
}

// RemoveGrant drops a membership entry.
func (s *Store) RemoveGrant(serverID, grantID string) error {
	_, err := s.mutate(serverID, func(srv *Server) error {
		out := srv.Grants[:0]
		found := false
		for _, g := range srv.Grants {
			if g.ID == grantID {
				found = true
				continue
			}
			out = append(out, g)
		}
		if !found {
			return ErrNotFound
		}
		srv.Grants = out
		return nil
	})
	return err
}

// ── mods ──────────────────────────────────────────────────────────────────────────────

// AddMod records an installed mod. The caller has already put the jar in the server's mods/ dir.
func (s *Store) AddMod(serverID string, m Mod) (Mod, error) {
	_, err := s.mutate(serverID, func(srv *Server) error {
		m.ID = genID("mod-")
		m.Added = time.Now().Unix()
		srv.Mods = append(srv.Mods, m)
		return nil
	})
	return m, err
}

// RemoveMod drops a mod record and returns it, so the caller can delete the jar.
func (s *Store) RemoveMod(serverID, modID string) (Mod, error) {
	var removed Mod
	_, err := s.mutate(serverID, func(srv *Server) error {
		out := srv.Mods[:0]
		found := false
		for _, m := range srv.Mods {
			if m.ID == modID {
				removed, found = m, true
				continue
			}
			out = append(out, m)
		}
		if !found {
			return ErrNotFound
		}
		srv.Mods = out
		return nil
	})
	if err != nil {
		return Mod{}, err
	}
	return removed, nil
}

// SetRestartRequired remembers whether the live server has drifted from its record. The store does not
// decide WHEN that is so — it is a passive pool, and the rule (which changes a running world actually
// cares about) lives with the operations, in api/ops.go.
func (s *Store) SetRestartRequired(serverID string, v bool) error {
	_, err := s.mutate(serverID, func(srv *Server) error {
		if srv.RestartRequired == v {
			return errUnchanged // no write for a no-op: every Start clears a flag that is usually already clear
		}
		srv.RestartRequired = v
		return nil
	})
	return err
}

// ── templates ─────────────────────────────────────────────────────────────────────────

// Template returns one template by id.
func (s *Store) Template(id string) (Template, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.st.Templates[id]
	return t, ok
}

// Templates returns every template, newest first — a template list is read chronologically ("the one
// I just made"), unlike the server list which is read by name.
func (s *Store) Templates() []Template {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Template, 0, len(s.st.Templates))
	for _, v := range s.st.Templates {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

// CreateTemplate registers a template. The caller has already written the payload zip.
func (s *Store) CreateTemplate(t Template) (Template, error) {
	if strings.TrimSpace(t.Name) == "" || t.Owner == "" || !ValidLoader(t.Loader) {
		return Template{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.ID = genID("tpl-")
	t.Game = "minecraft"
	t.Created = time.Now().Unix()
	s.st.Templates[t.ID] = t
	return t, s.save()
}

// DeleteTemplate drops a template record. The caller deletes the payload.
func (s *Store) DeleteTemplate(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Templates[id]; !ok {
		return ErrNotFound
	}
	delete(s.st.Templates, id)
	return s.save()
}

// SetMods replaces a server's mod set — used when the version/loader changes and every mod is
// re-resolved, and when a migrated or templated server adopts the mods that came with it.
//
// An entry with no id gets one here, and so does an entry with no timestamp. Identity is the store's
// to assign (AddMod does the same), and a mod without an id is not merely untidy: every operation
// the UI offers addresses a mod by id, so one that shipped without would be listed and then be
// impossible to remove.
func (s *Store) SetMods(serverID string, mods []Mod) error {
	_, err := s.mutate(serverID, func(srv *Server) error {
		srv.Mods = mods // mutate runs identifyMods on the way out
		return nil
	})
	return err
}

// identifyMods gives every mod an id and a timestamp. It runs on every write path that can carry a
// mod set (CreateServer, UpdateServer, SetMods), so the invariant holds regardless of how the set
// got here — a mod restored from a template or adopted from a migration arrives without one, and no
// caller should have to remember that.
func identifyMods(mods []Mod) []Mod {
	if len(mods) == 0 {
		return mods
	}
	now := time.Now().Unix()
	out := make([]Mod, 0, len(mods))
	for _, m := range mods {
		if m.ID == "" {
			m.ID = genID("mod-")
		}
		if m.Added == 0 {
			m.Added = now
		}
		out = append(out, m)
	}
	return out
}
