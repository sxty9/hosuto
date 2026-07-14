package export

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// NBT tag ids. The full space is End=0, Byte=1, Short=2, Int=3, Long=4, Float=5, Double=6,
// ByteArray=7, String=8, List=9, Compound=10; servers.dat needs only these four.
//
// The grammar, in the three shapes used below:
//
//	named tag     = id | u16 name length | name bytes | payload
//	TAG_String    = u16 byte length | UTF-8 bytes
//	TAG_List      = element id | i32 count | element PAYLOADS (unnamed, no id of their own)
//
// Everything is big-endian.
const (
	tagEnd      byte = 0
	tagString   byte = 8
	tagList     byte = 9
	tagCompound byte = 10
)

// WriteServersDat streams a Minecraft servers.dat holding exactly one entry, so a player who drops
// it into their .minecraft/ finds the server already sitting in their multiplayer list.
//
// servers.dat is UNCOMPRESSED NBT. This is the one place Minecraft departs from its own habit:
// level.dat, playerdata and the region files are gzipped, servers.dat is not, and a gzipped
// servers.dat is not rejected — it is silently ignored, and the player sees an empty server list.
//
// Strings are written as plain UTF-8. Minecraft's NBT is specified as Java's "modified UTF-8",
// which differs from UTF-8 in exactly two places: a NUL is written as the two bytes C0 80 rather
// than 00, and a rune outside the BMP is written as a surrogate PAIR (two three-byte sequences)
// rather than one four-byte sequence. For every other string the two encodings are byte-identical.
// Rather than implement modified UTF-8 for inputs that cannot legitimately occur — a hostname or a
// server name containing a NUL or an astral-plane rune — those two cases are rejected outright: a
// subtly wrong servers.dat that the client half-parses is worse than an error the caller can put in
// front of the user.
func WriteServersDat(w io.Writer, name, ip string) error {
	if err := checkNBTString("name", name); err != nil {
		return err
	}
	if err := checkNBTString("ip", ip); err != nil {
		return err
	}

	e := &nbtWriter{w: w}

	// The root is a TAG_Compound with an EMPTY name. The empty name is not an oversight in the
	// format: the client opens the file, expects an unnamed root compound, and looks inside it for
	// the "servers" list.
	e.named(tagCompound, "")
	e.named(tagList, "servers")
	e.u8(tagCompound) // every element is a compound
	e.i32(1)          // exactly one of them

	// A list element is a bare compound PAYLOAD: no id byte and no name of its own, just its fields
	// followed by the TAG_End that closes it.
	e.named(tagString, "name")
	e.str(name)
	e.named(tagString, "ip")
	e.str(ip)
	e.u8(tagEnd) // closes the entry

	e.u8(tagEnd) // closes the root compound
	return e.err
}

// checkNBTString rejects the strings for which plain UTF-8 would not be modified UTF-8, plus the
// ones the u16 length prefix cannot describe. See WriteServersDat for why these two, and only
// these two.
func checkNBTString(field, s string) error {
	if s == "" {
		return fmt.Errorf("servers.dat: %s is empty", field)
	}
	if len(s) > math.MaxUint16 {
		return fmt.Errorf("servers.dat: %s is %d bytes, more than NBT's %d", field, len(s), math.MaxUint16)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("servers.dat: %s is not valid UTF-8", field)
	}
	for _, r := range s {
		if r == 0 {
			return fmt.Errorf("servers.dat: %s contains a NUL, which NBT encodes differently", field)
		}
		if r > 0xFFFF {
			return fmt.Errorf("servers.dat: %s contains %q, which is outside the BMP and NBT encodes differently", field, r)
		}
	}
	return nil
}

// nbtWriter writes NBT straight to the wire, holding the first error so the encoding above reads as
// the format does rather than as a ladder of error checks. Nothing is buffered: the caller's writer
// is usually a zip entry inside an HTTP response.
type nbtWriter struct {
	w   io.Writer
	err error
}

func (e *nbtWriter) write(b []byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write(b)
}

func (e *nbtWriter) u8(b byte) { e.write([]byte{b}) }

func (e *nbtWriter) u16(n uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], n)
	e.write(b[:])
}

func (e *nbtWriter) i32(n int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	e.write(b[:])
}

// str writes a TAG_String payload: the byte length, then the bytes. checkNBTString has already
// bounded the length, so the conversion cannot truncate.
func (e *nbtWriter) str(s string) {
	e.u16(uint16(len(s)))
	e.write([]byte(s))
}

// named writes a tag header: the id, then the tag's own name as a length-prefixed string.
func (e *nbtWriter) named(id byte, name string) {
	e.u8(id)
	e.str(name)
}
