// This file serves the shared "Ask AI" conversations. Chats are per-SERVER and shared across the
// server's operators (owner, admin, op-level members) — the same people who may start and stop it —
// so every handler is gated by controlled(), the operator check. The model call happens in the
// browser (billed to each operator's own AI account); hosuto persists the result, so conversations
// survive reloads, every operator sees the same list, and each turn is attributed to whoever asked.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"hosuto/internal/auth"
	"hosuto/internal/chathub"
	"hosuto/internal/chatstore"
	"hosuto/internal/store"
)

// controlled resolves the server and checks the caller is an OPERATOR — owner, admin, or an op-level
// member. Identical to the rule the start/stop routes enforce.
func (s *Server) controlled(r *http.Request, u *auth.User) (store.Server, bool) {
	srv, ok := s.st.Server(r.PathValue("id"))
	if !ok {
		return store.Server{}, false
	}
	return srv, s.hasAccess(r.Context(), srv, u, accessControl)
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	list, err := s.chats.List(srv.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the chats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": list})
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	c, err := s.chats.Create(srv.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the chat")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	c, err := s.chats.Get(srv.ID, r.PathValue("cid"))
	if errors.Is(err, chatstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "No such chat")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the chat")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// appendChat records one exchange (the operator's message and the AI's answer, plus which engine and
// model produced it) into a conversation, stamping the user turn with WHO sent it, and returns the
// updated conversation.
func (s *Server) appendChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		User      string `json:"user"`
		Assistant string `json:"assistant"`
		Engine    string `json:"engine"`
		Model     string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	now := time.Now().UnixMilli()
	var msgs []chatstore.Msg
	if body.User != "" {
		// Stamp the operator's in-game name so the shared log shows the Minecraft identity behind each
		// request (the UI falls back to the username when there is no linked account). The face is
		// rendered live from the avatar endpoint, so only the name is denormalised here.
		name := ""
		if acc, ok := s.st.Account(u.Username); ok {
			name = acc.Name
		}
		msgs = append(msgs, chatstore.Msg{Role: "user", Content: body.User, Author: u.Username, Name: name, TS: now})
	}
	if body.Assistant != "" {
		msgs = append(msgs, chatstore.Msg{Role: "assistant", Content: body.Assistant, Engine: body.Engine, Model: body.Model, TS: now})
	}
	if len(msgs) == 0 {
		writeErr(w, http.StatusBadRequest, "Nothing to add")
		return
	}
	cid := r.PathValue("cid")
	c, err := s.chats.Append(srv.ID, cid, msgs...)
	if errors.Is(err, chatstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "No such chat")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the chat")
		return
	}
	// Push the updated conversation to everyone watching it live, and clear the sender's presence —
	// their request just landed, so they are no longer "asking the AI".
	if data, e := json.Marshal(c); e == nil {
		s.hub.Broadcast(cid, chathub.Event{Name: "conv", Data: data})
	}
	s.hub.SetPresence(cid, u.Username, "", "idle")
	writeJSON(w, http.StatusOK, c)
}

// chatEvents is the live event stream for one conversation (Server-Sent Events): the conversation
// itself on connect and on every new turn, plus presence ("who is typing / asking the AI"). Auth is
// the session cookie the browser sends with the stream; operator access is enforced like everywhere.
func (s *Server) chatEvents(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	cid := r.PathValue("cid")
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // ask any proxy not to buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, ch := s.hub.Subscribe(cid)
	defer s.hub.Unsubscribe(cid, id)

	// Snapshot on connect: the conversation as persisted, then who is currently active. A reconnecting
	// client thus always re-syncs to the truth (the persisted conversation is authoritative).
	if c, err := s.chats.Get(srv.ID, cid); err == nil {
		if data, e := json.Marshal(c); e == nil {
			writeSSE(w, flusher, "conv", data)
		}
	}
	if data, e := json.Marshal(map[string]any{"present": s.hub.Presence(cid)}); e == nil {
		writeSSE(w, flusher, "presence", data)
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return // dropped by the hub (slow consumer) — the client reconnects and re-snapshots
			}
			writeSSE(w, flusher, ev.Name, ev.Data)
		}
	}
}

// chatPresence records the caller's live activity ("typing" | "working" | "idle") in a conversation,
// which the hub pushes to the others watching it. Stamped with the operator's in-game name so the
// indicator reads "IchBinsHenry is typing…", not the username.
func (s *Server) chatPresence(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if _, ok := s.controlled(r, u); !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	name := ""
	if acc, ok := s.st.Account(u.Username); ok {
		name = acc.Name
	}
	s.hub.SetPresence(r.PathValue("cid"), u.Username, name, body.State)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeSSE writes one Server-Sent Event and flushes it.
func writeSSE(w http.ResponseWriter, f http.Flusher, name string, data []byte) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	f.Flush()
}

// deleteChat removes one conversation. Any operator may delete one — it is their shared surface.
func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if err := s.chats.Delete(srv.ID, r.PathValue("cid")); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not delete the chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
