// Package chatstore persists the "Ask AI" conversations for a server.
//
// A server has MANY conversations, and they are SHARED: every operator of the server (its owner, an
// admin, or an op-level member) sees the same list, opens the same threads, and appends to them, and
// it all survives restarts. That is the difference from a private per-user chat — the conversations
// belong to the SERVER, so hosuto owns them (no other service knows a server's operator set), and
// access is gated by the same rule that gates starting and stopping the server.
//
// Storage is one JSON file per conversation, under a per-server directory. One file per conversation
// keeps a write to one thread from contending with another, makes listing cheap, and makes deleting a
// conversation (or a whole server's chats) a single unlink / RemoveAll. Writes are atomic
// (temp→fsync→rename), guarded by one mutex, with the daemon as the sole writer.
package chatstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxMessages caps a single conversation so a long-running thread cannot grow without bound. The
// tail survives (that is what anyone reads, and what the model is sent as context); the head drops.
const maxMessages = 400

var (
	serverRe = regexp.MustCompile(`^srv-[0-9a-fA-F]+$`)
	convRe   = regexp.MustCompile(`^c[0-9a-f]{8,}$`)
)

var (
	// ErrBadID is returned for a server or conversation id that is not a safe filename.
	ErrBadID = errors.New("invalid id")
	// ErrNotFound is returned for a conversation that does not exist.
	ErrNotFound = errors.New("not found")
)

// Msg is one turn in a conversation.
type Msg struct {
	Role    string `json:"role"`             // "user" | "assistant"
	Content string `json:"content"`          // the text
	Author  string `json:"author,omitempty"` // the operator who sent a user turn (empty for the assistant)
	Engine  string `json:"engine,omitempty"` // assistant turn: the aigentic engine that answered
	Model   string `json:"model,omitempty"`  // assistant turn: the concrete model that answered
	TS      int64  `json:"ts"`               // epoch ms
}

// Conversation is one shared thread.
type Conversation struct {
	ID       string `json:"id"`
	Title    string `json:"title"`   // derived from the first user turn
	Updated  int64  `json:"updated"` // epoch ms of the last activity (drives list ordering)
	Messages []Msg  `json:"messages"`
}

// Summary is the lightweight sidebar view of a conversation (no message bodies).
type Summary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Updated int64  `json:"updated"`
	Count   int    `json:"count"`
}

// Store is the daemon's sole writer of per-server chat conversations.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New builds a store rooted at dir (created on first write).
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) serverDir(serverID string) (string, error) {
	if !serverRe.MatchString(serverID) {
		return "", ErrBadID
	}
	return filepath.Join(s.dir, serverID), nil
}

func (s *Store) convPath(serverID, convID string) (string, error) {
	d, err := s.serverDir(serverID)
	if err != nil {
		return "", err
	}
	if !convRe.MatchString(convID) {
		return "", ErrBadID
	}
	return filepath.Join(d, convID+".json"), nil
}

// List returns a server's conversation summaries, newest first.
func (s *Store) List(serverID string) ([]Summary, error) {
	d, err := s.serverDir(serverID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	des, err := os.ReadDir(d)
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		c, err := readConv(filepath.Join(d, de.Name()))
		if err != nil {
			continue // a half-written or foreign file is skipped, never fatal to the list
		}
		out = append(out, Summary{ID: c.ID, Title: c.Title, Updated: c.Updated, Count: len(c.Messages)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

// Create makes a new empty conversation and returns it.
func (s *Store) Create(serverID string) (Conversation, error) {
	if !serverRe.MatchString(serverID) {
		return Conversation{}, ErrBadID
	}
	c := Conversation{ID: genID(), Updated: time.Now().UnixMilli(), Messages: []Msg{}}
	p, err := s.convPath(serverID, c.ID)
	if err != nil {
		return Conversation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeConv(p, c); err != nil {
		return Conversation{}, err
	}
	return c, nil
}

// Get returns one conversation, or ErrNotFound.
func (s *Store) Get(serverID, convID string) (Conversation, error) {
	p, err := s.convPath(serverID, convID)
	if err != nil {
		return Conversation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readConv(p)
	if errors.Is(err, os.ErrNotExist) {
		return Conversation{}, ErrNotFound
	}
	return c, err
}

// Append adds messages to a conversation, gives it a title from its first user turn if it has none,
// stamps it updated, trims it to the cap, persists it, and returns it. Returns ErrNotFound if the
// conversation was deleted (e.g. by another operator) in the meantime.
func (s *Store) Append(serverID, convID string, msgs ...Msg) (Conversation, error) {
	p, err := s.convPath(serverID, convID)
	if err != nil {
		return Conversation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readConv(p)
	if errors.Is(err, os.ErrNotExist) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, err
	}
	c.Messages = append(c.Messages, msgs...)
	if len(c.Messages) > maxMessages {
		c.Messages = c.Messages[len(c.Messages)-maxMessages:]
	}
	if c.Title == "" {
		for _, m := range c.Messages {
			if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
				c.Title = titleOf(m.Content)
				break
			}
		}
	}
	c.Updated = time.Now().UnixMilli()
	if err := writeConv(p, c); err != nil {
		return Conversation{}, err
	}
	return c, nil
}

// Delete removes one conversation.
func (s *Store) Delete(serverID, convID string) error {
	p, err := s.convPath(serverID, convID)
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

// DeleteAll removes a server's whole chat directory (called when the server is deleted).
func (s *Store) DeleteAll(serverID string) error {
	d, err := s.serverDir(serverID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(d); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// titleOf derives a sidebar title from the first user turn: its first line, trimmed to 48 runes.
func titleOf(content string) string {
	line := strings.TrimSpace(content)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	r := []rune(line)
	if len(r) > 48 {
		return string(r[:48]) + "…"
	}
	return line
}

func genID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "c" + hex.EncodeToString(b)
}

func readConv(path string) (Conversation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Conversation{}, err
	}
	var c Conversation
	if err := json.Unmarshal(b, &c); err != nil {
		return Conversation{}, err
	}
	if c.Messages == nil {
		c.Messages = []Msg{}
	}
	return c, nil
}

// writeConv persists a conversation atomically: temp file → fsync → rename, mode 0600.
func writeConv(path string, c Conversation) error {
	b, err := json.MarshalIndent(c, "", "  ")
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
