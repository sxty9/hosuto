package modrinth

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testUA = "hosuto-test (holistic)"

// ── facets ────────────────────────────────────────────────────────────────────────────

func TestFacets(t *testing.T) {
	tests := []struct {
		name, mc, loader, want string
	}{
		{"full", "1.21.1", "fabric", `[["project_type:mod"],["versions:1.21.1"],["categories:fabric"]]`},
		{"neoforge", "1.20.4", "neoforge", `[["project_type:mod"],["versions:1.20.4"],["categories:neoforge"]]`},
		// vanilla has no loader category — sending one would match nothing.
		{"vanilla", "1.21.1", "vanilla", `[["project_type:mod"],["versions:1.21.1"]]`},
		{"no loader", "1.21.1", "", `[["project_type:mod"],["versions:1.21.1"]]`},
		{"no version", "", "fabric", `[["project_type:mod"],["categories:fabric"]]`},
		{"bare", "", "", `[["project_type:mod"]]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Facets(tc.mc, tc.loader)
			if err != nil {
				t.Fatalf("Facets: %v", err)
			}
			if got != tc.want {
				t.Errorf("Facets(%q,%q)\n got %s\nwant %s", tc.mc, tc.loader, got, tc.want)
			}
		})
	}
}

// The facets value must reach Modrinth as URL-encoded JSON: brackets and quotes percent-escaped in
// the raw query, decoding back to the exact JSON array-of-arrays.
func TestFacetsURLEncoding(t *testing.T) {
	fs, err := Facets("1.21.1", "fabric")
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{}
	q.Set("facets", fs)
	raw := q.Encode()

	if strings.ContainsAny(raw, `[]"`) {
		t.Errorf("raw query must not contain literal brackets/quotes: %s", raw)
	}
	if !strings.Contains(raw, "%5B%5B%22project_type%3Amod%22%5D") {
		t.Errorf("unexpected encoding: %s", raw)
	}
	back, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Get("facets"); got != fs {
		t.Errorf("round trip: got %s want %s", got, fs)
	}
}

// ── env normalisation ─────────────────────────────────────────────────────────────────

func TestEnvNeverGuesses(t *testing.T) {
	tests := []struct{ in, want string }{
		{"required", EnvRequired},
		{"optional", EnvOptional},
		{"unsupported", EnvUnsupported},
		{"", EnvUnknown},         // field absent
		{"REQUIRED", EnvUnknown}, // not the vocabulary — do not normalise case into a guess
		{"unknown", EnvUnknown},  // Modrinth's own "unknown", if it ever sends one
		{"whatever", EnvUnknown}, // a value added after this code was written
	}
	for _, tc := range tests {
		if got := env(tc.in); got != tc.want {
			t.Errorf("env(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSafety(t *testing.T) {
	tests := []struct {
		client, server         string
		wantClient, wantServer bool
	}{
		{EnvRequired, EnvRequired, true, true},
		{EnvOptional, EnvOptional, true, true},
		{EnvUnsupported, EnvRequired, false, true}, // server-only mod: never ship to the client
		{EnvRequired, EnvUnsupported, true, false}, // client-only mod: never install on the server
		{EnvUnknown, EnvUnknown, true, true},       // silence is not a prohibition
	}
	for _, tc := range tests {
		h := Hit{ClientSide: tc.client, ServerSide: tc.server}
		if h.ClientSafe() != tc.wantClient || h.ServerSafe() != tc.wantServer {
			t.Errorf("Hit{%s,%s}: client=%v server=%v, want client=%v server=%v",
				tc.client, tc.server, h.ClientSafe(), h.ServerSafe(), tc.wantClient, tc.wantServer)
		}
	}
}

// ── primary file selection ────────────────────────────────────────────────────────────

func TestPrimaryFile(t *testing.T) {
	tests := []struct {
		name  string
		files []File
		want  string // filename, "" = none
	}{
		{"primary flag wins over order", []File{
			{Filename: "sources.jar"},
			{Filename: "mod.jar", Primary: true},
		}, "mod.jar"},
		{"primary flag wins over a jar that comes first", []File{
			{Filename: "extra.jar"},
			{Filename: "real.jar", Primary: true},
			{Filename: "other.jar"},
		}, "real.jar"},
		{"no flag: first jar", []File{
			{Filename: "notes.txt"},
			{Filename: "mod.jar"},
			{Filename: "second.jar"},
		}, "mod.jar"},
		{"no flag: case-insensitive suffix", []File{{Filename: "MOD.JAR"}}, "MOD.JAR"},
		{"no jar at all", []File{{Filename: "mod.zip"}}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := Version{Files: tc.files}.PrimaryFile()
			if tc.want == "" {
				if ok {
					t.Fatalf("want no primary file, got %q", f.Filename)
				}
				return
			}
			if !ok || f.Filename != tc.want {
				t.Fatalf("got %q (ok=%v), want %q", f.Filename, ok, tc.want)
			}
		})
	}
}

func TestToMod(t *testing.T) {
	v := Version{
		ID: "vAAA", ProjectID: "P1", Name: "Sodium 0.6",
		Files: []File{
			{Filename: "sources.jar"},
			{Filename: "sodium.jar", URL: "https://cdn/sodium.jar", SHA1: "aa", SHA512: "bb", Size: 12, Primary: true},
		},
	}
	// A project whose environment Modrinth did not report must land as "unknown", not as a default.
	m, err := ToMod(v, Hit{ProjectID: "P1", Title: "Sodium", ClientSide: "required", ServerSide: ""})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "modrinth" || m.ProjectID != "P1" || m.VersionID != "vAAA" {
		t.Errorf("identity: %+v", m)
	}
	if m.Filename != "sodium.jar" || m.SHA1 != "aa" || m.SHA512 != "bb" || m.Size != 12 {
		t.Errorf("file: %+v", m)
	}
	if m.Name != "Sodium" {
		t.Errorf("Name = %q, want the project title", m.Name)
	}
	if m.ClientSide != EnvRequired || m.ServerSide != EnvUnknown {
		t.Errorf("env: client=%q server=%q, want required/unknown", m.ClientSide, m.ServerSide)
	}

	if _, err := ToMod(Version{ID: "v0", Files: []File{{Filename: "x.zip"}}}, Hit{}); err == nil {
		t.Error("a version with no jar must not become a store.Mod")
	}
}

// ── fixtures ──────────────────────────────────────────────────────────────────────────

const searchJSON = `{"hits":[
 {"project_id":"AANobbMI","slug":"sodium","title":"Sodium","description":"Rendering engine",
  "icon_url":"https://cdn/sodium.png","client_side":"required","server_side":"unsupported","downloads":1000},
 {"project_id":"P2","slug":"lithium","title":"Lithium","description":"Optimisation",
  "client_side":"optional","server_side":"optional","downloads":500},
 {"project_id":"P3","slug":"weird","title":"Weird","description":"No env fields","downloads":1}
]}`

const projectJSON = `{"id":"AANobbMI","slug":"sodium","title":"Sodium","description":"Rendering engine",
 "icon_url":"https://cdn/sodium.png","client_side":"required","server_side":"unsupported","downloads":1000}`

// Two matching versions (out of order) plus one for the wrong loader and one for the wrong game
// version — the API is asked to filter, but the client must not trust that it did.
const versionsJSON = `[
 {"id":"vOLD","project_id":"AANobbMI","name":"Sodium 0.5.8","version_number":"mc1.21.1-0.5.8",
  "date_published":"2024-08-01T10:00:00Z","game_versions":["1.21.1"],"loaders":["fabric"],
  "files":[{"url":"https://cdn/old.jar","filename":"old.jar","primary":true,"size":100,
            "hashes":{"sha1":"a1","sha512":"a5"}}],
  "dependencies":[]},
 {"id":"vNEW","project_id":"AANobbMI","name":"Sodium 0.6.0","version_number":"mc1.21.1-0.6.0",
  "date_published":"2024-12-01T10:00:00Z","game_versions":["1.21.1","1.21"],"loaders":["fabric"],
  "files":[{"url":"https://cdn/sources.jar","filename":"sources.jar","primary":false,"size":10,
            "hashes":{"sha1":"s1","sha512":"s5"}},
           {"url":"https://cdn/new.jar","filename":"new.jar","primary":true,"size":200,
            "hashes":{"sha1":"n1","sha512":"n5"}}],
  "dependencies":[{"project_id":"DEP1","version_id":null,"dependency_type":"required"},
                  {"project_id":null,"version_id":"vDEP","dependency_type":"optional"}]},
 {"id":"vWRONGLOADER","project_id":"AANobbMI","name":"neo","version_number":"n",
  "date_published":"2025-01-01T10:00:00Z","game_versions":["1.21.1"],"loaders":["neoforge"],
  "files":[{"url":"https://cdn/n.jar","filename":"n.jar","primary":true,"size":1,
            "hashes":{"sha1":"x","sha512":"y"}}],"dependencies":[]},
 {"id":"vWRONGMC","project_id":"AANobbMI","name":"old mc","version_number":"o",
  "date_published":"2025-02-01T10:00:00Z","game_versions":["1.20.1"],"loaders":["fabric"],
  "files":[{"url":"https://cdn/o.jar","filename":"o.jar","primary":true,"size":1,
            "hashes":{"sha1":"x","sha512":"y"}}],"dependencies":[]}
]`

// POST /version_files answers hash → version. Keyed by the sha1 of the primary file of vNEW.
const versionFilesJSON = `{"n1":
 {"id":"vNEW","project_id":"AANobbMI","name":"Sodium 0.6.0","version_number":"mc1.21.1-0.6.0",
  "date_published":"2024-12-01T10:00:00Z","game_versions":["1.21.1"],"loaders":["fabric"],
  "files":[{"url":"https://cdn/new.jar","filename":"new.jar","primary":true,"size":200,
            "hashes":{"sha1":"n1","sha512":"n5"}}],
  "dependencies":[]}}`

// recorder captures what the client actually put on the wire, so the encoding assertions test the
// request rather than the client's own idea of it.
type recorder struct {
	hits atomic.Int32
	last *http.Request
	body string
}

func newServer(t *testing.T) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.last = r
		b, _ := io.ReadAll(r.Body)
		rec.body = string(b)

		if r.Header.Get("User-Agent") != testUA {
			http.Error(w, `{"error":"bad user agent"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/search":
			_, _ = w.Write([]byte(searchJSON))
		case "/v2/project/AANobbMI":
			_, _ = w.Write([]byte(projectJSON))
		case "/v2/project/AANobbMI/version":
			_, _ = w.Write([]byte(versionsJSON))
		case "/v2/project/ghost":
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case "/v2/project/ghost/version":
			_, _ = w.Write([]byte(`[]`))
		case "/v2/version_files":
			_, _ = w.Write([]byte(versionFilesJSON))
		default:
			http.Error(w, `{"error":"unhandled"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return New(ts.URL+"/v2", testUA, ts.Client()), rec
}

func TestSearch(t *testing.T) {
	c, rec := newServer(t)
	hits, err := c.Search(context.Background(), "sodium", "1.21.1", "fabric", 20)
	if err != nil {
		t.Fatal(err)
	}

	q := rec.last.URL.Query()
	if got, want := q.Get("facets"), `[["project_type:mod"],["versions:1.21.1"],["categories:fabric"]]`; got != want {
		t.Errorf("facets on the wire:\n got %s\nwant %s", got, want)
	}
	if q.Get("query") != "sodium" || q.Get("limit") != "20" {
		t.Errorf("query/limit: %v", q)
	}

	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	if hits[0].ProjectID != "AANobbMI" || hits[0].Title != "Sodium" || hits[0].Downloads != 1000 {
		t.Errorf("hit[0] = %+v", hits[0])
	}
	// Sodium is client-only: it must be visible in search but barred from the server.
	if hits[0].ServerSide != EnvUnsupported || hits[0].ServerSafe() {
		t.Errorf("hit[0] server env = %q, ServerSafe=%v", hits[0].ServerSide, hits[0].ServerSafe())
	}
	// The third hit has no environment fields at all: they must read "unknown", not "".
	if hits[2].ClientSide != EnvUnknown || hits[2].ServerSide != EnvUnknown {
		t.Errorf("hit[2] env = %q/%q, want unknown/unknown", hits[2].ClientSide, hits[2].ServerSide)
	}
}

func TestSearchLimitClamped(t *testing.T) {
	c, rec := newServer(t)
	if _, err := c.Search(context.Background(), "x", "1.21.1", "fabric", 5000); err != nil {
		t.Fatal(err)
	}
	if got := rec.last.URL.Query().Get("limit"); got != "100" {
		t.Errorf("limit = %s, want it clamped to 100", got)
	}
}

// The project document names the id "id"; the search hit names it "project_id". Both must land in
// Hit.ProjectID, or Resolve returns a mod the store cannot re-resolve later.
func TestProject(t *testing.T) {
	c, _ := newServer(t)
	h, err := c.Project(context.Background(), "AANobbMI")
	if err != nil {
		t.Fatal(err)
	}
	if h.ProjectID != "AANobbMI" {
		t.Errorf("ProjectID = %q, want AANobbMI (from the document's \"id\")", h.ProjectID)
	}
	if h.ClientSide != EnvRequired || h.ServerSide != EnvUnsupported {
		t.Errorf("env = %q/%q", h.ClientSide, h.ServerSide)
	}
}

func TestProjectNotFound(t *testing.T) {
	c, _ := newServer(t)
	if _, err := c.Project(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestVersions(t *testing.T) {
	c, rec := newServer(t)
	vs, err := c.Versions(context.Background(), "AANobbMI", "1.21.1", "fabric")
	if err != nil {
		t.Fatal(err)
	}

	q := rec.last.URL.Query()
	if got, want := q.Get("game_versions"), `["1.21.1"]`; got != want {
		t.Errorf("game_versions = %s, want %s", got, want)
	}
	if got, want := q.Get("loaders"), `["fabric"]`; got != want {
		t.Errorf("loaders = %s, want %s", got, want)
	}

	// vWRONGLOADER and vWRONGMC are newer, and the fixture server returned them anyway. The client
	// must drop them rather than resolve to a jar that would brick the server.
	if len(vs) != 2 {
		t.Fatalf("got %d versions, want 2 (the two matching ones)", len(vs))
	}
	if vs[0].ID != "vNEW" || vs[1].ID != "vOLD" {
		t.Fatalf("order = %s,%s — want newest (vNEW) first", vs[0].ID, vs[1].ID)
	}
	f, ok := vs[0].PrimaryFile()
	if !ok || f.Filename != "new.jar" || f.SHA1 != "n1" || f.SHA512 != "n5" || f.Size != 200 {
		t.Errorf("primary file = %+v", f)
	}
	if len(vs[0].Dependencies) != 2 {
		t.Fatalf("deps = %+v", vs[0].Dependencies)
	}
	// A null project_id / version_id must decode to "", not blow up.
	if d := vs[0].Dependencies[0]; d.ProjectID != "DEP1" || d.VersionID != "" || d.Type != "required" {
		t.Errorf("dep[0] = %+v", d)
	}
	if d := vs[0].Dependencies[1]; d.ProjectID != "" || d.VersionID != "vDEP" || d.Type != "optional" {
		t.Errorf("dep[1] = %+v", d)
	}
}

func TestVersionsVanillaSendsNoLoaderFacet(t *testing.T) {
	c, rec := newServer(t)
	if _, err := c.Versions(context.Background(), "AANobbMI", "1.21.1", "vanilla"); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.last.URL.Query()["loaders"]; ok {
		t.Error("vanilla must not send a loaders parameter")
	}
}

func TestResolve(t *testing.T) {
	c, _ := newServer(t)
	v, h, err := c.Resolve(context.Background(), "AANobbMI", "1.21.1", "fabric")
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "vNEW" {
		t.Errorf("version = %s, want the newest matching (vNEW)", v.ID)
	}
	if h.ProjectID != "AANobbMI" || h.ClientSide != EnvRequired || h.ServerSide != EnvUnsupported {
		t.Errorf("project meta = %+v", h)
	}

	m, err := ToMod(v, h)
	if err != nil {
		t.Fatal(err)
	}
	if m.ServerSide != EnvUnsupported {
		t.Errorf("the store record must carry server_side=unsupported, got %q", m.ServerSide)
	}
}

func TestResolveNoMatchingVersion(t *testing.T) {
	c, _ := newServer(t)
	_, _, err := c.Resolve(context.Background(), "ghost", "1.21.1", "fabric")
	if !errors.Is(err, ErrNoVersion) {
		t.Fatalf("err = %v, want ErrNoVersion", err)
	}
}

func TestVersionsByHash(t *testing.T) {
	c, rec := newServer(t)
	got, err := c.VersionsByHash(context.Background(), []string{"N1"}) // upper case must be normalised
	if err != nil {
		t.Fatal(err)
	}
	if rec.last.Method != http.MethodPost || rec.last.URL.Path != "/v2/version_files" {
		t.Fatalf("want a single POST to /version_files, got %s %s", rec.last.Method, rec.last.URL.Path)
	}
	if !strings.Contains(rec.body, `"algorithm":"sha1"`) || !strings.Contains(rec.body, `"n1"`) {
		t.Errorf("request body = %s", rec.body)
	}
	v, ok := got["n1"]
	if !ok {
		t.Fatalf("no entry for n1: %v", got)
	}
	if v.ID != "vNEW" || v.ProjectID != "AANobbMI" {
		t.Errorf("version = %+v", v)
	}
}

// A hot path (the mod tab re-rendering) must not re-hit Modrinth for every keystroke.
func TestCacheSuppressesRepeatGET(t *testing.T) {
	c, rec := newServer(t)
	for range 3 {
		if _, err := c.Search(context.Background(), "sodium", "1.21.1", "fabric", 20); err != nil {
			t.Fatal(err)
		}
	}
	if n := rec.hits.Load(); n != 1 {
		t.Errorf("%d upstream requests for 3 identical searches, want 1", n)
	}
	// A different query is a different key and must go out.
	if _, err := c.Search(context.Background(), "lithium", "1.21.1", "fabric", 20); err != nil {
		t.Fatal(err)
	}
	if n := rec.hits.Load(); n != 2 {
		t.Errorf("hits = %d, want 2", n)
	}
}

// Modrinth's terms require a descriptive agent; the fixture server rejects anything else, standing
// in for Modrinth's own 403.
func TestUserAgentIsSent(t *testing.T) {
	c, _ := newServer(t)
	if _, err := c.Project(context.Background(), "AANobbMI"); err != nil {
		t.Fatalf("the configured user agent must be sent: %v", err)
	}
}

// ── download ──────────────────────────────────────────────────────────────────────────

func fileServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func hashes(b []byte) (string, string) {
	s1 := sha1.Sum(b)
	s5 := sha512.Sum512(b)
	return hex.EncodeToString(s1[:]), hex.EncodeToString(s5[:])
}

func TestDownload(t *testing.T) {
	payload := []byte("PK\x03\x04 pretend this is a jar")
	s1, s5 := hashes(payload)
	ts := fileServer(t, payload)
	c := New(ts.URL, testUA, ts.Client())

	dst := filepath.Join(t.TempDir(), "mods", "sodium.jar")
	f := File{URL: ts.URL + "/sodium.jar", Filename: "sodium.jar", SHA1: s1, SHA512: s5, Size: int64(len(payload))}
	if err := c.Download(context.Background(), f, dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("content = %q", got)
	}
}

// The core safety property: bytes that do not match the promised hash never reach dst.
func TestDownloadRejectsHashMismatch(t *testing.T) {
	payload := []byte("this is not the jar you asked for")
	good1, good5 := hashes([]byte("the real jar"))
	real1, real5 := hashes(payload)
	ts := fileServer(t, payload)
	c := New(ts.URL, testUA, ts.Client())

	tests := []struct {
		name string
		f    File
		want error
	}{
		{"sha1 mismatch", File{Filename: "m.jar", SHA1: good1, SHA512: real5}, ErrChecksum},
		{"sha512 mismatch", File{Filename: "m.jar", SHA1: real1, SHA512: good5}, ErrChecksum},
		{"both mismatch", File{Filename: "m.jar", SHA1: good1, SHA512: good5}, ErrChecksum},
		// A file with no checksum at all is unverifiable remote code: refuse it outright.
		{"no checksum", File{Filename: "m.jar"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "m.jar")
			tc.f.URL = ts.URL + "/m.jar"

			err := c.Download(context.Background(), tc.f, dst)
			if err == nil {
				t.Fatal("Download accepted a file it must reject")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatal("a rejected download must not be left at dst")
			}
			// Nor may the partial temp file survive.
			ents, _ := os.ReadDir(dir)
			if len(ents) != 0 {
				t.Fatalf("leftover files: %v", ents)
			}
		})
	}
}

func TestDownloadRejectsSizeMismatch(t *testing.T) {
	payload := []byte("short")
	s1, s5 := hashes(payload)
	ts := fileServer(t, payload)
	c := New(ts.URL, testUA, ts.Client())

	dst := filepath.Join(t.TempDir(), "m.jar")
	f := File{URL: ts.URL + "/m.jar", Filename: "m.jar", SHA1: s1, SHA512: s5, Size: 9999}
	if err := c.Download(context.Background(), f, dst); err == nil {
		t.Fatal("a size mismatch must be rejected even when the hashes match")
	}
}
