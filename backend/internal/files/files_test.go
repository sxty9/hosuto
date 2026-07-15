package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mktree(t *testing.T) (*Tree, string) {
	t.Helper()
	root := t.TempDir()
	// A realistic little server tree.
	must(t, os.MkdirAll(filepath.Join(root, "mods"), 0o770))
	must(t, os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=hi\n"), 0o640))
	must(t, os.WriteFile(filepath.Join(root, "mods", "sodium.jar"), []byte("PK\x03\x04"), 0o640))
	tr, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return tr, root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestListAndStat(t *testing.T) {
	tr, _ := mktree(t)
	es, err := tr.List("server")
	if err != nil {
		t.Fatal(err)
	}
	// dirs first: mods, then server.properties
	if len(es) != 2 || es[0].Name != "mods" || es[0].Kind != "dir" {
		t.Fatalf("unexpected listing: %+v", es)
	}
	if es[1].Name != "server.properties" || es[1].Viewer != "text" || es[1].Path != "server/server.properties" {
		t.Fatalf("server.properties classified wrong: %+v", es[1])
	}
	sub, err := tr.List("server/mods")
	if err != nil || len(sub) != 1 || sub[0].Path != "server/mods/sodium.jar" {
		t.Fatalf("mods listing wrong: %+v %v", sub, err)
	}
}

// TestConfinement is the load-bearing test: every way out of the tree must be refused.
func TestConfinement(t *testing.T) {
	tr, root := mktree(t)

	// A secret living OUTSIDE the tree, standing in for /etc/holistic/jwt-secret.
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	must(t, os.WriteFile(outside, []byte("TOPSECRET"), 0o600))

	// 1. Lexical traversal must never resolve.
	for _, vp := range []string{
		"server/../secret.txt",
		"server/../../etc/passwd",
		"server/mods/../../secret.txt",
		"../secret.txt",
		"/etc/passwd",
		"other/mods",    // wrong root key
		"serverextra/x", // must not prefix-match the root key
	} {
		if _, err := tr.List(vp); err == nil {
			t.Errorf("List(%q) must be refused, got nil error", vp)
		}
		if _, err := tr.Stat(vp); err == nil {
			t.Errorf("Stat(%q) must be refused, got nil error", vp)
		}
	}

	// 2. A SYMLINK inside the tree pointing OUT must not be followable — this is the jwt-secret escape.
	link := filepath.Join(root, "escape")
	must(t, os.Symlink(outside, link))
	if _, _, err := tr.OpenFile("server/escape"); err == nil {
		t.Error("opening a symlink that escapes the tree must be refused")
	}
	if _, err := tr.ReadTextOr("server/escape"); err == nil {
		t.Error("reading a symlink that escapes the tree must be refused")
	}
	// The symlink must not even appear in a listing.
	es, _ := tr.List("server")
	for _, e := range es {
		if e.Name == "escape" {
			t.Error("a symlink must never be listed")
		}
	}

	// 3. A symlink to a DIRECTORY outside must not be traversable either.
	outDir := filepath.Join(filepath.Dir(root), "outdir")
	must(t, os.MkdirAll(outDir, 0o770))
	must(t, os.Symlink(outDir, filepath.Join(root, "outlink")))
	if _, err := tr.List("server/outlink"); err == nil {
		t.Error("listing through a directory symlink that escapes must be refused")
	}
}

// ReadTextOr is a tiny test shim so the confinement test can call the read path uniformly.
func (t *Tree) ReadTextOr(vpath string) (string, error) {
	s, _, err := t.ReadText(vpath, 1<<20)
	return s, err
}

func TestWriteOps(t *testing.T) {
	tr, _ := mktree(t)

	if err := tr.Mkdir("server", "config"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Mkdir("server", "config"); !errors.Is(err, ErrExists) {
		t.Errorf("mkdir of an existing dir should be ErrExists, got %v", err)
	}
	// Bad names refused.
	for _, n := range []string{"", ".", "..", "a/b", "x\x00y"} {
		if err := tr.Mkdir("server", n); err == nil {
			t.Errorf("Mkdir with bad name %q must fail", n)
		}
	}

	// Upload replaces even a group-unwritable file via temp+rename.
	if err := tr.Save("server", "server.properties", strings.NewReader("motd=changed\n")); err != nil {
		t.Fatalf("upload/replace failed: %v", err)
	}
	got, _, err := tr.ReadText("server/server.properties", 1<<20)
	if err != nil || got != "motd=changed\n" {
		t.Fatalf("replace didn't take: %q %v", got, err)
	}

	// Rename, move, copy, delete round-trip.
	if err := tr.Rename("server/config", "cfg"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Move("server/server.properties", "server/cfg"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Stat("server/cfg/server.properties"); err != nil {
		t.Fatalf("move target missing: %v", err)
	}
	if err := tr.Copy("server/cfg/server.properties", "server"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Stat("server/server.properties"); err != nil {
		t.Fatalf("copy target missing: %v", err)
	}
	if err := tr.Delete("server/cfg", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("deleting a dir non-recursively must be refused, got %v", err)
	}
	if err := tr.Delete("server/cfg", true); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Stat("server/cfg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cfg should be gone, got %v", err)
	}

	// The server root itself is not deletable or renameable.
	if err := tr.Delete("server", true); !errors.Is(err, ErrInvalid) {
		t.Errorf("deleting the root must be refused, got %v", err)
	}
	if err := tr.Rename("server", "evil"); !errors.Is(err, ErrInvalid) {
		t.Errorf("renaming the root must be refused, got %v", err)
	}
}
