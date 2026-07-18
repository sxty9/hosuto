package ftp

import "testing"

func TestParsePASV(t *testing.T) {
	host, port, ok := parsePASV("227 Entering Passive Mode (10,0,0,1,200,55).")
	if !ok || host != "10.0.0.1" || port != 200<<8|55 {
		t.Fatalf("got %q:%d ok=%v", host, port, ok)
	}
	if _, _, ok := parsePASV("227 Entering Passive Mode"); ok {
		t.Fatal("a reply with no address was accepted")
	}
	if _, _, ok := parsePASV("227 (1,2,3,4,5)"); ok {
		t.Fatal("a five-field reply was accepted")
	}
	if _, _, ok := parsePASV("227 (1,2,3,999,5,6)"); ok {
		t.Fatal("an out-of-range octet was accepted")
	}
}

func TestParseEPSV(t *testing.T) {
	p, ok := parseEPSV("229 Entering Extended Passive Mode (|||6446|)")
	if !ok || p != 6446 {
		t.Fatalf("got %d ok=%v, want 6446", p, ok)
	}
	// The separator is whatever the server picked; it is not always '|'.
	if p, ok := parseEPSV("229 Entering Extended Passive Mode (!!!1234!)"); !ok || p != 1234 {
		t.Fatalf("got %d ok=%v, want 1234", p, ok)
	}
	if _, ok := parseEPSV("229 no parens here"); ok {
		t.Fatal("a malformed reply was accepted")
	}
}

func TestParseMLSD(t *testing.T) {
	e, ok := parseMLSD("type=file;size=1234;modify=20240101120000; server.properties")
	if !ok || e.Dir || e.Size != 1234 || e.Name != "server.properties" {
		t.Fatalf("got %+v ok=%v", e, ok)
	}
	if e.MTime.Year() != 2024 {
		t.Fatalf("MTime = %v", e.MTime)
	}

	d, ok := parseMLSD("type=dir;modify=20240101120000; mods")
	if !ok || !d.Dir || d.Name != "mods" {
		t.Fatalf("got %+v ok=%v", d, ok)
	}

	// A name with spaces must survive intact.
	s, ok := parseMLSD("type=file;size=10; my mod (1).jar")
	if !ok || s.Name != "my mod (1).jar" {
		t.Fatalf("Name = %q", s.Name)
	}

	// The directory itself, its parent, and a symlink are all things we must not copy.
	for _, line := range []string{
		"type=cdir; .",
		"type=pdir; ..",
		"type=OS.unix=slink:/etc/passwd; sneaky",
	} {
		if _, ok := parseMLSD(line); ok {
			t.Fatalf("%q was accepted", line)
		}
	}
}

func TestParseLIST(t *testing.T) {
	e, ok := parseLIST("-rw-r--r--   1 mc  mc     1234 Jan  1 12:00 server.properties")
	if !ok || e.Dir || e.Size != 1234 || e.Name != "server.properties" {
		t.Fatalf("got %+v ok=%v", e, ok)
	}

	d, ok := parseLIST("drwxr-xr-x   2 mc  mc     4096 Jan  1 12:00 mods")
	if !ok || !d.Dir || d.Name != "mods" {
		t.Fatalf("got %+v ok=%v", d, ok)
	}

	// A filename with spaces: the name is everything past the eighth field, not one split token.
	s, ok := parseLIST("-rw-r--r--   1 mc  mc       10 Jan  1 12:00 my mod (1).jar")
	if !ok || s.Name != "my mod (1).jar" {
		t.Fatalf("Name = %q, want the spaces preserved", s.Name)
	}

	// A year in place of the time, which `ls` prints for older files.
	y, ok := parseLIST("-rw-r--r--   1 mc  mc       10 Jan  1  2023 old.jar")
	if !ok || y.Name != "old.jar" {
		t.Fatalf("got %+v ok=%v", y, ok)
	}
}

// Following a symlink would copy from wherever it points, which is not the server being migrated.
// Devices, sockets and the "total 40" header are equally not files to fetch.
func TestParseLISTRefusesNonFiles(t *testing.T) {
	for _, line := range []string{
		"lrwxrwxrwx 1 mc mc 11 Jan  1 12:00 sneaky -> /etc/passwd",
		"crw-rw-rw- 1 root root 1, 3 Jan  1 12:00 null",
		"srwxrwxrwx 1 mc mc 0 Jan  1 12:00 sock",
		"total 40",
		"",
	} {
		if e, ok := parseLIST(line); ok {
			t.Fatalf("%q was accepted as %+v", line, e)
		}
	}
}

// A path is interpolated into a control-channel command, so a newline in it would inject a second
// command onto the connection.
func TestQuoteArgStripsControlCharacters(t *testing.T) {
	if got := quoteArg("mods\r\nDELE /world/level.dat"); got != "modsDELE /world/level.dat" {
		t.Fatalf("quoteArg = %q — a command could be injected", got)
	}
}
