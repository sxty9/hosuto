package mcfiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The properties file a real server writes on first boot, comments and all.
const vanillaProps = `#Minecraft server properties
#Mon Jul 14 18:45:01 CEST 2025
enable-jmx-monitoring=false
rcon.port=25575
level-seed=
gamemode=survival
enable-command-block=false
motd=A Minecraft Server
query.port=25565
server-ip=
max-players=20
level-name=world
`

func TestReadPropsMissingFile(t *testing.T) {
	p, err := ReadProps(filepath.Join(t.TempDir(), "nope.properties"))
	if err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if len(p) != 0 {
		t.Fatalf("want empty props, got %v", p)
	}
}

// The whole point of the read-overlay-write dance: a key hosuto has never heard of survives.
func TestPropsRoundTripPreservesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.properties")
	if err := os.WriteFile(path, []byte(vanillaProps), 0o640); err != nil {
		t.Fatal(err)
	}
	p, err := ReadProps(path)
	if err != nil {
		t.Fatal(err)
	}
	if p["level-name"] != "world" || p["max-players"] != "20" {
		t.Fatalf("parse lost keys: %v", p)
	}
	if _, ok := p["level-seed"]; !ok {
		t.Fatal("an empty value is still a key")
	}

	p, err = Apply(p, Settings{Port: 25601, RconPort: 25602, RconPass: "s3cret", MOTD: "nanu's world", Whitelist: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteProps(path, p); err != nil {
		t.Fatal(err)
	}

	got, err := ReadProps(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		// hosuto's keys.
		"server-ip":             "127.0.0.1",
		"server-port":           "25601",
		"enable-rcon":           "true",
		"rcon.port":             "25602",
		"rcon.password":         "s3cret",
		"broadcast-rcon-to-ops": "false",
		"enable-query":          "false",
		"white-list":            "true",
		"enforce-whitelist":     "true",
		"online-mode":           "true",
		"motd":                  "nanu's world",
		// the server's own, untouched.
		"level-name":            "world",
		"max-players":           "20",
		"gamemode":              "survival",
		"enable-jmx-monitoring": "false",
		"enable-command-block":  "false",
		"level-seed":            "",
		"query.port":            "25565",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("key count = %d, want %d (%v)", len(got), len(want), got)
	}

	// Comments are dropped — only hosuto's own header survives a rewrite.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Mon Jul 14") {
		t.Error("old comments should not survive a rewrite")
	}
	if !strings.HasPrefix(string(raw), "#") {
		t.Error("want a generated-by header comment")
	}
}

// server-ip is the isolation story: it binds the game port AND rcon.
func TestApplyPinsLoopbackAndKillsQuery(t *testing.T) {
	p, err := Apply(Props{"query.port": "25565"}, Settings{Port: 25565, RconPort: 25575, RconPass: "x", MOTD: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if p["server-ip"] != Loopback {
		t.Errorf("server-ip = %q, want %q", p["server-ip"], Loopback)
	}
	if p["enable-query"] != "false" {
		t.Error("query must stay off: query.port defaults to the game port")
	}
	if p["online-mode"] != "true" {
		t.Error("online-mode must never be false")
	}
	if p["white-list"] != "false" {
		t.Errorf("white-list = %q, want false for an open server", p["white-list"])
	}
}

func TestApplyFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		s    Settings
	}{
		{"empty rcon password disables rcon", Settings{Port: 25565, RconPort: 25575, RconPass: ""}},
		{"no game port", Settings{Port: 0, RconPort: 25575, RconPass: "x"}},
		{"no rcon port", Settings{Port: 25565, RconPort: 0, RconPass: "x"}},
		{"port out of range", Settings{Port: 70000, RconPort: 25575, RconPass: "x"}},
		{"rcon collides with game port", Settings{Port: 25565, RconPort: 25565, RconPass: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Apply(Props{}, tc.s); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestPropsEscaping(t *testing.T) {
	tests := []struct {
		name string
		k, v string
	}{
		{"plain", "motd", "A Minecraft Server"},
		{"empty value", "level-seed", ""},
		{"backslash", "motd", `C:\path\to`},
		{"newline", "motd", "line one\nline two"},
		{"tab and cr", "motd", "a\tb\rc"},
		{"equals in value", "motd", "a=b"},
		{"colon in value", "motd", "host:25565"},
		{"hash in value", "motd", "#1 server"},
		{"leading space", "motd", "  spaced"},
		{"trailing space", "motd", "spaced  "},
		{"unicode", "motd", "sürvival ✦ ネコ"},
		{"astral", "motd", "creeper 🧨"},
		{"separator in key", "weird=key:1", "v"},
		{"space in key", "weird key", "v"},
		{"password alphabet", "rcon.password", "aB3-_xY9zQw7-Kp2Lm4Nn6Rt8"},
	}
	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "p.properties")
			if err := WriteProps(path, Props{tc.k: tc.v}); err != nil {
				t.Fatal(err)
			}
			got, err := ReadProps(path)
			if err != nil {
				t.Fatal(err)
			}
			if got[tc.k] != tc.v {
				t.Fatalf("round-trip: %q = %q, want %q", tc.k, got[tc.k], tc.v)
			}
			// Whatever we escaped, the file must stay one physical line per key (plus the header),
			// or java's parser would read a value as a key.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(raw), "\n"); n != 2 {
				t.Fatalf("want header + 1 line, got %d lines: %q", n, raw)
			}
		})
	}
}

// Whatever the server (or an admin) wrote, we must read it the way java would.
func TestParsePropsQuirks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"comments and blanks", "# c\n! also a comment\n\n  \na=1\n", map[string]string{"a": "1"}},
		{"colon separator", "a:1\n", map[string]string{"a": "1"}},
		{"space separator", "a 1\n", map[string]string{"a": "1"}},
		{"padded", "  a  =  1  \n", map[string]string{"a": "1  "}}, // java keeps trailing space
		{"no separator", "a\n", map[string]string{"a": ""}},
		{"crlf", "a=1\r\nb=2\r\n", map[string]string{"a": "1", "b": "2"}},
		{"no trailing newline", "a=1", map[string]string{"a": "1"}},
		{"duplicate key: last wins", "a=1\na=2\n", map[string]string{"a": "2"}},
		{"continuation", "a=one\\\n  two\n", map[string]string{"a": "onetwo"}},
		{"escaped backslash is not a continuation", "a=one\\\\\nb=2\n", map[string]string{"a": `one\`, "b": "2"}},
		{"raw utf8 passes through", "a=sürvival\n", map[string]string{"a": "sürvival"}},
		{"unicode escape", "a=s\\u00fcrvival\n", map[string]string{"a": "sürvival"}},
		{"surrogate pair escape", "a=\\ud83e\\uddef\n", map[string]string{"a": "\U0001F9EF"}},
		{"escaped space in value", `a=\ x` + "\n", map[string]string{"a": " x"}},
		{"escaped separator in key", `a\=b=1` + "\n", map[string]string{"a=b": "1"}},
		{"empty file", "", map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProps(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// ── whitelist ─────────────────────────────────────────────────────────────────────────

// The headline invariant: never emit an entry the server would silently drop.
func TestWriteWhitelistRejectsIncompleteEntry(t *testing.T) {
	tests := []struct {
		name string
		e    Entry
	}{
		{"missing uuid", Entry{Name: "nanu"}},
		{"missing name", Entry{UUID: "069a79f4-44e9-4726-a5be-fca90e38aaf5"}},
		{"both missing", Entry{}},
		{"undashed uuid", Entry{UUID: "069a79f444e94726a5befca90e38aaf5", Name: "nanu"}},
		{"truncated uuid", Entry{UUID: "069a79f4-44e9-4726-a5be", Name: "nanu"}},
		{"not hex", Entry{UUID: "zzzzzzzz-44e9-4726-a5be-fca90e38aaf5", Name: "nanu"}},
	}
	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "whitelist.json")
			err := WriteWhitelist(path, []Entry{
				{UUID: "853c80ef-3c37-49fd-aa49-938b674adae6", Name: "jeb_"},
				tc.e,
			})
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("err = %v, want ErrInvalidEntry", err)
			}
			// And nothing was written: a rejected write must not have clobbered the good file.
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a rejected write must leave the file untouched")
			}
		})
	}
}

func TestWhitelistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	want := []Entry{
		{UUID: "069a79f4-44e9-4726-a5be-fca90e38aaf5", Name: "Notch"},
		{UUID: "853c80ef-3c37-49fd-aa49-938b674adae6", Name: "jeb_"},
	}
	if err := WriteWhitelist(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWhitelist(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Every emitted object carries BOTH keys — the server drops an entry that misses either.
	var raw []map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("not a json list: %v", err)
	}
	for i, o := range raw {
		if _, ok := o["uuid"]; !ok {
			t.Errorf("entry %d has no uuid key", i)
		}
		if _, ok := o["name"]; !ok {
			t.Errorf("entry %d has no name key", i)
		}
	}
}

func TestWriteWhitelistEmptyIsAList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	if err := WriteWhitelist(path, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("want [], got %q", b) // "null" is not a list the server can load
	}
}

func TestReadWhitelist(t *testing.T) {
	dir := t.TempDir()

	// Missing file → empty, no error.
	got, err := ReadWhitelist(filepath.Join(dir, "nope.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: got %v, err %v", got, err)
	}

	// An entry the SERVER would drop is dropped here too, so a read-modify-write repairs the file.
	path := filepath.Join(dir, "whitelist.json")
	body := `[{"uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","name":"Notch"},
	          {"uuid":"853c80ef-3c37-49fd-aa49-938b674adae6"},
	          {"name":"orphan"}]`
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err = ReadWhitelist(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Notch" {
		t.Fatalf("got %v, want only the complete entry", got)
	}

	// Corrupt json is an error: hosuto owns this file and would rather regenerate than guess.
	if err := os.WriteFile(path, []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWhitelist(path); err == nil {
		t.Fatal("want an error for malformed json")
	}
}

// ── ops ───────────────────────────────────────────────────────────────────────────────

// An ops entry without "level" deserializes to level 0: an operator who can do nothing.
func TestWriteOpsAlwaysCarriesLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	err := WriteOps(path, []Op{
		{UUID: "069a79f4-44e9-4726-a5be-fca90e38aaf5", Name: "Notch"}, // Level left at zero
		{UUID: "853c80ef-3c37-49fd-aa49-938b674adae6", Name: "jeb_", Level: 4, BypassesPlayerLimit: true},
		{UUID: "61699b2e-d327-4a01-9f1e-0ea8c3f06bc6", Name: "Dinnerbone", Level: 9}, // out of range
	})
	if err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("want 3 entries, got %d", len(raw))
	}
	for i, o := range raw {
		for _, k := range []string{"uuid", "name", "level", "bypassesPlayerLimit"} {
			if _, ok := o[k]; !ok {
				t.Errorf("entry %d is missing %q", i, k)
			}
		}
		if lvl, _ := o["level"].(float64); lvl != OpLevel {
			t.Errorf("entry %d level = %v, want %d (a level-0 op is a silent no-op)", i, o["level"], OpLevel)
		}
	}

	got, err := ReadOps(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Name != "jeb_" || !got[1].BypassesPlayerLimit || got[0].BypassesPlayerLimit {
		t.Fatalf("round-trip lost fields: %v", got)
	}
}

func TestWriteOpsRejectsIncompleteEntry(t *testing.T) {
	tests := []struct {
		name string
		o    Op
	}{
		{"missing uuid", Op{Name: "nanu", Level: 4}},
		{"missing name", Op{UUID: "069a79f4-44e9-4726-a5be-fca90e38aaf5", Level: 4}},
		{"undashed uuid", Op{UUID: "069a79f444e94726a5befca90e38aaf5", Name: "nanu", Level: 4}},
	}
	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "ops.json")
			if err := WriteOps(path, []Op{tc.o}); !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("err = %v, want ErrInvalidEntry", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a rejected write must not create the file")
			}
		})
	}
}

func TestReadOpsMissingFile(t *testing.T) {
	got, err := ReadOps(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestWriteOpsEmptyIsAList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.json")
	if err := WriteOps(path, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("want [], got %q", b)
	}
}

// ── eula, rcon password, io ───────────────────────────────────────────────────────────

func TestWriteEULA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eula.txt")
	if err := WriteEULA(path); err != nil {
		t.Fatal(err)
	}
	// It is a .properties file to the server, so read it as one.
	p, err := ReadProps(path)
	if err != nil {
		t.Fatal(err)
	}
	if p["eula"] != "true" {
		t.Fatalf("eula = %q, want true (without it the server exits on first run)", p["eula"])
	}
}

func TestGenRconPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		p, err := GenRconPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) < 24 {
			t.Fatalf("password %q is only %d chars", p, len(p))
		}
		if seen[p] {
			t.Fatalf("password %q repeated: not random", p)
		}
		seen[p] = true
		// url-safe: nothing that a .properties value or a command line would mangle.
		if strings.ContainsFunc(p, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) {
			t.Fatalf("password %q is not url-safe", p)
		}
	}
}

// A rewrite must land whole, and it must not leave temp files behind for the server to trip over.
func TestWritesAreAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.properties")
	if err := WriteProps(path, Props{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteProps(path, Props{"a": "2"}); err != nil {
		t.Fatal(err)
	}
	p, err := ReadProps(path)
	if err != nil {
		t.Fatal(err)
	}
	if p["a"] != "2" {
		t.Fatalf("a = %q, want the second write", p["a"])
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 (the file holds the rcon password)", fi.Mode().Perm())
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}
