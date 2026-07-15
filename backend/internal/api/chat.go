// This file serves the shared "Ask AI" thread. The chat is per-SERVER and shared across the server's
// operators (owner, admin, op-level members) — the same people who may start and stop it — so it is
// gated by controlled(), the operator check, not by mere visibility. The model call happens in the
// browser (billed to each operator's own AI account); hosuto only persists the result, so the thread
// survives reloads and every operator sees the same history, attributed to whoever asked.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"hosuto/internal/auth"
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

func (s *Server) getChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	msgs, err := s.chats.Load(srv.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// postChat appends one exchange (the operator's message and the AI's answer) to the shared thread,
// stamping the user turn with WHO sent it, and returns the full thread so the caller renders the
// authoritative state (which may also carry another operator's turns that landed meanwhile).
func (s *Server) postChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	var body struct {
		User      string `json:"user"`
		Assistant string `json:"assistant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	now := time.Now().UnixMilli()
	var msgs []chatstore.Msg
	if body.User != "" {
		msgs = append(msgs, chatstore.Msg{Role: "user", Content: body.User, Author: u.Username, TS: now})
	}
	if body.Assistant != "" {
		msgs = append(msgs, chatstore.Msg{Role: "assistant", Content: body.Assistant, TS: now})
	}
	if len(msgs) == 0 {
		writeErr(w, http.StatusBadRequest, "Nothing to add")
		return
	}
	full, err := s.chats.Append(srv.ID, msgs...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save the chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": full})
}

// clearChat wipes the shared thread. Any operator may clear it — it is their shared surface.
func (s *Server) clearChat(w http.ResponseWriter, r *http.Request, u *auth.User) {
	srv, ok := s.controlled(r, u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	if err := s.chats.Delete(srv.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not clear the chat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
