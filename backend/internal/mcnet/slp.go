package mcnet

// SLP — Server List Ping, the exchange a vanilla client performs before it draws a server in its
// multiplayer list.
//
// hosuto uses it as its reachability probe because it is the only check that walks the same path a
// player does: the handshake carries the server's REAL hostname, which is what mc-router routes
// on, so a green ping proves DNS → router → server end to end. A systemd "active" or an open local
// port proves none of that. It also happens to be where the live player count comes from.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Status is one server's advertised status, flattened into what hosuto's UI actually shows.
type Status struct {
	Online, Max int
	Sample      []string // the player-name sample the server chose to publish; often absent or partial
	Version     string   // e.g. "1.21.4", or "Paper 1.21.4"
	Description string   // the MOTD, flattened to plain text if the server sent a chat object
}

const (
	// protoUnknown is the conventional protocol version for a ping: "I am not announcing a
	// version". Every server answers a status request for it instead of rejecting it as
	// incompatible, which is what makes a single prober work across MC versions.
	protoUnknown = int32(-1)
	// stateStatus is nextState=1 in the handshake: status, not login.
	stateStatus = int32(1)
	// idHandshake is the packet id of both the handshake and the status request; in the status
	// state it is also the id of the response. The state, not the id, disambiguates.
	idHandshake = int32(0)

	// maxString bounds a string field before it is allocated. The protocol caps a string at 32767
	// characters, each at most 3 UTF-8 bytes.
	maxString = 32767 * 3
	// maxPacket bounds a whole packet. The status reply carries a base64 favicon, so it is the one
	// packet that is legitimately large — but it is a stranger's, so it is not unbounded.
	maxPacket = 1 << 20
	// maxDepth bounds chat-component nesting so a hostile MOTD cannot recurse us to death.
	maxDepth = 16
)

// Ping performs a Server List Ping.
//
// dialAddr is where we actually connect (mc-router, or the server itself); handshakeHost and port
// are what we *claim* to be connecting to, and they must be the server's real public hostname and
// port, because that pair is the only thing mc-router has to route on.
func Ping(ctx context.Context, dialAddr, handshakeHost string, port int, timeout time.Duration) (Status, error) {
	if port < 0 || port > 65535 {
		return Status{}, fmt.Errorf("slp: port %d out of range", port)
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		return Status{}, fmt.Errorf("slp: dial %s: %w", dialAddr, err)
	}
	defer conn.Close()

	// A cancelled context has to unblock a read that is already in flight, and closing the socket
	// is the only way to do that.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return Status{}, fmt.Errorf("slp: %w", err)
	}

	// Handshake and status request go out as two separate writes, the way a client sends them. A
	// proxy is entitled to parse the handshake and forward only what follows it, so we do not
	// coalesce the request into the same segment and hope it is passed on.
	var hs bytes.Buffer
	writeVarInt(&hs, idHandshake)
	writeVarInt(&hs, protoUnknown)
	if err := writeString(&hs, handshakeHost); err != nil {
		return Status{}, fmt.Errorf("slp: %w", err)
	}
	binary.Write(&hs, binary.BigEndian, uint16(port)) // the port is the one big-endian field here
	writeVarInt(&hs, stateStatus)
	if err := writeFrame(conn, hs.Bytes()); err != nil {
		return Status{}, fmt.Errorf("slp: handshake: %w", err)
	}

	var req bytes.Buffer
	writeVarInt(&req, idHandshake) // status request: an id and an empty body
	if err := writeFrame(conn, req.Bytes()); err != nil {
		return Status{}, fmt.Errorf("slp: status request: %w", err)
	}

	body, err := readFrame(bufio.NewReader(conn))
	if err != nil {
		return Status{}, fmt.Errorf("slp: status response: %w", err)
	}
	r := bytes.NewReader(body)
	id, err := readVarInt(r)
	if err != nil {
		return Status{}, fmt.Errorf("slp: status response: %w", err)
	}
	if id != idHandshake {
		return Status{}, fmt.Errorf("slp: unexpected packet id 0x%02x in status response", id)
	}
	js, err := readString(r)
	if err != nil {
		return Status{}, fmt.Errorf("slp: status response: %w", err)
	}
	return parseStatus(js)
}

// parseStatus reads the status JSON. Everything in it is optional or polymorphic in practice, so
// nothing here may fail on a shape it has not seen: a server whose MOTD we cannot render is still
// a server that is up, and up is what the caller asked about.
func parseStatus(js string) (Status, error) {
	var raw struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		// Absent on servers that hide their player count — a pointer, so we can tell "hidden"
		// from "zero online".
		Players *struct {
			Max    int `json:"max"`
			Online int `json:"online"`
			Sample []struct {
				Name string `json:"name"`
			} `json:"sample"`
		} `json:"players"`
		// A plain string on some servers, a chat component (or an array of them) on others.
		Description json.RawMessage `json:"description"`
	}
	if err := json.Unmarshal([]byte(js), &raw); err != nil {
		return Status{}, fmt.Errorf("slp: bad status json: %w", err)
	}

	st := Status{
		Version:     raw.Version.Name,
		Description: stripCodes(flatten(raw.Description, 0)),
	}
	if raw.Players != nil {
		st.Online, st.Max = raw.Players.Online, raw.Players.Max
		for _, p := range raw.Players.Sample {
			if p.Name != "" {
				st.Sample = append(st.Sample, p.Name)
			}
		}
	}
	return st, nil
}

// flatten renders a description as plain text. It may be a bare string, a chat component, or an
// array of either, and a component nests its children under "extra".
func flatten(raw json.RawMessage, depth int) string {
	if len(raw) == 0 || depth > maxDepth {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		var sb strings.Builder
		for _, e := range arr {
			sb.WriteString(flatten(e, depth+1))
		}
		return sb.String()
	}
	var obj struct {
		Text  string            `json:"text"`
		Extra []json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "" // a shape we do not know; an unreadable MOTD is never worth failing a probe
	}
	var sb strings.Builder
	sb.WriteString(obj.Text)
	for _, e := range obj.Extra {
		sb.WriteString(flatten(e, depth+1))
	}
	return sb.String()
}

// stripCodes removes the legacy §-colour codes. Description is documented as plain text, and a
// real MOTD is dense with them.
func stripCodes(s string) string {
	if !strings.ContainsRune(s, '§') {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	skip := false
	for _, r := range s {
		switch {
		case skip:
			skip = false // the code's argument: one character, whatever it is
		case r == '§':
			skip = true
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// writeVarInt appends v as LEB128: seven bits per byte, low group first, MSB set while more bytes
// follow. Negatives are written as their unsigned two's complement, so -1 is a full five bytes.
func writeVarInt(b *bytes.Buffer, v int32) {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			b.WriteByte(byte(u))
			return
		}
		b.WriteByte(byte(u&0x7F) | 0x80)
		u >>= 7
	}
}

// readVarInt reads a VarInt. Five bytes is the most an int32 can occupy; a sixth continuation byte
// is a malformed or hostile stream, and is refused rather than shifted into oblivion.
func readVarInt(r io.Reader) (int32, error) {
	var (
		v     uint32
		shift uint
		b     [1]byte
	)
	for i := 0; i < 5; i++ {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		v |= uint32(b[0]&0x7F) << shift
		if b[0]&0x80 == 0 {
			return int32(v), nil
		}
		shift += 7
	}
	return 0, errors.New("mcnet: VarInt longer than 5 bytes")
}

// writeString writes a VarInt-length-prefixed UTF-8 string.
func writeString(b *bytes.Buffer, s string) error {
	if len(s) > maxString {
		return fmt.Errorf("mcnet: string of %d bytes exceeds the protocol maximum", len(s))
	}
	writeVarInt(b, int32(len(s)))
	b.WriteString(s)
	return nil
}

func readString(r io.Reader) (string, error) {
	n, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if n < 0 || int(n) > maxString {
		return "", fmt.Errorf("mcnet: string length %d out of range", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// writeFrame wraps a packet body in the VarInt length prefix that envelopes every packet in the
// Minecraft protocol.
func writeFrame(w io.Writer, body []byte) error {
	var b bytes.Buffer
	writeVarInt(&b, int32(len(body)))
	b.Write(body)
	_, err := w.Write(b.Bytes())
	return err
}

// readFrame reads one length-prefixed packet body, bounding it before it is allocated.
func readFrame(r io.Reader) ([]byte, error) {
	n, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if n < 1 || int(n) > maxPacket {
		return nil, fmt.Errorf("mcnet: packet length %d out of range", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
