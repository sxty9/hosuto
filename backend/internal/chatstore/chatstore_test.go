package chatstore

import (
	"errors"
	"testing"
)

func TestConversations(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	const srv = "srv-abc12345"

	// A fresh server has no conversations.
	if list, err := s.List(srv); err != nil || len(list) != 0 {
		t.Fatalf("empty list: %v %v", list, err)
	}

	// Create two conversations.
	a, err := s.Create(srv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(srv)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("ids collide")
	}

	// Two operators append to the SAME conversation; both turns land, attributed, and the title comes
	// from the first user turn.
	if _, err := s.Append(srv, a.ID, Msg{Role: "user", Content: "start the server please", Author: "alice", TS: 1}, Msg{Role: "assistant", Content: "done", Engine: "claude-cli", Model: "claude-opus-4-8", TS: 2}); err != nil {
		t.Fatal(err)
	}
	conv, err := s.Append(srv, a.ID, Msg{Role: "user", Content: "who's on?", Author: "bob", TS: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 3 || conv.Title != "start the server please" || conv.Messages[1].Model != "claude-opus-4-8" {
		t.Fatalf("conversation wrong: %+v", conv)
	}

	// The list shows both, newest first (a was just appended, so it sorts above the untouched b).
	list, err := s.List(srv)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a.ID || list[0].Count != 3 || list[0].Title != "start the server please" {
		t.Fatalf("list wrong: %+v", list)
	}

	// Persistence: a fresh store over the same dir sees the same conversation.
	if got, err := New(dir).Get(srv, a.ID); err != nil || len(got.Messages) != 3 {
		t.Fatalf("not persisted: %+v %v", got, err)
	}

	// Delete one conversation; the other survives.
	if err := s.Delete(srv, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(srv, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted conv still present: %v", err)
	}
	if _, err := s.Get(srv, b.ID); err != nil {
		t.Fatalf("wrong conv deleted: %v", err)
	}

	// Appending to a missing conversation is ErrNotFound, not a silent create.
	if _, err := s.Append(srv, "c00000000000", Msg{Role: "user", Content: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to missing: %v", err)
	}

	// Cap: the tail survives.
	big := make([]Msg, maxMessages+30)
	for i := range big {
		big[i] = Msg{Role: "user", Content: "x", TS: int64(i)}
	}
	capped, err := s.Append(srv, b.ID, big...)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Messages) != maxMessages || capped.Messages[len(capped.Messages)-1].TS != int64(len(big)-1) {
		t.Fatalf("cap wrong: %d", len(capped.Messages))
	}

	// DeleteAll drops the server's whole chat dir.
	if err := s.DeleteAll(srv); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.List(srv); len(list) != 0 {
		t.Fatalf("DeleteAll left chats: %+v", list)
	}
}

func TestRejectsBadIDs(t *testing.T) {
	s := New(t.TempDir())
	for _, bad := range []string{"../etc", "nope", "srv-x/y", ""} {
		if _, err := s.List(bad); !errors.Is(err, ErrBadID) {
			t.Fatalf("bad server id %q accepted: %v", bad, err)
		}
	}
	for _, bad := range []string{"../x", "c/y", "nope", "x.json", ""} {
		if _, err := s.Get("srv-abc12345", bad); !errors.Is(err, ErrBadID) {
			t.Fatalf("bad conv id %q accepted: %v", bad, err)
		}
	}
}
