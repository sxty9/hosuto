package export

import (
	"fmt"
	"io"

	"hosuto/internal/store"
)

// WriteModsZip streams a flat zip of the server's client-side mod jars — "just the mods", for the
// player who already has a launcher and an instance and only needs the right jars to drop into
// their own mods/ directory.
//
// It is the artifact that redistributes the most and guarantees the least: the player still has to
// match the loader and the Minecraft version themselves, and hosuto has to ship every byte. Offer
// the .mrpack first; this is the fallback for a launcher that cannot import one.
//
// The entries are flat (sodium.jar, not mods/sodium.jar) because the player is extracting them INTO
// mods/. Server-only mods are excluded — see clientMods — and an empty result is an error rather
// than an empty zip: a fabric server with no client mods has nothing to hand anyone, and saying so
// beats delivering a zip with nothing in it.
//
// The loader guard lives in the caller: this takes a mod set, not a server, so the API layer must
// check store.LoaderHasClientMods itself (WriteMrpack and WritePrismZip do it for themselves).
func WriteModsZip(w io.Writer, mods []store.Mod, jarDir string, fetch Fetcher) error {
	client := clientMods(mods)
	if len(client) == 0 {
		return fmt.Errorf("this server has no client-side mods to export")
	}
	names, err := jarNames(client)
	if err != nil {
		return err
	}

	zw := newZipWriter(w)
	for i, m := range client {
		if err := copyJar(zw, names[i], m, jarDir, fetch); err != nil {
			return err
		}
	}
	// Close writes the central directory. Without it the zip has all its bytes and is still not a
	// zip, so its error is the one that matters most here.
	return zw.Close()
}
