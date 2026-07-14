// Package mcnet speaks the two Minecraft wire protocols hosuto needs: RCON, to drive a running
// server, and SLP (Server List Ping), to prove that one is actually reachable.
//
// Both are hand-rolled on the stdlib. That is not only hosuto's one-dependency rule: the vanilla
// server's RCON implementation has sharp edges that the popular libraries paper over wrongly, and
// they are edges hosuto sits directly on (whitelist edits, and the graceful-shutdown path).
//
// The invariants this package holds:
//
//   - RCON IS NOT PIPELINED. net.minecraft.server.rcon.RconClient does one read(buf, 0, 1460) per
//     loop iteration and then checks `if (len != read - 4) return;` — it silently CLOSES the
//     connection when two request packets coalesce into a single TCP segment, which is exactly
//     what happens when you write them back to back. Conn therefore serialises every command
//     behind a mutex and never writes ahead of a reply. The "empty sentinel packet" idiom from
//     Source-engine RCON must never be used against Minecraft; it trips this check.
//   - A FAILED AUTH IS A REQUEST ID OF -1, and nothing else. The server answers a bad password
//     with an otherwise ordinary AUTH_RESPONSE, so the id is the only signal there is.
//   - SLP is the only end-to-end check hosuto has. It traverses mc-router exactly as a player
//     does, so a green ping proves the whole path (router → server), not merely that a process is
//     listening somewhere.
//   - Nothing here trusts the peer. Every length is bounded before it is allocated.
package mcnet

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrAuth is returned when the server rejects the RCON password.
var ErrAuth = errors.New("rcon: authentication failed")

// RCON packet types. AUTH_RESPONSE shares its value with EXECCOMMAND — that is not a typo, the
// protocol reuses 2 in the two directions, and the direction is what disambiguates it.
const (
	typeResponse = int32(0) // RESPONSE_VALUE  server → client
	typeCommand  = int32(2) // EXECCOMMAND     client → server, AUTH_RESPONSE server → client
	typeAuth     = int32(3) // AUTH            client → server
)

const (
	// maxRequest bounds a packet we send. The server reads into a 1460-byte buffer and drops any
	// connection whose packet did not arrive whole in one read, so an oversized request does not
	// fail loudly — it hangs and then dies. Refuse it locally instead.
	maxRequest = 1460

	// rconChunk is the payload size at which the server splits a reply. sendMultipacketResponse
	// slices the message every 4096 *characters*, so one chunk is at most 3 UTF-8 bytes per
	// character; maxReply bounds a single inbound packet with room to spare.
	rconChunk = 4096
	maxReply  = 4 + 4 + 3*rconChunk + 2

	// chunkWait bounds the wait for a *continuation* chunk once a first one has arrived. The
	// server writes them back to back, so this only has to cover the wire, and it is only ever
	// paid by a reply that was actually long enough to be split.
	chunkWait = 250 * time.Millisecond

	defaultTimeout = 10 * time.Second
)

// Conn is an authenticated RCON session.
//
// It is safe for concurrent use, and that is load-bearing rather than a convenience: the mutex is
// what enforces one-request-per-round-trip. Two goroutines writing at once would coalesce their
// packets into one segment and the server would close the connection without a word.
type Conn struct {
	mu      sync.Mutex
	c       net.Conn
	r       *bufio.Reader
	timeout time.Duration
	id      int32
}

// Dial opens an RCON session and authenticates it. It returns ErrAuth if the password is rejected.
func Dial(addr, password string, timeout time.Duration) (*Conn, error) {
	if password == "" {
		// The server compares with `!s.isEmpty() && s.equals(pw)`, so an empty password can never
		// authenticate. Fail closed rather than spend a round trip on a certain rejection.
		return nil, ErrAuth
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	nc, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("rcon: dial %s: %w", addr, err)
	}
	c := &Conn{c: nc, r: bufio.NewReaderSize(nc, maxReply), timeout: timeout}
	if err := c.auth(password); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// auth runs the AUTH exchange. It needs no lock: the Conn has not escaped Dial yet.
func (c *Conn) auth(password string) error {
	id := c.next()
	if err := c.write(id, typeAuth, password); err != nil {
		return fmt.Errorf("rcon: auth: %w", err)
	}
	p, err := c.read()
	if err != nil {
		return fmt.Errorf("rcon: auth: %w", err)
	}
	// Vanilla answers AUTH with a single AUTH_RESPONSE. Some forks emit the Source-engine habit of
	// an empty RESPONSE_VALUE first; step over it rather than mistake it for the verdict. This is
	// safe to wait for only because it is already in flight when the AUTH_RESPONSE is.
	if p.typ == typeResponse && p.id != -1 {
		if p, err = c.read(); err != nil {
			return fmt.Errorf("rcon: auth: %w", err)
		}
	}
	if p.id == -1 {
		return ErrAuth
	}
	if p.id != id {
		return fmt.Errorf("rcon: auth reply id %d, want %d", p.id, id)
	}
	return nil
}

// Cmd runs one command and returns the server's reply.
//
// Exactly one packet goes out and then we read: see the package doc on why this must never be
// pipelined. "stop" is special — the server frequently drops the connection instead of answering,
// and that EOF is the success signal, not a failure. hosuto's graceful shutdown depends on it.
func (c *Conn) Cmd(cmd string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	stopping := isStop(cmd)
	id := c.next()
	if err := c.write(id, typeCommand, cmd); err != nil {
		if stopping && closedByPeer(err) {
			return "", nil
		}
		return "", fmt.Errorf("rcon: %q: %w", cmd, err)
	}

	p, err := c.read()
	if err != nil {
		if stopping && closedByPeer(err) {
			return "", nil
		}
		return "", fmt.Errorf("rcon: %q: %w", cmd, err)
	}
	if p.id == -1 {
		// The server forgot (or never granted) this session's authentication.
		return "", ErrAuth
	}

	var sb strings.Builder
	sb.WriteString(p.body)

	// A reply over 4096 characters comes back as several RESPONSE_VALUE packets written back to
	// back. Drain what is in flight — but only when there is reason to think more is coming, so
	// that the common single-packet reply costs no extra latency at all. A read error here (a
	// timeout, or the EOF of a "stop") just means the reply is complete.
	for c.r.Buffered() > 0 || len(p.body) >= rconChunk {
		if p, err = c.readWithin(chunkWait); err != nil {
			break
		}
		if p.typ != typeResponse || p.id != id {
			break
		}
		sb.WriteString(p.body)
	}
	return sb.String(), nil
}

// Close ends the session. Closing one the server already dropped (the "stop" case) is not an error.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.c.Close(); err != nil && !closedByPeer(err) {
		return err
	}
	return nil
}

// next hands out request ids. They stay positive: -1 is the protocol's auth-failure sentinel and a
// real request must never collide with it.
func (c *Conn) next() int32 {
	c.id++
	if c.id <= 0 {
		c.id = 1
	}
	return c.id
}

type packet struct {
	id   int32
	typ  int32
	body string
}

func (c *Conn) write(id, typ int32, payload string) error {
	b, err := encode(id, typ, payload)
	if err != nil {
		return err
	}
	if err := c.c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}
	_, err = c.c.Write(b)
	return err
}

// encode lays out one packet:
//
//	int32 LE length | int32 LE request id | int32 LE type | payload | 0x00 | 0x00
//
// where length counts every byte after itself, i.e. 4 + 4 + len(payload) + 2. The two trailing
// NULs are the payload's terminator and a second, always-empty string the server still expects.
func encode(id, typ int32, payload string) ([]byte, error) {
	n := 4 + 4 + len(payload) + 2
	if 4+n > maxRequest {
		return nil, fmt.Errorf("rcon: command of %d bytes exceeds the server's %d-byte read buffer", len(payload), maxRequest-14)
	}
	b := make([]byte, 4+n)
	binary.LittleEndian.PutUint32(b[0:4], uint32(n))
	binary.LittleEndian.PutUint32(b[4:8], uint32(id))
	binary.LittleEndian.PutUint32(b[8:12], uint32(typ))
	copy(b[12:], payload)
	return b, nil
}

func (c *Conn) read() (packet, error) { return c.readWithin(c.timeout) }

func (c *Conn) readWithin(d time.Duration) (packet, error) {
	if err := c.c.SetReadDeadline(time.Now().Add(d)); err != nil {
		return packet{}, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return packet{}, err
	}
	// Bound before allocating: a length is a stranger's int until it has been checked. 10 is the
	// floor (id + type + the two NULs).
	n := int32(binary.LittleEndian.Uint32(hdr[:]))
	if n < 10 || n > maxReply {
		return packet{}, fmt.Errorf("rcon: packet length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return packet{}, err
	}
	return packet{
		id:   int32(binary.LittleEndian.Uint32(buf[0:4])),
		typ:  int32(binary.LittleEndian.Uint32(buf[4:8])),
		body: string(buf[8 : n-2]),
	}, nil
}

// isStop reports whether cmd shuts the server down, and so whether a dead socket is good news.
func isStop(cmd string) bool {
	return strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(cmd), "/"), "stop")
}

// closedByPeer reports whether err means the far end simply went away, which is what a successful
// "stop" looks like from this side of the socket.
func closedByPeer(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}
