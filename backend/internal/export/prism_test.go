package export

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// parseCfg reads instance.cfg the way Prism does: flat key=value lines.
func parseCfg(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("instance.cfg line %q is not key=value", line)
		}
		out[k] = v
	}
	return out
}

func TestWritePrismZip(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePrismZip(&buf, testServer(), jarDir(t), fakeFetch(t)); err != nil {
		t.Fatalf("WritePrismZip: %v", err)
	}
	entries := readZip(t, buf.Bytes())

	// The layout InstanceImportTask::processZipPack looks for: instance.cfg and mmc-pack.json at
	// the root, the game directory beside them.
	for _, path := range []string{"instance.cfg", "mmc-pack.json", ".minecraft/servers.dat"} {
		if _, ok := entries[path]; !ok {
			t.Errorf("missing %s", path)
		}
	}

	// ── instance.cfg ──
	cfg := parseCfg(t, entries["instance.cfg"])
	want := map[string]string{
		"InstanceType":              "OneSix",
		"name":                      "Creative Sandbox",
		"JoinServerOnLaunch":        "true",
		"JoinServerOnLaunchAddress": "creative.example.org:25566",
		"OverrideMemory":            "true",
		"MinMemAlloc":               "2048",
		"MaxMemAlloc":               "4096",
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("instance.cfg %s = %q, want %q", k, cfg[k], v)
		}
	}

	// ── mmc-pack.json: minimal, and Prism resolves the rest from its meta service ──
	var pack mmcPack
	if err := json.Unmarshal(entries["mmc-pack.json"], &pack); err != nil {
		t.Fatalf("mmc-pack.json is not valid JSON: %v", err)
	}
	if pack.FormatVersion != 1 {
		t.Errorf("formatVersion = %d, want 1", pack.FormatVersion)
	}
	if len(pack.Components) != 2 {
		t.Fatalf("got %d components %v, want exactly 2 (minecraft + loader)", len(pack.Components), pack.Components)
	}
	if got := pack.Components[0]; got.UID != "net.minecraft" || got.Version != "1.21.1" || !got.Important {
		t.Errorf("component[0] = %+v, want net.minecraft 1.21.1 important", got)
	}
	if got := pack.Components[1]; got.UID != "net.fabricmc.fabric-loader" || got.Version != "0.16.14" {
		t.Errorf("component[1] = %+v, want net.fabricmc.fabric-loader 0.16.14", got)
	}

	// ── the bundled jars: a Prism instance has no manifest, so every client jar rides along ──
	jars := map[string]string{
		".minecraft/mods/sodium.jar":     "SODIUM-JAR-BYTES",
		".minecraft/mods/fabric-api.jar": "FABRIC-API-JAR-BYTES",
		".minecraft/mods/" + uploadName:  "HOMEBREW-JAR-BYTES",
	}
	for path, body := range jars {
		if got, ok := entries[path]; !ok {
			t.Errorf("missing %s", path)
		} else if string(got) != body {
			t.Errorf("%s = %q, want %q", path, got, body)
		}
	}
	// ...but never the server-only one.
	if _, ok := entries[".minecraft/mods/chunky.jar"]; ok {
		t.Error("the server-only mod (client_side=unsupported) is in the Prism instance")
	}

	// The preset connection sits at the .minecraft root, where the client reads it.
	var dat bytes.Buffer
	if err := WriteServersDat(&dat, "Creative Sandbox", "creative.example.org:25566"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(entries[".minecraft/servers.dat"], dat.Bytes()) {
		t.Error(".minecraft/servers.dat does not hold the server's own name and address")
	}
}

func TestWritePrismZipNeoforge(t *testing.T) {
	srv := testServer()
	srv.Loader, srv.LoaderVersion, srv.Mods = "neoforge", "21.1.235", nil

	var buf bytes.Buffer
	if err := WritePrismZip(&buf, srv, jarDir(t), fakeFetch(t)); err != nil {
		t.Fatalf("WritePrismZip: %v", err)
	}
	var pack mmcPack
	if err := json.Unmarshal(readZip(t, buf.Bytes())["mmc-pack.json"], &pack); err != nil {
		t.Fatal(err)
	}
	if got := pack.Components[1]; got.UID != "net.neoforged" || got.Version != "21.1.235" {
		t.Errorf("loader component = %+v, want net.neoforged 21.1.235", got)
	}
}

// instance.cfg is line-oriented: a newline in the server's name would inject a key of the player's
// choosing (JoinServerOnLaunchAddress, say) into their launcher config.
func TestInstanceCfgFlattensTheName(t *testing.T) {
	srv := testServer()
	srv.Name = "Evil\nJoinServerOnLaunchAddress=attacker.example.com:25565"

	cfg := parseCfg(t, []byte(instanceCfg(srv)))
	if got := cfg["JoinServerOnLaunchAddress"]; got != "creative.example.org:25566" {
		t.Errorf("JoinServerOnLaunchAddress = %q — a newline in the name injected a key", got)
	}
	if strings.Contains(cfg["name"], "\n") {
		t.Error("the name still holds a newline")
	}
}
