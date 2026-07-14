// Package export turns a hosuto server record into the artifacts a player can actually use on
// their own machine: a bare zip of the client mods, a Modrinth ".mrpack", and a Prism Launcher
// instance. It is the whole of the dashboard's "Client Export" tab.
//
// Two invariants hold across all three artifacts, and most of the code here exists to keep one of
// them:
//
//   - The client/server split. A mod with ClientSide == "unsupported" is a server-only mod (a chunk
//     pregenerator, an anti-cheat); handing it to a client makes the client crash on launch. The
//     filter lives in exactly one place — clientMods — and every artifact goes through it.
//   - Licence safety. hosuto redistributes bytes only where it may: a jar the user uploaded
//     themselves, or one they could just as well have downloaded from Modrinth. The .mrpack needs
//     to redistribute nothing at all — it REFERENCES each Modrinth jar by CDN URL + hash — which is
//     why it is the artifact to prefer, and why a Modrinth mod with no URL or no hashes is a hard
//     error here rather than a quiet fallback to shipping the bytes.
//
// Everything streams. A modpack is tens to hundreds of megabytes and the daemon holds none of it:
// each writer takes an io.Writer (in practice the http.ResponseWriter) and copies every jar
// straight from disk or from the Fetcher into the zip. Nothing is buffered, nothing is spooled to
// a temp file, and a client that hangs up mid-download simply makes the next write fail.
//
// The package does no HTTP itself. Downloading a Modrinth jar is the API layer's job — it owns the
// timeout, the User-Agent and the cache — and it passes the result in as a Fetcher.
package export

import (
	"archive/zip"
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"hosuto/internal/store"
)

// Fetcher hands the export one mod jar's bytes. The API layer injects it, which is how this package
// stays free of the network: the caller owns the timeout (8-10s, per house style), the User-Agent
// and any on-disk cache.
//
// sha1 is the hash the store recorded, passed so a caching implementation can find the jar it
// already has and verify what it serves. Verification is the Fetcher's job, not the export's: the
// export copies the stream straight into the zip and can never rewind to check it.
//
// The export has no request context of its own — its three entry points take none, by contract with
// the API layer — so it calls the Fetcher with context.Background(). That is deliberate rather than
// lazy: the artifact writers are driven by their io.Writer, so an abandoned download dies when the
// writes to the hung-up client start failing, and the bound on a wedged fetch is the timeout the
// Fetcher itself carries. A Fetcher that wants request-scoped cancellation on top can close over
// the request's context.
type Fetcher func(ctx context.Context, url, sha1 string) (io.ReadCloser, error)

// The client heap Prism should give the instance. Nothing to do with srv.HeapMB, which is the
// SERVER's heap: these are the player's. A modded 1.21 client will OOM inside Prism's own 1 GB
// default, so hosuto overrides it.
const (
	clientMinHeapMB = 2048
	clientMaxHeapMB = 4096
)

// checkClientExport refuses, up front, to build a client bundle that could not work.
//
// Paper runs Bukkit PLUGINS, which are server-side only, and vanilla has no mod surface at all:
// for either, LoaderHasClientMods is false and there is nothing to hand the player. The remaining
// checks are the fields every artifact needs — an instance pinned to no Minecraft version, or a
// servers.dat with no address in it, is a file that fails confusingly on the player's machine
// instead of clearly here.
func checkClientExport(srv store.Server) error {
	if !store.LoaderHasClientMods(srv.Loader) {
		return fmt.Errorf("a %s server has no client mods: there is nothing to export", srv.Loader)
	}
	if srv.MCVersion == "" {
		return fmt.Errorf("server %q has no Minecraft version recorded", srv.Slug)
	}
	if srv.LoaderVersion == "" {
		return fmt.Errorf("server %q has no %s version recorded", srv.Slug, srv.Loader)
	}
	if strings.TrimSpace(srv.Host) == "" {
		return fmt.Errorf("server %q has no host recorded: the client would have nothing to connect to", srv.Slug)
	}
	return nil
}

// JoinAddress is the address the player's client dials, and the single source of truth for it: the
// .mrpack's servers.dat, the Prism instance's servers.dat and Prism's JoinServerOnLaunchAddress all
// read it from here, so the three cannot drift apart.
//
// It is the HOSTNAME ALONE — never Host:Port.
//
// srv.Port is the loopback port the JVM binds (server-ip=127.0.0.1); it is not reachable from
// anywhere but this machine, and it is never what a player dials. Players reach mc-router on the
// default 25565, which reads the hostname out of their handshake and splices them to that backend.
// Emitting Host:Port here would put an unreachable address into every exported bundle and break the
// one-click join that is the whole point of the Prism export — silently, because the file looks
// perfectly well-formed.
//
// Omitting the port is also what keeps the SRV trap shut: a client that is given a bare hostname on
// the default port never needs an SRV record, and an SRV record would REPLACE the hostname in the
// handshake with its target — collapsing every server onto one backend.
func JoinAddress(srv store.Server) string {
	return srv.Host
}

// packName is what the player sees — in their launcher's instance list and in their server list.
func packName(srv store.Server) string {
	if n := strings.TrimSpace(srv.Name); n != "" {
		return n
	}
	return srv.Slug
}

// clientMods is the client/server split, in one place. Modrinth's "unsupported" means the mod does
// not run on that side; a server-only mod on a client is a crash on launch, not a warning.
func clientMods(mods []store.Mod) []store.Mod {
	out := make([]store.Mod, 0, len(mods))
	for _, m := range mods {
		if m.ClientSide == "unsupported" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// env maps the store's environment vocabulary onto Modrinth's. "unknown" is what hosuto records
// when it never learned the answer (a raw upload); "optional" is the only honest translation, since
// it lets the file install without claiming the pack requires it.
func env(side string) string {
	switch side {
	case "required", "optional", "unsupported":
		return side
	default: // "unknown", or a value hosuto has never seen
		return "optional"
	}
}

// jarName reduces a mod's filename to a name that is safe as a zip entry.
//
// The record is written by the daemon, but Filename itself came from an upload form or from
// Modrinth's API, so it is treated as hostile: "../../../.bashrc" would otherwise become an entry
// that a careless extractor writes outside the target directory (zip-slip), and it is also the name
// this package joins onto jarDir to read an upload back.
func jarName(m store.Mod) (string, error) {
	n := strings.TrimSpace(m.Filename)
	if n == "" {
		return "", fmt.Errorf("mod %q has no filename", m.Name)
	}
	if n == "." || n == ".." || strings.ContainsAny(n, `/\`) || n != filepath.Base(n) {
		return "", fmt.Errorf("mod %q has an unusable filename %q", m.Name, m.Filename)
	}
	return n, nil
}

// jarNames resolves every mod's entry name at once and rejects a collision. Two jars with the same
// name would make two zip entries at the same path: one mod silently shadows the other, and which
// one wins is up to the extractor.
func jarNames(mods []store.Mod) ([]string, error) {
	seen := make(map[string]bool, len(mods))
	out := make([]string, len(mods))
	for i, m := range mods {
		n, err := jarName(m)
		if err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, fmt.Errorf("two mods share the filename %q", n)
		}
		seen[n] = true
		out[i] = n
	}
	return out, nil
}

// checkDownloadURL bounds where a .mrpack may point a launcher.
//
// The launcher will download, unattended, whatever URL the index names. An index hosuto writes must
// therefore only ever name Modrinth's CDN: that is the entire basis on which the pack redistributes
// nothing, and the only host whose contents match the hashes hosuto recorded.
func checkDownloadURL(m store.Mod) error {
	u, err := url.Parse(m.URL)
	if err != nil {
		return fmt.Errorf("mod %q has an unparseable download URL: %w", m.Name, err)
	}
	if u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".modrinth.com") {
		return fmt.Errorf("mod %q points at %q, which is not the Modrinth CDN", m.Name, m.URL)
	}
	return nil
}

// newZipWriter builds the zip writer all three artifacts share.
//
// Every entry is DEFLATED — even the jars, which are already-compressed zips and gain nothing from
// a second pass. The reason is compatibility, not size. Go's streaming zip writer cannot seek back
// to patch a local header, so it must emit a data descriptor after every entry, and Java's
// ZipInputStream (which is how some launchers read a pack) rejects a STORED entry that carries one:
// "only DEFLATED entries can have EXT descriptor". Deflating at BestSpeed keeps every entry DEFLATED
// while spending almost no CPU on bytes that will not compress anyway.
func newZipWriter(w io.Writer) *zip.Writer {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	return zw
}

// addStream copies r into a new zip entry. The copy is the point: the jar never lands in memory.
func addStream(zw *zip.Writer, path string, r io.Reader) error {
	f, err := zw.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return err
}

// addJSON writes v as an indented JSON zip entry. Indented because a player who opens the pack to
// see what it will install should be able to read it.
func addJSON(zw *zip.Writer, path string, v any) error {
	f, err := zw.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// addServersDat writes the preset connection into the zip at path.
func addServersDat(zw *zip.Writer, path string, srv store.Server) error {
	f, err := zw.Create(path)
	if err != nil {
		return err
	}
	return WriteServersDat(f, packName(srv), JoinAddress(srv))
}

// copyJar streams one mod's bytes into the zip at path.
//
// An upload comes off disk, from the server's own mods directory; a Modrinth mod comes from the
// injected Fetcher. Either way the bytes go straight through — disk or network → zip → player —
// and are never held.
func copyJar(zw *zip.Writer, path string, m store.Mod, jarDir string, fetch Fetcher) error {
	name, err := jarName(m)
	if err != nil {
		return err
	}
	switch m.Source {
	case "upload":
		if jarDir == "" {
			return fmt.Errorf("mod %q was uploaded but no jar directory was given", m.Name)
		}
		f, err := os.Open(filepath.Join(jarDir, name))
		if err != nil {
			return fmt.Errorf("mod %q: %w", m.Name, err)
		}
		defer f.Close()
		return addStream(zw, path, f)
	case "modrinth":
		if fetch == nil {
			return fmt.Errorf("mod %q must be downloaded but no fetcher was given", m.Name)
		}
		if m.URL == "" {
			return fmt.Errorf("mod %q has no download URL", m.Name)
		}
		if err := checkDownloadURL(m); err != nil {
			return err
		}
		rc, err := fetch(context.Background(), m.URL, m.SHA1)
		if err != nil {
			return fmt.Errorf("mod %q: %w", m.Name, err)
		}
		defer rc.Close()
		return addStream(zw, path, rc)
	default:
		return fmt.Errorf("mod %q has an unknown source %q", m.Name, m.Source)
	}
}
