package ingame

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"hosuto/internal/aigentic"
	"hosuto/internal/chathub"
	"hosuto/internal/chatstore"
	"hosuto/internal/store"
)

// engineKind is the aigentic engine the in-game CLI forces: claude-cli is the only leaf that is a
// native MCP client, so it is the one that can actually drive hosuto's tools. Its per-user credential
// (subscription token or API key) bills the operator, exactly like the dashboard chat.
const engineKind = "claude-cli"

// startTurn runs one AI exchange for a player in a conversation. At most one turn per player runs at a
// time; a second request while busy is refused rather than queued. The user turn is persisted and
// broadcast BEFORE the model call (so the dashboard sees the question live); on failure no assistant
// line is written (a bad answer must not poison the thread's context), only a private error.
func (e *Engine) startTurn(ctx context.Context, srv store.Server, line chatLine, user, cid, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	ps := e.stateFor(srv.ID, user)
	e.mu.Lock()
	if ps.busy {
		e.mu.Unlock()
		e.reply(ctx, srv, line.Player, msgInfo, "Still working on your last question — one moment.")
		return
	}
	ps.busy = true
	e.mu.Unlock()

	go e.runTurn(srv, line, user, cid, text)
}

// runTurn is the turn body, on its own goroutine. It is deliberately independent of the follower's
// context: once started, it completes and persists even if the server stops mid-answer, so the
// dashboard stays authoritative.
func (e *Engine) runTurn(srv store.Server, line chatLine, user, cid, text string) {
	ps := e.stateFor(srv.ID, user)
	defer func() {
		e.mu.Lock()
		ps.busy = false
		e.mu.Unlock()
		e.Hub.SetPresence(cid, user, line.Player, "idle")
	}()

	// Persist + broadcast the operator's turn, stamped with WHO asked (Linux user + in-game name).
	conv, err := e.Chats.Append(srv.ID, cid, chatstore.Msg{
		Role: "user", Content: text, Author: user, Name: line.Player, TS: nowMS(),
	})
	if err != nil {
		e.reply(context.Background(), srv, line.Player, msgError, "Could not save your message.")
		return
	}
	e.broadcast(cid, conv)
	e.Hub.SetPresence(cid, user, line.Player, "working")

	// Mint a short-lived MCP token scoped to THIS server, on the operator's behalf. aigentic hands it
	// to the claude-cli leaf, which presents it to hosuto's MCP endpoint (provider "hosuto"); the
	// URL lives server-side in AIGENTIC_MCP_PROVIDERS, so nothing untrusted picks the target.
	token, _, err := e.Tokens.Mint(user, srv.ID, e.tokenTTL())
	if err != nil {
		e.reply(context.Background(), srv, line.Player, msgError, "Could not authorize the AI for this server.")
		return
	}

	ctx, cancel := ctxTimeout(context.Background(), e.aiTimeout())
	defer cancel()
	res, err := e.Aigentic.Run(ctx, user, aigentic.Req{
		Kind:   engineKind,
		System: systemPrompt(srv, line.Player),
		Prompt: transcript(conv.Messages),
		MCP:    []aigentic.MCPRef{{Name: "hosuto", Token: token}},
	})
	if err != nil {
		e.reply(context.Background(), srv, line.Player, msgError, aiErrorText(err))
		return
	}
	answer := strings.TrimSpace(res.Output)
	if answer == "" {
		e.reply(context.Background(), srv, line.Player, msgError, "The AI returned nothing — try rephrasing.")
		return
	}

	conv, err = e.Chats.Append(srv.ID, cid, chatstore.Msg{
		Role: "assistant", Content: answer, Engine: res.Engine, Model: res.Model, TS: nowMS(),
	})
	if err == nil {
		e.broadcast(cid, conv)
	}
	e.replyAnswer(context.Background(), srv, line.Player, answer)
}

// broadcast pushes the updated conversation to everyone watching it live (the dashboard SSE stream),
// so an in-game turn appears in the browser instantly — same event shape the HTTP path emits.
func (e *Engine) broadcast(cid string, conv chatstore.Conversation) {
	if data, err := json.Marshal(conv); err == nil {
		e.Hub.Broadcast(cid, chathub.Event{Name: "conv", Data: data})
	}
}

// transcript renders the conversation as a plain dialogue for the (stateless) claude-cli leaf, so the
// AI has the full context of the thread, not just the latest line.
func transcript(msgs []chatstore.Msg) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			who := m.Name
			if who == "" {
				who = m.Author
			}
			if who == "" {
				who = "Operator"
			}
			b.WriteString(who)
			b.WriteString(": ")
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString("AI: ")
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// systemPrompt binds the turn to one server and to the in-game surface (plain, short answers).
func systemPrompt(srv store.Server, player string) string {
	return "You are the in-game AI assistant for the Minecraft server \"" + srv.Slug + "\" (id " + srv.ID + "). " +
		"You are acting for " + player + ", an operator of this server, who is chatting from inside the game. " +
		"Use the hosuto MCP tools to inspect and control THIS server when it helps answer or act. " +
		"Reply in plain text fit for Minecraft chat: no markdown, short sentences, only a few lines."
}

// aiErrorText maps a turn failure to a short, actionable in-game message.
func aiErrorText(err error) string {
	switch {
	case errors.Is(err, aigentic.ErrNoCredential):
		return "No Claude credential linked — connect one in the aigentic tab to use !ai."
	case errors.Is(err, aigentic.ErrDisabled):
		return "The AI service is not available right now."
	case errors.Is(err, context.DeadlineExceeded):
		return "The AI took too long — try a shorter question."
	default:
		return "The AI could not answer right now."
	}
}
