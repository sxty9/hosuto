package export

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"hosuto/internal/store"
)

// The fixtures below are shared by every test in the package. They describe one realistic server:
// a Modrinth mod that is client-and-server (Fabric API), a Modrinth mod that is CLIENT-ONLY
// (Sodium: server "unsupported"), a Modrinth mod that is SERVER-ONLY (Chunky: client
// "unsupported" — the one that must never reach the player), and a raw upload.

const (
	sodiumURL  = "https://cdn.modrinth.com/data/AANobbMI/versions/x/sodium-fabric-0.6.0.jar"
	fabricAPI  = "https://cdn.modrinth.com/data/P7dR8mSH/versions/y/fabric-api-0.100.0.jar"
	chunkyURL  = "https://cdn.modrinth.com/data/fALzjamp/versions/z/chunky-1.4.0.jar"
	uploadName = "homebrew.jar"
)

func testMods() []store.Mod {
	return []store.Mod{
		{
			ID: "mod-1", Source: "modrinth", Name: "Sodium", Filename: "sodium.jar",
			URL: sodiumURL, SHA1: "aaaa1111", SHA512: "bbbb2222", Size: 1234,
			ClientSide: "required", ServerSide: "unsupported",
		},
		{
			ID: "mod-2", Source: "modrinth", Name: "Fabric API", Filename: "fabric-api.jar",
			URL: fabricAPI, SHA1: "cccc3333", SHA512: "dddd4444", Size: 2345,
			ClientSide: "required", ServerSide: "required",
		},
		{
			// Server-only. A chunk pregenerator on a client is a crash on launch; it must appear
			// in NO client artifact.
			ID: "mod-3", Source: "modrinth", Name: "Chunky", Filename: "chunky.jar",
			URL: chunkyURL, SHA1: "eeee5555", SHA512: "ffff6666", Size: 3456,
			ClientSide: "unsupported", ServerSide: "required",
		},
		{
			// An upload: Modrinth cannot serve it, so its bytes must ride along.
			ID: "mod-4", Source: "upload", Name: "Homebrew", Filename: uploadName,
			ClientSide: "unknown", ServerSide: "unknown",
		},
	}
}

func testServer() store.Server {
	return store.Server{
		ID: "srv-abc", Slug: "creative", Name: "Creative Sandbox", Owner: "nanu", Game: "minecraft",
		MCVersion: "1.21.1", Loader: "fabric", LoaderVersion: "0.16.14",
		HeapMB: 6144, Port: 25566, RconPort: 25576,
		Host: "creative.example.org", JoinPolicy: "whitelist",
		Mods: testMods(),
	}
}

// jarDir writes the upload's bytes to a temp dir and returns it, standing in for the server's own
// mods directory on disk.
func jarDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, uploadName), []byte("HOMEBREW-JAR-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeFetch is the injected downloader. Tests must never touch the network, and a Fetcher that
// tried to would fail here loudly rather than by hanging.
func fakeFetch(t *testing.T) Fetcher {
	t.Helper()
	body := map[string]string{
		sodiumURL: "SODIUM-JAR-BYTES",
		fabricAPI: "FABRIC-API-JAR-BYTES",
		chunkyURL: "CHUNKY-JAR-BYTES", // present on purpose: nothing may ever ask for it
	}
	return func(_ context.Context, url, _ string) (io.ReadCloser, error) {
		b, ok := body[url]
		if !ok {
			return nil, fmt.Errorf("fetcher asked for an unknown url %q", url)
		}
		return io.NopCloser(bytes.NewReader([]byte(b))), nil
	}
}

// readZip opens the artifact with archive/zip — which is itself the assertion that what was
// streamed is a well-formed zip — and returns every entry by path.
func readZip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("archive/zip cannot open the artifact: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		if _, dup := out[f.Name]; dup {
			t.Fatalf("duplicate zip entry %s", f.Name)
		}
		out[f.Name] = data
	}
	return out
}

func TestJoinAddress(t *testing.T) {
	// The player-facing address is the HOSTNAME ALONE. srv.Port is the loopback port the JVM binds
	// (server-ip=127.0.0.1); nothing outside this machine can dial it. Players reach mc-router on the
	// default 25565 and it splices them to that backend by the hostname in their handshake.
	//
	// This test exists because the opposite — emitting Host:Port — is the plausible-looking bug: the
	// exported bundle is perfectly well-formed and simply points at an address that can never answer,
	// so the one-click join fails at the player's end with nothing to debug.
	srv := store.Server{Host: "creative.example.org", Port: 25566}
	if got, want := JoinAddress(srv), "creative.example.org"; got != want {
		t.Errorf("JoinAddress = %q, want %q — the internal backend port must never reach a client", got, want)
	}
	srv.Port = 0
	if got, want := JoinAddress(srv), "creative.example.org"; got != want {
		t.Errorf("JoinAddress with no port = %q, want %q", got, want)
	}
}

// A server whose loader has no client side must be refused by every artifact that knows the loader,
// with an error the UI can show — not with an empty zip.
func TestClientExportRefusesLoadersWithoutClientMods(t *testing.T) {
	for _, loader := range []string{"paper", "vanilla"} {
		t.Run(loader, func(t *testing.T) {
			srv := testServer()
			srv.Loader = loader
			for name, write := range map[string]func(io.Writer) error{
				"mrpack": func(w io.Writer) error { return WriteMrpack(w, srv, jarDir(t), fakeFetch(t)) },
				"prism":  func(w io.Writer) error { return WritePrismZip(w, srv, jarDir(t), fakeFetch(t)) },
			} {
				var buf bytes.Buffer
				err := write(&buf)
				if err == nil {
					t.Errorf("%s: wrote a client bundle for a %s server, want an error", name, loader)
				}
				if buf.Len() != 0 {
					t.Errorf("%s: emitted %d bytes before failing; the guard must come first", name, buf.Len())
				}
			}
		})
	}
}

// The fields every artifact needs. Missing ones must fail here, where the user can be told, not on
// the player's machine.
func TestClientExportRequiresCompleteServer(t *testing.T) {
	tests := map[string]func(*store.Server){
		"no mc version":     func(s *store.Server) { s.MCVersion = "" },
		"no loader version": func(s *store.Server) { s.LoaderVersion = "" },
		"no host":           func(s *store.Server) { s.Host = "" },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			srv := testServer()
			break_(&srv)
			if err := WriteMrpack(io.Discard, srv, jarDir(t), fakeFetch(t)); err == nil {
				t.Error("WriteMrpack accepted an incomplete server")
			}
			if err := WritePrismZip(io.Discard, srv, jarDir(t), fakeFetch(t)); err == nil {
				t.Error("WritePrismZip accepted an incomplete server")
			}
		})
	}
}

// A hostile filename must never become a zip entry that escapes the extraction directory, and two
// mods must never collide onto one path.
func TestJarNamesRejectsUnsafeAndColliding(t *testing.T) {
	for _, name := range []string{"", "  ", "../../.bashrc", "sub/dir.jar", `..\..\evil.jar`, ".", ".."} {
		_, err := jarName(store.Mod{Name: "Evil", Filename: name})
		if err == nil {
			t.Errorf("jarName(%q) accepted an unsafe filename", name)
		}
	}
	if _, err := jarNames([]store.Mod{
		{Name: "A", Filename: "same.jar"},
		{Name: "B", Filename: "same.jar"},
	}); err == nil {
		t.Error("jarNames accepted two mods with the same filename")
	}
}
