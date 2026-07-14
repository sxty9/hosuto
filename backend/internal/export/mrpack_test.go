package export

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"hosuto/internal/store"
)

// legalDeps is the closed key set Prism's importer accepts. Any other key throws "Unknown
// dependency type" and the import dies.
var legalDeps = map[string]bool{
	"minecraft": true, "fabric-loader": true, "quilt-loader": true, "forge": true, "neoforge": true,
}

// The index is decoded into raw maps rather than back into mrIndex on purpose: the failures this
// guards against are MISSING KEYS, and unmarshalling into the struct that wrote them would happily
// invent the zero value for each one.
func mrpackIndex(t *testing.T, entries map[string][]byte) map[string]any {
	t.Helper()
	raw, ok := entries["modrinth.index.json"]
	if !ok {
		t.Fatal("no modrinth.index.json at the pack root")
	}
	var idx map[string]any
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("modrinth.index.json is not valid JSON: %v", err)
	}
	return idx
}

func TestWriteMrpackIndex(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMrpack(&buf, testServer(), jarDir(t), fakeFetch(t)); err != nil {
		t.Fatalf("WriteMrpack: %v", err)
	}
	entries := readZip(t, buf.Bytes())
	idx := mrpackIndex(t, entries)

	// Prism compares these two literally.
	if v, ok := idx["formatVersion"].(float64); !ok || v != 1 {
		t.Errorf("formatVersion = %v, want 1", idx["formatVersion"])
	}
	if v, ok := idx["game"].(string); !ok || v != "minecraft" {
		t.Errorf("game = %v, want \"minecraft\"", idx["game"])
	}
	if v, _ := idx["name"].(string); v != "Creative Sandbox" {
		t.Errorf("name = %q, want the server's name", v)
	}
	if v, _ := idx["versionId"].(string); v == "" {
		t.Error("versionId is empty")
	}

	// ── dependencies: the key set is closed ──
	deps, ok := idx["dependencies"].(map[string]any)
	if !ok {
		t.Fatal("dependencies is missing")
	}
	for k, v := range deps {
		if !legalDeps[k] {
			t.Errorf("dependency key %q is not one of minecraft|fabric-loader|quilt-loader|forge|neoforge", k)
		}
		if s, _ := v.(string); s == "" {
			t.Errorf("dependency %q has an empty version", k)
		}
	}
	if deps["minecraft"] != "1.21.1" {
		t.Errorf("minecraft = %v, want 1.21.1", deps["minecraft"])
	}
	if deps["fabric-loader"] != "0.16.14" {
		t.Errorf("fabric-loader = %v, want 0.16.14", deps["fabric-loader"])
	}

	// ── files: every one of them, every mandatory field ──
	files, ok := idx["files"].([]any)
	if !ok {
		t.Fatal("files is missing or not an array")
	}
	if len(files) != 2 {
		t.Fatalf("got %d referenced files, want 2 (the two client-side Modrinth mods)", len(files))
	}
	for _, f := range files {
		file, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("file entry is not an object: %v", f)
		}
		path, _ := file["path"].(string)
		if !strings.HasPrefix(path, "mods/") {
			t.Errorf("path %q is not under mods/", path)
		}

		// sha512 is MANDATORY: Prism reads it with requireString().
		hashes, ok := file["hashes"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no hashes", path)
		}
		if s, _ := hashes["sha512"].(string); s == "" {
			t.Errorf("%s: sha512 is missing or empty — Prism's requireString() fails the import", path)
		}
		if s, _ := hashes["sha1"].(string); s == "" {
			t.Errorf("%s: sha1 is missing or empty", path)
		}

		// The client KEY must be PRESENT. Prism does env["client"].toString("unsupported"), so a
		// missing key means the file is silently skipped on the client — no error, no mod.
		envObj, ok := file["env"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no env", path)
		}
		client, ok := envObj["client"]
		if !ok {
			t.Errorf("%s: env.client is MISSING — the file would be silently skipped on the client", path)
		}
		if s, _ := client.(string); s == "" || s == "unsupported" {
			t.Errorf("%s: env.client = %v, but only client-side mods belong in the pack", path, client)
		}
		if _, ok := envObj["server"]; !ok {
			t.Errorf("%s: env.server is missing", path)
		}

		dls, ok := file["downloads"].([]any)
		if !ok || len(dls) == 0 {
			t.Fatalf("%s: no downloads", path)
		}
		for _, d := range dls {
			if s, _ := d.(string); !strings.HasPrefix(s, "https://cdn.modrinth.com/") {
				t.Errorf("%s: download %v is not on the Modrinth CDN", path, d)
			}
		}
		if n, _ := file["fileSize"].(float64); n <= 0 {
			t.Errorf("%s: fileSize = %v", path, file["fileSize"])
		}
	}

	// Sodium is client-required and server-unsupported; both sides must survive the round trip.
	for _, f := range files {
		file := f.(map[string]any)
		if file["path"] == "mods/sodium.jar" {
			e := file["env"].(map[string]any)
			if e["client"] != "required" || e["server"] != "unsupported" {
				t.Errorf("sodium env = %v, want client=required server=unsupported", e)
			}
		}
	}
}

// The licence-safe property, stated as a test: a Modrinth jar is REFERENCED, never copied. Only the
// user's own upload rides along, and the server-only mod appears nowhere at all.
func TestWriteMrpackOverrides(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMrpack(&buf, testServer(), jarDir(t), fakeFetch(t)); err != nil {
		t.Fatal(err)
	}
	entries := readZip(t, buf.Bytes())

	want := map[string]bool{
		"modrinth.index.json":              true,
		"overrides/servers.dat":            true,
		"overrides/mods/" + uploadName:     true,
		"overrides/mods/sodium.jar":        false, // referenced by URL, so its bytes must NOT be here
		"overrides/mods/fabric-api.jar":    false,
		"overrides/mods/chunky.jar":        false, // server-only: excluded from the client artifact
		".minecraft/servers.dat":           false, // overrides/ IS the .minecraft root, not a parent
		"overrides/.minecraft/servers.dat": false,
	}
	for path, present := range want {
		if _, ok := entries[path]; ok != present {
			t.Errorf("entry %q present = %v, want %v", path, ok, present)
		}
	}
	if got := string(entries["overrides/mods/"+uploadName]); got != "HOMEBREW-JAR-BYTES" {
		t.Errorf("the bundled upload is %q", got)
	}

	// The index for the server-only mod must be absent too, not merely its bytes.
	if bytes.Contains(entries["modrinth.index.json"], []byte("chunky")) {
		t.Error("the server-only mod is referenced in modrinth.index.json")
	}

	// The preset connection, at the .minecraft root that overrides/ is.
	var dat bytes.Buffer
	if err := WriteServersDat(&dat, "Creative Sandbox", "creative.example.org:25566"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(entries["overrides/servers.dat"], dat.Bytes()) {
		t.Error("overrides/servers.dat does not hold the server's own name and address")
	}
}

func TestWriteMrpackNeoforge(t *testing.T) {
	srv := testServer()
	srv.Loader, srv.LoaderVersion = "neoforge", "21.1.235"
	srv.Mods = nil

	var buf bytes.Buffer
	if err := WriteMrpack(&buf, srv, jarDir(t), fakeFetch(t)); err != nil {
		t.Fatalf("WriteMrpack: %v", err)
	}
	idx := mrpackIndex(t, readZip(t, buf.Bytes()))
	deps := idx["dependencies"].(map[string]any)
	if deps["neoforge"] != "21.1.235" {
		t.Errorf("dependencies = %v, want neoforge 21.1.235", deps)
	}
	if _, ok := deps["fabric-loader"]; ok {
		t.Error("a neoforge pack declares fabric-loader")
	}
	// files must still marshal as [], never null: the launcher iterates it.
	if _, ok := idx["files"].([]any); !ok {
		t.Errorf("files = %v, want an empty array", idx["files"])
	}
}

// A Modrinth mod hosuto cannot fully describe cannot be referenced — and must not quietly become a
// bundled jar, which is the one thing this format exists to avoid.
func TestWriteMrpackFailsClosed(t *testing.T) {
	tests := map[string]func(*store.Mod){
		"no sha512": func(m *store.Mod) { m.SHA512 = "" },
		"no sha1":   func(m *store.Mod) { m.SHA1 = "" },
		"no url":    func(m *store.Mod) { m.URL = "" },
		"foreign url": func(m *store.Mod) {
			m.URL = "https://cdn.evil.example.com/data/x/sodium.jar"
		},
		"lookalike host": func(m *store.Mod) {
			m.URL = "https://cdn.modrinth.com.evil.example/x/sodium.jar"
		},
		"plaintext cdn": func(m *store.Mod) {
			m.URL = "http://cdn.modrinth.com/data/x/sodium.jar"
		},
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			srv := testServer()
			mods := testMods()
			break_(&mods[0]) // Sodium
			srv.Mods = mods
			if err := WriteMrpack(io.Discard, srv, jarDir(t), fakeFetch(t)); err == nil {
				t.Errorf("wrote a pack whose Sodium entry has %s", name)
			}
		})
	}
}
