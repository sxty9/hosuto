package mcnet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testHost = "smp.example.org"
	testPort = 25599
)

// wantHandshake is the byte sequence a client puts on the wire for testHost:testPort with an
// unknown protocol version. It is written out by hand on purpose: it pins the encoder against the
// protocol rather than against itself.
var wantHandshake = []byte{
	0x19,                         // frame length: 25 bytes follow
	0x00,                         // packet id 0x00: handshake
	0xff, 0xff, 0xff, 0xff, 0x0f, // protocol version -1, "unknown"
	0x0f, // server address: 15 bytes …
	's', 'm', 'p', '.', 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'o', 'r', 'g',
	0x63, 0xff, // port 25599, big-endian — the one big-endian field in the packet
	0x01, // next state 1: status
}

// wantRequest is the status request: a length, an id, and no body at all.
var wantRequest = []byte{0x01, 0x00}

// testVarInt is a second, independent LEB128 encoder. The fake server uses it so that a test does
// not check the encoder with the encoder.
func testVarInt(v int32) []byte {
	u := uint32(v)
	var out []byte
	for {
		b := byte(u & 0x7F)
		u >>= 7
		if u == 0 {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

func rawPacket(body []byte) []byte {
	return append(testVarInt(int32(len(body))), body...)
}

// statusPacket frames a status response the way a server does: id 0x00, then the JSON as a
// length-prefixed string.
func statusPacket(js string) []byte {
	body := []byte{0x00}
	body = append(body, testVarInt(int32(len(js)))...)
	body = append(body, js...)
	return rawPacket(body)
}

// slpServer speaks the exact byte sequence a vanilla server speaks, and asserts the exact byte
// sequence a client must speak — including the hostname, which is the only thing mc-router has to
// route on and so the field that matters most here.
type slpServer struct {
	ln   net.Listener
	resp []byte // written verbatim after the handshake; nil means "answer nothing"
	errc chan error
}

func startSLP(t *testing.T, resp []byte) *slpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &slpServer{ln: ln, resp: resp, errc: make(chan error, 1)}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *slpServer) addr() string { return s.ln.Addr().String() }

func (s *slpServer) serve() {
	c, err := s.ln.Accept()
	if err != nil {
		s.errc <- err
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))

	for _, want := range [][]byte{wantHandshake, wantRequest} {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(c, got); err != nil {
			s.errc <- fmt.Errorf("read: %w", err)
			return
		}
		if !bytes.Equal(got, want) {
			s.errc <- fmt.Errorf("client sent\n got % x\nwant % x", got, want)
			return
		}
	}
	s.errc <- nil

	if s.resp == nil {
		time.Sleep(3 * time.Second) // hold the socket open: the caller's deadline must end this
		return
	}
	c.Write(s.resp)
}

// wire fails the test unless the client spoke the protocol exactly.
func (s *slpServer) wire(t *testing.T) {
	t.Helper()
	select {
	case err := <-s.errc:
		if err != nil {
			t.Fatalf("fake server: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake server never saw a handshake")
	}
}

func TestPing(t *testing.T) {
	tests := []struct {
		name string
		json string
		want Status
	}{
		{
			name: "full",
			json: `{"version":{"name":"Paper 1.21.4","protocol":769},
			        "players":{"max":20,"online":2,"sample":[{"name":"alice","id":"a-1"},{"name":"bob","id":"b-2"}]},
			        "description":"A hosuto server",
			        "favicon":"data:image/png;base64,iVBORw0KGgo="}`,
			want: Status{
				Online: 2, Max: 20,
				Sample:      []string{"alice", "bob"},
				Version:     "Paper 1.21.4",
				Description: "A hosuto server",
			},
		},
		{
			// The description is a chat component as often as it is a string, and it nests.
			name: "chat object description",
			json: `{"version":{"name":"1.21.4","protocol":769},
			        "players":{"max":20,"online":1,"sample":[{"name":"alice","id":"a-1"}]},
			        "description":{"text":"§6hosuto","extra":[{"text":" — "},{"text":"survival","extra":[{"text":"!"}]}]}}`,
			want: Status{
				Online: 1, Max: 20,
				Sample:      []string{"alice"},
				Version:     "1.21.4",
				Description: "hosuto — survival!",
			},
		},
		{
			// "players" is optional. A server that omits it is up, and must be reported as up.
			name: "missing players",
			json: `{"version":{"name":"1.21.4","protocol":769},"description":"idle"}`,
			want: Status{Version: "1.21.4", Description: "idle"},
		},
		{
			name: "array description",
			json: `{"version":{"name":"1.20.1","protocol":763},"description":["a","b"]}`,
			want: Status{Version: "1.20.1", Description: "ab"},
		},
		{
			name: "null description",
			json: `{"version":{"name":"1.21"},"players":{"max":5,"online":0},"description":null}`,
			want: Status{Max: 5, Version: "1.21"},
		},
		{
			name: "empty sample",
			json: `{"version":{"name":"1.21"},"players":{"max":5,"online":0,"sample":[]}}`,
			want: Status{Max: 5, Version: "1.21"},
		},
		{
			// An unknown component shape (a translation key, say) is not a reason to fail a probe.
			name: "untranslatable description",
			json: `{"version":{"name":"1.21"},"description":{"translate":"multiplayer.status.old"}}`,
			want: Status{Version: "1.21"},
		},
		{
			name: "empty object",
			json: `{}`,
			want: Status{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startSLP(t, statusPacket(tc.json))
			got, err := Ping(context.Background(), s.addr(), testHost, testPort, time.Second)
			// The wire is checked first: a malformed handshake is the reason for any error that
			// follows it, and the byte diff says so where "i/o timeout" would not.
			s.wire(t)
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Ping = %#v\nwant   %#v", got, tc.want)
			}
		})
	}
}

func TestPingErrors(t *testing.T) {
	tests := []struct {
		name string
		resp []byte
	}{
		{"malformed json", statusPacket(`{"players":`)},
		{"not json at all", statusPacket(`<html>502 Bad Gateway</html>`)},
		{"wrong packet id", rawPacket(append([]byte{0x01}, append(testVarInt(2), "{}"...)...))},
		{"truncated string", rawPacket(append([]byte{0x00}, testVarInt(99)...))},
		{"string longer than the protocol allows", rawPacket(append([]byte{0x00}, testVarInt(maxString+1)...))},
		{"empty packet", []byte{0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startSLP(t, tc.resp)
			if _, err := Ping(context.Background(), s.addr(), testHost, testPort, time.Second); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestPingClosedPort(t *testing.T) {
	// Fail closed: nothing listening is an error, never an empty Status.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if _, err := Ping(context.Background(), addr, testHost, testPort, 500*time.Millisecond); err == nil {
		t.Fatal("want an error against a closed port")
	}
}

func TestPingCancel(t *testing.T) {
	// A server that accepts and then says nothing must not pin the caller to the full timeout.
	s := startSLP(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := Ping(ctx, s.addr(), testHost, testPort, 30*time.Second); err == nil {
		t.Fatal("want an error after cancellation")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancellation took %s; the context must close the socket", d)
	}
}

func TestPingTimeout(t *testing.T) {
	s := startSLP(t, nil)
	start := time.Now()
	if _, err := Ping(context.Background(), s.addr(), testHost, testPort, 200*time.Millisecond); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("timeout took %s", d)
	}
}

func TestPingBadPort(t *testing.T) {
	if _, err := Ping(context.Background(), "127.0.0.1:1", testHost, 70000, time.Second); err == nil {
		t.Fatal("want an error for a port outside the u16 range")
	}
}

func TestVarInt(t *testing.T) {
	// The canonical encodings from the protocol. Both directions are checked against them.
	tests := []struct {
		v    int32
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{2, []byte{0x02}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{255, []byte{0xff, 0x01}},
		{25565, []byte{0xdd, 0xc7, 0x01}},
		{2097151, []byte{0xff, 0xff, 0x7f}},
		{2147483647, []byte{0xff, 0xff, 0xff, 0xff, 0x07}},
		{-1, []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
		{-2147483648, []byte{0x80, 0x80, 0x80, 0x80, 0x08}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.v), func(t *testing.T) {
			var b bytes.Buffer
			writeVarInt(&b, tc.v)
			if !bytes.Equal(b.Bytes(), tc.want) {
				t.Errorf("writeVarInt(%d) = % x, want % x", tc.v, b.Bytes(), tc.want)
			}
			got, err := readVarInt(bytes.NewReader(tc.want))
			if err != nil {
				t.Fatalf("readVarInt(% x): %v", tc.want, err)
			}
			if got != tc.v {
				t.Errorf("readVarInt(% x) = %d, want %d", tc.want, got, tc.v)
			}
			// And the round trip, which is what the wire actually does.
			back, err := readVarInt(bytes.NewReader(b.Bytes()))
			if err != nil || back != tc.v {
				t.Errorf("round trip of %d = %d, %v", tc.v, back, err)
			}
		})
	}
}

func TestReadVarIntErrors(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"truncated", []byte{0x80}},
		{"truncated at four", []byte{0xff, 0xff, 0xff, 0xff}},
		{"six bytes", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readVarInt(bytes.NewReader(tc.in)); err == nil {
				t.Errorf("readVarInt(% x) = nil error", tc.in)
			}
		})
	}
}

func TestString(t *testing.T) {
	for _, s := range []string{"", "hosuto", testHost, "héllo ☃", strings.Repeat("x", 300)} {
		var b bytes.Buffer
		if err := writeString(&b, s); err != nil {
			t.Fatalf("writeString(%q): %v", s, err)
		}
		got, err := readString(bytes.NewReader(b.Bytes()))
		if err != nil {
			t.Fatalf("readString(%q): %v", s, err)
		}
		if got != s {
			t.Errorf("round trip = %q, want %q", got, s)
		}
	}
}

func TestStringBounds(t *testing.T) {
	// A hostile length must be refused before anything is allocated for it.
	if _, err := readString(bytes.NewReader(append(testVarInt(maxString+1), 'x'))); err == nil {
		t.Error("want an error for an over-long declared string")
	}
	if _, err := readString(bytes.NewReader(append(testVarInt(-1), 'x'))); err == nil {
		t.Error("want an error for a negative string length")
	}
	if err := writeString(&bytes.Buffer{}, strings.Repeat("x", maxString+1)); err == nil {
		t.Error("want an error when writing an over-long string")
	}
}

func TestReadFrameBounds(t *testing.T) {
	if _, err := readFrame(bytes.NewReader(testVarInt(maxPacket + 1))); err == nil {
		t.Error("want an error for an over-long packet")
	}
	if _, err := readFrame(bytes.NewReader(testVarInt(0))); err == nil {
		t.Error("want an error for a zero-length packet")
	}
}

func TestStripCodes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"§6hosuto", "hosuto"},
		{"§lBold§r and §anot", "Bold and not"},
		{"trailing §", "trailing "},
		{"§§", ""},
	} {
		if got := stripCodes(tc.in); got != tc.want {
			t.Errorf("stripCodes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFlattenDepth(t *testing.T) {
	// A hostile MOTD must not be able to recurse us to death.
	deep := strings.Repeat(`{"text":"x","extra":[`, 5000) + `{"text":"end"}` + strings.Repeat("]}", 5000)
	if got := flatten([]byte(deep), 0); len(got) > maxDepth+1 {
		t.Errorf("flatten recursed past the depth cap: %d chars", len(got))
	}
}
