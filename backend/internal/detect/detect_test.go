package detect

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ── fixtures ──────────────────────────────────────────────────────────────────────────

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeJar builds a jar carrying the given members, so a test can give a mod a real descriptor.
func writeJar(t *testing.T, dir, rel string, members map[string]string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// nbt builders — just enough to assemble a real level.dat.
func nbtStr(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s)))
	copy(b[2:], s)
	return b
}

// writeLevelDat lays down a gzipped level.dat whose Data.Version.Name is the given version.
func writeLevelDat(t *testing.T, dir, rel, version string, id int32) {
	t.Helper()
	var b bytes.Buffer
	b.WriteByte(tagCompound)
	b.Write(nbtStr("")) // root name
	b.WriteByte(tagCompound)
	b.Write(nbtStr("Data"))
	b.WriteByte(tagCompound)
	b.Write(nbtStr("Version"))
	b.WriteByte(tagString)
	b.Write(nbtStr("Name"))
	b.Write(nbtStr(version))
	b.WriteByte(tagInt)
	b.Write(nbtStr("Id"))
	_ = binary.Write(&b, binary.BigEndian, id)
	b.WriteByte(tagEnd) // Version
	b.WriteByte(tagEnd) // Data
	b.WriteByte(tagEnd) // root

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(b.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, dir, rel, gz.String())
}

// ── the world is the strongest source ─────────────────────────────────────────────────

// level.dat records what actually wrote the world, so it beats a jar filename — which is routinely a
// leftover from an upgrade that never finished.
func TestLevelDatBeatsJarFilename(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "server.properties", "level-name=world\n")
	writeLevelDat(t, dir, "world/level.dat", "1.20.4", 3700)
	// A stale jar from before an upgrade.
	write(t, dir, "server.jar", padded("old jar"))

	r, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.MCVersion != "1.20.4" {
		t.Fatalf("MCVersion = %q, want 1.20.4 from level.dat", r.MCVersion)
	}
}

// A world under a non-default level-name must still be found: server.properties names it, which is
// why the properties are read before the version.
func TestLevelDatHonoursLevelName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "server.properties", "level-name=my_world\n")
	writeLevelDat(t, dir, "my_world/level.dat", "1.21.1", 3955)

	r, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.MCVersion != "1.21.1" {
		t.Fatalf("MCVersion = %q, want 1.21.1", r.MCVersion)
	}
	if r.LevelName != "my_world" {
		t.Fatalf("LevelName = %q", r.LevelName)
	}
}

func TestLevelDatGarbageIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "world/level.dat", "this is not nbt at all")
	mkdir(t, dir, "libraries/net/fabricmc/fabric-loader/0.16.5")
	mkdir(t, dir, "libraries/net/fabricmc/intermediary/1.21.1")

	r, err := Inspect(dir)
	if err != nil {
		t.Fatalf("a corrupt level.dat must not fail the inspection: %v", err)
	}
	if r.MCVersion != "1.21.1" {
		t.Fatalf("MCVersion = %q, want the fabric intermediary fallback", r.MCVersion)
	}
}

// ── loaders ───────────────────────────────────────────────────────────────────────────

func TestDetectsFabric(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "libraries/net/fabricmc/fabric-loader/0.16.5")
	mkdir(t, dir, "libraries/net/fabricmc/intermediary/1.21.1")

	r, _ := Inspect(dir)
	if r.Loader != "fabric" || r.LoaderVersion != "0.16.5" {
		t.Fatalf("got %s/%s, want fabric/0.16.5", r.Loader, r.LoaderVersion)
	}
	if r.MCVersion != "1.21.1" {
		t.Fatalf("MCVersion = %q, want 1.21.1", r.MCVersion)
	}
}

func TestDetectsNeoForgeAndDerivesMinecraft(t *testing.T) {
	for _, tc := range []struct{ neo, mc string }{
		{"21.1.72", "1.21.1"},
		{"21.0.167", "1.21"},
		{"20.4.237", "1.20.4"},
	} {
		dir := t.TempDir()
		mkdir(t, dir, "libraries/net/neoforged/neoforge/"+tc.neo)

		r, _ := Inspect(dir)
		if r.Loader != "neoforge" || r.LoaderVersion != tc.neo {
			t.Fatalf("got %s/%s, want neoforge/%s", r.Loader, r.LoaderVersion, tc.neo)
		}
		if r.MCVersion != tc.mc {
			t.Fatalf("neoforge %s → MCVersion %q, want %s", tc.neo, r.MCVersion, tc.mc)
		}
	}
}

// An in-place upgrade leaves the old version directory behind. The highest is what runs, and it must
// be compared numerically — "9" is not newer than "72".
func TestLoaderVersionPicksHighestNumerically(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "libraries/net/neoforged/neoforge/21.1.9")
	mkdir(t, dir, "libraries/net/neoforged/neoforge/21.1.72")

	r, _ := Inspect(dir)
	if r.LoaderVersion != "21.1.72" {
		t.Fatalf("LoaderVersion = %q, want 21.1.72", r.LoaderVersion)
	}
}

func TestDetectsPaperFromVersionHistory(t *testing.T) {
	for _, cur := range []string{
		`git-Paper-196 (MC: 1.20.4)`,
		`1.20.4-196-master@abcdef (MC: 1.20.4)`,
	} {
		dir := t.TempDir()
		write(t, dir, "version_history.json", `{"currentVersion":"`+cur+`"}`)

		r, _ := Inspect(dir)
		if r.Loader != "paper" {
			t.Fatalf("%q → loader %q, want paper", cur, r.Loader)
		}
		if r.MCVersion != "1.20.4" {
			t.Fatalf("%q → MCVersion %q, want 1.20.4", cur, r.MCVersion)
		}
		if r.LoaderVersion != "196" {
			t.Fatalf("%q → build %q, want 196", cur, r.LoaderVersion)
		}
	}
}

// Forge is a different project from NeoForge and hosuto cannot install it. Setting up NeoForge
// instead would produce a server whose entire mod set refuses to load, so it must say so and decide
// nothing.
func TestForgeIsReportedNotGuessed(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "libraries/net/minecraftforge/forge/1.20.1-47.2.0")

	r, _ := Inspect(dir)
	if r.Loader != "" {
		t.Fatalf("Loader = %q, want no guess for Forge", r.Loader)
	}
	if len(r.Notes) == 0 {
		t.Fatal("Forge must be reported to the operator")
	}
}

func TestDetectsVanillaFromJarVersionJSON(t *testing.T) {
	dir := t.TempDir()
	// A jar big enough to read as a real server jar, carrying its own version.json.
	writeJar(t, dir, "server.jar", map[string]string{
		"version.json": `{"id":"1.20.6","name":"1.20.6"}`,
		"padding":      padded(""),
	})

	r, _ := Inspect(dir)
	if r.Loader != "vanilla" {
		t.Fatalf("Loader = %q, want vanilla", r.Loader)
	}
	if r.MCVersion != "1.20.6" {
		t.Fatalf("MCVersion = %q, want 1.20.6", r.MCVersion)
	}
}

// An unrecognisable tree must leave the version EMPTY. Guessing here installs the wrong Minecraft
// over somebody's world.
func TestUnknownTreeDecidesNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "readme.txt", "hello")

	r, _ := Inspect(dir)
	if r.MCVersion != "" {
		t.Fatalf("MCVersion = %q, want empty rather than a guess", r.MCVersion)
	}
	if len(r.Notes) == 0 {
		t.Fatal("an undecidable tree must be reported")
	}
}

// ── mods ──────────────────────────────────────────────────────────────────────────────

func TestScansModsAndReadsTheirDescriptors(t *testing.T) {
	dir := t.TempDir()
	writeJar(t, dir, "mods/whatever (1).jar", map[string]string{
		"fabric.mod.json": `{"id":"sodium","name":"Sodium"}`,
	})
	writeJar(t, dir, "mods/b.jar", map[string]string{
		"META-INF/neoforge.mods.toml": "modLoader=\"javafml\"\ndisplayName=\"Jade\"\n",
	})
	writeJar(t, dir, "plugins/c.jar", map[string]string{
		"plugin.yml": "name: EssentialsX\nversion: 2.20\n",
	})
	// A disabled mod is deliberately off and must stay off.
	write(t, dir, "mods/off.jar.disabled", "nope")

	r, _ := Inspect(dir)
	if len(r.Mods) != 3 {
		t.Fatalf("found %d jars, want 3 (the .disabled one must be left alone)", len(r.Mods))
	}
	byPath := map[string]Jar{}
	for _, j := range r.Mods {
		byPath[j.Path] = j
	}
	// A useless filename must still produce a readable name — that is the point of reading the jar.
	if got := byPath["mods/whatever (1).jar"]; got.Name != "Sodium" || got.Loader != "fabric" {
		t.Fatalf("got %+v, want name Sodium / loader fabric", got)
	}
	if got := byPath["mods/b.jar"]; got.Name != "Jade" || got.Loader != "neoforge" {
		t.Fatalf("got %+v, want name Jade / loader neoforge", got)
	}
	if got := byPath["plugins/c.jar"]; got.Name != "EssentialsX" || got.Loader != "paper" {
		t.Fatalf("got %+v, want name EssentialsX / loader paper", got)
	}
	for _, j := range r.Mods {
		if len(j.SHA1) != 40 || len(j.SHA512) != 128 || j.Size == 0 {
			t.Fatalf("%s was not hashed: %+v", j.Path, j)
		}
	}
}

// With no libraries/ and no launcher files, the mods themselves are the evidence — and they cannot
// be stale, because the server has been running them.
func TestLoaderFallsBackToModConsensus(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeJar(t, dir, "mods/"+n+".jar", map[string]string{
			"fabric.mod.json": `{"id":"` + n + `","name":"` + n + `"}`,
		})
	}
	r, _ := Inspect(dir)
	if r.Loader != "fabric" {
		t.Fatalf("Loader = %q, want fabric from the mods", r.Loader)
	}
}

// A Forge mod (META-INF/mods.toml) must not be counted as NeoForge evidence.
func TestForgeModsDoNotVoteNeoForge(t *testing.T) {
	dir := t.TempDir()
	writeJar(t, dir, "mods/a.jar", map[string]string{
		"META-INF/mods.toml": "displayName=\"Some Forge Mod\"\n",
	})
	r, _ := Inspect(dir)
	if r.Loader == "neoforge" {
		t.Fatal("an ambiguous mods.toml was taken as NeoForge")
	}
}

// ── the rest of the tabs ──────────────────────────────────────────────────────────────

// The join policy is carried across as it was. Defaulting either way would silently lock out a whole
// server's players, or expose one that was deliberately closed.
func TestJoinPolicyIsCarriedAcross(t *testing.T) {
	for _, tc := range []struct{ props, want string }{
		{"white-list=false\n", "open"},
		{"white-list=true\n", "whitelist"},
		{"level-name=world\n", ""}, // unstated: decide nothing
	} {
		dir := t.TempDir()
		write(t, dir, "server.properties", tc.props)
		r, _ := Inspect(dir)
		if r.JoinPolicy != tc.want {
			t.Fatalf("%q → JoinPolicy %q, want %q", tc.props, r.JoinPolicy, tc.want)
		}
	}
}

func TestReadsWhitelistAndOps(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "whitelist.json", `[
	  {"uuid":"11111111-2222-3333-4444-555555555555","name":"Ada"},
	  {"uuid":"66666666-7777-8888-9999-000000000000","name":"Bob"}
	]`)
	write(t, dir, "ops.json", `[{"uuid":"11111111-2222-3333-4444-555555555555","name":"Ada","level":4}]`)

	r, _ := Inspect(dir)
	if len(r.Whitelist) != 2 {
		t.Fatalf("whitelist has %d entries, want 2", len(r.Whitelist))
	}
	if len(r.Ops) != 1 || r.Ops[0].Name != "Ada" {
		t.Fatalf("ops = %+v", r.Ops)
	}
}

func TestReadsHeapFromLaunchFiles(t *testing.T) {
	for _, tc := range []struct {
		file, body string
		want       int
	}{
		{"user_jvm_args.txt", "-Xmx6G\n", 6144},
		{"user_jvm_args.txt", "-Xmx8192M\n", 8192},
		{"start.sh", "java -Xms1G -Xmx4096M -jar server.jar nogui\n", 4096},
		{"run.sh", "echo hi\n", 0},
	} {
		dir := t.TempDir()
		write(t, dir, tc.file, tc.body)
		r, _ := Inspect(dir)
		if r.HeapMB != tc.want {
			t.Fatalf("%s %q → HeapMB %d, want %d", tc.file, tc.body, r.HeapMB, tc.want)
		}
	}
}

// padded returns a body over the 1 MiB floor serverJar uses to tell a real server jar from a
// launcher stub.
func padded(prefix string) string {
	b := make([]byte, (1<<20)+len(prefix)+1)
	copy(b, prefix)
	return string(b)
}

// ── carrying the loader BUILD across ───────────────────────────────────────────────────

// A migration does not copy libraries/, so version.txt is the only thing left that names the loader
// build. Getting this wrong is not cosmetic: a modpack is assembled against one NeoForge build, and
// installing a different one makes it die on boot with a registry error naming an innocent mod.
func TestNeoForgeVersionTxtCarriesTheBuild(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "1.21.1 - 21.1.236")
	writeJar(t, dir, "mods/a.jar", map[string]string{
		"META-INF/neoforge.mods.toml": "displayName=\"Some Mod\"\n",
	})

	r, _ := Inspect(dir)
	if r.Loader != "neoforge" || r.LoaderVersion != "21.1.236" {
		t.Fatalf("got %s/%s, want neoforge/21.1.236", r.Loader, r.LoaderVersion)
	}
	if r.MCVersion != "1.21.1" {
		t.Fatalf("MCVersion = %q, want 1.21.1", r.MCVersion)
	}
}

// libraries/ still wins when it is present — it is written by the installer that actually ran.
func TestLibrariesBeatVersionTxt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "1.21.1 - 21.1.236")
	mkdir(t, dir, "libraries/net/neoforged/neoforge/21.1.240")

	r, _ := Inspect(dir)
	if r.LoaderVersion != "21.1.240" {
		t.Fatalf("LoaderVersion = %q, want the installed 21.1.240", r.LoaderVersion)
	}
}

// Forge writes "<mc>-<forge>" into the same file. Reading that as NeoForge would install the wrong
// loader entirely, so the pattern must not match it.
func TestVersionTxtIgnoresForge(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "1.20.1-47.2.0")

	r, _ := Inspect(dir)
	if r.Loader == "neoforge" {
		t.Fatalf("a Forge version.txt was read as NeoForge %q", r.LoaderVersion)
	}
}

// The transfer skips libraries/ but sees its paths. That listing is the only record of the build,
// so the evidence has to be read before the filter drops it.
func TestLibEvidenceFromPaths(t *testing.T) {
	var e LibEvidence
	for _, p := range []string{
		"libraries/net/neoforged/neoforge/21.1.236/neoforge-21.1.236-universal.jar",
		"libraries/net/minecraft/server/1.21.1-20240808.144430/server-1.21.1.jar",
		"mods/a.jar", "world/level.dat", "server.properties",
	} {
		e.Observe(p)
	}
	if e.Loader != "neoforge" || e.LoaderVersion != "21.1.236" {
		t.Fatalf("got %s/%s, want neoforge/21.1.236", e.Loader, e.LoaderVersion)
	}
	if e.MCVersion != "1.21.1" {
		t.Fatalf("MCVersion = %q, want 1.21.1", e.MCVersion)
	}
}

func TestLibEvidenceFabricAndHighestBuild(t *testing.T) {
	var e LibEvidence
	e.Observe("libraries/net/fabricmc/fabric-loader/0.16.9/x.jar")
	e.Observe("libraries/net/fabricmc/fabric-loader/0.16.10/x.jar")
	e.Observe("libraries/net/fabricmc/intermediary/1.21.1/y.jar")
	if e.Loader != "fabric" || e.LoaderVersion != "0.16.10" {
		t.Fatalf("got %s/%s, want fabric/0.16.10 (numeric, not string, ordering)", e.Loader, e.LoaderVersion)
	}
	if e.MCVersion != "1.21.1" {
		t.Fatalf("MCVersion = %q", e.MCVersion)
	}
}
