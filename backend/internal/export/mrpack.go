package export

import (
	"fmt"
	"io"

	"hosuto/internal/store"
)

// mrIndex is modrinth.index.json, the whole of a .mrpack's meaning. Every field below is load-
// bearing, and the failure mode of getting one wrong is not an error message — it is an import that
// appears to succeed and produces a broken instance. The shapes are what Prism's
// ModrinthInstanceCreationTask actually reads.
type mrIndex struct {
	FormatVersion int      `json:"formatVersion"` // must be exactly 1
	Game          string   `json:"game"`          // must be exactly "minecraft"
	VersionID     string   `json:"versionId"`
	Name          string   `json:"name"`
	Files         []mrFile `json:"files"`
	// Dependencies keys are a CLOSED set: minecraft | fabric-loader | quilt-loader | forge |
	// neoforge. Anything else throws "Unknown dependency type" and the import dies. Built only by
	// loaderDependency, which cannot produce another key.
	Dependencies map[string]string `json:"dependencies"`
}

// mrFile is one file the launcher will fetch for itself. This is the licence-safe core of the
// format: hosuto names the jar, its hashes and where Modrinth serves it, and ships none of it.
type mrFile struct {
	Path      string   `json:"path"`   // relative to the .minecraft root, e.g. "mods/sodium.jar"
	Hashes    mrHashes `json:"hashes"` //
	Env       mrEnv    `json:"env"`    //
	Downloads []string `json:"downloads"`
	FileSize  int64    `json:"fileSize"`
}

// mrHashes is a struct rather than a map so that sha512 cannot go missing: Prism reads it with
// requireString(), i.e. an entry without one fails the import outright.
type mrHashes struct {
	SHA1   string `json:"sha1"`
	SHA512 string `json:"sha512"`
}

// mrEnv is likewise a struct so that "client" cannot go missing — and here the consequence is worse
// than a failed import. Prism reads it as env["client"].toString("unsupported"): a file with no
// client key defaults to unsupported and is SILENTLY SKIPPED on the client. The pack imports, the
// instance launches, and the mod the player wanted is simply not there.
type mrEnv struct {
	Client string `json:"client"`
	Server string `json:"server"`
}

// WriteMrpack streams a Modrinth ".mrpack" — the format Prism, the Modrinth App and ATLauncher all
// import, and the artifact hosuto should offer first.
//
// It is the licence-safe one. A Modrinth mod is REFERENCED (CDN URL + sha1 + sha512) and the
// launcher downloads it from Modrinth itself, exactly as the player would have; hosuto redistributes
// nothing it was not given. Consequently a Modrinth mod whose URL or hashes hosuto never recorded is
// a hard error, not a quiet fallback to bundling the bytes: bundling would defeat the only reason
// this format exists.
//
// The only jars that ride along are the user's own uploads, which by definition Modrinth cannot
// serve, and which the user themselves put on the server. They go in overrides/, which IS the
// .minecraft root — not a parent of it:
//
//	overrides/servers.dat      the preset connection
//	overrides/mods/<file>.jar  the uploads, and nothing else
func WriteMrpack(w io.Writer, srv store.Server, jarDir string, fetch Fetcher) error {
	if err := checkClientExport(srv); err != nil {
		return err
	}
	loaderKey, err := loaderDependency(srv)
	if err != nil {
		return err
	}

	client := clientMods(srv.Mods)
	names, err := jarNames(client)
	if err != nil {
		return err
	}

	idx := mrIndex{
		FormatVersion: 1,
		Game:          "minecraft",
		// The pack's own version, not the game's. Slug + game version is stable across re-exports
		// of the same server and changes when the thing it pins changes, which is what a launcher
		// showing "you have version X" wants.
		VersionID: srv.Slug + "-" + srv.MCVersion,
		Name:      packName(srv),
		Files:     []mrFile{}, // never null: the launcher iterates it
		Dependencies: map[string]string{
			"minecraft": srv.MCVersion,
			loaderKey:   srv.LoaderVersion,
		},
	}

	// Uploads cannot be referenced — nobody but hosuto has them — so their bytes go into overrides/.
	var bundled []store.Mod
	var bundledNames []string
	for i, m := range client {
		switch m.Source {
		case "modrinth":
			f, err := mrpackFile(m, names[i])
			if err != nil {
				return err
			}
			idx.Files = append(idx.Files, f)
		case "upload":
			bundled = append(bundled, m)
			bundledNames = append(bundledNames, names[i])
		default:
			return fmt.Errorf("mod %q has an unknown source %q", m.Name, m.Source)
		}
	}

	zw := newZipWriter(w)
	if err := addJSON(zw, "modrinth.index.json", idx); err != nil {
		return err
	}
	if err := addServersDat(zw, "overrides/servers.dat", srv); err != nil {
		return err
	}
	for i, m := range bundled {
		if err := copyJar(zw, "overrides/mods/"+bundledNames[i], m, jarDir, fetch); err != nil {
			return err
		}
	}
	return zw.Close()
}

// mrpackFile turns a Modrinth mod into an index entry — a reference, never a copy.
//
// Every missing field here is fatal by design. No sha512: Prism's requireString() rejects the pack.
// No URL: there is nothing to reference. Not a Modrinth URL: hosuto would be telling the player's
// launcher to download, unattended, from a host hosuto does not vouch for. In each case the honest
// answer is to fail and tell the user, because the alternative — shipping the jar in overrides/ —
// silently turns the licence-safe artifact into a redistributing one.
func mrpackFile(m store.Mod, name string) (mrFile, error) {
	switch {
	case m.URL == "":
		return mrFile{}, fmt.Errorf("mod %q has no download URL and cannot be referenced in a .mrpack", m.Name)
	case m.SHA1 == "":
		return mrFile{}, fmt.Errorf("mod %q has no sha1 recorded", m.Name)
	case m.SHA512 == "":
		return mrFile{}, fmt.Errorf("mod %q has no sha512 recorded, which a .mrpack requires", m.Name)
	}
	if err := checkDownloadURL(m); err != nil {
		return mrFile{}, err
	}
	return mrFile{
		Path:      "mods/" + name,
		Hashes:    mrHashes{SHA1: m.SHA1, SHA512: m.SHA512},
		Env:       mrEnv{Client: env(m.ClientSide), Server: env(m.ServerSide)},
		Downloads: []string{m.URL},
		FileSize:  m.Size,
	}, nil
}

// loaderDependency maps hosuto's loader onto the one legal dependency key that names it. It is the
// only thing that ever writes a key into mrIndex.Dependencies besides "minecraft", which is how the
// closed key set stays closed. checkClientExport has already ruled out vanilla and paper; the
// default arm is there so that a new loader in the store cannot silently produce an invalid pack.
func loaderDependency(srv store.Server) (string, error) {
	switch srv.Loader {
	case "fabric":
		return "fabric-loader", nil
	case "neoforge":
		return "neoforge", nil
	default:
		return "", fmt.Errorf("loader %q has no .mrpack dependency key", srv.Loader)
	}
}
