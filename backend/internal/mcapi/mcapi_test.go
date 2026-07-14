package mcapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The real jeb_ profile, as returned by the live endpoint. It is the fixture everything else is
// checked against: if Dash ever disagrees with this, whitelist.json is wrong and players silently
// cannot join.
const (
	jebID     = "853c80ef3c3749fdaa49938b674adae6"
	jebDashed = "853c80ef-3c37-49fd-aa49-938b674adae6"
)

func TestDash(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"real jeb_ id", jebID, jebDashed, false},
		{"real Notch id", "069a79f444e94726a5befca90e38aaf5", "069a79f4-44e9-4726-a5be-fca90e38aaf5", false},
		{"uppercase hex is folded to lowercase", strings.ToUpper(jebID), jebDashed, false},
		{"mixed case", "853C80ef3C3749FDaa49938b674ADAe6", jebDashed, false},
		{"already dashed passes through", jebDashed, jebDashed, false},
		{"already dashed, uppercase", strings.ToUpper(jebDashed), jebDashed, false},
		{"surrounding whitespace is trimmed", "  " + jebID + "\n", jebDashed, false},
		{"all zeroes", strings.Repeat("0", 32), "00000000-0000-0000-0000-000000000000", false},
		{"all f", strings.Repeat("f", 32), "ffffffff-ffff-ffff-ffff-ffffffffffff", false},

		{"empty", "", "", true},
		{"one char short", strings.Repeat("a", 31), "", true},
		{"one char long", strings.Repeat("a", 33), "", true},
		{"non-hex letter", strings.Repeat("g", 32), "", true},
		{"one bad nibble at the end", jebID[:31] + "z", "", true},
		{"one bad nibble at the start", "z" + jebID[1:], "", true},
		{"32 chars but contains a hyphen", "853c80ef-3c3749fdaa49938b674adae", "", true},
		{"36 chars with hyphens in the wrong places", "853c80ef3-c37-49fd-aa49-938b674adae6", "", true},
		{"36 chars, no hyphens at all", strings.Repeat("a", 36), "", true},
		{"dashed but with a bad nibble", "853c80ef-3c37-49fd-aa49-938b674adaez", "", true},
		{"double dashed", "853c80ef--3c37-49fd-aa49-938b674adae", "", true},
		{"non-ascii", strings.Repeat("é", 16), "", true}, // 32 bytes, 16 runes: must error, not panic
		{"a name, not an id", "jeb_", "", true},
		{"nil-ish garbage", "{}", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Dash(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("Dash(%q) = %q, want error", tc.in, got)
				}
				if got != "" {
					t.Fatalf("Dash(%q) returned %q alongside an error; a caller ignoring err must not get a usable-looking uuid", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Dash(%q) errored: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Dash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDashShape pins the structural invariant independently of any table row: whatever Dash emits
// is 36 chars, hyphenated 8-4-4-4-12, and lowercase hex everywhere else.
func TestDashShape(t *testing.T) {
	got, err := Dash(jebID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 36 {
		t.Fatalf("len(%q) = %d, want 36", got, len(got))
	}
	for _, i := range []int{8, 13, 18, 23} {
		if got[i] != '-' {
			t.Errorf("byte %d of %q = %q, want '-'", i, got, got[i])
		}
	}
	groups := strings.Split(got, "-")
	wantLens := []int{8, 4, 4, 4, 12}
	if len(groups) != len(wantLens) {
		t.Fatalf("%q split into %d groups, want 5", got, len(groups))
	}
	for i, g := range groups {
		if len(g) != wantLens[i] {
			t.Errorf("group %d of %q is %d chars, want %d", i, got, len(g), wantLens[i])
		}
		if strings.ToLower(g) != g {
			t.Errorf("group %d of %q is not lowercase", i, got)
		}
	}
	// Round trip: stripping the hyphens must give the id back.
	if bare := strings.ReplaceAll(got, "-", ""); bare != jebID {
		t.Errorf("undashing %q gave %q, want %q", got, bare, jebID)
	}
	// Dash is idempotent — the export package may hand it a uuid that already came from here.
	again, err := Dash(got)
	if err != nil || again != got {
		t.Errorf("Dash(Dash(id)) = %q, %v; want %q, nil", again, err, got)
	}
}

func FuzzDash(f *testing.F) {
	for _, s := range []string{"", jebID, jebDashed, "jeb_", strings.Repeat("0", 32), "----"} {
		f.Add(s)
	}
	// No input may panic, and any success must be a well-formed dashed uuid.
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Dash(s)
		if err != nil {
			return
		}
		if len(got) != 36 {
			t.Fatalf("Dash(%q) = %q: length %d, want 36", s, got, len(got))
		}
		for i, c := range []byte(got) {
			switch i {
			case 8, 13, 18, 23:
				if c != '-' {
					t.Fatalf("Dash(%q) = %q: byte %d is not a hyphen", s, got, i)
				}
			default:
				if _, ok := hexDigit(c); !ok || c >= 'A' && c <= 'F' {
					t.Fatalf("Dash(%q) = %q: byte %d is not lowercase hex", s, got, i)
				}
			}
		}
	})
}

func TestValidName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"jeb_", true},
		{"Notch", true},
		{"abc", true},              // 3 is the floor
		{"aaaaaaaaaaaaaaaa", true}, // 16 is the ceiling
		{"_______", true},          // underscores only is legal
		{"Player123", true},
		{"", false},
		{"ab", false},                // too short
		{"aaaaaaaaaaaaaaaaa", false}, // 17, too long
		{"bad name", false},          // space
		{"bad-name", false},          // hyphen
		{"bad.name", false},          // dot
		{"bad/name", false},          // would escape the URL path
		{"bad\nname", false},         // would smuggle a header
		{"jeb_ ", false},             // trailing space is not trimmed away for us
		{"héllo", false},             // non-ascii
	}
	for _, tc := range tests {
		if got := ValidName(tc.in); got != tc.want {
			t.Errorf("ValidName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// profiles is the fake Mojang universe the httptest servers below resolve against.
var profiles = map[string]struct{ id, name string }{
	"jeb_":       {jebID, "jeb_"},
	"notch":      {"069a79f444e94726a5befca90e38aaf5", "Notch"},
	"dinnerbone": {"2f5d0e6b5a0c4d2e8f1a3b4c5d6e7f80", "Dinnerbone"},
}

// recorder is a stub Mojang. It records every request so a test can assert that a request was NOT
// made (an invalid name must never reach the wire) and that chunking respects the cap of 10.
type recorder struct {
	mu     sync.Mutex
	paths  []string
	chunks [][]string
}

func (rec *recorder) record(path string, names []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.paths = append(rec.paths, path)
	if names != nil {
		rec.chunks = append(rec.chunks, names)
	}
}

func (rec *recorder) calls() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.paths)
}

// mojang serves the two verified endpoints against the fake universe.
func mojang(t *testing.T, rec *recorder) *Client {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+lookupPath+"{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		rec.record(r.URL.Path, nil)
		switch name {
		case "ratelimited":
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case "corrupt": // Mojang answering 200 with an id that cannot be a uuid
			writeJSON(w, map[string]string{"id": "not-a-uuid", "name": "corrupt"})
			return
		case "boom":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "upstream on fire")
			return
		}
		p, ok := profiles[strings.ToLower(name)]
		if !ok {
			w.WriteHeader(http.StatusNotFound) // the verified miss
			return
		}
		writeJSON(w, map[string]string{"id": p.id, "name": p.name})
	})

	mux.HandleFunc("POST "+bulkPath, func(w http.ResponseWriter, r *http.Request) {
		var names []string
		if err := json.NewDecoder(r.Body).Decode(&names); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.record(r.URL.Path, names)
		if len(names) > bulkMax { // the real API's CONSTRAINT_VIOLATION
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errorMessage":"CONSTRAINT_VIOLATION"}`)
			return
		}
		out := []map[string]string{}
		for _, n := range names {
			if n == "boom" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if p, ok := profiles[strings.ToLower(n)]; ok {
				out = append(out, map[string]string{"id": p.id, "name": p.name})
			}
			// misses are silently omitted, exactly as Mojang does
		}
		writeJSON(w, out)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, srv.Client())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestNewBaseURL(t *testing.T) {
	if c := New("", nil); c.base != DefaultBaseURL {
		t.Errorf("New(\"\") base = %q, want %q", c.base, DefaultBaseURL)
	}
	if c := New("", nil); c.hc == nil || c.hc.Timeout != timeout {
		t.Errorf("New with a nil client must supply one with the package timeout")
	}
	// A trailing slash must not produce a double slash in the path.
	if c := New("http://x.test/", nil); c.base != "http://x.test" {
		t.Errorf("base = %q, want the trailing slash trimmed", c.base)
	}
}

func TestLookup(t *testing.T) {
	rec := &recorder{}
	c := mojang(t, rec)
	ctx := context.Background()

	t.Run("hit", func(t *testing.T) {
		got, err := c.Lookup(ctx, "jeb_")
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if got.UUID != jebDashed {
			t.Errorf("UUID = %q, want %q (dashed — whitelist.json rejects anything else)", got.UUID, jebDashed)
		}
		if got.Name != "jeb_" {
			t.Errorf("Name = %q, want %q", got.Name, "jeb_")
		}
	})

	t.Run("mojang's spelling wins", func(t *testing.T) {
		got, err := c.Lookup(ctx, "notch") // asked in lowercase
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if got.Name != "Notch" {
			t.Errorf("Name = %q, want %q — the canonical spelling is what goes in the store", got.Name, "Notch")
		}
	})

	t.Run("the endpoint is the verified one", func(t *testing.T) {
		if _, err := c.Lookup(ctx, "jeb_"); err != nil {
			t.Fatal(err)
		}
		want := "/minecraft/profile/lookup/name/jeb_"
		rec.mu.Lock()
		defer rec.mu.Unlock()
		last := rec.paths[len(rec.paths)-1]
		if last != want {
			t.Errorf("requested %q, want %q", last, want)
		}
	})

	t.Run("404 is ErrNoSuchPlayer", func(t *testing.T) {
		_, err := c.Lookup(ctx, "nobody")
		if !errors.Is(err, ErrNoSuchPlayer) {
			t.Fatalf("err = %v, want ErrNoSuchPlayer", err)
		}
	})

	t.Run("rate limit is not a miss", func(t *testing.T) {
		_, err := c.Lookup(ctx, "ratelimited")
		if err == nil {
			t.Fatal("want an error")
		}
		// Critical: a 429 must NOT look like "no such player", or the UI would tell the user their
		// name does not exist and they would go and change it.
		if errors.Is(err, ErrNoSuchPlayer) {
			t.Fatalf("429 reported as ErrNoSuchPlayer: %v", err)
		}
		if !strings.Contains(err.Error(), "rate limited") {
			t.Errorf("err = %v, want it to name the rate limit", err)
		}
	})

	t.Run("5xx is not a miss", func(t *testing.T) {
		_, err := c.Lookup(ctx, "boom")
		if err == nil || errors.Is(err, ErrNoSuchPlayer) {
			t.Fatalf("err = %v, want a non-ErrNoSuchPlayer error", err)
		}
	})

	t.Run("an unusable id is an error, not a bad uuid", func(t *testing.T) {
		got, err := c.Lookup(ctx, "corrupt")
		if err == nil {
			t.Fatalf("Lookup = %+v, want an error: a uuid that will not dash must never reach the store", got)
		}
	})

	t.Run("invalid names never reach the wire", func(t *testing.T) {
		before := rec.calls()
		for _, bad := range []string{"", "ab", strings.Repeat("a", 17), "bad name", "bad-name", "../../etc/passwd"} {
			_, err := c.Lookup(ctx, bad)
			if !errors.Is(err, ErrNoSuchPlayer) {
				t.Errorf("Lookup(%q) err = %v, want ErrNoSuchPlayer", bad, err)
			}
		}
		if n := rec.calls() - before; n != 0 {
			t.Errorf("%d request(s) spent on names that cannot exist, want 0", n)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		dead, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := c.Lookup(dead, "jeb_"); err == nil {
			t.Fatal("want an error on a cancelled context")
		}
	})
}

func TestLookupBulk(t *testing.T) {
	t.Run("hits, misses and case", func(t *testing.T) {
		rec := &recorder{}
		c := mojang(t, rec)

		got, err := c.LookupBulk(context.Background(), []string{"JEB_", "notch", "nobody"})
		if err != nil {
			t.Fatalf("LookupBulk: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d profiles, want 2 (the miss is silently omitted)", len(got))
		}
		// Keyed by the LOWERCASED name, whatever case the caller or Mojang used.
		jeb, ok := got["jeb_"]
		if !ok {
			t.Fatalf("no key %q in %v", "jeb_", got)
		}
		if jeb.UUID != jebDashed {
			t.Errorf("UUID = %q, want %q", jeb.UUID, jebDashed)
		}
		if got["notch"].Name != "Notch" {
			t.Errorf("Name = %q, want the canonical %q", got["notch"].Name, "Notch")
		}
		if _, ok := got["nobody"]; ok {
			t.Error("a name Mojang does not know must be absent, not zero-valued")
		}
	})

	t.Run("chunks by ten", func(t *testing.T) {
		rec := &recorder{}
		c := mojang(t, rec)

		// 23 distinct names — the stub 400s on an 11th name, so a chunking bug fails loudly here.
		names := make([]string, 0, 23)
		for i := 0; i < 23; i++ {
			names = append(names, "player"+string(rune('A'+i)))
		}
		names[0], names[15] = "jeb_", "Notch" // two real ones, in different chunks

		got, err := c.LookupBulk(context.Background(), names)
		if err != nil {
			t.Fatalf("LookupBulk: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d profiles, want 2", len(got))
		}

		rec.mu.Lock()
		defer rec.mu.Unlock()
		if len(rec.chunks) != 3 {
			t.Fatalf("made %d requests for 23 names, want 3", len(rec.chunks))
		}
		var total int
		for i, ch := range rec.chunks {
			if len(ch) > bulkMax {
				t.Errorf("chunk %d has %d names, over the cap of %d — the real API 400s on this", i, len(ch), bulkMax)
			}
			total += len(ch)
		}
		if total != 23 {
			t.Errorf("sent %d names across the chunks, want 23 — none may be dropped", total)
		}
	})

	t.Run("dedupes case-insensitively and drops impossible names", func(t *testing.T) {
		rec := &recorder{}
		c := mojang(t, rec)

		got, err := c.LookupBulk(context.Background(), []string{"jeb_", "JEB_", "Jeb_", "ab", "bad name", ""})
		if err != nil {
			t.Fatalf("LookupBulk: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d profiles, want 1", len(got))
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if len(rec.chunks) != 1 {
			t.Fatalf("made %d requests, want 1", len(rec.chunks))
		}
		if len(rec.chunks[0]) != 1 {
			t.Errorf("sent %v, want the one name that could possibly resolve", rec.chunks[0])
		}
	})

	t.Run("empty input spends nothing", func(t *testing.T) {
		rec := &recorder{}
		c := mojang(t, rec)

		got, err := c.LookupBulk(context.Background(), nil)
		if err != nil {
			t.Fatalf("LookupBulk: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want an empty map", got)
		}
		if rec.calls() != 0 {
			t.Errorf("made %d requests for no names, want 0", rec.calls())
		}
	})

	t.Run("fails closed on an upstream error", func(t *testing.T) {
		rec := &recorder{}
		c := mojang(t, rec)

		// "boom" makes the stub 500. It sits in the SECOND chunk, so the first chunk already
		// resolved successfully — and must still not be returned: a partial map is
		// indistinguishable from "these players have no account" and would quietly drop them from
		// whitelist.json.
		names := []string{"jeb_", "aaa1", "aaa2", "aaa3", "aaa4", "aaa5", "aaa6", "aaa7", "aaa8", "aaa9", "boom", "notch"}
		got, err := c.LookupBulk(context.Background(), names)
		if err == nil {
			t.Fatalf("LookupBulk = %v, want an error", got)
		}
		if got != nil {
			t.Errorf("LookupBulk returned a partial map alongside the error: %v", got)
		}
	})
}
