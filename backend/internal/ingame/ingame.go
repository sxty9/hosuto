// Package ingame gives Minecraft operators the shared "Ask AI" chats from inside the game. A player
// types `!ai …` in chat; hosuto reads the line from the server's log, runs one agentic turn on that
// operator's own aigentic credential (via the internal M2M endpoint) against hosuto's MCP tools, and
// answers privately with `tellraw`. The conversations are the SAME per-server threads the dashboard
// shows (chatstore + chathub), so an in-game turn appears live in the browser and vice versa — one
// data path, two surfaces.
//
// Nothing here escalates privilege or duplicates the authorisation rule: identity comes from the log's
// own UUID anchor (name is only a fallback), and every command is gated by the one operator check in
// package access — the same rule the start/stop routes and the dashboard chat enforce.
package ingame

import (
	"context"
	"strings"
	"sync"
	"time"

	"hosuto/internal/access"
	"hosuto/internal/aigentic"
	"hosuto/internal/chathub"
	"hosuto/internal/chatstore"
	"hosuto/internal/hconfig"
	"hosuto/internal/mcp"
	"hosuto/internal/runtime"
	"hosuto/internal/store"
)

// Deps are the collaborators the engine borrows — every one already exists and is shared with the
// HTTP surface, so the in-game path reuses the exact same store, runtime, access rule, chats, live
// hub, MCP token mint and aigentic client rather than standing up parallels.
type Deps struct {
	Store    *store.Store
	Mgr      *runtime.Manager
	Access   *access.Resolver
	Chats    *chatstore.Store
	Hub      *chathub.Hub
	Tokens   *mcp.TokenStore
	Aigentic *aigentic.Client
	Cfg      *hconfig.Config
}

// Engine owns the per-server log followers and the per-(server,operator) CLI state. It is safe for
// concurrent use: one goroutine per followed server feeds lines in, and turns run on their own
// goroutines, all coordinated through mu.
type Engine struct {
	Deps

	mu      sync.Mutex
	players map[string]*playerState // key: serverID \x00 linux-user
}

// playerState is one operator's ephemeral CLI context on one server. The conversations themselves
// persist in chatstore; this only tracks which one the player is "in", the snapshot the last `!ai
// list` showed (so `resume <n>` is stable while the live list reorders), and whether a turn is running.
type playerState struct {
	activeConv string   // conversation the player is attached to; "" => none
	listSnap   []string // conversation IDs from the player's last `!ai list`, for stable resume <n>
	busy       bool     // a turn is in flight; a second request is refused until it finishes
}

// New builds an engine. It does not start following; call Run for that.
func New(d Deps) *Engine {
	return &Engine{Deps: d, players: map[string]*playerState{}}
}

// stateFor returns (creating if needed) the CLI state for one operator on one server.
func (e *Engine) stateFor(serverID, user string) *playerState {
	key := serverID + "\x00" + user
	e.mu.Lock()
	defer e.mu.Unlock()
	ps, ok := e.players[key]
	if !ok {
		ps = &playerState{}
		e.players[key] = ps
	}
	return ps
}

// --- live configuration (read from the central Configuration tab every time, no restart) ---

func (e *Engine) enabled() bool { return e.Cfg.Bool("ingameEnabled", true) }

func (e *Engine) trigger() string {
	t := strings.TrimSpace(e.Cfg.String("ingameTrigger", "!ai"))
	if t == "" {
		return "!ai"
	}
	return t
}

func (e *Engine) pingTrigger() string {
	t := strings.TrimSpace(e.Cfg.String("ingamePingTrigger", "!ping"))
	if t == "" {
		return "!ping"
	}
	return t
}

func (e *Engine) interval() time.Duration {
	s := e.Cfg.Int("ingameInterval", 10)
	if s < 2 {
		s = 2
	}
	return time.Duration(s) * time.Second
}

func (e *Engine) aiTimeout() time.Duration {
	s := e.Cfg.Int("ingameAiTimeout", 120)
	if s < 15 {
		s = 15
	}
	return time.Duration(s) * time.Second
}

func (e *Engine) replyPrivate() bool { return e.Cfg.Bool("ingameReplyPrivate", true) }

func (e *Engine) tokenTTL() time.Duration {
	m := e.Cfg.Int("ingameTokenTTLMin", 5)
	if m < 1 {
		m = 1
	}
	return time.Duration(m) * time.Minute
}

func nowMS() int64 { return time.Now().UnixMilli() }

// ctxTimeout is a small helper so callers read clearly; the daemon runs real wall-clock time.
func ctxTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
