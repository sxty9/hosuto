// Package detect reads a server directory somebody else produced and works out what it actually is.
//
// It is the difference between a migration that moves FILES and one that moves a SERVER. Dropping a
// foreign tree into place fills the Spieledateien tab and nothing else: the Modding tab would be
// empty while forty jars sit in mods/, the Mitglieder tab would show nobody while whitelist.json is
// full, and hosuto would install the wrong Minecraft over the top of a world that cannot open it.
// So every tab's truth is read out of the tree here, once, and handed to the caller to commit.
//
// Everything is EVIDENCE-BASED and layered. A foreign server has been hand-edited for years — jars
// renamed, launcher scripts rewritten, libraries/ left stale by an in-place upgrade — so no single
// file is trusted. Each fact is looked for in several places, strongest source first, and where the
// sources disagree the conflict is recorded in Notes rather than silently resolved. What cannot be
// established is left empty for the operator to fill in; this package never guesses a Minecraft
// version, because installing the wrong one over a world is the one mistake a migration must not
// make on its own.
package detect

import (
	"archive/zip"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"hosuto/internal/mcfiles"
)

// Jar is one mod or plugin found in the tree, hashed so it can be identified against Modrinth.
//
// Name comes from the jar's OWN metadata (fabric.mod.json, neoforge.mods.toml, plugin.yml) rather
// than its filename. A file called "sodium-fabric-0.5.8.jar" is readable; one called "mod (1).jar"
// is not, and a migrated server is full of the second kind.
type Jar struct {
	Path     string `json:"path"` // relative to the server dir, e.g. "mods/sodium.jar"
	Filename string `json:"filename"`
	Name     string `json:"name"`
	SHA1     string `json:"sha1"`
	SHA512   string `json:"sha512"`
	Size     int64  `json:"size"`
	Loader   string `json:"loader,omitempty"` // what the jar's own metadata says it is built for
}

// Result is everything the tree told us. Empty fields mean "not established", never "absent".
type Result struct {
	Loader        string          `json:"loader,omitempty"`
	LoaderVersion string          `json:"loaderVersion,omitempty"`
	MCVersion     string          `json:"mcVersion,omitempty"`
	HeapMB        int             `json:"heapMB,omitempty"`
	JoinPolicy    string          `json:"joinPolicy,omitempty"`
	LevelName     string          `json:"levelName,omitempty"`
	Props         mcfiles.Props   `json:"-"`
	Whitelist     []mcfiles.Entry `json:"whitelist,omitempty"`
	Ops           []mcfiles.Op    `json:"ops,omitempty"`
	Mods          []Jar           `json:"mods,omitempty"`
	// Notes are the operator-facing account of what was found, assumed and left undecided. A
	// migration reports them; they are not diagnostics for a log nobody reads.
	Notes []string `json:"notes,omitempty"`
}

func (r *Result) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// Inspect reads dir and reports what kind of server it holds.
//
// A missing or unreadable file is never fatal: the whole point is to make sense of an incomplete
// tree. Only a dir that cannot be opened at all is an error.
func Inspect(dir string) (Result, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		if err == nil {
			err = fmt.Errorf("detect: %s is not a directory", dir)
		}
		return Result{}, err
	}
	var r Result

	// server.properties first: it names the world directory, which is where the most reliable
	// Minecraft version lives.
	props, err := mcfiles.ReadProps(filepath.Join(dir, "server.properties"))
	if err == nil {
		r.Props = props
	} else {
		r.Props = mcfiles.Props{}
	}
	r.LevelName = strings.TrimSpace(r.Props["level-name"])
	if r.LevelName == "" {
		r.LevelName = "world"
	}
	// An imported server keeps the join policy it had. Defaulting to whitelist would lock out
	// everyone on a server that was deliberately open; defaulting to open would expose one that was
	// not. Only an explicit "false" means open.
	if v, ok := r.Props["white-list"]; ok {
		if strings.EqualFold(strings.TrimSpace(v), "false") {
			r.JoinPolicy = "open"
		} else {
			r.JoinPolicy = "whitelist"
		}
	}

	if wl, err := mcfiles.ReadWhitelist(filepath.Join(dir, "whitelist.json")); err == nil {
		r.Whitelist = wl
	}
	if ops, err := mcfiles.ReadOps(filepath.Join(dir, "ops.json")); err == nil {
		r.Ops = ops
	}

	r.Mods = scanJars(dir)
	detectLoader(dir, &r)
	detectMCVersion(dir, &r)
	r.HeapMB = detectHeap(dir)

	return r, nil
}

// ── loader ────────────────────────────────────────────────────────────────────────────

// detectLoader establishes the loader and its version, strongest evidence first.
//
// The order matters. libraries/ is written by the loader's own installer and names an exact version,
// so it beats a jar filename anybody could have typed. The mod jars are consulted last but are not
// merely a tiebreak: they are the one source that cannot be stale, because the server has been
// running them.
func detectLoader(dir string, r *Result) {
	if v, ok := libVersion(dir, "net/neoforged/neoforge"); ok {
		r.Loader, r.LoaderVersion = "neoforge", v
		return
	}
	// version.txt is written by the NeoForge/Forge installer next to the server and says
	// "<minecraft> - <loader>". It matters most when libraries/ is NOT here — a migration does not
	// copy libraries/ (hundreds of megabytes that get reinstalled anyway), and without this the only
	// record of WHICH loader build was running would be gone. Installing a different build under a
	// modpack breaks it, so this file is the difference between carrying a server over and changing it.
	if mc, lv, ok := installerVersionTxt(dir); ok {
		r.Loader, r.LoaderVersion = "neoforge", lv
		if r.MCVersion == "" && plausibleMC(mc) {
			r.MCVersion = mc
		}
		return
	}
	if v, ok := libVersion(dir, "net/fabricmc/fabric-loader"); ok {
		r.Loader, r.LoaderVersion = "fabric", v
		return
	}
	// Forge proper is a different project from NeoForge and hosuto does not install it. Say so
	// plainly here — the alternative is a migration that quietly sets up NeoForge and hands back a
	// server whose entire mod set refuses to load.
	if _, ok := libVersion(dir, "net/minecraftforge/forge"); ok {
		r.note("This server runs Forge, which hosuto cannot install — pick NeoForge or Fabric and expect to replace the mods.")
		return
	}

	// Paper and its forks. version_history.json is written by the server itself on every boot, so it
	// is the most current thing in the tree.
	if v, mc, ok := paperHistory(dir); ok {
		r.Loader, r.LoaderVersion = "paper", v
		if mc != "" && r.MCVersion == "" {
			r.MCVersion = mc
		}
		return
	}
	if fabricLauncher(dir) {
		r.Loader = "fabric"
		r.note("Recognised Fabric from its launcher files, but not which Fabric build — the newest one will be installed.")
		return
	}
	if exists(filepath.Join(dir, "plugins")) || exists(filepath.Join(dir, "bukkit.yml")) ||
		exists(filepath.Join(dir, "spigot.yml")) || exists(filepath.Join(dir, "config", "paper-global.yml")) {
		r.Loader = "paper"
		return
	}
	if l, ok := jarConsensus(r.Mods); ok {
		r.Loader = l
		r.note("Recognised %s from the installed mods.", loaderLabel(l))
		return
	}
	if serverJar(dir) != "" {
		r.Loader = "vanilla"
		return
	}
	r.note("Could not tell which server software this is — choose it yourself.")
}

// LibEvidence accumulates what a libraries/ TREE says, from paths alone.
//
// It exists because a migration does not copy libraries/ — it is hundreds of megabytes that the
// loader's own installer rebuilds. But those paths are also the only record of WHICH loader build was
// running, and installing a different one under a modpack breaks it. So the transfer reads the
// evidence out of the remote listing and drops the bytes, instead of dropping both.
//
// The group → loader mapping lives here rather than at the call site, so it cannot drift from
// libVersion, which reads the same layout off a local tree.
type LibEvidence struct {
	Loader        string
	LoaderVersion string
	MCVersion     string
}

// Observe folds one relative path (slash-separated, e.g.
// "libraries/net/neoforged/neoforge/21.1.236/neoforge-21.1.236-universal.jar") into the evidence.
// Paths that say nothing are ignored, and the highest build wins when several are present — an
// in-place upgrade leaves the old one behind, and the highest is what actually ran.
func (e *LibEvidence) Observe(rel string) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(rel, "/"), "libraries/")
	if !ok {
		return
	}
	take := func(group, loader string) bool {
		tail, ok := strings.CutPrefix(rest, group+"/")
		if !ok {
			return false
		}
		v, _, _ := strings.Cut(tail, "/")
		if v == "" {
			return false
		}
		switch loader {
		case "": // the vanilla server jar directory names the Minecraft version
			if mc, _, _ := strings.Cut(v, "-"); plausibleMC(mc) && (e.MCVersion == "" || lessVersion(e.MCVersion, mc)) {
				e.MCVersion = mc
			}
		case "mc": // fabric's intermediary mappings are named for the Minecraft they map
			if plausibleMC(v) && (e.MCVersion == "" || lessVersion(e.MCVersion, v)) {
				e.MCVersion = v
			}
		default:
			if e.Loader != loader || lessVersion(e.LoaderVersion, v) {
				e.Loader, e.LoaderVersion = loader, v
			}
		}
		return true
	}
	switch {
	case take("net/neoforged/neoforge", "neoforge"):
	case take("net/fabricmc/fabric-loader", "fabric"):
	case take("net/fabricmc/intermediary", "mc"):
	case take("net/minecraft/server", ""):
	}
}

// libVersion reads the single version directory under libraries/<group>, e.g.
// libraries/net/neoforged/neoforge/21.1.72. More than one means an in-place upgrade left the old one
// behind, so the highest is taken and the ambiguity is not worth a note — the highest is what runs.
func libVersion(dir, group string) (string, bool) {
	base := filepath.Join(dir, "libraries", filepath.FromSlash(group))
	ents, err := os.ReadDir(base)
	if err != nil {
		return "", false
	}
	var versions []string
	for _, e := range ents {
		if e.IsDir() && e.Name() != "" && !strings.HasPrefix(e.Name(), ".") {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", false
	}
	sort.Slice(versions, func(i, j int) bool { return lessVersion(versions[i], versions[j]) })
	return versions[len(versions)-1], true
}

var paperVerRe = regexp.MustCompile(`\(MC:\s*([0-9][0-9A-Za-z.\-]*)\s*\)`)

// paperHistory parses version_history.json, which Paper rewrites on every start. currentVersion has
// varied across Paper generations ("git-Paper-196 (MC: 1.20.4)", "1.20.6-14-master@abc (MC: 1.20.6)"),
// so the two facts are pulled out by pattern rather than by assuming one layout.
func paperHistory(dir string) (build, mc string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "version_history.json"))
	if err != nil {
		return "", "", false
	}
	var doc struct {
		CurrentVersion string `json:"currentVersion"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.CurrentVersion == "" {
		return "", "", false
	}
	if m := paperVerRe.FindStringSubmatch(doc.CurrentVersion); m != nil {
		mc = m[1]
	}
	return paperBuild(doc.CurrentVersion), mc, true
}

// paperBuild pulls the build number out of Paper's currentVersion string, which has had two shapes:
//
//	git-Paper-196 (MC: 1.20.4)                 — the old one, build last
//	1.20.6-14-master@f8b7be2 (MC: 1.20.6)      — the current one, <mc>-<build>-<branch>@<commit>
//
// Taking the LAST purely-numeric dash-separated field handles both, and it handles them for the same
// reason rather than by recognising two patterns: the Minecraft version has dots and the branch has
// letters, so the build is the only bare number in either.
func paperBuild(current string) string {
	head, _, _ := strings.Cut(current, "(MC:")
	build := ""
	for _, f := range strings.Split(strings.TrimSpace(head), "-") {
		if f == "" {
			continue
		}
		if _, err := strconv.Atoi(f); err == nil {
			build = f
		}
	}
	return build
}

// neoVerTxtRe splits the installer's version.txt into its two halves. NeoForge writes
// "1.21.1 - 21.1.236"; Forge writes "1.20.1-47.2.0" into a file of the same name, so matching alone
// proves nothing about WHICH project wrote it.
var neoVerTxtRe = regexp.MustCompile(`^\s*(1\.\d{1,2}(?:\.\d{1,2})?)\s*-\s*(\d{1,3}\.\d+\.\d+[0-9A-Za-z.\-]*)\s*$`)

// installerVersionTxt reads version.txt as NeoForge's, and ONLY as NeoForge's.
//
// The two projects are told apart by consistency rather than by punctuation: NeoForge numbers itself
// <minor>.<patch>.<build> for Minecraft 1.<minor>.<patch>, so a genuine NeoForge line agrees with
// itself — "1.21.1 - 21.1.236" folds back to 1.21.1. Forge's "1.20.1-47.2.0" folds to 1.47.2 and is
// refused. Whitespace would have been the easy tell and the wrong one; a file hand-edited or written
// by a launcher that trims differently would break it, while the arithmetic holds regardless.
func installerVersionTxt(dir string) (mc, loaderVersion string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		return "", "", false
	}
	m := neoVerTxtRe.FindStringSubmatch(strings.TrimSpace(string(b)))
	if m == nil {
		return "", "", false
	}
	if mcFromNeoForge(m[2]) != m[1] {
		return "", "", false
	}
	return m[1], m[2], true
}

func fabricLauncher(dir string) bool {
	return exists(filepath.Join(dir, "fabric-server-launcher.properties")) ||
		exists(filepath.Join(dir, "fabric-server-launch.jar")) ||
		exists(filepath.Join(dir, ".fabric"))
}

// jarConsensus reports the loader the installed mods agree on. A tree can hold leftovers from a
// previous loader, so a bare majority is not enough — it takes a clear one, else the mods say nothing.
func jarConsensus(jars []Jar) (string, bool) {
	counts := map[string]int{}
	total := 0
	for _, j := range jars {
		if j.Loader != "" {
			counts[j.Loader]++
			total++
		}
	}
	if total == 0 {
		return "", false
	}
	best, bestN := "", 0
	for l, n := range counts {
		if n > bestN {
			best, bestN = l, n
		}
	}
	if bestN*2 <= total {
		return "", false
	}
	return best, true
}

// ── minecraft version ─────────────────────────────────────────────────────────────────

var mcInJarRe = regexp.MustCompile(`(?:^|[-_.])(1\.\d{1,2}(?:\.\d{1,2})?)(?:[-_.]|$)`)

// detectMCVersion resolves the Minecraft version, strongest source first.
//
// level.dat wins over everything, including the jars: it is what the world was last SAVED with, and
// the world is the thing that breaks if hosuto installs the wrong version. A jar can be a leftover
// or a half-finished upgrade; a world cannot lie about what wrote it.
func detectMCVersion(dir string, r *Result) {
	if r.MCVersion != "" {
		return // paperHistory already established it
	}
	lvl := filepath.Join(dir, r.LevelName, "level.dat")
	if v, _, err := levelVersion(lvl); err == nil && plausibleMC(v) {
		r.MCVersion = v
		return
	}
	switch r.Loader {
	case "fabric":
		// The intermediary mappings are named for the exact Minecraft they map.
		if v, ok := libVersion(dir, "net/fabricmc/intermediary"); ok && plausibleMC(v) {
			r.MCVersion = v
			return
		}
	case "neoforge":
		if v := mcFromNeoForge(r.LoaderVersion); v != "" {
			r.MCVersion = v
			return
		}
	}
	// libraries/net/minecraft/server/<mc>-<timestamp>: every loader's installer lays this down.
	if v, ok := libVersion(dir, "net/minecraft/server"); ok {
		if mc, _, _ := strings.Cut(v, "-"); plausibleMC(mc) {
			r.MCVersion = mc
			return
		}
	}
	// The server jar carries a version.json naming itself. This is authoritative for vanilla and for
	// the bundled jar Paper patches.
	if jar := serverJar(dir); jar != "" {
		if v := jarVersionJSON(jar); plausibleMC(v) {
			r.MCVersion = v
			return
		}
		if m := mcInJarRe.FindStringSubmatch(filepath.Base(jar)); m != nil {
			r.MCVersion = m[1]
			r.note("Read the Minecraft version from the file name %q — check it before starting.", filepath.Base(jar))
			return
		}
	}
	r.note("Could not tell which Minecraft version this server ran — choose it yourself.")
}

// mcFromNeoForge maps a NeoForge version onto its Minecraft version. NeoForge numbers itself
// <minor>.<patch>.<build> for Minecraft 1.<minor>.<patch>, with patch 0 meaning the .0-less release
// (21.0.x is Minecraft 1.21, not 1.21.0).
func mcFromNeoForge(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return ""
	}
	minor, err1 := strconv.Atoi(parts[0])
	patch, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || minor < 20 {
		return "" // NeoForge started at Minecraft 1.20.1; anything lower is not this scheme
	}
	if patch == 0 {
		return fmt.Sprintf("1.%d", minor)
	}
	return fmt.Sprintf("1.%d.%d", minor, patch)
}

// jarVersionJSON reads the version.json a Minecraft server jar carries, which names itself.
func jarVersionJSON(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "version.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ""
		}
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return ""
		}
		var doc struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &doc) != nil {
			return ""
		}
		if doc.ID != "" {
			return doc.ID
		}
		return doc.Name
	}
	return ""
}

// serverJar picks the jar the server most likely boots from. Names vary wildly across hosts, so the
// well-known ones are tried first and only then the largest jar in the root — the server jar is
// always far bigger than a helper.
func serverJar(dir string) string {
	for _, n := range []string{"server.jar", "paperclip.jar", "minecraft_server.jar"} {
		p := filepath.Join(dir, n)
		if exists(p) {
			return p
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestSize := "", int64(0)
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			best, bestSize = filepath.Join(dir, e.Name()), info.Size()
		}
	}
	// A launcher stub is a few hundred kilobytes; a real server jar is tens of megabytes. Below this
	// we are looking at a wrapper, and reading a Minecraft version out of it would be wrong.
	if bestSize < 1<<20 {
		return ""
	}
	return best
}

// plausibleMC keeps a stray string ("unknown", a snapshot id, a mod version) from being adopted as a
// Minecraft version and installed.
var mcRe = regexp.MustCompile(`^1\.\d{1,2}(\.\d{1,2})?$`)

func plausibleMC(v string) bool { return mcRe.MatchString(strings.TrimSpace(v)) }

// ── heap ──────────────────────────────────────────────────────────────────────────────

var xmxRe = regexp.MustCompile(`-Xmx\s*(\d+)\s*([gGmMkK]?)`)

// detectHeap reads the heap the old host gave this server, so a migrated modpack does not come up on
// hosuto's 2 GB default and die on its first chunk load. The caller still clamps it to the instance's
// maximum — this only carries the intent across.
func detectHeap(dir string) int {
	for _, n := range []string{
		"user_jvm_args.txt", "start.sh", "run.sh", "startserver.sh", "ServerStart.sh",
		"start.bat", "run.bat", "settings.sh", "variables.txt",
	} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		// A file may carry several: the last -Xmx on a java command line is the one that wins.
		ms := xmxRe.FindAllStringSubmatch(string(b), -1)
		for i := len(ms) - 1; i >= 0; i-- {
			if mb := toMB(ms[i][1], ms[i][2]); mb > 0 {
				return mb
			}
		}
	}
	return 0
}

func toMB(num, unit string) int {
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0
	}
	switch strings.ToLower(unit) {
	case "g":
		return n * 1024
	case "k":
		return n / 1024
	case "m", "":
		return n
	}
	return 0
}

// ── jars ──────────────────────────────────────────────────────────────────────────────

// modDirs are where a server keeps code. plugins/ is Paper's, mods/ is Fabric's and NeoForge's; a
// migrated tree often has both because it was converted at some point, and the Modding tab should
// show whatever is actually there.
var modDirs = []string{"mods", "plugins"}

// maxJar bounds a file we will hash and open as a zip. Mods run into the tens of megabytes; anything
// far past that in mods/ is not a mod, and hashing it would stall the migration.
const maxJar = 512 << 20

func scanJars(dir string) []Jar {
	var out []Jar
	for _, sub := range modDirs {
		base := filepath.Join(dir, sub)
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
				continue // .jar.disabled and friends are deliberately off; leave them off
			}
			info, err := e.Info()
			if err != nil || info.Size() > maxJar || !info.Mode().IsRegular() {
				continue
			}
			p := filepath.Join(base, e.Name())
			s1, s5, err := hashFile(p)
			if err != nil {
				continue
			}
			j := Jar{
				Path: sub + "/" + e.Name(), Filename: e.Name(),
				SHA1: s1, SHA512: s5, Size: info.Size(),
			}
			j.Name, j.Loader = jarIdentity(p)
			if j.Name == "" {
				j.Name = strings.TrimSuffix(e.Name(), ".jar")
			}
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func hashFile(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h1, h5 := sha1.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(h1, h5), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h5.Sum(nil)), nil
}

var (
	tomlNameRe = regexp.MustCompile(`(?m)^\s*displayName\s*=\s*["']([^"']+)["']`)
	ymlNameRe  = regexp.MustCompile(`(?m)^\s*name\s*:\s*["']?([^"'\r\n]+)`)
)

// jarIdentity reads a jar's own descriptor for its display name and the loader it targets.
//
// This is what makes a migrated Modding tab readable. Modrinth identifies most jars by hash, but the
// ones it does not know — a private build, a mod from CurseForge, a patched jar — would otherwise
// appear as whatever filename the previous host happened to leave behind.
func jarIdentity(path string) (name, loader string) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", ""
	}
	defer zr.Close()

	read := func(f *zip.File) []byte {
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		if err != nil {
			return nil
		}
		return b
	}
	for _, f := range zr.File {
		switch f.Name {
		case "fabric.mod.json":
			var doc struct {
				Name string `json:"name"`
				ID   string `json:"id"`
			}
			// Fabric descriptors are frequently hand-written and occasionally have trailing commas;
			// a parse failure still tells us the loader, which is the more valuable half.
			if json.Unmarshal(read(f), &doc) == nil {
				if doc.Name != "" {
					return doc.Name, "fabric"
				}
				return doc.ID, "fabric"
			}
			return "", "fabric"
		case "quilt.mod.json":
			return "", "fabric" // Quilt mods load on Fabric; hosuto has no separate Quilt loader
		case "META-INF/neoforge.mods.toml":
			if m := tomlNameRe.FindSubmatch(read(f)); m != nil {
				return string(m[1]), "neoforge"
			}
			return "", "neoforge"
		case "META-INF/mods.toml":
			// Shared by Forge and older NeoForge. The loader is left unsaid rather than guessed:
			// calling a Forge mod NeoForge is exactly how a migration ends up with a dead server.
			if m := tomlNameRe.FindSubmatch(read(f)); m != nil {
				return string(m[1]), ""
			}
			return "", ""
		case "plugin.yml", "paper-plugin.yml":
			if m := ymlNameRe.FindSubmatch(read(f)); m != nil {
				return strings.TrimSpace(string(m[1])), "paper"
			}
			return "", "paper"
		}
	}
	return "", ""
}

// ── helpers ───────────────────────────────────────────────────────────────────────────

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func loaderLabel(l string) string {
	switch l {
	case "fabric":
		return "Fabric"
	case "neoforge":
		return "NeoForge"
	case "paper":
		return "Paper"
	case "vanilla":
		return "Vanilla"
	}
	return l
}

// lessVersion orders dotted version strings numerically where it can, so "21.1.9" sorts below
// "21.1.72" instead of above it the way a plain string compare would.
func lessVersion(a, b string) bool {
	as, bs := strings.FieldsFunc(a, isSep), strings.FieldsFunc(b, isSep)
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aerr := strconv.Atoi(as[i])
		bi, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			if ai != bi {
				return ai < bi
			}
			continue
		}
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

func isSep(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '_' }
