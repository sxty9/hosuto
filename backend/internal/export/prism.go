package export

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"hosuto/internal/store"
)

// mmcPack is mmc-pack.json: which components Prism must resolve for this instance.
//
// The list is deliberately MINIMAL — Minecraft and the loader, nothing else. Prism's meta service
// knows that fabric-loader 0.16.14 wants a particular intermediary, and which LWJGL a 1.21 client
// needs; pinning those here would only let them go stale and would break the instance on a Prism
// that disagrees. State the two facts hosuto actually knows and let Prism resolve the rest.
type mmcPack struct {
	FormatVersion int            `json:"formatVersion"`
	Components    []mmcComponent `json:"components"`
}

type mmcComponent struct {
	UID     string `json:"uid"`
	Version string `json:"version"`
	// Important marks the component the instance is "about" — it is what Prism shows as the
	// instance's version and what it refuses to silently change.
	Important bool `json:"important,omitempty"`
}

// WritePrismZip streams a Prism Launcher instance: the one-click artifact. The player imports the
// zip, hits Launch, and lands on the server — Prism resolves the loader, installs the mods that
// rode along, and JoinServerOnLaunch dials the address without them typing it.
//
// Honest about what it is NOT: this is not an offline bundle. Prism still reaches the network on
// first launch to fetch its meta index, Minecraft's libraries and the asset objects — everything
// hosuto cannot ship and would have no right to. What the zip saves the player is the configuring,
// not the downloading.
//
// The layout is what InstanceImportTask::processZipPack looks for:
//
//	instance.cfg        the INI Prism matches by BASENAME at any depth; at the root, so the zip
//	                    extracts whole rather than having a wrapper directory stripped
//	mmc-pack.json       the components above
//	.minecraft/         the game directory itself
//
// Unlike the .mrpack this bundles every client jar, Modrinth-sourced ones included: a Prism instance
// has no manifest of things to download later, so the bytes are the only way in.
func WritePrismZip(w io.Writer, srv store.Server, jarDir string, fetch Fetcher) error {
	if err := checkClientExport(srv); err != nil {
		return err
	}
	loader, err := prismLoader(srv)
	if err != nil {
		return err
	}

	client := clientMods(srv.Mods)
	names, err := jarNames(client)
	if err != nil {
		return err
	}

	pack := mmcPack{
		FormatVersion: 1,
		Components: []mmcComponent{
			{UID: "net.minecraft", Version: srv.MCVersion, Important: true},
			{UID: loader, Version: srv.LoaderVersion},
		},
	}

	zw := newZipWriter(w)
	if err := addStream(zw, "instance.cfg", strings.NewReader(instanceCfg(srv))); err != nil {
		return err
	}
	if err := addJSON(zw, "mmc-pack.json", pack); err != nil {
		return err
	}
	if err := addServersDat(zw, ".minecraft/servers.dat", srv); err != nil {
		return err
	}
	for i, m := range client {
		if err := copyJar(zw, ".minecraft/mods/"+names[i], m, jarDir, fetch); err != nil {
			return err
		}
	}
	return zw.Close()
}

// instanceCfg is Prism's per-instance INI: flat key=value, no section header. Only the keys hosuto
// has a reason to set are written — Prism supplies the rest of its defaults, and a key hosuto does
// not understand is a key it cannot keep correct.
//
// JoinServerOnLaunch/JoinServerOnLaunchAddress are the real names (MinecraftInstance.cpp:245-247);
// they are what turns "import this and launch it" into "you are on the server".
func instanceCfg(srv store.Server) string {
	var b strings.Builder
	// A value runs to the end of the line, so a newline inside one would inject a key. The server
	// name is user-supplied; flatten it.
	write := func(k, v string) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, v))
		b.WriteByte('\n')
	}
	write("InstanceType", "OneSix")
	write("name", packName(srv))
	write("JoinServerOnLaunch", "true")
	write("JoinServerOnLaunchAddress", JoinAddress(srv))
	// The client's heap, not the server's: see clientMinHeapMB.
	write("OverrideMemory", "true")
	write("MinMemAlloc", strconv.Itoa(clientMinHeapMB))
	write("MaxMemAlloc", strconv.Itoa(clientMaxHeapMB))
	return b.String()
}

// prismLoader maps hosuto's loader onto Prism's component uid. These are the ids of Prism's own
// meta service; a uid it does not know leaves the instance unresolvable.
func prismLoader(srv store.Server) (string, error) {
	switch srv.Loader {
	case "fabric":
		return "net.fabricmc.fabric-loader", nil
	case "neoforge":
		return "net.neoforged", nil
	default:
		return "", fmt.Errorf("loader %q has no Prism component", srv.Loader)
	}
}
