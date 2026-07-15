// Package chathub is the real-time layer for the shared "Ask AI" chats: an in-memory pub/sub that
// pushes new turns and presence ("who is typing / asking the AI") to every operator watching a
// conversation, over Server-Sent Events.
//
// It is deliberately in-memory and best-effort. hosuto is a single daemon, so there is one hub; the
// persisted conversation (chatstore) remains the source of truth, and the hub only carries live
// notifications. A subscriber whose buffer overflows (a slow client) is dropped, its SSE response
// ends, and its EventSource reconnects and re-snapshots — so a missed push self-heals rather than
// corrupting state.
package chathub

import (
	"encoding/json"
	"sync"
	"time"
)

const (
	// presenceTTL: a heartbeat older than this is treated as gone (covers a client that vanished
	// without saying goodbye). Clients heartbeat every ~3s, so this tolerates one missed beat.
	presenceTTL = 8 * time.Second
	// sweepInterval expires stale presence and pings live streams (so proxies keep the connection).
	sweepInterval = 3 * time.Second
	// buffer is the per-subscriber event queue; a slower consumer than this is dropped to reconnect.
	buffer = 32
)

// Event is one SSE event: Name is the SSE "event:" field, Data the JSON payload.
type Event struct {
	Name string
	Data json.RawMessage
}

// PresenceEntry is one operator's live activity in a conversation.
type PresenceEntry struct {
	Author string `json:"author"`
	Name   string `json:"name,omitempty"`
	State  string `json:"state"` // "typing" | "working"
}

type present struct {
	name  string
	state string
	ts    time.Time
}

// Hub fans conversation events out to subscribers and tracks presence.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[int]chan Event // convID -> subID -> queue
	pres map[string]map[string]present // convID -> author -> presence
	next int
}

// New builds a hub and starts its background sweeper.
func New() *Hub {
	h := &Hub{subs: map[string]map[int]chan Event{}, pres: map[string]map[string]present{}}
	go h.sweep()
	return h
}

// Subscribe registers a listener for a conversation and returns its id and event queue.
func (h *Hub) Subscribe(conv string) (int, <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	ch := make(chan Event, buffer)
	if h.subs[conv] == nil {
		h.subs[conv] = map[int]chan Event{}
	}
	h.subs[conv][id] = ch
	return id, ch
}

// Unsubscribe removes a listener and closes its queue (no-op if already dropped by the hub).
func (h *Hub) Unsubscribe(conv string, id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.subs[conv]; m != nil {
		if ch, ok := m[id]; ok {
			close(ch)
			delete(m, id)
		}
		if len(m) == 0 {
			delete(h.subs, conv)
		}
	}
}

// Broadcast delivers an event to every subscriber of a conversation.
func (h *Hub) Broadcast(conv string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fan(conv, ev)
}

// fan sends to each subscriber non-blocking; a full queue means the client fell behind, so it is
// dropped (its channel closed) to force a reconnect + re-snapshot. Caller holds the lock.
func (h *Hub) fan(conv string, ev Event) {
	m := h.subs[conv]
	for id, ch := range m {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(m, id)
		}
	}
	if len(m) == 0 {
		delete(h.subs, conv)
	}
}

// SetPresence records (or clears, with state "idle"/"") an operator's activity and pushes the new set.
func (h *Hub) SetPresence(conv, author, name, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pres[conv] == nil {
		h.pres[conv] = map[string]present{}
	}
	if state == "" || state == "idle" {
		delete(h.pres[conv], author)
	} else {
		h.pres[conv][author] = present{name: name, state: state, ts: time.Now()}
	}
	if len(h.pres[conv]) == 0 {
		delete(h.pres, conv)
	}
	h.fanPresence(conv)
}

// Presence returns the current (non-expired) activity for a conversation — used for the snapshot a
// new subscriber gets on connect.
func (h *Hub) Presence(conv string) []PresenceEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.activeLocked(conv)
}

func (h *Hub) activeLocked(conv string) []PresenceEntry {
	out := []PresenceEntry{}
	now := time.Now()
	for author, p := range h.pres[conv] {
		if now.Sub(p.ts) <= presenceTTL {
			out = append(out, PresenceEntry{Author: author, Name: p.name, State: p.state})
		}
	}
	return out
}

func (h *Hub) fanPresence(conv string) {
	data, _ := json.Marshal(map[string]any{"present": h.activeLocked(conv)})
	h.fan(conv, Event{Name: "presence", Data: data})
}

// sweep expires stale presence (pushing the change) and pings live streams so intermediaries don't
// close an idle connection.
func (h *Hub) sweep() {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	ping := Event{Name: "ping", Data: json.RawMessage(`{}`)}
	for range t.C {
		h.mu.Lock()
		now := time.Now()
		for conv, m := range h.pres {
			changed := false
			for author, p := range m {
				if now.Sub(p.ts) > presenceTTL {
					delete(m, author)
					changed = true
				}
			}
			if len(m) == 0 {
				delete(h.pres, conv)
			}
			if changed {
				h.fanPresence(conv)
			}
		}
		for conv := range h.subs {
			h.fan(conv, ping)
		}
		h.mu.Unlock()
	}
}
