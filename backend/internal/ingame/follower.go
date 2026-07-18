package ingame

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"hosuto/internal/runtime"
	"hosuto/internal/store"
)

// The log lines we care about. Two layouts exist and BOTH have to match, because the loader chooses
// the log4j pattern and the difference is invisible until the feature silently does nothing:
//
//	vanilla / Paper / Fabric   [HH:MM:SS] [<thread>/INFO]: <Name> message text
//	NeoForge / Forge           [ddMMMyyyy HH:MM:SS.SSS] [<thread>/INFO] [<logger>/]: <Name> message
//
// So the timestamp is not parsed at all — it is simply the first bracketed field — and any number of
// further bracketed fields may follow before the colon. Pinning the timestamp to HH:MM:SS and to
// exactly one thread tag is what made `!ai` and `!ping` dead on every NeoForge server: the follower
// read the lines, matched nothing, and reported no error anywhere.
//
// The bracket run is anchored at the start of the line and must be followed by ": <", so it can only
// ever match the prefix the SERVER wrote. A player who types brackets and angle-brackets into chat
// lands in the message capture, never in the name.
var (
	chatRe = regexp.MustCompile(`^\[[^]]*\](?: \[[^]]*\])*: <([^>]+)> (.*)$`)
	uuidRe = regexp.MustCompile(`UUID of player (\S+) is ([0-9a-fA-F-]{32,36})`)
)

// pollInterval is how often a follower checks the log for new bytes. Short enough that `!ai` feels
// responsive, cheap enough to run per active server (a stat + a small read).
const pollInterval = 200 * time.Millisecond

// maxReadChunk bounds one poll's read so a startup log burst cannot balloon memory; the offset still
// advances, so the remainder is picked up on the next ticks.
const maxReadChunk = 128 << 10

// follow tails one server's latest.log until ctx is cancelled (the supervisor cancels it when the
// server stops). It seeks to the END on open so it never replays history, detects rotation (a new
// file at the same path) and truncation, and feeds each completed chat/UUID line to the engine.
func (e *Engine) follow(ctx context.Context, srv store.Server) {
	path := filepath.Join(runtime.Dir(srv.Owner, srv.Slug), "logs", "latest.log")
	names := map[string]string{} // in-game name -> uuid, learned from the log's own UUID anchors

	var f *os.File
	var openInfo os.FileInfo
	var offset int64
	var carry []byte

	// reopen (re)opens the file. seekEnd=true skips existing content (first open / server already
	// running); seekEnd=false starts from the top (a freshly rotated file has no history to skip).
	reopen := func(seekEnd bool) {
		if f != nil {
			f.Close()
			f = nil
		}
		nf, err := os.Open(path)
		if err != nil {
			return
		}
		fi, err := nf.Stat()
		if err != nil {
			nf.Close()
			return
		}
		f, openInfo, carry = nf, fi, nil
		if seekEnd {
			offset = fi.Size()
		} else {
			offset = 0
		}
	}

	reopen(true)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if f == nil {
			reopen(true) // file wasn't there yet (world still creating logs/) — try again
			continue
		}
		// Rotation: a different inode now lives at the path. Reopen from the top of the new file.
		if pi, err := os.Stat(path); err == nil && !os.SameFile(pi, openInfo) {
			reopen(false)
			if f == nil {
				continue
			}
		}
		cur, err := f.Stat()
		if err != nil {
			continue
		}
		if cur.Size() < offset { // truncated in place
			offset, carry = 0, nil
		}
		avail := cur.Size() - offset
		if avail <= 0 {
			continue
		}
		if avail > maxReadChunk {
			avail = maxReadChunk
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			continue
		}
		buf := make([]byte, avail)
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			continue
		}
		offset += int64(n)

		data := append(carry, buf[:n]...)
		lines := bytes.Split(data, []byte{'\n'})
		carry = lines[len(lines)-1] // trailing fragment (no newline yet) waits for the rest
		for _, ln := range lines[:len(lines)-1] {
			e.onLogLine(ctx, srv, names, strings.TrimRight(string(ln), "\r"))
		}
	}
}

// onLogLine reacts to one complete log line: it records UUID anchors (so a later chat line can be
// tied to a stable identity) and, for a chat line that starts with the trigger, dispatches the CLI.
func (e *Engine) onLogLine(ctx context.Context, srv store.Server, names map[string]string, line string) {
	if m := uuidRe.FindStringSubmatch(line); m != nil {
		names[m[1]] = m[2]
		return
	}
	m := chatRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	player, text := m[1], strings.TrimSpace(m[2])
	// !ping: a cheap liveness + latency check, no AI and no operator gate — anyone in the chat may ask.
	if strings.EqualFold(text, e.pingTrigger()) {
		e.handlePing(ctx, srv, player)
		return
	}
	trigger := e.trigger()
	if text != trigger && !strings.HasPrefix(text, trigger+" ") {
		return
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, trigger))
	e.handleCommand(ctx, srv, chatLine{Player: player, UUID: names[player], Text: rest})
}

// chatLine is one triggered `!ai …` message: who typed it, their UUID if the log revealed it, and the
// text AFTER the trigger.
type chatLine struct {
	Player string
	UUID   string
	Text   string
}
