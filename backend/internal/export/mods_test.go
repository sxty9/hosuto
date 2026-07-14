package export

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"hosuto/internal/store"
)

// The flat zip holds exactly the client jars: the Modrinth ones fetched, the upload read off disk,
// and the server-only mod nowhere at all.
func TestWriteModsZip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteModsZip(&buf, testMods(), jarDir(t), fakeFetch(t)); err != nil {
		t.Fatalf("WriteModsZip: %v", err)
	}
	entries := readZip(t, buf.Bytes())

	want := map[string]string{
		"sodium.jar":     "SODIUM-JAR-BYTES",
		"fabric-api.jar": "FABRIC-API-JAR-BYTES",
		uploadName:       "HOMEBREW-JAR-BYTES",
	}
	if len(entries) != len(want) {
		t.Errorf("got %d entries %v, want %d", len(entries), keys(entries), len(want))
	}
	for path, body := range want {
		got, ok := entries[path]
		if !ok {
			t.Errorf("missing entry %s", path)
			continue
		}
		if string(got) != body {
			t.Errorf("%s = %q, want %q", path, got, body)
		}
	}
	// The whole point of clientMods: a client_side=="unsupported" mod is not shipped to a client.
	if _, ok := entries["chunky.jar"]; ok {
		t.Error("the server-only mod (client_side=unsupported) is in the client zip")
	}
}

// Entries are flat: the player extracts them INTO mods/, so a mods/ prefix would nest one inside
// the other.
func TestWriteModsZipIsFlat(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteModsZip(&buf, testMods(), jarDir(t), fakeFetch(t)); err != nil {
		t.Fatal(err)
	}
	for path := range readZip(t, buf.Bytes()) {
		if filepath.Dir(path) != "." {
			t.Errorf("entry %q is not at the zip root", path)
		}
	}
}

func TestWriteModsZipErrors(t *testing.T) {
	t.Run("no client mods", func(t *testing.T) {
		// A server whose every mod is server-only has nothing to hand the player. Saying so beats
		// handing them an empty zip.
		mods := []store.Mod{{
			Source: "modrinth", Name: "Chunky", Filename: "chunky.jar", URL: chunkyURL,
			ClientSide: "unsupported", ServerSide: "required",
		}}
		if err := WriteModsZip(io.Discard, mods, jarDir(t), fakeFetch(t)); err == nil {
			t.Error("wrote a zip for a server with no client mods, want an error")
		}
	})

	t.Run("missing upload", func(t *testing.T) {
		mods := []store.Mod{{
			Source: "upload", Name: "Gone", Filename: "gone.jar", ClientSide: "unknown",
		}}
		if err := WriteModsZip(io.Discard, mods, t.TempDir(), fakeFetch(t)); err == nil {
			t.Error("accepted an upload whose jar is not on disk")
		}
	})

	t.Run("unsafe filename", func(t *testing.T) {
		// A traversing name must be refused before it can become a zip entry that escapes the
		// player's extraction directory.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "evil.jar"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mods := []store.Mod{{
			Source: "upload", Name: "Evil", Filename: "../../evil.jar", ClientSide: "required",
		}}
		if err := WriteModsZip(io.Discard, mods, dir, fakeFetch(t)); err == nil {
			t.Error("accepted a path-traversing filename")
		}
	})

	t.Run("unknown source", func(t *testing.T) {
		mods := []store.Mod{{
			Source: "curseforge", Name: "Mystery", Filename: "mystery.jar", ClientSide: "required",
		}}
		if err := WriteModsZip(io.Discard, mods, jarDir(t), fakeFetch(t)); err == nil {
			t.Error("accepted a mod from a source hosuto cannot redistribute")
		}
	})

	t.Run("non-modrinth url", func(t *testing.T) {
		// hosuto never fetches a jar from a host it does not vouch for, whatever the record says.
		mods := []store.Mod{{
			Source: "modrinth", Name: "Sneaky", Filename: "sneaky.jar",
			URL: "https://evil.example.com/sneaky.jar", ClientSide: "required",
		}}
		fetch := func(_ context.Context, _, _ string) (io.ReadCloser, error) {
			t.Error("the fetcher was called for a non-Modrinth URL")
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		if err := WriteModsZip(io.Discard, mods, jarDir(t), fetch); err == nil {
			t.Error("accepted a download URL outside the Modrinth CDN")
		}
	})
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
