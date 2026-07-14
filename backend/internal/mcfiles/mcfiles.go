// Package mcfiles reads and writes the Minecraft server's OWN on-disk configuration:
// server.properties, whitelist.json, ops.json and eula.txt.
//
// hosuto owns these files and regenerates them from the store — a user never hand-edits them, and
// the daemon is their only writer. That ownership is why every write here is atomic
// (temp → fsync → rename): the server may be reloading a list at the exact moment we rewrite it,
// and it must see either the old file or the new one, never a truncated one.
//
// The package exists because the vanilla parsers fail SILENTLY. Get a field wrong and nothing
// errors, nothing logs at a level anyone reads — a player simply cannot join. The three traps this
// package exists to make unrepresentable:
//
//   - whitelist.json: WhiteListEntry's deserializer is `if (json.has("uuid") && json.has("name"))
//     … else return null` — an entry missing EITHER key is dropped on the floor. So WriteWhitelist
//     refuses to emit one rather than write a file that silently locks a player out.
//   - ops.json: an entry with no "level" key deserializes to level 0 — an operator with no
//     operator powers. `level` is therefore never omitempty and WriteOps fills in a real level.
//   - server.properties: server-ip binds BOTH the game port and rcon (RconThread reads
//     getServerIp(); there is no rcon.ip key). Binding it to loopback is the entire isolation
//     story — the game port is public only through mc-router, and rcon is reachable only from the
//     host itself. Settings/Apply centralise that so no caller can forget it.
//
// server.properties is also a file hosuto only PARTLY owns: the server writes its own defaults
// into it and an admin may have added keys hosuto has never heard of. So it is always
// read → overlay → write, never regenerated wholesale.
package mcfiles

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Loopback is what server-ip is pinned to. Both the game port and rcon bind to it, so a server is
// reachable from the outside only through mc-router, and its rcon only from the host.
const Loopback = "127.0.0.1"

// OpLevel is a full operator. hosuto has exactly two membership levels (play | op), so an entry in
// ops.json is always a full op — there is no vocabulary here for levels 1–3.
const OpLevel = 4

// ErrInvalidEntry is returned rather than emitting a list entry the server would silently ignore.
var ErrInvalidEntry = errors.New("mcfiles: list entry needs a dashed uuid and a name")

// uuidRe is the dashed 8-4-4-4-12 form. The vanilla parser calls UUID.fromString on this field; an
// undashed uuid throws inside the loader and takes the whole entry (or the whole file) with it.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ── server.properties ─────────────────────────────────────────────────────────────────

// Props is a java .properties file as a flat map. Comments and key order are NOT modelled: they
// carry no meaning to the server, and preserving them would mean keeping a parse tree alive across
// a read-modify-write for no gain. Unknown KEYS, which do carry meaning, always survive.
type Props map[string]string

// Settings is the set of server.properties keys hosuto controls. Everything else in the file —
// max-players, level-name, view-distance, whatever the admin added — is left exactly as found.
type Settings struct {
	Port      int    // the game port mc-router forwards to
	RconPort  int    // loopback-only, see Loopback
	RconPass  string // non-empty, else the server disables rcon with a warning and hosuto goes blind
	MOTD      string // the server's display name
	Whitelist bool   // store.Server.JoinPolicy == "whitelist"
}

// Apply overlays hosuto's keys onto p (which the caller read from the existing file) and returns
// it. It fails closed on the settings that would silently break the service rather than writing a
// file that looks fine and does not work.
func Apply(p Props, s Settings) (Props, error) {
	if s.Port <= 0 || s.Port > 65535 {
		return nil, fmt.Errorf("mcfiles: bad server port %d", s.Port)
	}
	if s.RconPort <= 0 || s.RconPort > 65535 {
		return nil, fmt.Errorf("mcfiles: bad rcon port %d", s.RconPort)
	}
	if s.RconPort == s.Port {
		return nil, fmt.Errorf("mcfiles: rcon port %d collides with the game port", s.RconPort)
	}
	// An empty rcon.password makes the server log a warning and run WITHOUT rcon — every console
	// command hosuto issues would then vanish into a closed port.
	if s.RconPass == "" {
		return nil, errors.New("mcfiles: empty rcon password would disable rcon")
	}
	if p == nil {
		p = Props{}
	}
	p["server-ip"] = Loopback // binds the game port AND rcon; there is no rcon.ip key
	p["server-port"] = strconv.Itoa(s.Port)
	p["enable-rcon"] = "true"
	p["rcon.port"] = strconv.Itoa(s.RconPort)
	p["rcon.password"] = s.RconPass
	p["broadcast-rcon-to-ops"] = "false" // hosuto's commands are plumbing, not chat
	p["enable-query"] = "false"          // query.port DEFAULTS to the game port and would collide
	p["white-list"] = strconv.FormatBool(s.Whitelist)
	p["enforce-whitelist"] = "true" // a whitelist reload kicks players no longer on it
	p["online-mode"] = "true"       // NEVER false: hosuto authenticates players against Mojang
	p["motd"] = s.MOTD
	return p, nil
}

// ReadProps parses a .properties file. A missing file is not an error — a server that has never
// started has no server.properties yet, and the caller's job is precisely to create one.
func ReadProps(path string) (Props, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Props{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseProps(string(b)), nil
}

// WriteProps writes p atomically, sorted by key so the file is stable across regenerations and a
// diff shows only what actually changed. Comments in the previous file are lost; every key is kept.
func WriteProps(path string, p Props) error {
	keys := make([]string, 0, len(p))
	for k := range p {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("#Minecraft server properties — generated by hosuto, do not hand-edit\n")
	for _, k := range keys {
		b.WriteString(escape(k, true))
		b.WriteByte('=')
		b.WriteString(escape(p[k], false))
		b.WriteByte('\n')
	}
	return writeAtomic(path, []byte(b.String()))
}

// parseProps implements enough of java.util.Properties to survive a file the server itself wrote:
// '#'/'!' comments, '=' / ':' / whitespace separators, backslash line continuations and \uXXXX
// escapes. A line it cannot make sense of is skipped, never fatal — hosuto is going to overwrite
// its own keys anyway, and refusing to boot over a junk line in a file the user was told not to
// touch would be the wrong trade.
func parseProps(text string) Props {
	p := Props{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); {
		line := strings.TrimSuffix(lines[i], "\r")
		i++
		t := strings.TrimLeft(line, " \t\f")
		if t == "" || t[0] == '#' || t[0] == '!' {
			continue
		}
		// A logical line continues while it ends in an ODD number of backslashes (an even count is
		// escaped backslashes, not a continuation).
		for oddTrailingSlashes(t) && i < len(lines) {
			next := strings.TrimSuffix(lines[i], "\r")
			i++
			t = t[:len(t)-1] + strings.TrimLeft(next, " \t\f")
		}
		k, v := splitKV(t)
		if k != "" {
			p[k] = v // last one wins, as java does
		}
	}
	return p
}

func oddTrailingSlashes(s string) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// splitKV cuts a logical line at the first UNESCAPED separator ('=', ':' or whitespace), then
// unescapes both halves. A line with no separator at all is a key with an empty value.
func splitKV(s string) (string, string) {
	var key strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			key.WriteByte(c)
			key.WriteByte(s[i+1])
			i += 2
			continue
		}
		if c == '=' || c == ':' || c == ' ' || c == '\t' || c == '\f' {
			break
		}
		key.WriteByte(c)
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\f') {
		i++
	}
	if i < len(s) && (s[i] == '=' || s[i] == ':') {
		i++
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\f') {
			i++
		}
	}
	return unescape(key.String()), unescape(s[i:])
}

// escape renders a key or a value. Values only need the characters that would end the line or the
// escape itself; keys additionally need the separators, or the key would be cut short. Anything
// outside printable ASCII becomes a \uXXXX escape, which every version of the server's parser
// understands regardless of the charset it opened the file with.
func escape(s string, isKey bool) string {
	var b strings.Builder
	for i, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\f':
			b.WriteString(`\f`)
		case ' ':
			// Leading space in a value is stripped by the parser, so it must be escaped; inside a
			// value it is ordinary. In a key it always separates.
			if isKey || i == 0 {
				b.WriteString(`\ `)
			} else {
				b.WriteByte(' ')
			}
		case '=', ':':
			if isKey {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		case '#', '!':
			// Only meaningful at the start of a line, i.e. at the start of a key.
			if isKey || i == 0 {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		default:
			if r < 0x20 || r > 0x7e {
				writeUnicode(&b, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// writeUnicode emits \uXXXX, splitting astral runes into the surrogate pair java expects.
func writeUnicode(b *strings.Builder, r rune) {
	if r > 0xFFFF {
		hi, lo := utf16.EncodeRune(r)
		fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
		return
	}
	fmt.Fprintf(b, `\u%04x`, r)
}

func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		i++
		if i >= len(s) {
			break // a lone trailing backslash: java drops it
		}
		c := s[i]
		i++
		switch c {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			r, ok := hex4(s[i:])
			if !ok {
				b.WriteByte('u') // malformed escape: java throws, we take it literally
				continue
			}
			i += 4
			// A surrogate is only half a rune; pair it with the next escape or go's strings cannot
			// hold it.
			if utf16.IsSurrogate(r) && strings.HasPrefix(s[i:], `\u`) {
				if lo, ok := hex4(s[i+2:]); ok {
					if dec := utf16.DecodeRune(r, lo); dec != utf8.RuneError {
						b.WriteRune(dec)
						i += 6
						continue
					}
				}
			}
			if utf16.IsSurrogate(r) {
				b.WriteRune(utf8.RuneError)
			} else {
				b.WriteRune(r)
			}
		default:
			b.WriteByte(c) // \= \: \  \# … are just the character
		}
	}
	return b.String()
}

func hex4(s string) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	n, err := strconv.ParseUint(s[:4], 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(n), true
}

// ── whitelist.json ────────────────────────────────────────────────────────────────────

// Entry is one whitelist record. BOTH fields are mandatory and neither is omitempty: the server
// drops an entry that is missing either key, without a word.
type Entry struct {
	UUID string `json:"uuid"` // dashed
	Name string `json:"name"`
}

// ReadWhitelist reads whitelist.json. A missing file is an empty whitelist, as it is for the
// server. Entries the SERVER would drop are dropped here too, so what hosuto reports is what is
// actually in effect — and a read-modify-write quietly repairs the file.
func ReadWhitelist(path string) ([]Entry, error) {
	var raw []Entry
	if err := readJSON(path, &raw); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(raw))
	for _, e := range raw {
		if e.UUID == "" || e.Name == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// WriteWhitelist writes whitelist.json atomically. It refuses the whole write if any entry is
// incomplete: emitting it would produce a file the server accepts and partially ignores, i.e. a
// player who cannot join and no error anywhere.
func WriteWhitelist(path string, entries []Entry) error {
	for i, e := range entries {
		if e.Name == "" || !uuidRe.MatchString(e.UUID) {
			return fmt.Errorf("%w: whitelist entry %d (uuid=%q name=%q)", ErrInvalidEntry, i, e.UUID, e.Name)
		}
	}
	if entries == nil {
		entries = []Entry{} // "null" is not a list; the server wants []
	}
	return writeJSON(path, entries)
}

// ── ops.json ──────────────────────────────────────────────────────────────────────────

// Op is one operator record. Level has no omitempty ON PURPOSE: an entry without the key
// deserializes to level 0, an operator who can run nothing.
type Op struct {
	UUID                string `json:"uuid"` // dashed
	Name                string `json:"name"`
	Level               int    `json:"level"`
	BypassesPlayerLimit bool   `json:"bypassesPlayerLimit"`
}

// ReadOps reads ops.json. Like the whitelist, it reports what the server would actually honour —
// including a level-0 entry, which is real (and useless) rather than filtered away.
func ReadOps(path string) ([]Op, error) {
	var raw []Op
	if err := readJSON(path, &raw); err != nil {
		return nil, err
	}
	out := make([]Op, 0, len(raw))
	for _, o := range raw {
		if o.UUID == "" || o.Name == "" {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// WriteOps writes ops.json atomically, always with an explicit level.
//
// A zero (or out-of-range) level is normalised to OpLevel rather than rejected: the only reason
// hosuto puts anyone in ops.json is an op grant, so "in the file at level 0" is never what the
// caller meant — it is what a forgotten field looks like.
func WriteOps(path string, ops []Op) error {
	out := make([]Op, 0, len(ops))
	for i, o := range ops {
		if o.Name == "" || !uuidRe.MatchString(o.UUID) {
			return fmt.Errorf("%w: ops entry %d (uuid=%q name=%q)", ErrInvalidEntry, i, o.UUID, o.Name)
		}
		if o.Level < 1 || o.Level > OpLevel {
			o.Level = OpLevel
		}
		out = append(out, o)
	}
	return writeJSON(path, out)
}

// ── eula.txt ──────────────────────────────────────────────────────────────────────────

// WriteEULA accepts the EULA. Without this file the server prints a notice and exits immediately
// on first run, so hosuto writes it when the owner accepts in the UI — never on its own initiative.
func WriteEULA(path string) error {
	body := "#By changing the setting below to TRUE you are indicating your agreement to our EULA (https://aka.ms/MinecraftEULA).\n" +
		"#Accepted through hosuto by the owner of this server.\n" +
		"eula=true\n"
	return writeAtomic(path, []byte(body))
}

// ── rcon password ─────────────────────────────────────────────────────────────────────

// GenRconPassword mints an rcon password: 24 random bytes, base64url, so 32 characters with no
// padding. The alphabet matters — the password lands in a .properties value and is passed on a
// command line, so it must contain no backslash, quote or whitespace.
func GenRconPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ── io ────────────────────────────────────────────────────────────────────────────────

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil // the server leaves an empty file behind often enough
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// writeAtomic writes through a temp file in the SAME directory (so the rename cannot cross a
// filesystem) and fsyncs before renaming. The server may be reloading the file as we write it: a
// rename is the only way it sees a whole file or the old one, never half of one.
//
// The directory is never created here. A server's tree is created by the privileged wrapper with
// the owner's uid; a directory conjured up by the daemon would have the wrong owner and the server
// could not write its world into it.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has succeeded
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
	// 0640: the file holds the rcon password. The server reads it as the owning user; nobody else
	// on the host has any business with it.
	if err := os.Chmod(tmp, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
