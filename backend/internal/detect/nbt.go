package detect

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"unicode/utf16"
	"unicode/utf8"
)

// level.dat is the one file every real Minecraft world has, and its Data.Version.Name is the exact
// version string the world was last saved with. That makes it the most trustworthy answer to "what
// Minecraft is this?" — jars get renamed, launcher scripts get hand-edited and libraries/ can be
// stale, but a world records what actually wrote it.
//
// So this file holds just enough NBT to read that one value. It is a real parser rather than a byte
// scan on purpose: level.dat is an untrusted file from a foreign host, and a scan for the bytes
// "Name" would happily read a length prefix out of the middle of a chunk of player data. Every
// length here is bounded and every read is checked.

// NBT tag ids.
const (
	tagEnd = iota
	tagByte
	tagShort
	tagInt
	tagLong
	tagFloat
	tagDouble
	tagByteArray
	tagString
	tagList
	tagCompound
	tagIntArray
	tagLongArray
)

var errNBT = errors.New("detect: malformed nbt")

// maxNBT bounds the decompressed level.dat. A real one is a few kilobytes; this leaves enormous room
// while refusing a gzip bomb dressed up as a world.
const maxNBT = 32 << 20

// levelVersion returns the version name recorded in a level.dat (e.g. "1.20.4"), and the numeric
// DataVersion when present. Both are best-effort: a pre-1.9 world has neither.
func levelVersion(path string) (name string, dataVersion int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	var r io.Reader = f
	// level.dat is gzipped in practice, but a hand-repaired one may not be. Sniff rather than assume.
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return "", 0, err
		}
		defer zr.Close()
		r = zr
	}
	b, err := io.ReadAll(io.LimitReader(r, maxNBT+1))
	if err != nil {
		return "", 0, err
	}
	if len(b) > maxNBT {
		return "", 0, errNBT
	}

	root, err := parseNBT(b)
	if err != nil {
		return "", 0, err
	}
	data, _ := root["Data"].(compound)
	if data == nil {
		return "", 0, errNBT
	}
	if v, ok := data["Version"].(compound); ok {
		if s, ok := v["Name"].(string); ok {
			name = s
		}
		if i, ok := v["Id"].(int32); ok {
			dataVersion = int(i)
		}
	}
	if dataVersion == 0 {
		if i, ok := data["DataVersion"].(int32); ok {
			dataVersion = int(i)
		}
	}
	return name, dataVersion, nil
}

type compound map[string]any

// parseNBT decodes the root compound. Only the values this package needs are materialised with real
// types; the rest are skipped over, which keeps memory bounded on a big level.dat.
func parseNBT(b []byte) (compound, error) {
	d := &nbtDec{b: b}
	t, err := d.u8()
	if err != nil {
		return nil, err
	}
	if t != tagCompound {
		return nil, errNBT
	}
	if _, err := d.str(); err != nil { // the root's own (empty) name
		return nil, err
	}
	return d.compound()
}

type nbtDec struct {
	b []byte
	i int
	// depth guards against a deeply nested compound crafted to blow the Go stack.
	depth int
}

const maxDepth = 64

func (d *nbtDec) need(n int) ([]byte, error) {
	if n < 0 || d.i+n > len(d.b) {
		return nil, errNBT
	}
	s := d.b[d.i : d.i+n]
	d.i += n
	return s, nil
}

func (d *nbtDec) u8() (byte, error) {
	s, err := d.need(1)
	if err != nil {
		return 0, err
	}
	return s[0], nil
}

func (d *nbtDec) u16() (int, error) {
	s, err := d.need(2)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(s)), nil
}

func (d *nbtDec) i32() (int32, error) {
	s, err := d.need(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(s)), nil
}

// str reads a TAG_String payload: a uint16 length and that many bytes of java's "modified UTF-8".
func (d *nbtDec) str() (string, error) {
	n, err := d.u16()
	if err != nil {
		return "", err
	}
	s, err := d.need(n)
	if err != nil {
		return "", err
	}
	return modifiedUTF8(s), nil
}

func (d *nbtDec) compound() (compound, error) {
	if d.depth++; d.depth > maxDepth {
		return nil, errNBT
	}
	defer func() { d.depth-- }()

	c := compound{}
	for {
		t, err := d.u8()
		if err != nil {
			return nil, err
		}
		if t == tagEnd {
			return c, nil
		}
		name, err := d.str()
		if err != nil {
			return nil, err
		}
		v, err := d.value(t)
		if err != nil {
			return nil, err
		}
		if v != nil {
			c[name] = v
		}
	}
}

// value decodes a payload, materialising only strings, ints and compounds — everything this package
// reads. Anything else is consumed and discarded, so the cursor stays correct without the cost of
// building objects nobody asks for.
func (d *nbtDec) value(t byte) (any, error) {
	switch t {
	case tagByte:
		_, err := d.need(1)
		return nil, err
	case tagShort:
		_, err := d.need(2)
		return nil, err
	case tagInt:
		v, err := d.i32()
		if err != nil {
			return nil, err
		}
		return v, nil
	case tagLong, tagDouble:
		_, err := d.need(8)
		return nil, err
	case tagFloat:
		_, err := d.need(4)
		return nil, err
	case tagByteArray:
		n, err := d.i32()
		if err != nil {
			return nil, err
		}
		_, err = d.need(int(n))
		return nil, err
	case tagString:
		s, err := d.str()
		if err != nil {
			return nil, err
		}
		return s, nil
	case tagList:
		et, err := d.u8()
		if err != nil {
			return nil, err
		}
		n, err := d.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 {
			n = 0
		}
		for range int(n) {
			if _, err := d.value(et); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case tagCompound:
		c, err := d.compound()
		if err != nil {
			return nil, err
		}
		return c, nil
	case tagIntArray:
		n, err := d.i32()
		if err != nil {
			return nil, err
		}
		_, err = d.need(int(n) * 4)
		return nil, err
	case tagLongArray:
		n, err := d.i32()
		if err != nil {
			return nil, err
		}
		_, err = d.need(int(n) * 8)
		return nil, err
	}
	return nil, errNBT
}

// modifiedUTF8 decodes java's string encoding: like UTF-8, except NUL is two bytes and astral runes
// arrive as a CESU-8 surrogate pair rather than a single four-byte sequence. Go's own decoder rejects
// both, and a version string that failed to decode would send the whole detection down the fallback
// path for no reason.
func modifiedUTF8(b []byte) string {
	if utf8.Valid(b) && !bytes.ContainsRune(b, utf8.RuneError) {
		return string(b)
	}
	var out []rune
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0xC0 && i+1 < len(b) && b[i+1] == 0x80:
			out = append(out, 0)
			i += 2
		case c < 0x80:
			out = append(out, rune(c))
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				out = append(out, utf8.RuneError)
				i++
				continue
			}
			// A surrogate half means CESU-8: pair it with the next one to recover the real rune.
			if utf16.IsSurrogate(r) {
				r2, size2 := utf8.DecodeRune(b[i+size:])
				if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
					out = append(out, dec)
					i += size + size2
					continue
				}
			}
			out = append(out, r)
			i += size
		}
	}
	return string(out)
}
