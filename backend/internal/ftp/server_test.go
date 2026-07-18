package ftp

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// A real FTP conversation, end to end. The parser tests cover the strings; this covers the part that
// actually breaks against a live host — the ORDER of things. Open the data connection, send the
// command, read the 1xx, drain the data connection, close it, and only then read the 226. Getting
// that sequence wrong produces a client that works against nothing, and no amount of unit-testing
// parseLIST would show it.
type fakeFTP struct {
	t     *testing.T
	files map[string]string // absolute remote path → contents
	ln    net.Listener

	mu       sync.Mutex
	useMLSD  bool
	noEPSV   bool
	authFail bool
	wg       sync.WaitGroup
}

func newFakeFTP(t *testing.T, files map[string]string) *fakeFTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFTP{t: t, files: files, ln: ln, useMLSD: true}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeFTP) config() Config {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	var p int
	fmt.Sscanf(port, "%d", &p)
	return Config{Host: host, Port: p, User: "mc", Pass: "secret"}
}

// children returns the direct entries of a directory in the fake tree.
func (f *fakeFTP) children(dir string) (dirs, files []string) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	seenDir := map[string]bool{}
	for p := range f.files {
		if !strings.HasPrefix(p, dir) {
			continue
		}
		rest := strings.TrimPrefix(p, dir)
		if head, _, nested := strings.Cut(rest, "/"); nested {
			if !seenDir[head] {
				seenDir[head] = true
				dirs = append(dirs, head)
			}
		} else {
			files = append(files, rest)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	return dirs, files
}

func (f *fakeFTP) serve(c net.Conn) {
	defer c.Close()
	fmt.Fprint(c, "220 fake ftp ready\r\n")

	buf := make([]byte, 4096)
	var pending net.Listener
	defer func() {
		if pending != nil {
			pending.Close()
		}
	}()

	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		for _, line := range strings.Split(strings.TrimRight(string(buf[:n]), "\r\n"), "\r\n") {
			verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch strings.ToUpper(verb) {
			case "USER":
				if f.authFail {
					fmt.Fprint(c, "530 Login incorrect\r\n")
					continue
				}
				fmt.Fprint(c, "331 password please\r\n")
			case "PASS":
				fmt.Fprint(c, "230 logged in\r\n")
			case "TYPE":
				fmt.Fprint(c, "200 binary\r\n")
			case "FEAT":
				// Multi-line, exactly as RFC 959 frames it — the shape textproto has to unpick.
				if f.useMLSD {
					fmt.Fprint(c, "211-Features:\r\n MLSD\r\n SIZE\r\n211 End\r\n")
				} else {
					fmt.Fprint(c, "211-Features:\r\n SIZE\r\n211 End\r\n")
				}
			case "EPSV":
				if f.noEPSV {
					fmt.Fprint(c, "500 not understood\r\n")
					continue
				}
				ln, port := f.openData()
				pending = ln
				fmt.Fprintf(c, "229 Entering Extended Passive Mode (|||%d|)\r\n", port)
			case "PASV":
				ln, port := f.openData()
				pending = ln
				fmt.Fprintf(c, "227 Entering Passive Mode (127,0,0,1,%d,%d)\r\n", port>>8, port&0xff)
			case "MLSD", "LIST":
				fmt.Fprint(c, "150 here it comes\r\n")
				f.writeListing(pending, arg, strings.ToUpper(verb) == "MLSD")
				pending = nil
				fmt.Fprint(c, "226 done\r\n")
			case "RETR":
				body, ok := f.files[arg]
				if !ok {
					fmt.Fprint(c, "550 no such file\r\n")
					continue
				}
				fmt.Fprint(c, "150 sending\r\n")
				if dc, err := pending.Accept(); err == nil {
					dc.Write([]byte(body))
					dc.Close()
				}
				pending.Close()
				pending = nil
				fmt.Fprint(c, "226 transfer complete\r\n")
			case "QUIT":
				fmt.Fprint(c, "221 bye\r\n")
				return
			default:
				fmt.Fprint(c, "502 not implemented\r\n")
			}
		}
	}
}

func (f *fakeFTP) openData() (net.Listener, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(p, "%d", &port)
	return ln, port
}

func (f *fakeFTP) writeListing(ln net.Listener, dir string, mlsd bool) {
	if ln == nil {
		return
	}
	defer ln.Close()
	dc, err := ln.Accept()
	if err != nil {
		return
	}
	defer dc.Close()
	if dir == "" {
		dir = "/"
	}
	dirs, files := f.children(dir)
	for _, d := range dirs {
		if mlsd {
			fmt.Fprintf(dc, "type=dir;modify=20240101120000; %s\r\n", d)
		} else {
			fmt.Fprintf(dc, "drwxr-xr-x 2 mc mc 4096 Jan  1 12:00 %s\r\n", d)
		}
	}
	for _, name := range files {
		size := len(f.files[path.Join(dir, name)])
		if mlsd {
			fmt.Fprintf(dc, "type=file;size=%d;modify=20240101120000; %s\r\n", size, name)
		} else {
			fmt.Fprintf(dc, "-rw-r--r-- 1 mc mc %d Jan  1 12:00 %s\r\n", size, name)
		}
	}
}

// ── the tests ─────────────────────────────────────────────────────────────────────────

var tree = map[string]string{
	"/server.properties":      "level-name=world\nwhite-list=false\n",
	"/whitelist.json":         `[{"uuid":"11111111-2222-3333-4444-555555555555","name":"Ada"}]`,
	"/mods/sodium.jar":        "jar bytes here",
	"/mods/my mod (1).jar":    "another jar",
	"/world/level.dat":        "nbt",
	"/world/region/r.0.0.mca": "region bytes",
	"/logs/latest.log":        "noise",
}

func TestDialListAndRetrieve(t *testing.T) {
	f := newFakeFTP(t, tree)
	c, err := Dial(context.Background(), f.config())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	entries, err := c.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = e.Dir
	}
	for name, wantDir := range map[string]bool{
		"server.properties": false, "whitelist.json": false,
		"mods": true, "world": true, "logs": true,
	} {
		d, ok := got[name]
		if !ok {
			t.Fatalf("List missed %q — got %v", name, got)
		}
		if d != wantDir {
			t.Fatalf("%q dir=%v, want %v", name, d, wantDir)
		}
	}

	// A file with spaces in its name, which is the one that breaks a naive listing parser.
	dst := filepath.Join(t.TempDir(), "my mod (1).jar")
	var progress int64
	n, err := c.Retrieve(context.Background(), "/mods/my mod (1).jar", dst, func(b int64) { progress += b })
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "another jar" {
		t.Fatalf("got %q, want %q", body, "another jar")
	}
	if n != int64(len(body)) || progress != n {
		t.Fatalf("n=%d progress=%d, want %d", n, progress, len(body))
	}
}

// Two commands in a row must both work. A client that leaves an unread reply on the control channel
// passes its first command and fails its second, which is the classic way this goes wrong.
func TestSequentialCommands(t *testing.T) {
	f := newFakeFTP(t, tree)
	c, err := Dial(context.Background(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for _, dir := range []string{"/", "/mods", "/world", "/world/region", "/"} {
		if _, err := c.List(context.Background(), dir); err != nil {
			t.Fatalf("List(%q): %v", dir, err)
		}
	}
	tmp := t.TempDir()
	for _, p := range []string{"/server.properties", "/mods/sodium.jar", "/world/level.dat"} {
		if _, err := c.Retrieve(context.Background(), p, filepath.Join(tmp, path.Base(p)), nil); err != nil {
			t.Fatalf("Retrieve(%q): %v", p, err)
		}
	}
}

func TestWalkFindsEveryFile(t *testing.T) {
	f := newFakeFTP(t, tree)
	c, err := Dial(context.Background(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	seen := map[string]int64{}
	if err := c.Walk(context.Background(), "/", WalkLimits{}, func(e Entry) error {
		seen[e.Path] = e.Size
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != len(tree) {
		t.Fatalf("walked %d files, want %d: %v", len(seen), len(tree), seen)
	}
	for p, body := range tree {
		if seen[p] != int64(len(body)) {
			t.Fatalf("%s size %d, want %d", p, seen[p], len(body))
		}
	}
}

// A server that advertises no MLSD and refuses EPSV — the older, dumber end of what is out there.
// Both fallbacks have to work or a migration off an old host fails at the listing.
func TestFallsBackToListAndPASV(t *testing.T) {
	f := newFakeFTP(t, tree)
	f.useMLSD = false
	f.noEPSV = true

	c, err := Dial(context.Background(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	seen := 0
	if err := c.Walk(context.Background(), "/", WalkLimits{}, func(Entry) error {
		seen++
		return nil
	}); err != nil {
		t.Fatalf("Walk over LIST/PASV: %v", err)
	}
	if seen != len(tree) {
		t.Fatalf("walked %d files over LIST/PASV, want %d", seen, len(tree))
	}
}

// Bad credentials must be distinguishable from an unreachable host: they send the operator to
// completely different places, and "could not connect" for a wrong password is a wasted afternoon.
func TestAuthFailureIsDistinct(t *testing.T) {
	f := newFakeFTP(t, tree)
	f.authFail = true

	_, err := Dial(context.Background(), f.config())
	if err == nil {
		t.Fatal("a rejected login was accepted")
	}
	if !strings.Contains(err.Error(), "ftp: login rejected") {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestWalkHonoursCancellation(t *testing.T) {
	f := newFakeFTP(t, tree)
	c, err := Dial(context.Background(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Walk(ctx, "/", WalkLimits{}, func(Entry) error { return nil }); err == nil {
		t.Fatal("a cancelled walk kept going")
	}
}
