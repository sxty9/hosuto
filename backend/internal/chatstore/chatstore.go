// Package chatstore persists the "Ask AI" conversation for a server.
//
// The chat is a SHARED, per-server thread: every operator of a server (its owner, an admin, or an
// op-level member) sees and appends to the same log, and it survives restarts. That is the whole
// difference from a private per-user chat — the conversation belongs to the SERVER, so hosuto owns
// it (no other service knows a server's operator set), and access is gated by the same rule that
// gates starting and stopping the server.
//
// It follows hosuto's own store shape: one flat JSON file per server, an atomic temp→fsync→rename
// write, one mutex, the daemon as the sole writer. Each server's log lives in its own file so a
// write to one never contends with another, and deleting a server drops exactly one file.
package chatstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// maxMessages caps a thread so an old, busy server's log cannot grow without bound. When it is
// exceeded the oldest messages are dropped — the tail is what anyone reads, and the model is sent
// the tail as context, so trimming the head loses nothing anyone was looking at.
const maxMessages = 400

// idRe bounds a server id used as a filename. The id is always "srv-"+hex from the store, but this
// is validated here too so a bad caller can never write outside the chats directory.
var idRe = regexp.MustCompile(`^srv-[0-9a-fA-F]+$`)

// ErrBadID is returned for a server id that is not a safe filename.
var ErrBadID = errors.New("invalid server id")

// Msg is one turn in the shared thread.
type Msg struct {
	Role    string `json:"role"`             // "user" | "assistant"
	Content string `json:"content"`          // the text
	Author  string `json:"author,omitempty"` // the operator who sent a user turn (empty for the assistant)
	Engine  string `json:"engine,omitempty"` // assistant turn: the aigentic engine that answered
	Model   string `json:"model,omitempty"`  // assistant turn: the concrete model that answered
	TS      int64  `json:"ts"`               // epoch ms
}

// Store is the daemon's sole writer of per-server chat logs.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New builds a store rooted at dir (created on first write).
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(serverID string) (string, error) {
	if !idRe.MatchString(serverID) {
		return "", ErrBadID
	}
	return filepath.Join(s.dir, serverID+".json"), nil
}

// Load returns a server's thread, or an empty slice when there is none yet.
func (s *Store) Load(serverID string) ([]Msg, error) {
	p, err := s.path(serverID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return read(p)
}

// Append adds messages to a server's thread, trims it to the cap, persists it, and returns the full
// thread. The whole thread is returned so a caller renders the shared, authoritative state (which
// may also carry another operator's turns that arrived in the meantime).
func (s *Store) Append(serverID string, msgs ...Msg) ([]Msg, error) {
	p, err := s.path(serverID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := read(p)
	if err != nil {
		return nil, err
	}
	cur = append(cur, msgs...)
	if len(cur) > maxMessages {
		cur = cur[len(cur)-maxMessages:]
	}
	if err := write(p, cur); err != nil {
		return nil, err
	}
	return cur, nil
}

// Delete drops a server's thread (called when the server itself is deleted).
func (s *Store) Delete(serverID string) error {
	p, err := s.path(serverID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func read(path string) ([]Msg, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || len(b) == 0 {
		return []Msg{}, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []Msg
	if err := json.Unmarshal(b, &msgs); err != nil {
		return nil, err
	}
	if msgs == nil {
		msgs = []Msg{}
	}
	return msgs, nil
}

// write persists the thread atomically: temp file → fsync → rename, mode 0600.
func write(path string, msgs []Msg) error {
	b, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".chat-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
