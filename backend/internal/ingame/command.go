package ingame

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"hosuto/internal/access"
	"hosuto/internal/chatstore"
	"hosuto/internal/store"
)

// handleCommand runs the `!ai` grammar for one triggered line. It resolves the typing player to a
// holistic identity, enforces the operator gate (the SAME rule as the dashboard chat), then dispatches
// the verb. Everything replies privately (or publicly, per config) via tellraw; nothing here mutates
// game state directly — the AI does that through the MCP tools.
func (e *Engine) handleCommand(ctx context.Context, srv store.Server, line chatLine) {
	// Identity: prefer the UUID from the log's own anchor (rename-stable), fall back to the name.
	user, ok := e.resolvePlayer(line)
	if !ok {
		e.reply(ctx, srv, line.Player, msgError, "No linked holistic account — link your Minecraft name in the dashboard to use !ai.")
		return
	}
	u, ok := e.Access.UserAsAuth(user)
	if !ok {
		e.reply(ctx, srv, line.Player, msgError, "Your account could not be resolved.")
		return
	}
	// Operator gate: owner, admin, or an op-level member of THIS server. The real security net —
	// identity may be spoofable in chat, but access is checked against the live OS/grants.
	gctx, cancel := ctxTimeout(ctx, 10*time.Second)
	allowed := e.Access.HasAccess(gctx, srv, u, access.Control)
	cancel()
	if !allowed {
		e.reply(ctx, srv, line.Player, msgError, "Only this server's operators can use !ai.")
		return
	}

	verb, arg := splitVerb(line.Text)
	switch verb {
	case "", "help":
		e.replyHelp(ctx, srv, line.Player)
	case "list", "chats":
		e.cmdList(ctx, srv, line.Player, user)
	case "resume":
		e.cmdResume(ctx, srv, line, user, arg)
	case "end":
		e.cmdEnd(ctx, srv, line.Player, user)
	case "new":
		e.cmdNew(ctx, srv, line, user, arg)
	default:
		// Anything else is a message to the AI in the active conversation (auto-created if none).
		e.cmdSay(ctx, srv, line, user, line.Text)
	}
}

// resolvePlayer maps the typing player to a Linux username: UUID first (survives renames), then name.
func (e *Engine) resolvePlayer(line chatLine) (string, bool) {
	if line.UUID != "" {
		if u, ok := e.Access.UserByUUID(line.UUID); ok {
			return u, true
		}
	}
	return e.Access.UserByName(line.Player)
}

// splitVerb splits the text after the trigger into a lowercase first word and the remainder.
func splitVerb(text string) (verb, rest string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	if i := strings.IndexAny(text, " \t"); i >= 0 {
		return strings.ToLower(text[:i]), strings.TrimSpace(text[i+1:])
	}
	return strings.ToLower(text), ""
}

// cmdNew opens a fresh conversation, makes it the player's active one, and — if a message trailed
// `new` — sends it as the first turn.
func (e *Engine) cmdNew(ctx context.Context, srv store.Server, line chatLine, user, arg string) {
	c, err := e.Chats.Create(srv.ID)
	if err != nil {
		e.reply(ctx, srv, line.Player, msgError, "Could not open a new chat.")
		return
	}
	ps := e.stateFor(srv.ID, user)
	e.mu.Lock()
	ps.activeConv = c.ID
	e.mu.Unlock()
	if strings.TrimSpace(arg) == "" {
		e.reply(ctx, srv, line.Player, msgInfo, "New chat started. Ask with !ai <question>.")
		return
	}
	e.startTurn(ctx, srv, line, user, c.ID, arg)
}

// cmdList shows the server's conversations, newest first, as clickable lines. The player's snapshot of
// this list backs a later `resume <n>`, so the numbering stays put even as the live list reorders.
func (e *Engine) cmdList(ctx context.Context, srv store.Server, player, user string) {
	list, err := e.Chats.List(srv.ID)
	if err != nil {
		e.reply(ctx, srv, player, msgError, "Could not load the chats.")
		return
	}
	if len(list) == 0 {
		e.reply(ctx, srv, player, msgInfo, "No chats yet. Start one with !ai new.")
		return
	}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	ps := e.stateFor(srv.ID, user)
	e.mu.Lock()
	ps.listSnap = ids
	active := ps.activeConv
	e.mu.Unlock()
	e.replyList(ctx, srv, player, list, active)
}

// cmdResume re-attaches the player to the n-th conversation from their last `!ai list`.
func (e *Engine) cmdResume(ctx context.Context, srv store.Server, line chatLine, user, arg string) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 {
		e.reply(ctx, srv, line.Player, msgError, "Use !ai resume <number> — run !ai list first.")
		return
	}
	ps := e.stateFor(srv.ID, user)
	e.mu.Lock()
	snap := ps.listSnap
	e.mu.Unlock()
	if n > len(snap) {
		e.reply(ctx, srv, line.Player, msgError, "No chat with that number — run !ai list again.")
		return
	}
	cid := snap[n-1]
	c, err := e.Chats.Get(srv.ID, cid)
	if errors.Is(err, chatstore.ErrNotFound) {
		e.reply(ctx, srv, line.Player, msgError, "That chat is gone — run !ai list again.")
		return
	}
	if err != nil {
		e.reply(ctx, srv, line.Player, msgError, "Could not open that chat.")
		return
	}
	e.mu.Lock()
	ps.activeConv = c.ID
	e.mu.Unlock()
	title := c.Title
	if title == "" {
		title = "chat"
	}
	e.reply(ctx, srv, line.Player, msgInfo, "Resumed \""+title+"\". Ask with !ai <question>.")
}

// cmdEnd detaches the player from the active conversation (it stays persisted for next time).
func (e *Engine) cmdEnd(ctx context.Context, srv store.Server, player, user string) {
	ps := e.stateFor(srv.ID, user)
	e.mu.Lock()
	had := ps.activeConv != ""
	ps.activeConv = ""
	e.mu.Unlock()
	if had {
		e.reply(ctx, srv, player, msgInfo, "Left the chat. Start or resume one anytime with !ai.")
	} else {
		e.reply(ctx, srv, player, msgInfo, "You weren't in a chat.")
	}
}

// cmdSay routes a plain message into the active conversation, opening one if the player has none.
func (e *Engine) cmdSay(ctx context.Context, srv store.Server, line chatLine, user, text string) {
	ps := e.stateFor(srv.ID, user)
	e.mu.Lock()
	cid := ps.activeConv
	e.mu.Unlock()
	if cid == "" {
		c, err := e.Chats.Create(srv.ID)
		if err != nil {
			e.reply(ctx, srv, line.Player, msgError, "Could not open a chat.")
			return
		}
		cid = c.ID
		e.mu.Lock()
		ps.activeConv = cid
		e.mu.Unlock()
	}
	e.startTurn(ctx, srv, line, user, cid, text)
}
