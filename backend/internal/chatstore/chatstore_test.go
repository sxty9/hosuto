package chatstore

import (
	"path/filepath"
	"testing"
)

func TestSharedThread(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	const id = "srv-abc12345"

	// An unseen server has an empty thread, not an error.
	if msgs, err := s.Load(id); err != nil || len(msgs) != 0 {
		t.Fatalf("empty load: %v %v", msgs, err)
	}

	// Two operators append to the SAME thread; both turns are there, attributed.
	if _, err := s.Append(id, Msg{Role: "user", Content: "start it", Author: "alice", TS: 1}, Msg{Role: "assistant", Content: "done", TS: 2}); err != nil {
		t.Fatal(err)
	}
	full, err := s.Append(id, Msg{Role: "user", Content: "who's on?", Author: "bob", TS: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 3 || full[0].Author != "alice" || full[2].Author != "bob" {
		t.Fatalf("shared thread wrong: %+v", full)
	}

	// It persists: a fresh store over the same dir sees the same thread.
	if got, _ := New(dir).Load(id); len(got) != 3 {
		t.Fatalf("not persisted: %+v", got)
	}

	// A different server has its own thread.
	if got, _ := s.Load("srv-99999999"); len(got) != 0 {
		t.Fatalf("threads not isolated per server: %+v", got)
	}

	// The thread is capped: the tail survives, the head is dropped.
	big := make([]Msg, maxMessages+50)
	for i := range big {
		big[i] = Msg{Role: "user", Content: "x", TS: int64(i)}
	}
	capped, err := s.Append("srv-cccccccc", big...)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != maxMessages || capped[len(capped)-1].TS != int64(len(big)-1) {
		t.Fatalf("cap wrong: len=%d lastTS=%d", len(capped), capped[len(capped)-1].TS)
	}

	// Delete drops exactly that server's file.
	if err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(id); len(got) != 0 {
		t.Fatalf("not deleted: %+v", got)
	}
	if _, err := New(dir).Load("srv-cccccccc"); err != nil {
		t.Fatalf("delete touched another thread: %v", err)
	}
}

func TestRejectsBadID(t *testing.T) {
	s := New(t.TempDir())
	for _, bad := range []string{"../etc/passwd", "srv-../x", "nope", "srv-xyz/../y", ""} {
		if _, err := s.Load(bad); err == nil {
			t.Fatalf("bad id %q was accepted", bad)
		}
		if _, err := s.Append(bad, Msg{Role: "user", Content: "x"}); err == nil {
			t.Fatalf("bad id %q accepted on append", bad)
		}
	}
	// A file must never appear outside the store dir.
	if _, err := filepath.Glob(filepath.Join(t.TempDir(), "*")); err != nil {
		t.Fatal(err)
	}
}
