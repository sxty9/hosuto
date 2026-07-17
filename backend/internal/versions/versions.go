// Package versions is the catalogue behind hosuto's "Version & Modding" tab, and the code that
// actually lays a server down on disk.
//
// It exists because "which Minecraft can I run, on which loader, on which Java?" has four
// different answers from four different upstreams — Mojang's launcher meta, Fabric's meta service,
// NeoForge's maven, PaperMC's API — and none of them agree on a shape. This package is the single
// place that knows those shapes. Everything above it (the API layer, the unit generator) sees only
// []Version, []string and an argv.
//
// Two invariants:
//
//   - Nothing lands in a server directory unverified. Every download whose upstream publishes a
//     digest is checked against it and the file is renamed into place only after it matches. A
//     mismatch is a hard error, never a warning — a silently corrupt server.jar is a support
//     nightmare and a supply-chain hole.
//   - Install returns the argv the systemd unit must exec, because the loaders do not agree on how
//     a server is started. Vanilla, Fabric and Paper are `java -jar <jar> nogui`; NeoForge is a
//     generated @argfile whose heap comes from user_jvm_args.txt, NOT from the command line. The
//     caller must not guess; it runs what Install hands back.
//
// The unit must set WorkingDirectory=<dir> for every loader: NeoForge's argfiles reference
// libraries/ by relative path, and the server writes its world next to itself.
//
// This package does not write eula.txt or server.properties. Accepting Mojang's EULA is a policy
// decision the owner makes, and server.properties belongs to the on-disk tree the privileged
// wrapper owns.
package versions

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"hosuto/internal/store"
)

// Upstream endpoints. Fields on Client rather than constants so tests can point them at an
// httptest.Server; nothing in this package's test suite touches the network.
const (
	defaultManifestURL = "https://launchermeta.mojang.com/mc/game/version_manifest_v2.json"
	defaultFabricBase  = "https://meta.fabricmc.net"
	defaultNeoBase     = "https://maven.neoforged.net/releases/net/neoforged/neoforge"
	// PaperMC v2 is on a deprecation path in favour of the v3 "Fill" API (api.papermc.io/v3).
	// v2 is what is live and stable today; when it goes, only paperBuilds and installPaper change.
	defaultPaperBase = "https://api.papermc.io/v2/projects/paper"
)

const (
	// catalogueTimeout bounds a metadata call. The catalogue sits on the UI's hot path, so it must
	// fail fast rather than hang a tab open.
	catalogueTimeout = 10 * time.Second
	// downloadTimeout bounds a jar fetch. A server jar is tens of megabytes and the NeoForge
	// installer pulls a whole library tree, so downloads get their own client — reusing the
	// catalogue client's short timeout would abort every large download mid-body.
	downloadTimeout = 20 * time.Minute
	// cacheTTL bounds how stale the catalogue may be. Long enough that opening the tab repeatedly
	// costs one request; short enough that a version released today shows up today.
	cacheTTL = 5 * time.Minute
	// maxCatalogueBytes caps a cached metadata body. Mojang's manifest is ~1.5 MB; anything
	// wildly larger means the endpoint is not what we think it is, and we refuse to buffer it.
	maxCatalogueBytes = 32 << 20
	// systemJVMDir is where Debian's openjdk-*-jre-headless packages land.
	systemJVMDir = "/usr/lib/jvm"
)

var (
	// ErrUnsupportedLoader is returned for a loader outside store's vocabulary.
	ErrUnsupportedLoader = errors.New("unsupported loader")
	// ErrUnknownVersion is returned when an upstream does not publish the requested version.
	ErrUnknownVersion = errors.New("unknown version")
	// ErrHashMismatch is returned when a download does not match its published digest. It is
	// always fatal: the file is discarded, never installed.
	ErrHashMismatch = errors.New("download hash mismatch")
	// ErrNoJava is returned when no JRE of the required major version can be found on the host.
	ErrNoJava = errors.New("no suitable java runtime")
	// ErrInvalid is returned for a malformed argument (relative dir, non-positive heap).
	ErrInvalid = errors.New("invalid")
)

// Version is one catalogue entry.
type Version struct {
	ID   string `json:"id"`
	Type string `json:"type"` // release | snapshot | old_beta | old_alpha
}

// Client talks to the four upstreams and installs servers. Safe for concurrent use.
type Client struct {
	hc *http.Client // metadata; short timeout
	dl *http.Client // jars; long timeout, same transport

	manifestURL string
	fabricBase  string
	neoBase     string
	paperBase   string

	// javaBin resolves a JRE by major version. Injectable so tests never depend on the host
	// having a JDK installed; production leaves it as JavaBin.
	javaBin func(major int) (string, error)
	// runInstaller execs NeoForge's installer. Injectable for the same reason: the tests exercise
	// the layout it produces and the argv derived from it, without needing a JVM.
	runInstaller func(ctx context.Context, java, jar, dir string) error

	now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	body []byte
	at   time.Time
}

// New builds a Client. A nil http.Client gets a sane default.
func New(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: catalogueTimeout}
	}
	dl := *hc // share the transport (and its connection pool); override only the deadline
	dl.Timeout = downloadTimeout
	return &Client{
		hc:           hc,
		dl:           &dl,
		manifestURL:  defaultManifestURL,
		fabricBase:   defaultFabricBase,
		neoBase:      defaultNeoBase,
		paperBase:    defaultPaperBase,
		javaBin:      JavaBin,
		runInstaller: runInstaller,
		now:          time.Now,
		cache:        map[string]cacheEntry{},
	}
}

// runInstaller execs NeoForge's installer headlessly. Its output is kept only to be quoted back in
// the error: a failed install must say why, not just "exit status 1".
func runInstaller(ctx context.Context, java, jar, dir string) error {
	cmd := exec.CommandContext(ctx, java, "-jar", jar, "--installServer", dir)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, tail(out, 2000))
	}
	return nil
}

// ── mojang launcher meta ──────────────────────────────────────────────────────────────

type manifest struct {
	Latest struct {
		Release  string `json:"release"`
		Snapshot string `json:"snapshot"`
	} `json:"latest"`
	Versions []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
		SHA1 string `json:"sha1"`
	} `json:"versions"`
}

// versionMeta is the per-version JSON the manifest points at. javaVersion is authoritative: it is
// what the official launcher itself uses to pick a JRE, so it beats any matrix we could hardcode.
type versionMeta struct {
	Downloads struct {
		Server struct {
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"server"`
	} `json:"downloads"`
	JavaVersion struct {
		MajorVersion int `json:"majorVersion"`
	} `json:"javaVersion"`
}

func (c *Client) manifest(ctx context.Context) (*manifest, error) {
	b, err := c.fetch(ctx, c.manifestURL)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("version manifest: %w", err)
	}
	if len(m.Versions) == 0 {
		return nil, errors.New("version manifest: no versions")
	}
	return &m, nil
}

// typeRank orders the catalogue. Mojang publishes newest-first but interleaves snapshots with
// releases; the UI wants releases at the top, so partition by type and keep upstream's order
// (which is chronological) inside each partition.
func typeRank(t string) int {
	switch t {
	case "release":
		return 0
	case "snapshot":
		return 1
	case "old_beta":
		return 2
	default: // old_alpha and anything Mojang invents later
		return 3
	}
}

// MinecraftVersions returns the catalogue: releases first, newest first, then snapshots, then the
// old_beta/old_alpha tail.
func (c *Client) MinecraftVersions(ctx context.Context) ([]Version, error) {
	m, err := c.manifest(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Version, 0, len(m.Versions))
	for _, v := range m.Versions {
		out = append(out, Version{ID: v.ID, Type: v.Type})
	}
	sort.SliceStable(out, func(i, j int) bool { return typeRank(out[i].Type) < typeRank(out[j].Type) })
	return out, nil
}

// Latest returns Mojang's current release and snapshot ids — the defaults the create form offers.
func (c *Client) Latest(ctx context.Context) (release, snapshot string, err error) {
	m, err := c.manifest(ctx)
	if err != nil {
		return "", "", err
	}
	return m.Latest.Release, m.Latest.Snapshot, nil
}

// versionMeta resolves one Minecraft version's metadata document.
func (c *Client) versionMeta(ctx context.Context, mcVersion string) (*versionMeta, error) {
	m, err := c.manifest(ctx)
	if err != nil {
		return nil, err
	}
	url := ""
	for _, v := range m.Versions {
		if v.ID == mcVersion {
			url = v.URL
			break
		}
	}
	if url == "" {
		return nil, fmt.Errorf("%w: minecraft %s", ErrUnknownVersion, mcVersion)
	}
	b, err := c.fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	var vm versionMeta
	if err := json.Unmarshal(b, &vm); err != nil {
		return nil, fmt.Errorf("version meta %s: %w", mcVersion, err)
	}
	return &vm, nil
}

// ── java ──────────────────────────────────────────────────────────────────────────────

// JavaMajorFor reports the Java major version a Minecraft version needs.
//
// It prefers the javaVersion.majorVersion field Mojang publishes per version, and only falls back
// to the matrix when that field is absent (it is, for old versions). A network failure is an
// error, not a fallback: guessing the JRE for a version we could not look up is how you get a
// server that boots and then crashes on a class file version.
func (c *Client) JavaMajorFor(ctx context.Context, mcVersion string) (int, error) {
	vm, err := c.versionMeta(ctx, mcVersion)
	if err != nil {
		return 0, err
	}
	if vm.JavaVersion.MajorVersion > 0 {
		return vm.JavaVersion.MajorVersion, nil
	}
	return javaMajorFallback(mcVersion), nil
}

// parseMC splits a release id into its minor and patch numbers: "1.20.4" → (20, 4), "1.21" →
// (21, 0). Snapshots ("24w14a") and anything else do not parse.
func parseMC(id string) (minor, patch int, ok bool) {
	parts := strings.Split(strings.TrimSpace(id), ".")
	if len(parts) < 2 || parts[0] != "1" {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	if len(parts) >= 3 {
		if patch, err = strconv.Atoi(parts[2]); err != nil {
			return 0, 0, false
		}
	}
	return minor, patch, true
}

// javaMajorFallback is the documented matrix, used only when Mojang omits javaVersion.
//
// An unparseable id (a snapshot, or a shape Mojang has not invented yet) resolves to 21: snapshots
// track the newest release, and 21 is the newest JRE hosuto installs.
func javaMajorFallback(mcVersion string) int {
	minor, patch, ok := parseMC(mcVersion)
	if !ok {
		return 21
	}
	switch {
	case minor >= 21:
		return 21
	case minor == 20 && patch >= 5: // 1.20.5 moved to 21 mid-cycle
		return 21
	case minor >= 18: // 1.18 – 1.20.4
		return 17
	case minor == 17:
		return 16
	default:
		return 8
	}
}

// JavaBin resolves the java binary for a major version.
//
// Java is not installed on the host by default; the hosuto CLI installs openjdk-17/21-jre-headless,
// which land under /usr/lib/jvm. The exact Debian path is arch-suffixed, so try that first, then
// glob for any arch, and only then fall back to whatever `java` is on PATH — that last step is a
// guess (PATH java may be the wrong major), so it is deliberately last.
func JavaBin(needed int) (string, error) {
	return javaBinIn(systemJVMDir, needed)
}

// javaDirRe matches the canonical Debian JDK directory and captures its major: "java-21-openjdk-amd64"
// -> 21. It deliberately does NOT match the dotted legacy alias "java-1.21.0-openjdk-amd64" (a symlink
// to the same JDK), so each JDK is counted once, by its clean major.
var javaDirRe = regexp.MustCompile(`^java-([0-9]+)-openjdk-`)

// javaBinIn is JavaBin's filesystem half, rooted so tests can point it at a fixture tree.
//
// It returns the SMALLEST installed JDK whose major is >= needed. Newer Java runs a modern Minecraft
// fine, so requiring the exact major would reject a perfectly good runtime; preferring the closest
// keeps an old version off a needlessly new one.
//
// Crucially it does NOT fall back to whatever "java" is on PATH. That fallback was a
// silent-wrong-version trap: a host with only Java 21 would hand it to a Minecraft that needs Java 25,
// and the server would die on boot with an opaque UnsupportedClassVersionError instead of failing
// here — at create time, with a message naming the package to install.
func javaBinIn(root string, needed int) (string, error) {
	entries, _ := os.ReadDir(root)
	best, bestPath := -1, ""
	for _, e := range entries {
		m := javaDirRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		if major < needed {
			continue
		}
		bin := filepath.Join(root, e.Name(), "bin", "java")
		if !executable(bin) {
			continue
		}
		if best == -1 || major < best {
			best, bestPath = major, bin
		}
	}
	if bestPath != "" {
		return bestPath, nil
	}
	return "", fmt.Errorf("%w: Minecraft needs Java %d, but no Java %d or newer is installed here (install openjdk-%d-jre-headless)",
		ErrNoJava, needed, needed, needed)
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// ── loader catalogues ─────────────────────────────────────────────────────────────────

// LoaderVersions lists the loader builds installable for a Minecraft version, newest first.
//
// Vanilla has no loader, so it returns nothing. An empty list with a nil error is a normal answer
// and means "this loader has nothing for that Minecraft version" — the UI says so; it is not an
// error condition.
func (c *Client) LoaderVersions(ctx context.Context, loader, mcVersion string) ([]string, error) {
	if !store.ValidLoader(loader) {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedLoader, loader)
	}
	switch loader {
	case "vanilla":
		return nil, nil
	case "fabric":
		return c.fabricLoaders(ctx)
	case "neoforge":
		return c.neoforgeVersions(ctx, mcVersion)
	case "paper":
		return c.paperBuilds(ctx, mcVersion)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnsupportedLoader, loader)
}

type fabricEntry struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

// fabricLoaders returns Fabric's stable loader versions, newest first.
//
// mcVersion is not a parameter: a Fabric loader build is game-version-independent (it is the
// intermediary mappings, resolved at install time by the meta service, that bind a loader to a
// game version). Offering the same loader list for every game version is therefore correct, and an
// impossible combination fails loudly at Install with a 404 from the meta service rather than
// silently producing a broken jar.
func (c *Client) fabricLoaders(ctx context.Context) ([]string, error) {
	b, err := c.fetch(ctx, c.fabricBase+"/v2/versions/loader")
	if err != nil {
		return nil, err
	}
	var list []fabricEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("fabric loaders: %w", err)
	}
	return stableFirst(list), nil
}

// stableFirst keeps upstream's newest-first order but drops pre-releases — unless every entry is a
// pre-release, in which case the user gets what exists rather than an empty picker.
func stableFirst(list []fabricEntry) []string {
	var stable, all []string
	for _, e := range list {
		if e.Version == "" {
			continue
		}
		all = append(all, e.Version)
		if e.Stable {
			stable = append(stable, e.Version)
		}
	}
	if len(stable) > 0 {
		return stable
	}
	return all
}

// fabricInstaller returns the newest stable installer version, which the server-jar endpoint
// requires as a path segment.
func (c *Client) fabricInstaller(ctx context.Context) (string, error) {
	b, err := c.fetch(ctx, c.fabricBase+"/v2/versions/installer")
	if err != nil {
		return "", err
	}
	var list []fabricEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return "", fmt.Errorf("fabric installers: %w", err)
	}
	v := stableFirst(list)
	if len(v) == 0 {
		return "", errors.New("fabric: no installer versions published")
	}
	return v[0], nil
}

type mavenMetadata struct {
	Versioning struct {
		Versions struct {
			Version []string `xml:"version"`
		} `xml:"versions"`
	} `xml:"versioning"`
}

// neoforgeMC maps a NeoForge version onto the Minecraft version it targets.
//
// NeoForge numbers releases <mcMinor>.<mcPatch>.<build>: 21.1.235 is MC 1.21.1, build 235. The
// patch-zero case is the trap — 21.0.167 targets MC "1.21", which Mojang does not spell "1.21.0",
// so a naive join produces a version id that matches nothing. A "-beta" suffix marks a
// pre-release.
func neoforgeMC(v string) (mc string, stable bool, ok bool) {
	base, suffix, _ := strings.Cut(strings.TrimSpace(v), "-")
	parts := strings.Split(base, ".")
	if len(parts) < 3 {
		return "", false, false
	}
	minor, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", false, false
	}
	patch, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", false, false
	}
	if _, err := strconv.Atoi(parts[2]); err != nil {
		return "", false, false
	}
	mc = "1." + strconv.Itoa(minor)
	if patch > 0 {
		mc += "." + strconv.Itoa(patch)
	}
	return mc, suffix == "", true
}

// neoforgeVersions lists the NeoForge builds for one Minecraft version, newest first. Maven
// metadata is chronological (oldest first), so it is reversed.
func (c *Client) neoforgeVersions(ctx context.Context, mcVersion string) ([]string, error) {
	b, err := c.fetch(ctx, c.neoBase+"/maven-metadata.xml")
	if err != nil {
		return nil, err
	}
	var md mavenMetadata
	if err := xml.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("neoforge maven metadata: %w", err)
	}
	var stable, all []string
	for _, v := range md.Versioning.Versions.Version {
		mc, isStable, ok := neoforgeMC(v)
		if !ok || (mcVersion != "" && mc != mcVersion) {
			continue
		}
		all = append(all, v)
		if isStable {
			stable = append(stable, v)
		}
	}
	out := stable
	if len(out) == 0 {
		out = all
	}
	reverse(out)
	return out, nil
}

type paperBuildList struct {
	Builds []struct {
		Build     int    `json:"build"`
		Channel   string `json:"channel"` // default | experimental
		Downloads struct {
			Application struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
			} `json:"application"`
		} `json:"downloads"`
	} `json:"builds"`
}

// paperBuilds lists Paper's stable builds for a Minecraft version, newest first.
//
// Paper supports far fewer Minecraft versions than Mojang publishes, and answers 404 for the rest.
// That is a normal answer ("Paper has no build for 1.7.10"), not a failure, so it maps to an empty
// list.
func (c *Client) paperBuilds(ctx context.Context, mcVersion string) ([]string, error) {
	bl, err := c.paperBuildList(ctx, mcVersion)
	if err != nil {
		if errors.Is(err, ErrUnknownVersion) {
			return nil, nil
		}
		return nil, err
	}
	var stable, all []string
	for _, b := range bl.Builds {
		id := strconv.Itoa(b.Build)
		all = append(all, id)
		if b.Channel == "default" {
			stable = append(stable, id)
		}
	}
	out := stable
	if len(out) == 0 {
		out = all
	}
	reverse(out)
	return out, nil
}

func (c *Client) paperBuildList(ctx context.Context, mcVersion string) (*paperBuildList, error) {
	b, err := c.fetch(ctx, c.paperBase+"/versions/"+mcVersion+"/builds")
	if err != nil {
		return nil, err
	}
	var bl paperBuildList
	if err := json.Unmarshal(b, &bl); err != nil {
		return nil, fmt.Errorf("paper builds %s: %w", mcVersion, err)
	}
	return &bl, nil
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// ── install ───────────────────────────────────────────────────────────────────────────

// Install downloads, verifies and lays a server down in dir. It returns the argv the systemd unit
// must exec (with WorkingDirectory=dir), and the loader version it actually installed.
//
// loaderVersion may be empty, in which case the newest stable build is chosen — the UI's "just
// give me a server" path. That choice is made HERE, per loader, so it is also reported back here:
// the caller must record what was installed, not what was asked for. Nobody else can answer the
// question — Paper picks the newest build on the "default" channel, which is not simply the head of
// the public build list — and a server whose record says nothing (or says the wrong thing) cannot be
// exported as a .mrpack or a Prism instance, both of which must name a concrete loader version. The
// returned version is empty only for vanilla, which has no loader at all.
//
// dir must already exist: it is created by the privileged wrapper with the owner's uid, and creating
// it here would produce a tree owned by the daemon.
func (c *Client) Install(ctx context.Context, dir, loader, mcVersion, loaderVersion string, heapMB int) ([]string, string, error) {
	if !store.ValidLoader(loader) {
		return nil, "", fmt.Errorf("%w: %q", ErrUnsupportedLoader, loader)
	}
	if !filepath.IsAbs(dir) {
		return nil, "", fmt.Errorf("%w: server dir must be absolute, got %q", ErrInvalid, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, "", fmt.Errorf("%w: server dir %s must exist", ErrInvalid, dir)
	}
	if heapMB <= 0 {
		return nil, "", fmt.Errorf("%w: heap must be positive, got %d", ErrInvalid, heapMB)
	}
	if mcVersion == "" {
		return nil, "", fmt.Errorf("%w: no minecraft version", ErrInvalid)
	}

	// Resolved before anything is downloaded: a host with no matching JRE should fail before it
	// has spent two minutes pulling jars.
	major, err := c.JavaMajorFor(ctx, mcVersion)
	if err != nil {
		return nil, "", err
	}
	java, err := c.javaBin(major)
	if err != nil {
		return nil, "", err
	}

	switch loader {
	case "vanilla":
		return c.installVanilla(ctx, dir, mcVersion, java, heapMB)
	case "fabric":
		return c.installFabric(ctx, dir, mcVersion, loaderVersion, java, heapMB)
	case "paper":
		return c.installPaper(ctx, dir, mcVersion, loaderVersion, java, heapMB)
	case "neoforge":
		return c.installNeoForge(ctx, dir, mcVersion, loaderVersion, java, heapMB)
	}
	return nil, "", fmt.Errorf("%w: %q", ErrUnsupportedLoader, loader)
}

// jarArgv is the launch line for every loader that ships a runnable jar. The heap goes on the
// command line here; NeoForge is the exception and puts it in user_jvm_args.txt.
func jarArgv(java, jar string, heapMB int) []string {
	return []string{java, fmt.Sprintf("-Xms%dM", heapMB), fmt.Sprintf("-Xmx%dM", heapMB), "-jar", jar, "nogui"}
}

// vanilla has no loader, so it reports no loader version — the one honest empty string in Install's
// contract.
func (c *Client) installVanilla(ctx context.Context, dir, mcVersion, java string, heapMB int) ([]string, string, error) {
	vm, err := c.versionMeta(ctx, mcVersion)
	if err != nil {
		return nil, "", err
	}
	if vm.Downloads.Server.URL == "" {
		// True for the old_alpha/old_beta tail: Mojang never published a server jar for them.
		return nil, "", fmt.Errorf("%w: minecraft %s publishes no server jar", ErrUnknownVersion, mcVersion)
	}
	jar := filepath.Join(dir, "server.jar")
	// Mojang publishes SHA-1, not SHA-256. Verifying with the wrong algorithm would fail every
	// install; verifying with none would install anything the CDN handed us.
	if err := c.download(ctx, vm.Downloads.Server.URL, jar, "sha1", vm.Downloads.Server.SHA1); err != nil {
		return nil, "", err
	}
	return jarArgv(java, jar, heapMB), "", nil
}

func (c *Client) installFabric(ctx context.Context, dir, mcVersion, loaderVersion, java string, heapMB int) ([]string, string, error) {
	if loaderVersion == "" {
		vs, err := c.fabricLoaders(ctx)
		if err != nil {
			return nil, "", err
		}
		if len(vs) == 0 {
			return nil, "", fmt.Errorf("%w: fabric publishes no loader versions", ErrUnknownVersion)
		}
		loaderVersion = vs[0]
	}
	installer, err := c.fabricInstaller(ctx)
	if err != nil {
		return nil, "", err
	}
	// Fabric's meta service assembles a launchable server jar on demand. It publishes no digest
	// for it (the jar is generated per request), so this is the one download with nothing to check
	// against — the transport is HTTPS and the URL is pinned, which is the whole guarantee.
	url := fmt.Sprintf("%s/v2/versions/loader/%s/%s/%s/server/jar",
		c.fabricBase, mcVersion, loaderVersion, installer)
	jar := filepath.Join(dir, "fabric-server-launch.jar")
	if err := c.download(ctx, url, jar, "", ""); err != nil {
		return nil, "", err
	}
	return jarArgv(java, jar, heapMB), loaderVersion, nil
}

func (c *Client) installPaper(ctx context.Context, dir, mcVersion, build, java string, heapMB int) ([]string, string, error) {
	bl, err := c.paperBuildList(ctx, mcVersion)
	if err != nil {
		return nil, "", err
	}
	// Pick the requested build, or the newest stable one. Paper's list is ascending, so the last
	// match wins.
	name, sum, chosen := "", "", 0
	for _, b := range bl.Builds {
		if build != "" {
			if strconv.Itoa(b.Build) != build {
				continue
			}
		} else if b.Channel != "default" {
			continue
		}
		name, sum, chosen = b.Downloads.Application.Name, b.Downloads.Application.SHA256, b.Build
	}
	if chosen == 0 {
		return nil, "", fmt.Errorf("%w: paper build %q for minecraft %s", ErrUnknownVersion, build, mcVersion)
	}
	url := fmt.Sprintf("%s/versions/%s/builds/%d/downloads/%s", c.paperBase, mcVersion, chosen, name)
	jar := filepath.Join(dir, "server.jar")
	// Paper publishes SHA-256 (unlike Mojang's SHA-1).
	if err := c.download(ctx, url, jar, "sha256", sum); err != nil {
		return nil, "", err
	}
	// The build actually taken — not the head of the public list, which includes channels this
	// installer skips.
	return jarArgv(java, jar, heapMB), strconv.Itoa(chosen), nil
}

// installNeoForge runs the official installer headlessly. NeoForge does not ship a fat server jar:
// the installer materialises run.sh, libraries/ and the @argfiles that the launch line references.
func (c *Client) installNeoForge(ctx context.Context, dir, mcVersion, version, java string, heapMB int) ([]string, string, error) {
	if version == "" {
		vs, err := c.neoforgeVersions(ctx, mcVersion)
		if err != nil {
			return nil, "", err
		}
		if len(vs) == 0 {
			return nil, "", fmt.Errorf("%w: neoforge has no build for minecraft %s", ErrUnknownVersion, mcVersion)
		}
		version = vs[0]
	} else if mc, _, ok := neoforgeMC(version); !ok || mc != mcVersion {
		// A NeoForge version encodes its Minecraft version, so a mismatch is caught here rather
		// than by a confusing crash on first boot.
		return nil, "", fmt.Errorf("%w: neoforge %s does not target minecraft %s", ErrInvalid, version, mcVersion)
	}

	base := fmt.Sprintf("%s/%s/neoforge-%s-installer.jar", c.neoBase, version, version)
	installer := filepath.Join(dir, ".neoforge-installer.jar")
	// Maven publishes a .sha1 sidecar next to every artifact. Verify against it when it is there;
	// a missing sidecar is not fatal, a wrong digest always is.
	sum, err := c.mavenSHA1(ctx, base+".sha1")
	if err != nil {
		return nil, "", err
	}
	if err := c.download(ctx, base, installer, "sha1", sum); err != nil {
		return nil, "", err
	}
	defer os.Remove(installer) // the installer jar is build-time only; it must not sit in the server dir

	if err := c.runInstaller(ctx, java, installer, dir); err != nil {
		return nil, "", fmt.Errorf("neoforge installer %s: %w", version, err)
	}

	// The heap MUST go in user_jvm_args.txt: the launch line is an @argfile pair, and JVM flags
	// passed after them are ignored by the game's own arg parsing.
	if err := writeJVMArgs(filepath.Join(dir, "user_jvm_args.txt"), heapMB); err != nil {
		return nil, "", err
	}

	// Prefer exec'ing java directly over run.sh: systemd then owns the JVM process itself, so
	// SIGTERM on stop reaches the server rather than a shell wrapper. run.sh is the fallback for
	// layouts that do not produce unix_args.txt.
	unixArgs := filepath.Join(dir, "libraries", "net", "neoforged", "neoforge", version, "unix_args.txt")
	if _, err := os.Stat(unixArgs); err == nil {
		return []string{java, "@" + filepath.Join(dir, "user_jvm_args.txt"), "@" + unixArgs, "nogui"}, version, nil
	}
	run := filepath.Join(dir, "run.sh")
	if fi, err := os.Stat(run); err == nil && fi.Mode().IsRegular() {
		if err := os.Chmod(run, 0o755); err != nil {
			return nil, "", err
		}
		return []string{run, "nogui"}, version, nil
	}
	return nil, "", fmt.Errorf("neoforge %s: installer produced neither %s nor run.sh", version, unixArgs)
}

// writeJVMArgs sets the heap in NeoForge's user_jvm_args.txt, preserving the installer's comments
// and dropping any heap flags already there (re-installing with a new heap must not stack -Xmx
// lines: the JVM would take the last one and the file would drift).
func writeJVMArgs(path string, heapMB int) error {
	var kept []string
	if b, err := os.ReadFile(path); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			t := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(t, "-Xmx") || strings.HasPrefix(t, "-Xms") {
				continue
			}
			kept = append(kept, sc.Text())
		}
		if err := sc.Err(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	kept = append(kept, fmt.Sprintf("-Xms%dM", heapMB), fmt.Sprintf("-Xmx%dM", heapMB))
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

// mavenSHA1 fetches a maven digest sidecar. A 404 means the repository does not publish one, which
// is not an error; any other failure is.
func (c *Client) mavenSHA1(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	// The sidecar is the hex digest, sometimes followed by the filename. An empty body is treated
	// as "no digest published" rather than as a digest of "" — which would fail every install.
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", nil
	}
	return strings.ToLower(fields[0]), nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

// ── http ──────────────────────────────────────────────────────────────────────────────

// fetch GETs a metadata document, memoised for cacheTTL. The catalogue is hit on every open of the
// Version & Modding tab, and the manifests are the same bytes for everyone.
func (c *Client) fetch(ctx context.Context, url string) ([]byte, error) {
	c.mu.Lock()
	e, ok := c.cache[url]
	fresh := ok && c.now().Sub(e.at) < cacheTTL
	c.mu.Unlock()
	if fresh {
		return e.body, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/xml, */*")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: GET %s", ErrUnknownVersion, url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogueBytes))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}

	c.mu.Lock()
	c.cache[url] = cacheEntry{body: body, at: c.now()}
	c.mu.Unlock()
	return body, nil
}

func newHash(algo string) (hash.Hash, error) {
	switch algo {
	case "sha1":
		return sha1.New(), nil
	case "sha256":
		return sha256.New(), nil
	}
	return nil, fmt.Errorf("unknown digest algorithm %q", algo)
}

// download streams url to dest and verifies it against want (hex, algorithm algo) before the file
// is visible under its final name. An empty want skips verification, which is only legitimate for
// an upstream that publishes no digest at all.
//
// The bytes land in dest+".part" and are renamed only after the digest matches, so a failed or
// interrupted download can never be mistaken for an installed server.
func (c *Client) download(ctx context.Context, url, dest, algo, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.dl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	defer os.Remove(part) // no-op once the rename below succeeds

	var w io.Writer = f
	var h hash.Hash
	if want != "" {
		if h, err = newHash(algo); err != nil {
			f.Close()
			return err
		}
		w = io.MultiWriter(f, h)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("GET %s: %w", url, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if h != nil {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("%w: %s: %s want %s got %s", ErrHashMismatch, url, algo, strings.ToLower(want), got)
		}
	}
	return os.Rename(part, dest)
}
