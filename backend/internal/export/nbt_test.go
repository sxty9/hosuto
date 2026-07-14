package export

import (
	"bytes"
	"strings"
	"testing"
)

// The highest-value test in the package: servers.dat, byte for byte, against bytes computed by hand
// from the NBT grammar. Every other test can only tell you the file is THERE. Minecraft will not
// tell you it is wrong — it drops a malformed server list silently — so this is the only place the
// encoding is actually checked, and it is written out longhand on purpose.
//
//	name = "Test"
//	ip   = "mc.example.com:25565"   (20 = 0x14 bytes)
func TestWriteServersDatBytes(t *testing.T) {
	want := []byte{
		// ── root: a TAG_Compound with an EMPTY name ──
		0x0A,       // id 10, TAG_Compound
		0x00, 0x00, // name length 0 — the root is unnamed

		// ── TAG_List "servers" ──
		0x09,       // id 9, TAG_List
		0x00, 0x07, // name length 7
		's', 'e', 'r', 'v', 'e', 'r', 's',
		0x0A,                   // element type: TAG_Compound
		0x00, 0x00, 0x00, 0x01, // i32 count: one entry

		// ── entry 0: a bare compound payload — no id, no name of its own ──
		0x08,       // id 8, TAG_String
		0x00, 0x04, // name length 4
		'n', 'a', 'm', 'e',
		0x00, 0x04, // payload length 4
		'T', 'e', 's', 't',

		0x08,       // id 8, TAG_String
		0x00, 0x02, // name length 2
		'i', 'p',
		0x00, 0x14, // payload length 20
		'm', 'c', '.', 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm', ':', '2', '5', '5', '6', '5',

		0x00, // TAG_End — closes the entry
		0x00, // TAG_End — closes the root compound
	}

	var got bytes.Buffer
	if err := WriteServersDat(&got, "Test", "mc.example.com:25565"); err != nil {
		t.Fatalf("WriteServersDat: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("servers.dat mismatch\ngot  (%d bytes): % x\nwant (%d bytes): % x",
			got.Len(), got.Bytes(), len(want), want)
	}
}

// servers.dat is the one Minecraft data file that is NOT gzipped. A gzip header here would be
// accepted by nothing and reported by no one: the client just shows an empty server list.
func TestWriteServersDatIsNotCompressed(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteServersDat(&buf, "Test", "example.org"); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if b[0] == 0x1F && b[1] == 0x8B {
		t.Fatal("servers.dat is gzipped; it must be raw NBT")
	}
	if b[0] != 0x0A {
		t.Errorf("first byte is %#02x, want %#02x (TAG_Compound)", b[0], 0x0A)
	}
}

// Plain UTF-8 is only byte-identical to NBT's modified UTF-8 for the strings accepted here. The two
// exceptions are rejected rather than mis-encoded — see WriteServersDat.
func TestWriteServersDatRejects(t *testing.T) {
	tests := []struct {
		why      string
		name, ip string
	}{
		{"empty name", "", "example.org"},
		{"empty ip", "Test", ""},
		{"NUL in name", "Te\x00st", "example.org"},
		{"NUL in ip", "Test", "exam\x00ple.org"},
		{"astral-plane rune in name", "Test \U0001F600", "example.org"}, // emoji: 4-byte UTF-8, surrogate pair in NBT
		{"astral-plane rune in ip", "Test", "\U0001F600.example.org"},
		{"invalid UTF-8 in name", "Te\xffst", "example.org"},
		{"name longer than the u16 length prefix", strings.Repeat("a", 65536), "example.org"},
	}
	for _, tc := range tests {
		t.Run(tc.why, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteServersDat(&buf, tc.name, tc.ip); err == nil {
				t.Fatalf("accepted %s, want an error", tc.why)
			}
			if buf.Len() != 0 {
				t.Errorf("wrote %d bytes before rejecting; validation must precede the first write", buf.Len())
			}
		})
	}
}

// A BMP rune is fine: its UTF-8 and its modified UTF-8 are the same bytes.
func TestWriteServersDatAcceptsBMP(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteServersDat(&buf, "Sören's Wörld ✓", "mc.example.org"); err != nil {
		t.Fatalf("rejected a BMP name: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Sören's Wörld ✓")) {
		t.Error("the name is not in the output verbatim")
	}
}
