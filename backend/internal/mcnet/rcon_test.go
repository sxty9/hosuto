package mcnet

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRcon stands in for net.minecraft.server.rcon.RconClient, and deliberately reproduces its
// framing rule: one read of at most 1460 bytes per iteration, then `if (len != read - 4) return;`.
// A client that pipelines therefore fails here in exactly the way it fails against a real server —
// silently, with a closed socket — which is the whole point of testing against this shape.
type fakeRcon struct {
	password string
	reply    func(cmd string) []string // the RESPONSE_VALUE chunks to send back
	closeOn  string                    // a command the server dies on instead of answering ("stop")
	sentinel bool                      // emit an empty RESPONSE_VALUE before AUTH_RESPONSE

	coalesced atomic.Bool // set if two packets ever arrived in one segment
	ln        net.Listener
}

func startFakeRcon(t *testing.T, f *fakeRcon) *fakeRcon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeRcon) addr() string { return f.ln.Addr().String() }

func (f *fakeRcon) serve() {
	c, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()

	buf := make([]byte, maxRequest)
	authed := false
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(buf)
		if err != nil || n < 10 {
			return
		}
		length := int32(binary.LittleEndian.Uint32(buf[0:4]))
		if int(length) != n-4 {
			f.coalesced.Store(true) // vanilla's silent close
			return
		}
		id := int32(binary.LittleEndian.Uint32(buf[4:8]))
		typ := int32(binary.LittleEndian.Uint32(buf[8:12]))
		body := string(buf[12 : n-2])

		switch typ {
		case typeAuth:
			if body != "" && body == f.password {
				authed = true
				if f.sentinel {
					f.send(c, id, typeResponse, "")
				}
				f.send(c, id, typeCommand, "") // AUTH_RESPONSE, id echoed back
				continue
			}
			f.send(c, -1, typeCommand, "") // the only auth-failure signal there is
		case typeCommand:
			if !authed {
				f.send(c, -1, typeCommand, "")
				continue
			}
			if f.closeOn != "" && body == f.closeOn {
				return // drop the connection with no reply, as "stop" does
			}
			for _, chunk := range f.reply(body) {
				f.send(c, id, typeResponse, chunk)
			}
		}
	}
}

func (f *fakeRcon) send(c net.Conn, id, typ int32, body string) {
	b, err := encode(id, typ, body)
	if err != nil {
		// The server is allowed to send more than it accepts; build the frame by hand.
		n := 4 + 4 + len(body) + 2
		b = make([]byte, 4+n)
		binary.LittleEndian.PutUint32(b[0:4], uint32(n))
		binary.LittleEndian.PutUint32(b[4:8], uint32(id))
		binary.LittleEndian.PutUint32(b[8:12], uint32(typ))
		copy(b[12:], body)
	}
	c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	c.Write(b)
}

func echoServer(t *testing.T, reply func(string) []string) *fakeRcon {
	return startFakeRcon(t, &fakeRcon{password: "s3cret", reply: reply})
}

func TestEncode(t *testing.T) {
	tests := []struct {
		name    string
		id, typ int32
		payload string
		want    []byte
		wantErr bool
	}{
		{
			name: "auth", id: 1, typ: typeAuth, payload: "pw",
			want: []byte{
				0x0c, 0x00, 0x00, 0x00, // length = 4 + 4 + 2 + 2
				0x01, 0x00, 0x00, 0x00, // request id
				0x03, 0x00, 0x00, 0x00, // AUTH
				'p', 'w', 0x00, 0x00,
			},
		},
		{
			name: "empty payload", id: 7, typ: typeCommand, payload: "",
			want: []byte{
				0x0a, 0x00, 0x00, 0x00,
				0x07, 0x00, 0x00, 0x00,
				0x02, 0x00, 0x00, 0x00,
				0x00, 0x00,
			},
		},
		{
			name: "negative id is little-endian two's complement", id: -1, typ: typeCommand, payload: "",
			want: []byte{
				0x0a, 0x00, 0x00, 0x00,
				0xff, 0xff, 0xff, 0xff,
				0x02, 0x00, 0x00, 0x00,
				0x00, 0x00,
			},
		},
		{
			// The server reads 1460 bytes at most and closes on anything that does not fit.
			name: "over the server's read buffer", id: 1, typ: typeCommand,
			payload: strings.Repeat("x", maxRequest-13), wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encode(tc.id, tc.typ, tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != string(tc.want) {
				t.Errorf("encode = % x\nwant     % x", got, tc.want)
			}
			// Length must count everything after itself, which is what the server checks.
			n := int32(binary.LittleEndian.Uint32(got[0:4]))
			if int(n) != len(got)-4 {
				t.Errorf("length field %d, want %d", n, len(got)-4)
			}
		})
	}
}

func TestEncodeAtTheLimit(t *testing.T) {
	// 1460 on the nose is the largest packet the server can read in one go.
	if _, err := encode(1, typeCommand, strings.Repeat("x", maxRequest-14)); err != nil {
		t.Errorf("a %d-byte packet must be accepted: %v", maxRequest, err)
	}
}

func TestCmd(t *testing.T) {
	const listed = "There are 2 of a max of 20 players online: alice, bob"
	f := echoServer(t, func(cmd string) []string {
		switch cmd {
		case "list":
			return []string{listed}
		case "whitelist add alice":
			return []string{"Added alice to the whitelist"}
		}
		return []string{"Unknown command"}
	})

	c, err := Dial(f.addr(), "s3cret", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	got, err := c.Cmd("list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got != listed {
		t.Errorf("list = %q, want %q", got, listed)
	}
	if got, err = c.Cmd("whitelist add alice"); err != nil || got != "Added alice to the whitelist" {
		t.Errorf("whitelist add = %q, %v", got, err)
	}
	// Several commands over one session, and the server must never have seen a coalesced segment.
	if f.coalesced.Load() {
		t.Error("packets were pipelined; the real server would have closed the connection")
	}
}

func TestCmdEmptyReply(t *testing.T) {
	// sendMultipacketResponse is a do/while: even an empty reply produces exactly one packet.
	f := echoServer(t, func(string) []string { return []string{""} })
	c, err := Dial(f.addr(), "s3cret", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	got, err := c.Cmd("whitelist reload")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCmdChunked(t *testing.T) {
	// A reply over 4096 characters comes back split; the pieces must be concatenated in order.
	head := strings.Repeat("a", rconChunk)
	tail := "the rest"
	f := echoServer(t, func(string) []string { return []string{head, tail} })

	c, err := Dial(f.addr(), "s3cret", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	got, err := c.Cmd("list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got != head+tail {
		t.Errorf("got %d bytes (%q…%q), want %d", len(got), got[:8], got[len(got)-8:], len(head+tail))
	}
}

func TestDialAuthFailure(t *testing.T) {
	f := echoServer(t, func(string) []string { return []string{"ok"} })
	_, err := Dial(f.addr(), "wrong", time.Second)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestDialEmptyPassword(t *testing.T) {
	// The server's own check is `!s.isEmpty() && s.equals(pw)`: an empty password never
	// authenticates, so we must not even try. No listener is needed to prove it.
	if _, err := Dial("127.0.0.1:1", "", time.Second); !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestDialAuthSentinel(t *testing.T) {
	// A fork that emits Source's empty RESPONSE_VALUE before the AUTH_RESPONSE must still work.
	f := startFakeRcon(t, &fakeRcon{
		password: "s3cret",
		sentinel: true,
		reply:    func(string) []string { return []string{"pong"} },
	})
	c, err := Dial(f.addr(), "s3cret", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got, err := c.Cmd("list"); err != nil || got != "pong" {
		t.Errorf("list = %q, %v", got, err)
	}
}

func TestCmdStopEOF(t *testing.T) {
	// "stop" is on the graceful-shutdown path: the server usually closes the socket instead of
	// answering, and that EOF is the success signal, not an error.
	f := startFakeRcon(t, &fakeRcon{
		password: "s3cret",
		closeOn:  "stop",
		reply:    func(string) []string { return []string{""} },
	})
	c, err := Dial(f.addr(), "s3cret", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	got, err := c.Cmd("stop")
	if err != nil {
		t.Fatalf("stop returned %v; an EOF after stop is success", err)
	}
	if got != "" {
		t.Errorf("stop = %q, want empty", got)
	}
}

func TestCmdEOFIsAnErrorForAnythingElse(t *testing.T) {
	// The same dropped connection under any other command is a genuine failure and must surface.
	f := startFakeRcon(t, &fakeRcon{
		password: "s3cret",
		closeOn:  "list",
		reply:    func(string) []string { return []string{""} },
	})
	c, err := Dial(f.addr(), "s3cret", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Cmd("list"); err == nil {
		t.Fatal("a dropped connection during list must be an error")
	}
}

func TestIsStop(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"stop", true},
		{" stop ", true},
		{"/stop", true},
		{"STOP", true},
		{"stopwatch", false},
		{"list", false},
		{"", false},
	} {
		if got := isStop(tc.cmd); got != tc.want {
			t.Errorf("isStop(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestReadRejectsAbsurdLength(t *testing.T) {
	// A hostile length must be refused before it is allocated.
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], 1<<30)
		server.Write(hdr[:])
	}()

	c := &Conn{c: client, r: bufio.NewReaderSize(client, maxReply), timeout: time.Second}
	if _, err := c.read(); err == nil {
		t.Fatal("want an error for a 1 GiB packet length")
	}
}
