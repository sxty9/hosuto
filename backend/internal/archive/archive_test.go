package archive

import (
	"archive/zip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeZip builds a zip from a list of members. A nil body means a directory entry.
func writeZip(t *testing.T, members map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range members {
		if body == nil {
			if _, err := zw.Create(name + "/"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeZipRaw builds a zip with explicit headers, so a test can forge a mode (a symlink) that
// zip.Writer.Create would never produce on its own.
func writeZipRaw(t *testing.T, build func(*zip.Writer)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	build(zw)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The attack this package exists to stop: a member name that climbs out of the destination. Nothing
// may be written outside dest, and the entry must be REPORTED rather than dropped in silence.
func TestExtractRefusesZipSlip(t *testing.T) {
	dest := t.TempDir()
	outside := filepath.Join(dest, "..", "pwned.txt")

	src := writeZip(t, map[string][]byte{
		"../pwned.txt":         []byte("no"),
		"../../pwned.txt":      []byte("no"),
		"a/../../../pwned.txt": []byte("no"),
		`..\pwned.txt`:         []byte("no"), // windows separators must not slip past
		"server.properties":    []byte("motd=hi\n"),
	})

	res, err := Extract(src, dest, Limits{}, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("zip slip wrote outside the destination")
	}
	if res.Files != 1 {
		t.Fatalf("wrote %d files, want only the safe one", res.Files)
	}
	if len(res.Skipped) != 4 {
		t.Fatalf("skipped %v, want all four traversal entries reported", res.Skipped)
	}
}

func TestExtractRefusesAbsoluteAndDeviceNames(t *testing.T) {
	dest := t.TempDir()
	src := writeZip(t, map[string][]byte{
		"/etc/shadow":  []byte("no"),
		"C:/windows/x": []byte("no"),
		"ok.txt":       []byte("yes"),
	})
	res, err := Extract(src, dest, Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("wrote %d files, want 1", res.Files)
	}
	if _, err := os.Stat(filepath.Join(dest, "ok.txt")); err != nil {
		t.Fatalf("the safe entry did not land: %v", err)
	}
}

// A symlink in the archive must never be created. The server tree is writable by the run account and
// the daemon can read /etc/holistic — unpacking "mods -> /etc/holistic" then writing through it is
// precisely the escape files.Tree exists to prevent.
func TestExtractRefusesSymlinks(t *testing.T) {
	dest := t.TempDir()
	src := writeZipRaw(t, func(zw *zip.Writer) {
		hdr := &zip.FileHeader{Name: "evil-link"}
		hdr.SetMode(fs.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("/etc/holistic")); err != nil {
			t.Fatal(err)
		}
	})

	res, err := Extract(src, dest, Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Fatal("a symlink entry was written")
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped %v, want the symlink reported", res.Skipped)
	}
	if _, err := os.Lstat(filepath.Join(dest, "evil-link")); err == nil {
		t.Fatal("the symlink exists on disk")
	}
}

// The declared uncompressed size is a claim. The budget is enforced on the bytes that actually
// arrive, so a lying header runs into the same ceiling.
func TestExtractEnforcesSizeBudget(t *testing.T) {
	dest := t.TempDir()
	src := writeZip(t, map[string][]byte{"big.bin": make([]byte, 4096)})

	if _, err := Extract(src, dest, Limits{MaxFile: 1024, MaxTotal: 1 << 20}, nil); err == nil {
		t.Fatal("oversized member was accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "big.bin")); err == nil {
		t.Fatal("the oversized member was left behind on disk")
	}
}

func TestExtractEnforcesEntryCount(t *testing.T) {
	members := map[string][]byte{}
	for i := range 10 {
		members[string(rune('a'+i))+".txt"] = []byte("x")
	}
	if _, err := Extract(writeZip(t, members), t.TempDir(), Limits{MaxEntries: 3}, nil); err == nil {
		t.Fatal("entry count limit was not enforced")
	}
}

// "Zip up your server" produces both shapes. A folder-wrapped archive is unwrapped...
func TestExtractStripsWrapperDirectory(t *testing.T) {
	dest := t.TempDir()
	src := writeZip(t, map[string][]byte{
		"myserver":                   nil,
		"myserver/server.properties": []byte("motd=hi\n"),
		"myserver/mods/a.jar":        []byte("jar"),
	})
	res, err := Extract(src, dest, Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Root != "myserver" {
		t.Fatalf("Root = %q, want myserver", res.Root)
	}
	if _, err := os.Stat(filepath.Join(dest, "server.properties")); err != nil {
		t.Fatalf("server.properties was not unwrapped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "mods", "a.jar")); err != nil {
		t.Fatalf("mods/a.jar was not unwrapped: %v", err)
	}
}

// ...while a correctly-rooted archive is left exactly as it is. A "world" directory at the root is
// the trap here: it is a lone top-level directory, and stripping it would destroy the world.
func TestExtractKeepsCorrectlyRootedArchive(t *testing.T) {
	dest := t.TempDir()
	src := writeZip(t, map[string][]byte{
		"server.properties": []byte("motd=hi\n"),
		"world/level.dat":   []byte("nbt"),
	})
	res, err := Extract(src, dest, Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Root != "" {
		t.Fatalf("Root = %q, want no wrapper stripped", res.Root)
	}
	if _, err := os.Stat(filepath.Join(dest, "world", "level.dat")); err != nil {
		t.Fatalf("world/level.dat should have stayed put: %v", err)
	}
}

func TestExtractKeepsLoneWorldDirectory(t *testing.T) {
	dest := t.TempDir()
	src := writeZip(t, map[string][]byte{
		"world":                  nil,
		"world/level.dat":        []byte("nbt"),
		"world/region/r.0.0.mca": []byte("region"),
	})
	res, err := Extract(src, dest, Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Root != "" {
		t.Fatalf("Root = %q — stripping a lone world/ would destroy it", res.Root)
	}
	if _, err := os.Stat(filepath.Join(dest, "world", "level.dat")); err != nil {
		t.Fatal("world/level.dat was mangled")
	}
}

func TestExtractReportsProgress(t *testing.T) {
	src := writeZip(t, map[string][]byte{"a.txt": []byte("hello"), "b.txt": []byte("world!")})
	var seen int64
	res, err := Extract(src, t.TempDir(), Limits{}, func(n int64) { seen += n })
	if err != nil {
		t.Fatal(err)
	}
	if seen != res.Bytes || seen != 11 {
		t.Fatalf("progress reported %d bytes, result says %d, want 11", seen, res.Bytes)
	}
}

// ── writing ───────────────────────────────────────────────────────────────────────────

func TestCreateSkipsAndInjects(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "server.properties"), "rcon.password=secret\ndifficulty=hard\n")
	mustWrite(t, filepath.Join(src, "mods", "a.jar"), "jar")
	mustWrite(t, filepath.Join(src, "world", "level.dat"), "nbt")
	mustWrite(t, filepath.Join(src, "logs", "latest.log"), "noise")

	out := filepath.Join(t.TempDir(), "tpl.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	skip := func(rel string, d fs.DirEntry) bool {
		top, _, _ := strings.Cut(rel, "/")
		return top == "logs" || top == "world" || rel == "server.properties"
	}
	extra := map[string][]byte{"server.properties": []byte("difficulty=hard\n")}
	if err := Create(f, src, skip, extra); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dest := t.TempDir()
	if _, err := Extract(out, dest, Limits{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "logs")); err == nil {
		t.Fatal("logs/ was packed despite the skip")
	}
	if _, err := os.Stat(filepath.Join(dest, "world")); err == nil {
		t.Fatal("world/ was packed despite the skip")
	}
	if _, err := os.Stat(filepath.Join(dest, "mods", "a.jar")); err != nil {
		t.Fatal("mods/a.jar should have been packed")
	}
	// The injected copy must win over the real file — that is how the rcon password stays out.
	b, err := os.ReadFile(filepath.Join(dest, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("the rcon password leaked into the template: %q", b)
	}
}

// A symlink inside the tree must not be followed into the payload: it can point at anything the
// daemon can read.
func TestCreateSkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real.txt"), "fine")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "sneaky")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out := filepath.Join(t.TempDir(), "tpl.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(f, src, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, m := range zr.File {
		if m.Name == "sneaky" {
			t.Fatal("a symlink was packed into the template")
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
