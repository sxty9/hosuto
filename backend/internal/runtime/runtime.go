// Package runtime is hosuto's control plane for a game server's life: it owns the on-disk tree, the
// systemd unit, the port allocation, the mc-router route and the live status.
//
// The privilege model, which everything here obeys:
//
//   - hosutod is unprivileged. Every OS-level write goes through the narrow, allow-listed wrapper
//     /usr/local/sbin/hosuto-server, invoked as `sudo -n`. The wrapper re-derives every guard from
//     the kernel and trusts nothing this package tells it.
//   - A server's directory is <owner>:hosuto, mode 2770 (setgid). The GAME PROCESS runs as the
//     OWNER — that is the isolation boundary, so a hostile mod is confined to its owner's rights.
//     The daemon is in the group so it can manage the config files it owns (server.properties,
//     whitelist.json, ops.json); it never runs game code.
//   - RCON and the game port are bound to 127.0.0.1 (via server-ip in server.properties — there is
//     no rcon.ip key). Nothing but mc-router and the daemon can reach a server.
//
// Reachability is deliberately two-sourced: systemd says whether the unit is running, and a Server
// List Ping through mc-router says whether a player could actually connect. Only the second one is
// the truth the "Erreichbarkeit" tab promises, so both are reported.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hosuto/internal/hconfig"
	"hosuto/internal/mcfiles"
	"hosuto/internal/mcnet"
	"hosuto/internal/router"
	"hosuto/internal/store"
	"hosuto/internal/versions"
)

const (
	// Root is the parent of every server tree. The wrapper independently confines writes to it.
	Root = "/var/lib/hosuto/servers"
	// wrapper is the ONLY command hosutod may run through sudo.
	wrapper = "/usr/local/sbin/hosuto-server"

	rconOffset = 100 // rcon port = game port + 100; keeps the two pools trivially disjoint
)

// ErrNoPort is returned when the game-port pool is exhausted.
var ErrNoPort = errors.New("no free port in the pool")

// Status is what the Erreichbarkeit tab shows.
type Status struct {
	State     string   `json:"state"`     // active | inactive | failed | activating
	Reachable bool     `json:"reachable"` // a real Server List Ping succeeded
	Online    int      `json:"online"`
	Max       int      `json:"max"`
	Sample    []string `json:"sample,omitempty"`
	Autostart bool     `json:"autostart"` // comes up with the OS
	// RestartRequired: the live server is running a mod set that no longer matches its record. Only
	// ever true while the server is actually up — a stopped one reads mods/ fresh on its next start,
	// so telling anyone to restart it would be nonsense.
	RestartRequired bool `json:"restartRequired,omitempty"`
}

// Manager drives server lifecycles.
type Manager struct {
	st  *store.Store
	cfg *hconfig.Config
	rt  *router.Client
	vc  *versions.Client
}

// New builds the manager.
func New(st *store.Store, cfg *hconfig.Config, rt *router.Client, vc *versions.Client) *Manager {
	return &Manager{st: st, cfg: cfg, rt: rt, vc: vc}
}

// Dir is a server's on-disk tree. Rebuilt from owner+slug, never stored, so a poisoned store record
// cannot redirect a write. (The wrapper re-derives it too and refuses anything outside Root.)
func Dir(owner, slug string) string { return filepath.Join(Root, owner, slug) }

// Instance is the systemd template instance name.
func Instance(owner, slug string) string { return owner + "-" + slug }

// Unit is the full systemd unit name.
func Unit(owner, slug string) string { return "hosuto-mc@" + Instance(owner, slug) + ".service" }

// Host is a server's public domain.
func (m *Manager) Host(slug string) string {
	return slug + "." + m.cfg.String("zone", "mc.henrysoase.org")
}

// run invokes the privileged wrapper, surfacing its stderr (the wrapper is the one that knows why
// it refused).
func run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", wrapper}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// AllocatePorts picks a free game port (and its rcon partner) from the configured pool.
//
// It skips ports the store already handed out, then TEST-BINDS the candidate. The test bind sets
// SO_REUSEADDR, because the JVM's ServerSocket does: without it a port left in TIME_WAIT by a
// server that just stopped would read as a collision, and the pool would churn on every restart.
func (m *Manager) AllocatePorts() (int, int, error) {
	lo := m.cfg.Int("portLo", 25601)
	hi := m.cfg.Int("portHi", 25699)
	used := m.st.UsedPorts()
	for p := lo; p <= hi; p++ {
		if used[p] || used[p+rconOffset] {
			continue
		}
		if free(p) && free(p+rconOffset) {
			return p, p + rconOffset, nil
		}
	}
	return 0, 0, ErrNoPort
}

// free test-binds a loopback port with SO_REUSEADDR (Go's net.Listen sets it by default, matching
// the JVM).
func free(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// Create lays down a server: the wrapper makes the tree and the unit drop-in, then hosuto installs
// the server jar, writes the files it owns, and registers the mc-router route.
//
// It is written to be safely re-runnable: a half-created server (say, the jar download died) can be
// created again without hand-cleanup.
func (m *Manager) Create(ctx context.Context, srv store.Server) (store.Server, error) {
	dir := Dir(srv.Owner, srv.Slug)

	if err := run(ctx, "create", srv.Owner, srv.Slug,
		strconv.Itoa(srv.HeapMB), strconv.Itoa(srv.Port), strconv.Itoa(srv.RconPort)); err != nil {
		return srv, fmt.Errorf("create server tree: %w", err)
	}

	// Install the jar/loader. This is the slow part (a download plus, for NeoForge, an installer run).
	//
	// Record the version Install actually chose, not the one that was asked for: "" means "give me the
	// newest", and a record that still says "" afterwards cannot be exported as a .mrpack or a Prism
	// instance — both must name a concrete loader version — nor can it tell anyone what is running.
	argv, resolved, err := m.vc.Install(ctx, dir, srv.Loader, srv.MCVersion, srv.LoaderVersion, srv.HeapMB)
	if err != nil {
		return srv, fmt.Errorf("install %s %s: %w", srv.Loader, srv.MCVersion, err)
	}
	srv.LoaderVersion = resolved
	// The unit reads its ExecStart argv from this file, so a version/loader change is a file write
	// plus a restart — no unit rewrite, no daemon-reload.
	if err := os.WriteFile(filepath.Join(dir, "exec.argv"), []byte(strings.Join(argv, "\n")+"\n"), 0o640); err != nil {
		return srv, err
	}

	if err := mcfiles.WriteEULA(filepath.Join(dir, "eula.txt")); err != nil {
		return srv, err
	}
	if err := m.writeProps(srv); err != nil {
		return srv, err
	}
	// Seed the owner into the whitelist. From JE 26.3 white-list defaults to true, so a server whose
	// whitelist is empty would lock out even the person who just created it.
	if acc, ok := m.st.Account(srv.Owner); ok {
		_ = mcfiles.WriteWhitelist(filepath.Join(dir, "whitelist.json"),
			[]mcfiles.Entry{{UUID: acc.UUID, Name: acc.Name}})
		_ = mcfiles.WriteOps(filepath.Join(dir, "ops.json"),
			[]mcfiles.Op{{UUID: acc.UUID, Name: acc.Name, Level: 4}})
	}

	if err := m.Route(ctx, srv); err != nil {
		return srv, fmt.Errorf("register route: %w", err)
	}
	return srv, nil
}

// writeProps merges hosuto's owned keys over whatever is already in server.properties, so a key the
// user or the server itself added survives. mcfiles.Apply is the single writer of those keys (it fails
// closed on an empty rcon password), so this path can never produce a file that boots with rcon
// disabled.
func (m *Manager) writeProps(srv store.Server) error {
	path := filepath.Join(Dir(srv.Owner, srv.Slug), "server.properties")
	p, err := mcfiles.ReadProps(path)
	if err != nil {
		return err
	}
	// rcon.password is the one hosuto-owned key NOT persisted in state (json:"-"); server.properties
	// is its source of truth. Prefer the in-memory value (fresh create), else keep the on-disk one
	// (after a daemon restart the field is empty), else mint a fresh one. Without this, a rewrite would
	// blank the password with the empty in-memory field and the server would silently disable rcon —
	// leaving hosuto (whitelist reload, MCP tools, the in-game "!ai") unable to reach the console.
	pass := srv.RconPass
	if pass == "" {
		pass = strings.TrimSpace(p["rcon.password"])
	}
	if pass == "" {
		if pass, err = mcfiles.GenRconPassword(); err != nil {
			return err
		}
	}
	// Preserve an admin-customised MOTD; default it to the server name only when unset.
	motd := srv.Name
	if existing, ok := p["motd"]; ok {
		motd = existing
	}
	p, err = mcfiles.Apply(p, mcfiles.Settings{
		Port:      srv.Port,
		RconPort:  srv.RconPort,
		RconPass:  pass,
		MOTD:      motd,
		Whitelist: srv.JoinPolicy == "whitelist",
	})
	if err != nil {
		return err
	}
	return mcfiles.WriteProps(path, p)
}

// SyncConfig re-applies the server.properties keys hosuto owns (after a policy or version change).
func (m *Manager) SyncConfig(ctx context.Context, srv store.Server) error { return m.writeProps(srv) }

// Route registers the server's domain with mc-router.
func (m *Manager) Route(ctx context.Context, srv store.Server) error {
	if !m.rt.Enabled() {
		return nil
	}
	return m.rt.Put(ctx, srv.Host, "127.0.0.1:"+strconv.Itoa(srv.Port))
}

// Unroute drops the domain from mc-router.
func (m *Manager) Unroute(ctx context.Context, srv store.Server) error {
	if !m.rt.Enabled() {
		return nil
	}
	return m.rt.Delete(ctx, srv.Host)
}

// SyncRoutes reconciles mc-router's table with the servers hosuto knows about. Called on daemon
// start so a crash between "delete server" and "delete route" cannot orphan a route that would keep
// pointing a live public domain at a recycled port.
func (m *Manager) SyncRoutes(ctx context.Context) error {
	if !m.rt.Enabled() {
		return nil
	}
	want := map[string]string{}
	for _, srv := range m.st.Servers() {
		want[srv.Host] = "127.0.0.1:" + strconv.Itoa(srv.Port)
	}
	if err := m.rt.Sync(ctx, want); err != nil {
		return err
	}
	return m.SyncDefault(ctx)
}

// SyncDefault points mc-router's fallback at the server named by the `defaultServer` config setting
// (a slug), or clears it when the setting is empty.
//
// Why it exists: a host with no port forward is reached through a tunnel (playit.gg and the like),
// and such a tunnel presents the connection under ITS OWN hostname. Our domain never appears in the
// handshake, so mc-router has nothing to match and refuses everything. A fallback is then the only
// way anyone connects at all.
//
// It is deliberately an explicit admin choice and not a default: on a host where several members
// have servers, a fallback hands every stray or guessed connection to whoever is named in it. It is
// safe exactly where it is needed — a host with one server — and unsafe everywhere else, which is
// why hosuto will not infer it.
func (m *Manager) SyncDefault(ctx context.Context) error {
	if !m.rt.Enabled() {
		return nil
	}
	slug := strings.TrimSpace(m.cfg.String("defaultServer", ""))
	if slug == "" {
		return m.rt.SetDefault(ctx, "")
	}
	for _, srv := range m.st.Servers() {
		if srv.Slug == slug {
			return m.rt.SetDefault(ctx, "127.0.0.1:"+strconv.Itoa(srv.Port))
		}
	}
	// The setting names a server that does not exist (a typo, or one that was deleted). Clearing is
	// the safe reading: better that nobody reaches anything than that everybody reaches the wrong one.
	return m.rt.SetDefault(ctx, "")
}

// Start/Stop/Restart drive the unit through the wrapper. Start and Restart re-assert hosuto's owned
// server.properties keys first, so a server always boots with the canonical config — in particular a
// valid rcon.password, even after a daemon restart dropped the in-memory copy. Stop needs no rewrite.
//
// Start and Restart are also where a pending "restart required" is answered: whatever the unit comes up
// with IS the mod set in mods/, so the drift the record remembered is resolved by definition. Clearing
// it here rather than in the handlers means every door — REST, the MCP tool, the in-game CLI — closes
// the flag by doing the one thing that actually fixes it.
func (m *Manager) Start(ctx context.Context, srv store.Server) error {
	if err := m.writeProps(srv); err != nil {
		return err
	}
	if err := run(ctx, "start", srv.Owner, srv.Slug); err != nil {
		return err
	}
	m.clearDrift(srv)
	return nil
}
func (m *Manager) Stop(ctx context.Context, srv store.Server) error {
	return run(ctx, "stop", srv.Owner, srv.Slug)
}
func (m *Manager) Restart(ctx context.Context, srv store.Server) error {
	if err := m.writeProps(srv); err != nil {
		return err
	}
	if err := run(ctx, "restart", srv.Owner, srv.Slug); err != nil {
		return err
	}
	m.clearDrift(srv)
	return nil
}

// clearDrift is best-effort: the server is up with the right mods either way, and a flag that failed to
// clear must not turn a successful start into a reported failure. The next start clears it again.
func (m *Manager) clearDrift(srv store.Server) {
	_ = m.st.SetRestartRequired(srv.ID, false)
}

// Destroy stops the unit, removes the drop-in and deletes the tree — and drops the route first, so a
// failure half-way cannot leave a public domain pointing at a port that is about to be reused.
func (m *Manager) Destroy(ctx context.Context, srv store.Server) error {
	if err := m.Unroute(ctx, srv); err != nil {
		return fmt.Errorf("drop route: %w", err)
	}
	return run(ctx, "destroy", srv.Owner, srv.Slug)
}

// State asks systemd whether the unit is running. Cheap; safe to call per row.
func (m *Manager) State(ctx context.Context, srv store.Server) string {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", Unit(srv.Owner, srv.Slug)).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "inactive"
	}
	return s
}

// Autostart reports whether the server is set to come up with the OS. `systemctl is-enabled` is
// read-only, so the daemon runs it directly — no escalation needed for a query.
func (m *Manager) Autostart(ctx context.Context, srv store.Server) bool {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-enabled", Unit(srv.Owner, srv.Slug)).Output()
	return strings.TrimSpace(string(out)) == "enabled"
}

// SetAutostart turns "start with the system" on or off. Enabling writes a systemd .wants symlink
// under /etc, so it goes through the privileged wrapper. It deliberately does not start or stop the
// server now: toggling the boot behaviour must not bounce a running world.
func (m *Manager) SetAutostart(ctx context.Context, srv store.Server, on bool) error {
	verb := "disable"
	if on {
		verb = "enable"
	}
	return run(ctx, verb, srv.Owner, srv.Slug)
}

// Status combines systemd's view with a real Server List Ping.
//
// The ping goes to the LOCAL backend port but sends the PUBLIC hostname in the handshake, which is
// what a player's client does and what mc-router routes on. A server can be "active" to systemd and
// still be unreachable (still loading the world, or bound wrong), so the two are reported separately
// rather than collapsed into one lie.
func (m *Manager) Status(ctx context.Context, srv store.Server) Status {
	st := Status{State: m.State(ctx, srv), Autostart: m.Autostart(ctx, srv)}
	if st.State != "active" {
		return st
	}
	// Set before the ping: a server that is up but not yet answering still has drifted mods, and that
	// is exactly when an operator wants to be told.
	st.RestartRequired = srv.RestartRequired
	ping, err := mcnet.Ping(ctx, "127.0.0.1:"+strconv.Itoa(srv.Port), srv.Host, srv.Port, 2*time.Second)
	if err != nil {
		return st // running, but not yet answering — the world is probably still loading
	}
	st.Reachable = true
	st.Online, st.Max, st.Sample = ping.Online, ping.Max, ping.Sample
	return st
}

// rcon opens an authenticated RCON connection to a running server.
func (m *Manager) rcon(srv store.Server) (*mcnet.Conn, error) {
	return mcnet.Dial("127.0.0.1:"+strconv.Itoa(srv.RconPort), m.rconPass(srv), 5*time.Second)
}

// rconPass resolves the server's rcon password. RconPass is json:"-" (never persisted), so after a
// daemon restart the in-memory field is empty and server.properties — where writeProps keeps it — is
// the source of truth. Returns "" only when the file is unreadable or the key is genuinely unset.
func (m *Manager) rconPass(srv store.Server) string {
	if srv.RconPass != "" {
		return srv.RconPass
	}
	p, err := mcfiles.ReadProps(filepath.Join(Dir(srv.Owner, srv.Slug), "server.properties"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(p["rcon.password"])
}

// ApplyWhitelist writes whitelist.json + ops.json and, if the server is running, makes it take
// effect immediately.
//
// The files are the source of truth (they survive a restart); the RCON reload is what makes a change
// land on a LIVE server without bouncing it. If the server is down, the file write is enough and the
// RCON failure is not an error — that distinction is the whole point of doing both.
func (m *Manager) ApplyWhitelist(ctx context.Context, srv store.Server, players []mcfiles.Entry, ops []mcfiles.Op) error {
	dir := Dir(srv.Owner, srv.Slug)
	if err := mcfiles.WriteWhitelist(filepath.Join(dir, "whitelist.json"), players); err != nil {
		return err
	}
	if err := mcfiles.WriteOps(filepath.Join(dir, "ops.json"), ops); err != nil {
		return err
	}
	if m.State(ctx, srv) != "active" {
		return nil
	}
	c, err := m.rcon(srv)
	if err != nil {
		return nil // running but rcon not up yet (world still loading); the files are already correct
	}
	defer c.Close()
	_, _ = c.Cmd("whitelist reload")
	return nil
}

// LogTail returns the last n lines of the server's latest.log.
//
// It reads the file directly rather than shelling out to journalctl: the tree is <owner>:hosuto and
// the daemon is in the group, so the log is readable without escalation — whereas an unprivileged
// process has no guaranteed access to the systemd journal. The file is the same one the console shows.
func (m *Manager) LogTail(srv store.Server, lines int) (string, error) {
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	return tailFile(filepath.Join(Dir(srv.Owner, srv.Slug), "logs", "latest.log"), lines)
}

// tailFile returns the last `lines` lines of a file. It reads at most the final 64 KiB — a crash log
// worth reading is never further back than that, and it keeps a multi-hundred-megabyte latest.log from
// being slurped whole.
func tailFile(path string, lines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	const window = 64 << 10
	var off int64
	if info.Size() > window {
		off = info.Size() - window
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	text := string(b)
	// When we started mid-file, the first line is a fragment — drop it.
	if off > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

// Ping measures how quickly the server answers a Server List Ping and reports the round-trip latency.
// hosuto runs on the same host, so this is the loopback round-trip — a measure of the server's own
// responsiveness (it climbs when the network thread is saturated), not a remote player's client ping,
// which the vanilla protocol does not expose to the server. ok is false when the server is not up or
// does not answer within the timeout.
func (m *Manager) Ping(ctx context.Context, srv store.Server) (time.Duration, bool) {
	if m.State(ctx, srv) != "active" {
		return 0, false
	}
	start := time.Now()
	if _, err := mcnet.Ping(ctx, "127.0.0.1:"+strconv.Itoa(srv.Port), srv.Host, srv.Port, 3*time.Second); err != nil {
		return 0, false
	}
	return time.Since(start), true
}

// OnlinePlayers returns the names currently connected, read authoritatively from the server console
// (`list`) rather than the Server List Ping sample, which vanilla caps at 12 names and a server may
// hide. ok is false when the server is not up or its console does not answer; an empty (non-nil) slice
// with ok=true means the server is up with nobody on.
func (m *Manager) OnlinePlayers(ctx context.Context, srv store.Server) ([]string, bool) {
	replies, ok, err := m.Command(ctx, srv, "list")
	if !ok || err != nil || len(replies) == 0 {
		return nil, false
	}
	return parsePlayerList(replies[0]), true
}

// parsePlayerList pulls the names out of a `list` reply, e.g.
// "There are 2 of a max of 20 players online: Alice, Bob". The names always follow the final colon;
// a reply with nothing after it means nobody is online. Returns a non-nil empty slice in that case.
func parsePlayerList(reply string) []string {
	out := []string{}
	i := strings.LastIndex(reply, ":")
	if i < 0 {
		return out
	}
	for _, p := range strings.Split(reply[i+1:], ",") {
		if n := strings.TrimSpace(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Say sends a chat line to a running server (used to tell players a change landed).
func (m *Manager) Say(ctx context.Context, srv store.Server, msg string) {
	if m.State(ctx, srv) != "active" {
		return
	}
	c, err := m.rcon(srv)
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.Cmd("say " + msg)
}

// Command runs one or more RCON commands over a SINGLE session and returns each reply in order. It is
// the return-value sibling of Say: the in-game "!ai" CLI uses it to query the server (e.g. `list`) and
// to answer the operator with chunked `tellraw` lines, all on one dial.
//
// The gate matches Say and ApplyWhitelist: a server that is not "active" to systemd, or is active but
// whose RCON is not up yet (world still loading), is not an error — it yields (nil, false). A caller
// that needs to know whether the commands actually ran reads ok; one that is fire-and-forget ignores
// it. A mid-session RCON error stops the batch and returns what succeeded so far with that error. Each
// command's payload must stay under the RCON frame limit (~1446 bytes); the caller chunks to fit.
func (m *Manager) Command(ctx context.Context, srv store.Server, cmds ...string) (replies []string, ok bool, err error) {
	if len(cmds) == 0 {
		return nil, false, nil
	}
	if m.State(ctx, srv) != "active" {
		return nil, false, nil
	}
	c, derr := m.rcon(srv)
	if derr != nil {
		return nil, false, nil // running but rcon not up yet (world still loading)
	}
	defer c.Close()
	replies = make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		reply, cerr := c.Cmd(cmd)
		if cerr != nil {
			return replies, true, cerr
		}
		replies = append(replies, reply)
	}
	return replies, true, nil
}
