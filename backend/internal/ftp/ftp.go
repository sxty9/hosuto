// Package ftp is a small FTP client, just large enough to pull a game server off a host like
// GPortal or Nitrado.
//
// It is written on the standard library rather than pulled in as a dependency because FTP's client
// side is genuinely small: a line-based control channel, a second connection per transfer, and two
// listing formats. net/textproto already implements RFC 959's multi-line reply format (the same
// "code-" continuation SMTP uses), which is the only fiddly part of the protocol.
//
// What it deliberately does NOT do: active mode (a listening socket on the daemon, which no NAT
// would let the far side reach anyway), resumption, or writing. hosuto only ever reads from a
// foreign host, so RETR and the two list verbs are the whole surface.
//
// Security posture. FTP sends credentials in clear text — that is the protocol, and it is what these
// hosts offer. So the connection is upgraded opportunistically: AUTH TLS is attempted first with
// ordinary certificate verification, and only a server that cannot do it is spoken to in the clear.
// TLS() reports which happened, so the caller can tell the operator the truth instead of implying a
// protection that was not there.
package ftp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrAuth is returned when the host rejected the username or password, so the caller can tell that
// apart from "the host is unreachable" — the two send the operator to very different places.
var ErrAuth = errors.New("ftp: login rejected")

// Config is one remote host.
type Config struct {
	Host    string
	Port    int           // 0 means 21
	User    string        // empty means "anonymous"
	Pass    string
	Timeout time.Duration // per operation; 0 means DefaultTimeout
}

// DefaultTimeout bounds a single control exchange or the start of a transfer. It is generous because
// a game host under load can take seconds to answer a LIST, and short timeouts here produce a
// migration that fails halfway for no reason the operator can act on.
const DefaultTimeout = 60 * time.Second

// Entry is one remote file or directory.
type Entry struct {
	Name  string
	Path  string // absolute remote path
	Dir   bool
	Size  int64
	MTime time.Time
}

// Client is one FTP session. Not safe for concurrent use: FTP multiplexes nothing, every command
// shares the one control channel.
type Client struct {
	conn net.Conn
	tp   *textproto.Conn
	cfg  Config
	tls  *tls.Config // non-nil once the control channel is encrypted
	feat map[string]bool
}

// TLS reports whether the control channel was encrypted.
func (c *Client) TLS() bool { return c.tls != nil }

func (c *Config) port() int {
	if c.Port <= 0 {
		return 21
	}
	return c.Port
}

func (c *Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

func (c *Config) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.port())) }

// Dial opens a session and logs in.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("ftp: no host")
	}
	d := net.Dialer{Timeout: cfg.timeout()}
	conn, err := d.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("ftp: connect %s: %w", cfg.addr(), err)
	}
	c := &Client{conn: conn, cfg: cfg, feat: map[string]bool{}}
	c.rebind()

	if _, _, err := c.expect(220); err != nil {
		c.Close()
		return nil, err
	}
	c.readFeat()

	// Opportunistic encryption. A server that answers 234 is willing; anything else means we carry on
	// in the clear rather than refusing to migrate at all.
	if c.feat["AUTH TLS"] || c.feat["AUTH SSL"] {
		if err := c.startTLS(); err != nil {
			// The control channel is in an unknown state after a failed upgrade, so start over
			// cleanly rather than issuing USER down a half-negotiated socket.
			c.Close()
			return dialPlain(ctx, cfg)
		}
	}
	if err := c.login(); err != nil {
		c.Close()
		return nil, err
	}
	if _, _, err := c.cmd("TYPE I", 200); err != nil { // binary; ASCII mode would corrupt every jar
		c.Close()
		return nil, err
	}
	return c, nil
}

// dialPlain is the fallback path taken when a TLS upgrade was advertised but did not work.
func dialPlain(ctx context.Context, cfg Config) (*Client, error) {
	d := net.Dialer{Timeout: cfg.timeout()}
	conn, err := d.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("ftp: connect %s: %w", cfg.addr(), err)
	}
	c := &Client{conn: conn, cfg: cfg, feat: map[string]bool{}}
	c.rebind()
	if _, _, err := c.expect(220); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.login(); err != nil {
		c.Close()
		return nil, err
	}
	if _, _, err := c.cmd("TYPE I", 200); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) login() error {
	user := c.cfg.User
	if user == "" {
		user = "anonymous"
	}
	code, msg, err := c.cmd("USER "+user, 230, 331)
	if err != nil {
		return loginErr(code, msg, err)
	}
	if code == 331 { // password wanted
		if code, msg, err = c.cmd("PASS "+c.cfg.Pass, 230, 202); err != nil {
			return loginErr(code, msg, err)
		}
	}
	return nil
}

// loginErr turns the 5xx family into ErrAuth. 530 is "not logged in"; 332 asks for an ACCOUNT, which
// no game host uses and which we cannot supply — both mean these credentials will not work.
func loginErr(code int, msg string, err error) error {
	if code == 530 || code == 332 || (code >= 430 && code < 440) {
		if msg = strings.TrimSpace(msg); msg != "" {
			return fmt.Errorf("%w: %s", ErrAuth, msg)
		}
		return ErrAuth
	}
	return err
}

// rebind rebuilds the textproto reader over the current connection. It is called again after a TLS
// upgrade, because the buffered reader from before wraps the plaintext socket.
func (c *Client) rebind() { c.tp = textproto.NewConn(c.conn) }

func (c *Client) startTLS() error {
	if _, _, err := c.cmd("AUTH TLS", 234); err != nil {
		return err
	}
	cfg := &tls.Config{
		ServerName: c.cfg.Host,
		// Many servers insist the data connection resume the control connection's session, and
		// refuse it outright otherwise. Sharing one cache across both is what makes that work.
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		MinVersion:         tls.VersionTLS12,
	}
	tc := tls.Client(c.conn, cfg)
	if err := tc.HandshakeContext(context.Background()); err != nil {
		return err
	}
	c.conn = tc
	c.tls = cfg
	c.rebind()

	// PBSZ 0 then PROT P: encrypt the data connections too. Without PROT P the file bytes would
	// travel in the clear behind an encrypted control channel, which is the worst of both.
	if _, _, err := c.cmd("PBSZ 0", 200); err != nil {
		return err
	}
	if _, _, err := c.cmd("PROT P", 200); err != nil {
		return err
	}
	return nil
}

// readFeat asks what the server can do. A server that does not implement FEAT simply gets the
// conservative path — plain FTP with LIST.
func (c *Client) readFeat() {
	id, err := c.tp.Cmd("FEAT")
	if err != nil {
		return
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)
	_ = c.conn.SetDeadline(time.Now().Add(c.cfg.timeout()))
	code, msg, err := c.tp.ReadResponse(211)
	if err != nil || code != 211 {
		return
	}
	for _, line := range strings.Split(msg, "\n") {
		if f := strings.ToUpper(strings.TrimSpace(line)); f != "" {
			c.feat[f] = true
			// "AUTH TLS" is often advertised as "AUTH TLS;TLS-C;SSL", and MLST names its options
			// after a space. Record the verb on its own so lookups stay simple.
			if verb, _, ok := strings.Cut(f, " "); ok && (verb == "MLST" || verb == "MLSD") {
				c.feat[verb] = true
			}
			if strings.HasPrefix(f, "AUTH") && strings.Contains(f, "TLS") {
				c.feat["AUTH TLS"] = true
			}
		}
	}
}

// Close ends the session politely, then closes the socket regardless.
func (c *Client) Close() error {
	if c.tp != nil {
		if id, err := c.tp.Cmd("QUIT"); err == nil {
			c.tp.StartResponse(id)
			_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, _, _ = c.tp.ReadResponse(221)
			c.tp.EndResponse(id)
		}
	}
	return c.conn.Close()
}

// cmd sends one command and requires one of the accepted codes. It returns the code and message even
// on failure, because the server's own text ("530 Login incorrect") is the only useful thing to show
// an operator whose migration just failed.
func (c *Client) cmd(line string, accept ...int) (int, string, error) {
	id, err := c.tp.Cmd("%s", line)
	if err != nil {
		return 0, "", fmt.Errorf("ftp: %s: %w", verb(line), err)
	}
	c.tp.StartResponse(id)
	defer c.tp.EndResponse(id)
	return c.check(verb(line), accept...)
}

// expect reads a bare response (the greeting, or a transfer's completion notice).
func (c *Client) expect(accept ...int) (int, string, error) {
	return c.check("", accept...)
}

func (c *Client) check(what string, accept ...int) (int, string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(c.cfg.timeout()))
	code, msg, err := c.tp.ReadResponse(0)
	if err != nil {
		// textproto returns *textproto.Error for a well-formed reply with an unwanted code; that is
		// not a transport failure and the code inside it is what the caller needs.
		var pe *textproto.Error
		if errors.As(err, &pe) {
			code, msg = pe.Code, pe.Msg
		} else {
			return 0, "", fmt.Errorf("ftp: %s: %w", what, err)
		}
	}
	for _, a := range accept {
		if code == a {
			return code, msg, nil
		}
	}
	if what == "" {
		return code, msg, fmt.Errorf("ftp: unexpected reply %d %s", code, strings.TrimSpace(msg))
	}
	return code, msg, fmt.Errorf("ftp: %s: %d %s", what, code, strings.TrimSpace(msg))
}

func verb(line string) string {
	v, _, _ := strings.Cut(line, " ")
	return v
}

// ── data connections ──────────────────────────────────────────────────────────────────

// openData puts the server in passive mode and connects to the port it names.
//
// EPSV is tried first: it returns a port only, so it works over IPv6 and — more importantly here —
// cannot hand back the private address of a host sitting behind NAT, which is the classic reason a
// PASV transfer hangs until it times out.
func (c *Client) openData(ctx context.Context) (net.Conn, error) {
	addr, err := c.passiveAddr()
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: c.cfg.timeout()}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ftp: data connection to %s: %w", addr, err)
	}
	if c.tls != nil {
		tc := tls.Client(conn, c.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ftp: data tls: %w", err)
		}
		return tc, nil
	}
	return conn, nil
}

func (c *Client) passiveAddr() (string, error) {
	if _, msg, err := c.cmd("EPSV", 229); err == nil {
		if p, ok := parseEPSV(msg); ok {
			return net.JoinHostPort(c.cfg.Host, strconv.Itoa(p)), nil
		}
	}
	_, msg, err := c.cmd("PASV", 227)
	if err != nil {
		return "", err
	}
	host, port, ok := parsePASV(msg)
	if !ok {
		return "", fmt.Errorf("ftp: could not parse passive reply %q", strings.TrimSpace(msg))
	}
	// A host that reports a private address while we reached it over the public internet is behind a
	// NAT it does not know about. Its control address is the one that demonstrably works.
	if isPrivate(host) && !isPrivate(c.cfg.Host) {
		host = c.cfg.Host
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// parseEPSV pulls the port out of "229 Entering Extended Passive Mode (|||6446|)".
func parseEPSV(msg string) (int, bool) {
	open := strings.IndexByte(msg, '(')
	closeIdx := strings.LastIndexByte(msg, ')')
	if open < 0 || closeIdx <= open {
		return 0, false
	}
	inner := msg[open+1 : closeIdx]
	if len(inner) < 4 {
		return 0, false
	}
	sep := inner[0]
	parts := strings.Split(inner, string(sep))
	if len(parts) < 4 {
		return 0, false
	}
	p, err := strconv.Atoi(strings.TrimSpace(parts[3]))
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

// parsePASV pulls host and port out of "227 Entering Passive Mode (10,0,0,1,200,55)".
func parsePASV(msg string) (string, int, bool) {
	open := strings.IndexByte(msg, '(')
	closeIdx := strings.LastIndexByte(msg, ')')
	if open < 0 || closeIdx <= open {
		return "", 0, false
	}
	parts := strings.Split(msg[open+1:closeIdx], ",")
	if len(parts) != 6 {
		return "", 0, false
	}
	n := make([]int, 6)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 || v > 255 {
			return "", 0, false
		}
		n[i] = v
	}
	return fmt.Sprintf("%d.%d.%d.%d", n[0], n[1], n[2], n[3]), n[4]<<8 | n[5], true
}

func isPrivate(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

// ── listing ───────────────────────────────────────────────────────────────────────────

// List returns the contents of one remote directory. MLSD is used when the server has it: its output
// is machine-defined, while LIST's is whatever the host's `ls` prints and varies by operating system.
func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	useMLSD := c.feat["MLSD"]
	verb := "LIST"
	if useMLSD {
		verb = "MLSD"
	}
	lines, err := c.dataLines(ctx, verb+" "+quoteArg(dir))
	if err != nil {
		if !useMLSD {
			return nil, err
		}
		// Some servers advertise MLSD and then refuse it. One retry on LIST is worth it.
		if lines, err = c.dataLines(ctx, "LIST "+quoteArg(dir)); err != nil {
			return nil, err
		}
		useMLSD = false
	}

	var out []Entry
	for _, ln := range lines {
		var e Entry
		var ok bool
		if useMLSD {
			e, ok = parseMLSD(ln)
		} else {
			e, ok = parseLIST(ln)
		}
		if !ok || e.Name == "." || e.Name == ".." || e.Name == "" {
			continue
		}
		e.Path = path.Join(dir, e.Name)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// dataLines runs a command whose payload is a text listing on the data connection.
//
// The order is fixed by the protocol and easy to get wrong: open the data connection, send the
// command, read the 1xx preliminary reply, drain the data connection, close it, and only then read
// the 226 completion. Reading the completion early deadlocks against a server still writing.
func (c *Client) dataLines(ctx context.Context, cmd string) ([]string, error) {
	conn, err := c.openData(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	id, err := c.tp.Cmd("%s", cmd)
	if err != nil {
		return nil, err
	}
	c.tp.StartResponse(id)
	code, msg, err := c.check(verb(cmd), 125, 150, 226, 250)
	c.tp.EndResponse(id)
	if err != nil {
		return nil, err
	}

	var body []byte
	if code == 125 || code == 150 {
		_ = conn.SetDeadline(time.Now().Add(c.cfg.timeout()))
		body, err = io.ReadAll(io.LimitReader(conn, maxListing))
		if err != nil {
			return nil, fmt.Errorf("ftp: read listing: %w", err)
		}
		conn.Close()
		if _, _, err := c.expect(226, 250); err != nil {
			return nil, err
		}
	} else {
		_ = msg // an empty directory answered in one reply
	}

	var lines []string
	for _, ln := range strings.Split(string(body), "\n") {
		if ln = strings.TrimRight(ln, "\r"); strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return lines, nil
}

// maxListing bounds one directory listing. A server directory with more entries than this is not
// something a migration should try to walk.
const maxListing = 32 << 20

// parseMLSD reads "type=file;size=1234;modify=20240101120000; name".
func parseMLSD(line string) (Entry, bool) {
	facts, name, ok := strings.Cut(line, "; ")
	if !ok {
		return Entry{}, false
	}
	e := Entry{Name: strings.TrimSpace(name)}
	for _, f := range strings.Split(facts, ";") {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "type":
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "dir":
				e.Dir = true
			case "file":
			case "cdir", "pdir":
				return Entry{}, false // the directory itself and its parent
			default:
				// OS.unix=slink and anything else exotic: not something to copy.
				return Entry{}, false
			}
		case "size":
			e.Size, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		case "modify":
			if t, err := time.Parse("20060102150405", strings.TrimSpace(v)); err == nil {
				e.MTime = t
			}
		}
	}
	return e, e.Name != ""
}

// parseLIST reads the unix `ls -l` format every game host's FTP server emits.
//
// The name is taken as everything after the eighth field rather than by splitting, because file
// names contain spaces and a naive split loses them. A symlink line ("lrwxrwxrwx … a -> b") is
// refused outright: following one would copy from wherever it points, which is not this server.
func parseLIST(line string) (Entry, bool) {
	if len(line) == 0 {
		return Entry{}, false
	}
	switch line[0] {
	case 'l', 'c', 'b', 's', 'p':
		return Entry{}, false // symlink, device, socket, fifo
	case 'd', '-':
	default:
		return Entry{}, false // not a unix listing line (a DOS-style server, or a "total 40" header)
	}
	fields := strings.Fields(line)
	if len(fields) < 9 {
		return Entry{}, false
	}
	e := Entry{Dir: line[0] == 'd'}
	e.Size, _ = strconv.ParseInt(fields[4], 10, 64)

	// Walk past exactly eight whitespace-separated fields; the rest of the line is the name.
	rest := line
	for range 8 {
		rest = strings.TrimLeft(rest, " \t")
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			return Entry{}, false
		}
		rest = rest[i:]
	}
	e.Name = strings.TrimLeft(rest, " \t")
	if e.Name == "" || strings.Contains(e.Name, " -> ") {
		return Entry{}, false
	}
	return e, true
}

// quoteArg guards against a path with a newline in it, which would inject a second FTP command.
func quoteArg(p string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(p)
}

// ── retrieval ─────────────────────────────────────────────────────────────────────────

// Retrieve downloads one remote file to a local path, reporting bytes as they arrive.
//
// The bytes land in a temp file in the destination directory and are renamed into place only on a
// clean finish, so an interrupted transfer can never leave a half file that looks complete — the
// same discipline the store and the mod downloader use.
func (c *Client) Retrieve(ctx context.Context, remote, local string, onBytes func(int64)) (int64, error) {
	if err := os.MkdirAll(path.Dir(local), 0o770); err != nil {
		return 0, err
	}
	conn, err := c.openData(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	id, err := c.tp.Cmd("RETR %s", quoteArg(remote))
	if err != nil {
		return 0, err
	}
	c.tp.StartResponse(id)
	_, _, err = c.check("RETR", 125, 150)
	c.tp.EndResponse(id)
	if err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp(path.Dir(local), ".ftp-*.part")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename succeeds

	n, err := copyWithProgress(ctx, tmp, conn, c.cfg.timeout(), onBytes)
	if err != nil {
		tmp.Close()
		return n, fmt.Errorf("ftp: retrieve %s: %w", remote, err)
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	conn.Close()
	// The completion reply is the only confirmation the file is whole; a data connection that simply
	// ended could equally be the far side dropping mid-transfer.
	if _, _, err := c.expect(226, 250); err != nil {
		return n, err
	}
	if err := os.Chmod(tmpName, 0o660); err != nil {
		return n, err
	}
	return n, os.Rename(tmpName, local)
}

// copyWithProgress streams the data connection to disk, extending the deadline as bytes arrive so a
// slow-but-alive transfer is not killed, and honouring cancellation between chunks.
func copyWithProgress(ctx context.Context, dst io.Writer, src net.Conn, timeout time.Duration, onBytes func(int64)) (int64, error) {
	buf := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		_ = src.SetReadDeadline(time.Now().Add(timeout))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if onBytes != nil {
				onBytes(int64(n))
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// ── walking ───────────────────────────────────────────────────────────────────────────

// WalkLimits bounds a recursive listing, so a hostile or merely enormous remote tree cannot make the
// migration run forever.
type WalkLimits struct {
	MaxEntries int
	MaxDepth   int
}

// DefaultWalkLimits is sized for a large modpack server with years of logs and backups in it.
var DefaultWalkLimits = WalkLimits{MaxEntries: 200_000, MaxDepth: 24}

// Walk lists root recursively, calling fn for every file (never for a directory). Directories that
// cannot be listed are skipped rather than failing the walk: a permission-denied subfolder is normal
// on a shared host, and it must not cost the operator the whole migration.
func (c *Client) Walk(ctx context.Context, root string, lim WalkLimits, fn func(Entry) error) error {
	if lim.MaxEntries <= 0 {
		lim.MaxEntries = DefaultWalkLimits.MaxEntries
	}
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = DefaultWalkLimits.MaxDepth
	}
	seen := 0
	type item struct {
		path  string
		depth int
	}
	queue := []item{{root, 0}}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur := queue[0]
		queue = queue[1:]
		if cur.depth > lim.MaxDepth {
			continue
		}
		entries, err := c.List(ctx, cur.path)
		if err != nil {
			if cur.path == root {
				return err // the root has to be listable, or there is nothing to migrate
			}
			continue
		}
		for _, e := range entries {
			if seen++; seen > lim.MaxEntries {
				return fmt.Errorf("ftp: more than %d entries under %s", lim.MaxEntries, root)
			}
			if e.Dir {
				queue = append(queue, item{e.Path, cur.depth + 1})
				continue
			}
			if err := fn(e); err != nil {
				return err
			}
		}
	}
	return nil
}
