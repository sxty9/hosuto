package versions

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ── fixtures ──────────────────────────────────────────────────────────────────────────
//
// Trimmed captures of each response shape. They are the contract this package parses; if an
// upstream changes shape, a test here is what should break.

const fabricLoadersJSON = `[
  {"separator":".","build":15,"maturity":"unstable","version":"0.17.0-beta.1","stable":false},
  {"separator":".","build":14,"maturity":"stable","version":"0.16.14","stable":true},
  {"separator":".","build":13,"maturity":"stable","version":"0.16.13","stable":true}
]`

const fabricInstallersJSON = `[
  {"url":"https://maven.fabricmc.net/x-1.1.0.jar","maven":"net.fabricmc:fabric-installer:1.1.0","version":"1.1.0","stable":false},
  {"url":"https://maven.fabricmc.net/x-1.0.3.jar","maven":"net.fabricmc:fabric-installer:1.0.3","version":"1.0.3","stable":true}
]`

// Note 21.0.x: those target MC "1.21", not "1.21.0".
const neoMavenXML = `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>net.neoforged</groupId>
  <artifactId>neoforge</artifactId>
  <versioning>
    <versions>
      <version>20.4.237</version>
      <version>21.0.167</version>
      <version>21.1.1</version>
      <version>21.1.235</version>
      <version>21.2.0-beta</version>
    </versions>
  </versioning>
</metadata>`

const paperBuildsJSON = `{"project_id":"paper","version":"1.21.1","builds":[
  {"build":128,"channel":"default","downloads":{"application":{"name":"paper-1.21.1-128.jar","sha256":"aa"}}},
  {"build":131,"channel":"default","downloads":{"application":{"name":"paper-1.21.1-131.jar","sha256":"bb"}}},
  {"build":132,"channel":"experimental","downloads":{"application":{"name":"paper-1.21.1-132.jar","sha256":"cc"}}}
]}`

// fake is an httptest server standing in for all four upstreams, wired into a Client.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	c      *Client
	hits   map[string]int
	jar    []byte // bytes served for any /jar path
	jarSHA string // sha1 advertised in the vanilla version meta (may be wrong, to test mismatch)
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, hits: map[string]int{}, jar: []byte("PK\x03\x04 pretend server jar")}
	sum := sha1.Sum(f.jar)
	f.jarSHA = hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	base := f.srv.URL

	count := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.hits[r.URL.Path]++
			h(w, r)
		}
	}

	mux.HandleFunc("/manifest", count(func(w http.ResponseWriter, _ *http.Request) {
		// Snapshot deliberately listed above the releases, as Mojang really does.
		fmt.Fprintf(w, `{"latest":{"release":"1.21.1","snapshot":"24w14a"},"versions":[
		  {"id":"24w14a","type":"snapshot","url":"%[1]s/v/24w14a.json","sha1":"x"},
		  {"id":"1.21.1","type":"release","url":"%[1]s/v/1.21.1.json","sha1":"x"},
		  {"id":"1.20.4","type":"release","url":"%[1]s/v/1.20.4.json","sha1":"x"},
		  {"id":"1.16.5","type":"release","url":"%[1]s/v/1.16.5.json","sha1":"x"},
		  {"id":"b1.7.3","type":"old_beta","url":"%[1]s/v/b1.7.3.json","sha1":"x"}
		]}`, base)
	}))
	// 1.21.1 carries javaVersion; 1.16.5 (like the real old entries) does not, and must fall back
	// to the matrix. b1.7.3 has no server download at all.
	mux.HandleFunc("/v/1.21.1.json", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"downloads":{"server":{"sha1":"%s","size":%d,"url":"%s/jar/server.jar"}},
		  "javaVersion":{"component":"java-runtime-delta","majorVersion":21}}`, f.jarSHA, len(f.jar), base)
	}))
	mux.HandleFunc("/v/1.16.5.json", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"downloads":{"server":{"sha1":"%s","size":%d,"url":"%s/jar/server.jar"}}}`,
			f.jarSHA, len(f.jar), base)
	}))
	mux.HandleFunc("/v/1.20.4.json", count(func(w http.ResponseWriter, _ *http.Request) {
		// Advertises a digest that does not match the bytes: the mismatch path.
		fmt.Fprintf(w, `{"downloads":{"server":{"sha1":"%s","size":9,"url":"%s/jar/server.jar"}},
		  "javaVersion":{"majorVersion":17}}`, strings.Repeat("0", 40), base)
	}))
	mux.HandleFunc("/v/b1.7.3.json", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"downloads":{}}`)
	}))

	mux.HandleFunc("/v2/versions/loader", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, fabricLoadersJSON)
	}))
	mux.HandleFunc("/v2/versions/installer", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, fabricInstallersJSON)
	}))
	mux.HandleFunc("/neo/maven-metadata.xml", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, neoMavenXML)
	}))
	mux.HandleFunc("/paper/versions/1.21.1/builds", count(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, paperBuildsJSON)
	}))
	// Paper answers 404 for a Minecraft version it never supported.
	mux.HandleFunc("/paper/versions/1.7.10/builds", count(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))

	mux.HandleFunc("/jar/", count(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(f.jar)
	}))
	// Paper serves the jar under the build it came from.
	mux.HandleFunc("/paper/versions/{v}/builds/{b}/downloads/{name}", count(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(f.jar)
	}))
	mux.HandleFunc("/v2/versions/loader/1.21.1/0.16.14/1.0.3/server/jar", count(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(f.jar)
	}))

	f.c = New(f.srv.Client())
	f.c.manifestURL = base + "/manifest"
	f.c.fabricBase = base
	f.c.neoBase = base + "/neo"
	f.c.paperBase = base + "/paper"
	// The host running these tests need not have a JDK.
	f.c.javaBin = func(major int) (string, error) { return fmt.Sprintf("/jvm/%d/java", major), nil }
	return f
}

// ── catalogue ─────────────────────────────────────────────────────────────────────────

func TestMinecraftVersionsOrder(t *testing.T) {
	f := newFake(t)
	got, err := f.c.MinecraftVersions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Version{
		{"1.21.1", "release"}, {"1.20.4", "release"}, {"1.16.5", "release"},
		{"24w14a", "snapshot"},
		{"b1.7.3", "old_beta"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("releases must come first, newest first within a type\n got %v\nwant %v", got, want)
	}
}

func TestManifestCached(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.c.MinecraftVersions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits["/manifest"] != 1 {
		t.Errorf("manifest fetched %d times, want 1 (the catalogue is on the UI hot path)", f.hits["/manifest"])
	}
	// Past the TTL it must be re-fetched, or an admin never sees a new release.
	f.c.now = func() time.Time { return time.Now().Add(cacheTTL + time.Second) }
	if _, err := f.c.MinecraftVersions(ctx); err != nil {
		t.Fatal(err)
	}
	if f.hits["/manifest"] != 2 {
		t.Errorf("manifest not re-fetched after TTL: %d hits", f.hits["/manifest"])
	}
}

func TestLatest(t *testing.T) {
	f := newFake(t)
	rel, snap, err := f.c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel != "1.21.1" || snap != "24w14a" {
		t.Errorf("Latest() = %q, %q", rel, snap)
	}
}

func TestLoaderVersions(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	tests := []struct {
		name, loader, mc string
		want             []string
	}{
		// Vanilla has no loader at all.
		{"vanilla", "vanilla", "1.21.1", nil},
		// Pre-releases are dropped; upstream's newest-first order is kept.
		{"fabric stable only", "fabric", "1.21.1", []string{"0.16.14", "0.16.13"}},
		// Maven is chronological, so it must come back reversed; the -beta is dropped.
		{"neoforge 1.21.1", "neoforge", "1.21.1", []string{"21.1.235", "21.1.1"}},
		// The patch-zero trap: 21.0.167 targets "1.21", not "1.21.0".
		{"neoforge 1.21", "neoforge", "1.21", []string{"21.0.167"}},
		{"neoforge 1.20.4", "neoforge", "1.20.4", []string{"20.4.237"}},
		// Nothing stable for 1.21.2, so the beta is offered rather than an empty picker.
		{"neoforge beta only", "neoforge", "1.21.2", []string{"21.2.0-beta"}},
		{"neoforge unsupported mc", "neoforge", "1.7.10", nil},
		// Experimental Paper builds are dropped; newest first.
		{"paper", "paper", "1.21.1", []string{"131", "128"}},
		// A 404 from Paper means "no build for that version" — a normal answer, not an error.
		{"paper unsupported mc", "paper", "1.7.10", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.c.LoaderVersions(ctx, tt.loader, tt.mc)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LoaderVersions(%q, %q) = %v, want %v", tt.loader, tt.mc, got, tt.want)
			}
		})
	}

	if _, err := f.c.LoaderVersions(ctx, "forge", "1.21.1"); !errors.Is(err, ErrUnsupportedLoader) {
		t.Errorf("unknown loader: err = %v, want ErrUnsupportedLoader", err)
	}
}

// ── java ──────────────────────────────────────────────────────────────────────────────

func TestJavaMajorFor(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	// Mojang publishes javaVersion for modern versions: it wins.
	if got, err := f.c.JavaMajorFor(ctx, "1.21.1"); err != nil || got != 21 {
		t.Errorf("1.21.1 = %d, %v; want 21", got, err)
	}
	if got, err := f.c.JavaMajorFor(ctx, "1.20.4"); err != nil || got != 17 {
		t.Errorf("1.20.4 = %d, %v; want 17", got, err)
	}
	// 1.16.5's meta omits javaVersion, so the matrix decides.
	if got, err := f.c.JavaMajorFor(ctx, "1.16.5"); err != nil || got != 8 {
		t.Errorf("1.16.5 (no javaVersion) = %d, %v; want the matrix's 8", got, err)
	}
	// A version Mojang does not publish must fail loudly, never silently guess a JRE.
	if _, err := f.c.JavaMajorFor(ctx, "1.99.9"); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unknown version: err = %v, want ErrUnknownVersion", err)
	}
}

func TestJavaMajorFallback(t *testing.T) {
	tests := []struct {
		mc   string
		want int
	}{
		{"1.16.5", 8},
		{"1.17", 16},
		{"1.17.1", 16},
		{"1.18", 17},
		{"1.19.4", 17},
		{"1.20", 17},
		{"1.20.4", 17},
		{"1.20.5", 21}, // the mid-cycle jump
		{"1.20.6", 21},
		{"1.21", 21},
		{"1.21.4", 21},
		{"1.22", 21},
		{"24w14a", 21}, // snapshot: unparseable, assume newest
		{"", 21},
		{"garbage", 21},
		{"1.x.y", 21},
	}
	for _, tt := range tests {
		if got := javaMajorFallback(tt.mc); got != tt.want {
			t.Errorf("javaMajorFallback(%q) = %d, want %d", tt.mc, got, tt.want)
		}
	}
}

func TestParseMC(t *testing.T) {
	tests := []struct {
		id           string
		minor, patch int
		ok           bool
	}{
		{"1.21.1", 21, 1, true},
		{"1.21", 21, 0, true},
		{"1.20.4", 20, 4, true},
		{"24w14a", 0, 0, false},
		{"1.21.1-pre1", 0, 0, false},
		{"a1.2.6", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		minor, patch, ok := parseMC(tt.id)
		if minor != tt.minor || patch != tt.patch || ok != tt.ok {
			t.Errorf("parseMC(%q) = %d, %d, %v; want %d, %d, %v",
				tt.id, minor, patch, ok, tt.minor, tt.patch, tt.ok)
		}
	}
}

func TestJavaBinIn(t *testing.T) {
	root := t.TempDir()
	mkjava := func(dir string, mode os.FileMode) string {
		p := filepath.Join(root, dir, "bin", "java")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	j21 := mkjava("java-21-openjdk-amd64", 0o755)
	j17 := mkjava("java-17-openjdk-arm64", 0o755)
	mkjava("java-11-openjdk-amd64", 0o644)     // present but NOT executable → never selected
	mkjava("java-1.21.0-openjdk-amd64", 0o755) // dotted legacy alias → ignored (not a clean major)

	// Exact match available → use it.
	if got, err := javaBinIn(root, 21); err != nil || got != j21 {
		t.Errorf("javaBinIn(21) = %q, %v; want %q", got, err, j21)
	}
	// Needs 17, both 17 and 21 present → the CLOSEST that satisfies, i.e. 17.
	if got, err := javaBinIn(root, 17); err != nil || got != j17 {
		t.Errorf("javaBinIn(17) = %q, %v; want the closest satisfying JDK %q", got, err, j17)
	}
	// Needs 11: java-11 is non-executable and must be skipped, but java-17 satisfies >= 11 — a newer
	// runtime is a valid answer, not an error.
	if got, err := javaBinIn(root, 11); err != nil || got != j17 {
		t.Errorf("javaBinIn(11) = %q, %v; want the next-newest executable JDK %q (non-exec java-11 skipped)", got, err, j17)
	}
	// Needs 25: nothing that new is installed → a clear error, NOT a silent fall-through to Java 21.
	if _, err := javaBinIn(root, 25); !errors.Is(err, ErrNoJava) {
		t.Errorf("javaBinIn(25) with no Java 25+ must be ErrNoJava, got %v", err)
	}
}

// ── neoforge version mapping ──────────────────────────────────────────────────────────

func TestNeoforgeMC(t *testing.T) {
	tests := []struct {
		v      string
		mc     string
		stable bool
		ok     bool
	}{
		{"21.1.235", "1.21.1", true, true},
		{"20.4.237", "1.20.4", true, true},
		{"21.0.167", "1.21", true, true}, // patch 0 ⇒ no third component
		{"21.2.0-beta", "1.21.2", false, true},
		{"20.2.86", "1.20.2", true, true},
		{"21.1", "", false, false}, // too few components
		{"x.1.235", "", false, false},
		{"21.x.235", "", false, false},
		{"21.1.x", "", false, false},
		{"", "", false, false},
	}
	for _, tt := range tests {
		mc, stable, ok := neoforgeMC(tt.v)
		if mc != tt.mc || stable != tt.stable || ok != tt.ok {
			t.Errorf("neoforgeMC(%q) = %q, %v, %v; want %q, %v, %v",
				tt.v, mc, stable, ok, tt.mc, tt.stable, tt.ok)
		}
	}
}

// ── install ───────────────────────────────────────────────────────────────────────────

func TestInstallVanilla(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	argv, resolved, err := f.c.Install(context.Background(), dir, "vanilla", "1.21.1", "", 2048)
	if err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(dir, "server.jar")
	want := []string{"/jvm/21/java", "-Xms2048M", "-Xmx2048M", "-jar", jar, "nogui"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	// Vanilla has no loader, so it must claim no loader version rather than invent one.
	if resolved != "" {
		t.Errorf("resolved loader version = %q, want empty for vanilla", resolved)
	}
	b, err := os.ReadFile(jar)
	if err != nil {
		t.Fatalf("server.jar not installed: %v", err)
	}
	if string(b) != string(f.jar) {
		t.Error("server.jar contents differ from what was served")
	}
}

func TestInstallVanillaHashMismatch(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	// 1.20.4's meta advertises a digest that does not match the served bytes.
	_, _, err := f.c.Install(context.Background(), dir, "vanilla", "1.20.4", "", 1024)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}
	// Nothing may survive a failed verification — not the final name, not the partial.
	for _, name := range []string{"server.jar", "server.jar.part"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists after a hash mismatch; a corrupt jar must never be left behind", name)
		}
	}
}

func TestInstallVanillaNoServerJar(t *testing.T) {
	f := newFake(t)
	// The old_beta tail has no server download; the error must say so rather than 404 obscurely.
	_, _, err := f.c.Install(context.Background(), t.TempDir(), "vanilla", "b1.7.3", "", 1024)
	if !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("err = %v, want ErrUnknownVersion", err)
	}
}

func TestInstallFabric(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	// An empty loader version must resolve to the newest stable loader (0.16.14) and the newest
	// stable installer (1.0.3) — the fake only serves that exact path.
	argv, resolved, err := f.c.Install(context.Background(), dir, "fabric", "1.21.1", "", 4096)
	if err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(dir, "fabric-server-launch.jar")
	want := []string{"/jvm/21/java", "-Xms4096M", "-Xmx4096M", "-jar", jar, "nogui"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if _, err := os.Stat(jar); err != nil {
		t.Errorf("fabric launch jar not installed: %v", err)
	}
	// The whole point of reporting it: the caller asked for "" and must learn which loader that
	// actually became, or the server record can never name what it is running.
	if resolved != "0.16.14" {
		t.Errorf("resolved loader version = %q, want 0.16.14 (the newest stable)", resolved)
	}
}

func TestInstallPaper(t *testing.T) {
	f := newFake(t)
	// Paper's digest is SHA-256 (Mojang's is SHA-1), and the served bytes must match it, so the
	// build list advertises the real digest of the jar the fake serves. 1.20.4 is used because
	// Install resolves the Java major from Mojang's manifest first, and that is a version the
	// fake manifest carries — with javaVersion 17, which must end up in argv[0].
	sum := sha256.Sum256(f.jar)
	f.srv.Config.Handler.(*http.ServeMux).HandleFunc("/paper/versions/1.20.4/builds",
		func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"builds":[
			  {"build":8,"channel":"default","downloads":{"application":{"name":"paper-1.20.4-8.jar","sha256":"%[1]s"}}},
			  {"build":9,"channel":"default","downloads":{"application":{"name":"paper-1.20.4-9.jar","sha256":"%[1]s"}}}
			]}`, hex.EncodeToString(sum[:]))
		})

	dir := t.TempDir()
	argv, resolved, err := f.c.Install(context.Background(), dir, "paper", "1.20.4", "", 1024)
	if err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(dir, "server.jar")
	want := []string{"/jvm/17/java", "-Xms1024M", "-Xmx1024M", "-jar", jar, "nogui"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if _, err := os.Stat(jar); err != nil {
		t.Errorf("paper jar not installed: %v", err)
	}
	// Paper is why the installer, not the catalogue, must answer this: it takes the newest build on
	// the "default" channel, which the public build list does not simply end with.
	if resolved != "9" {
		t.Errorf("resolved build = %q, want 9 (newest default-channel build)", resolved)
	}
	// An explicit build must be honoured.
	if _, _, err := f.c.Install(context.Background(), t.TempDir(), "paper", "1.20.4", "8", 1024); err != nil {
		t.Errorf("explicit build 8: %v", err)
	}
	// A build the version does not have must fail, not silently install the newest.
	if _, _, err := f.c.Install(context.Background(), t.TempDir(), "paper", "1.20.4", "999", 1024); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unknown build: err = %v, want ErrUnknownVersion", err)
	}
}

// neoFake extends the fake with NeoForge's maven artifacts: the installer jar, its .sha1 sidecar,
// and a stub exec that materialises the layout the real installer would produce.
func neoFake(t *testing.T, layout func(dir string)) *fake {
	t.Helper()
	f := newFake(t)
	sum := sha1.Sum(f.jar)
	mux := f.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/neo/{v}/{artifact}", func(w http.ResponseWriter, r *http.Request) {
		f.hits[r.URL.Path]++
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			// Maven's sidecar: the digest, sometimes followed by the filename.
			fmt.Fprintf(w, "%s  neoforge-installer.jar\n", hex.EncodeToString(sum[:]))
			return
		}
		w.Write(f.jar)
	})
	f.c.runInstaller = func(_ context.Context, java, jar, dir string) error {
		if _, err := os.Stat(jar); err != nil {
			return fmt.Errorf("installer jar not on disk: %w", err)
		}
		layout(dir)
		return nil
	}
	return f
}

// The modern layout: an @argfile pair, which is what the unit should exec directly.
func neoModernLayout(t *testing.T, version string) func(string) {
	return func(dir string) {
		libs := filepath.Join(dir, "libraries", "net", "neoforged", "neoforge", version)
		if err := os.MkdirAll(libs, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libs, "unix_args.txt"), []byte("-p libraries/...\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// The installer ships a user_jvm_args.txt of comments; the heap must be merged into it.
		if err := os.WriteFile(filepath.Join(dir, "user_jvm_args.txt"), []byte("# -Xmx4G\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallNeoForge(t *testing.T) {
	const version = "21.1.235"
	f := neoFake(t, neoModernLayout(t, version))
	dir := t.TempDir()

	argv, resolved, err := f.c.Install(context.Background(), dir, "neoforge", "1.21.1", version, 6144)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != version {
		t.Errorf("resolved version = %q, want the requested %q", resolved, version)
	}

	// java is exec'd directly against the argfiles rather than through run.sh, so systemd owns the
	// JVM and SIGTERM reaches the server instead of a shell wrapper.
	unixArgs := filepath.Join(dir, "libraries", "net", "neoforged", "neoforge", version, "unix_args.txt")
	want := []string{
		"/jvm/21/java",
		"@" + filepath.Join(dir, "user_jvm_args.txt"),
		"@" + unixArgs,
		"nogui",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv =\n %v\nwant\n %v", argv, want)
	}

	// The heap MUST be in user_jvm_args.txt and MUST NOT be on the command line: NeoForge's
	// launch line ignores JVM flags passed after the argfiles.
	for _, a := range argv {
		if strings.HasPrefix(a, "-Xm") {
			t.Errorf("heap flag %q on the neoforge command line; it belongs in user_jvm_args.txt", a)
		}
	}
	got := readLines(t, filepath.Join(dir, "user_jvm_args.txt"))
	if want := []string{"# -Xmx4G", "-Xms6144M", "-Xmx6144M"}; !reflect.DeepEqual(got, want) {
		t.Errorf("user_jvm_args.txt = %q, want %q", got, want)
	}

	// The installer jar is a build-time artifact; leaving it in the server dir would ship it.
	if _, err := os.Stat(filepath.Join(dir, ".neoforge-installer.jar")); !os.IsNotExist(err) {
		t.Error("installer jar left behind in the server dir")
	}
}

func TestInstallNeoForgeRunShFallback(t *testing.T) {
	// An older layout: run.sh but no unix_args.txt.
	f := neoFake(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	dir := t.TempDir()
	argv, resolved, err := f.c.Install(context.Background(), dir, "neoforge", "1.21.1", "21.1.235", 2048)
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run.sh")
	if want := []string{run, "nogui"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	// The fallback layout must still report its version — an exported pack needs it either way.
	if resolved != "21.1.235" {
		t.Errorf("resolved version = %q, want 21.1.235", resolved)
	}
	// systemd cannot exec a file that is not executable, and the installer does not always chmod it.
	fi, err := os.Stat(run)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh mode %v is not executable", fi.Mode().Perm())
	}
	// Even on this path the heap goes in the file run.sh reads.
	if got, want := readLines(t, filepath.Join(dir, "user_jvm_args.txt")), []string{"-Xms2048M", "-Xmx2048M"}; !reflect.DeepEqual(got, want) {
		t.Errorf("user_jvm_args.txt = %q, want %q", got, want)
	}
}

func TestInstallNeoForgeNoLaunchable(t *testing.T) {
	// The installer "succeeded" but produced neither launch path: fail loudly rather than hand
	// back an argv that cannot start.
	f := neoFake(t, func(string) {})
	_, _, err := f.c.Install(context.Background(), t.TempDir(), "neoforge", "1.21.1", "21.1.235", 1024)
	if err == nil {
		t.Fatal("want an error when the installer produces no launchable layout")
	}
	if !strings.Contains(err.Error(), "run.sh") {
		t.Errorf("error should name what was missing, got %v", err)
	}
}

func TestInstallNeoForgeBadSidecar(t *testing.T) {
	f := newFake(t)
	// Maven's .sha1 sidecar disagrees with the bytes: the installer is never executed.
	mux := f.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/neo/{v}/{artifact}", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			fmt.Fprint(w, strings.Repeat("0", 40))
			return
		}
		w.Write(f.jar)
	})
	ran := false
	f.c.runInstaller = func(context.Context, string, string, string) error { ran = true; return nil }

	dir := t.TempDir()
	_, _, err := f.c.Install(context.Background(), dir, "neoforge", "1.21.1", "21.1.235", 1024)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}
	if ran {
		t.Error("the installer was executed on a jar that failed verification")
	}
	if _, err := os.Stat(filepath.Join(dir, ".neoforge-installer.jar")); !os.IsNotExist(err) {
		t.Error("a jar that failed verification was left on disk")
	}
}

// A version omitted by the caller must resolve to the newest stable build for that Minecraft
// version — 21.1.235 in the maven fixture, not the 21.2.0-beta above it.
func TestInstallNeoForgePicksNewestStable(t *testing.T) {
	f := neoFake(t, neoModernLayout(t, "21.1.235"))
	dir := t.TempDir()
	argv, resolved, err := f.c.Install(context.Background(), dir, "neoforge", "1.21.1", "", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "neoforge/21.1.235/unix_args.txt") {
		t.Errorf("argv = %v, want the 21.1.235 argfile", argv)
	}
	if resolved != "21.1.235" {
		t.Errorf("resolved version = %q, want 21.1.235 (newest stable, not the beta above it)", resolved)
	}
}

func TestInstallNeoForgeVersionMismatch(t *testing.T) {
	f := newFake(t)
	// 21.1.235 targets 1.21.1, not 1.20.4: caught before anything is downloaded.
	_, _, err := f.c.Install(context.Background(), t.TempDir(), "neoforge", "1.20.4", "21.1.235", 1024)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if f.hits["/neo/21.1.235/neoforge-21.1.235-installer.jar"] != 0 {
		t.Error("mismatched neoforge version must be rejected before the installer is downloaded")
	}
}

func TestInstallRejectsBadArgs(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, _, err := f.c.Install(ctx, dir, "forge", "1.21.1", "", 1024); !errors.Is(err, ErrUnsupportedLoader) {
		t.Errorf("bad loader: err = %v, want ErrUnsupportedLoader", err)
	}
	if _, _, err := f.c.Install(ctx, "relative/dir", "vanilla", "1.21.1", "", 1024); !errors.Is(err, ErrInvalid) {
		t.Errorf("relative dir: err = %v, want ErrInvalid", err)
	}
	// The dir is created by the privileged wrapper with the owner's uid; Install must not make one.
	missing := filepath.Join(dir, "nope")
	if _, _, err := f.c.Install(ctx, missing, "vanilla", "1.21.1", "", 1024); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing dir: err = %v, want ErrInvalid", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("Install created the server dir; that tree must be owned by the server's owner")
	}
	if _, _, err := f.c.Install(ctx, dir, "vanilla", "1.21.1", "", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("zero heap: err = %v, want ErrInvalid", err)
	}
	if _, _, err := f.c.Install(ctx, dir, "vanilla", "", "", 1024); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty mc version: err = %v, want ErrInvalid", err)
	}
}

// ── neoforge jvm args ─────────────────────────────────────────────────────────────────

func TestWriteJVMArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user_jvm_args.txt")

	// The installer's own file: comments plus a commented-out heap suggestion.
	seed := "# Xmx and Xms set the maximum and minimum RAM usage\n# -Xmx4G\n-XX:+UseG1GC\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeJVMArgs(path, 3072); err != nil {
		t.Fatal(err)
	}
	got := readLines(t, path)
	want := []string{
		"# Xmx and Xms set the maximum and minimum RAM usage",
		"# -Xmx4G", // still a comment, so it is preserved untouched
		"-XX:+UseG1GC",
		"-Xms3072M",
		"-Xmx3072M",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after first write\n got %q\nwant %q", got, want)
	}

	// Re-installing with a new heap must replace the flags, not stack a second -Xmx line.
	if err := writeJVMArgs(path, 8192); err != nil {
		t.Fatal(err)
	}
	got = readLines(t, path)
	want = []string{
		"# Xmx and Xms set the maximum and minimum RAM usage",
		"# -Xmx4G",
		"-XX:+UseG1GC",
		"-Xms8192M",
		"-Xmx8192M",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after second write\n got %q\nwant %q", got, want)
	}

	// No file yet (an installer layout that does not ship one): create it.
	fresh := filepath.Join(dir, "fresh.txt")
	if err := writeJVMArgs(fresh, 1024); err != nil {
		t.Fatal(err)
	}
	if got, want := readLines(t, fresh), []string{"-Xms1024M", "-Xmx1024M"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fresh file = %q, want %q", got, want)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// ── download ──────────────────────────────────────────────────────────────────────────

func TestDownloadVerification(t *testing.T) {
	body := []byte("some jar bytes")
	s1 := sha1.Sum(body)
	s256 := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	c := New(srv.Client())
	ctx := context.Background()
	dir := t.TempDir()

	tests := []struct {
		name, algo, want string
		ok               bool
	}{
		{"sha1 match", "sha1", hex.EncodeToString(s1[:]), true},
		{"sha1 uppercase", "sha1", strings.ToUpper(hex.EncodeToString(s1[:])), true},
		{"sha256 match", "sha256", hex.EncodeToString(s256[:]), true},
		{"no digest published", "", "", true},
		{"sha1 mismatch", "sha1", strings.Repeat("0", 40), false},
		// Verifying Mojang's SHA-1 as if it were SHA-256 would fail every install.
		{"wrong algorithm", "sha256", hex.EncodeToString(s1[:]), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_"))
			err := c.download(ctx, srv.URL+"/x.jar", dest, tt.algo, tt.want)
			if tt.ok {
				if err != nil {
					t.Fatalf("download = %v, want success", err)
				}
				b, err := os.ReadFile(dest)
				if err != nil || string(b) != string(body) {
					t.Fatalf("file = %q, %v", b, err)
				}
				return
			}
			if !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("err = %v, want ErrHashMismatch", err)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Error("a file that failed verification must not be left in place")
			}
		})
	}

	if err := c.download(ctx, srv.URL+"/gone", filepath.Join(dir, "gone.jar"), "", ""); err == nil {
		t.Error("a 404 must be an error, not an empty jar")
	}
}
