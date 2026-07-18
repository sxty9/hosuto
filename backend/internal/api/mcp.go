// This file is hosuto's MCP face: the same server operations the REST surface exposes, offered as
// Model-Context-Protocol tools so an AI — the "Ask AI" tab, or an external client like Claude
// Desktop/Code — can drive a server directly. It is deliberately thin: every tool resolves its target
// through mcpTarget (the one access rule) and then calls a core method shared with the REST handlers
// (ops.go). Nothing here re-implements authorisation or a server operation; the MCP endpoint is a
// second door onto the same room, not a parallel house.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"hosuto/internal/auth"
	"hosuto/internal/files"
	"hosuto/internal/mcp"
	"hosuto/internal/rights"
	"hosuto/internal/runtime"
	"hosuto/internal/store"
)

// mcpCaller is the opaque identity the registry hands every tool: the resolved holistic user, plus
// the server this connection is bound to (empty for an account-wide token, which must then be told a
// server per call).
type mcpCaller struct {
	user  *auth.User
	scope string // server id, or ""
}

// authenticateMCP turns a request into an mcpCaller. The two credentials it accepts — a minted bearer
// token from an external MCP client (Claude Desktop/Code, or aigentic's `claude` binary), or a
// same-origin browser's session cookie — are read by resolveCaller, which the REST guard uses too, so
// the two doors cannot drift apart on who a caller is. Scope enforcement differs by door and stays
// with each: here mcpTarget applies it per tool; there guard applies it per route.
func (s *Server) authenticateMCP(r *http.Request) (any, error) {
	u, scope, _, err := s.resolveCaller(r)
	if err != nil {
		return nil, err
	}
	return &mcpCaller{user: u, scope: scope}, nil
}

// mcpTarget resolves the server a tool acts on, from the connection's bound scope and/or an explicit
// "server" argument, and checks the caller's access at the required level. This is the single choke
// point every tool passes through.
func (s *Server) mcpTarget(ctx context.Context, c *mcpCaller, ref string, level accessLevel) (store.Server, error) {
	if ref == "" {
		ref = c.scope
	}
	if ref == "" {
		return store.Server{}, errors.New(`no server given: pass "server" (an id or slug), or use a server-bound token`)
	}
	srv, ok := s.findServer(ref)
	if !ok {
		return store.Server{}, errors.New("no such server: " + ref)
	}
	if c.scope != "" && srv.ID != c.scope {
		return store.Server{}, errors.New("this connection is bound to a different server")
	}
	if !s.hasAccess(ctx, srv, c.user, level) {
		return store.Server{}, errors.New("you do not have permission for this server")
	}
	return srv, nil
}

// mcp builds the tool registry once, at wiring time.
func (s *Server) mcp() *mcp.Registry {
	reg := mcp.NewRegistry(service, version)

	reg.Register(mcp.Tool{
		Name:        "server_list",
		Description: "List the Minecraft servers you can see (the ones you own, plus any you were added to). Use this first to discover a server's id, slug, address and run state.",
		InputSchema: schema(nil),
		Handler: func(ctx context.Context, cAny any, _ json.RawMessage) (any, error) {
			c := cAny.(*mcpCaller)
			list := []map[string]any{}
			for _, srv := range s.st.Servers() {
				if c.scope != "" && srv.ID != c.scope {
					continue
				}
				if !s.hasAccess(ctx, srv, c.user, accessVisible) {
					continue
				}
				owned := srv.Owner == c.user.Username || c.user.Can(rights.GroupAdmin)
				level := ""
				if !owned {
					level = s.resolve(ctx, srv)[c.user.Username]
				}
				list = append(list, map[string]any{
					"id": srv.ID, "slug": srv.Slug, "name": srv.Name, "host": srv.Host,
					"owner": srv.Owner, "mcVersion": srv.MCVersion, "loader": srv.Loader,
					"state": s.rt.State(ctx, srv), "owned": owned, "level": level,
				})
			}
			return map[string]any{"servers": list}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "server_status",
		Description: "Get a server's run state and live reachability: whether the unit is up, whether a real Server List Ping succeeds, and the current/maximum player count. Call this when the user asks whether the server is up or reachable.",
		InputSchema: schema(map[string]any{"server": serverProp}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, argServer(args), accessVisible)
			if err != nil {
				return nil, err
			}
			return map[string]any{"server": srv.Slug, "host": srv.Host, "status": s.rt.Status(ctx, srv)}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "players_online",
		Description: "List who is currently online on a server (from a live Server List Ping). Call this when the user asks who is playing or how many players are on.",
		InputSchema: schema(map[string]any{"server": serverProp}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, argServer(args), accessVisible)
			if err != nil {
				return nil, err
			}
			st := s.rt.Status(ctx, srv)
			players := st.Sample
			if players == nil {
				players = []string{}
			}
			return map[string]any{"reachable": st.Reachable, "online": st.Online, "max": st.Max, "players": players}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "logs_tail",
		Description: "Read the tail of a server's console log (latest.log). Call this to diagnose why a server crashed or failed to start. Owner or admin only.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"lines":  map[string]any{"type": "integer", "description": "How many trailing lines to return (default 200, max 2000)."},
		}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct {
				Server string `json:"server"`
				Lines  int    `json:"lines"`
			}
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			out, err := s.rt.LogTail(srv, a.Lines)
			if err != nil {
				return nil, errors.New("no log to read yet — the server may not have started")
			}
			return map[string]any{"log": out}, nil
		},
	})

	reg.Register(s.lifecycleTool("start", "Start the Minecraft server (bring the world up). Owner, admin, or an op-level member.", "started"))
	reg.Register(s.lifecycleTool("stop", "Stop the Minecraft server (shut the world down gracefully). This disconnects players — confirm with the user before calling it. Owner, admin, or an op-level member.", "stopped"))
	reg.Register(s.lifecycleTool("restart", "Restart the Minecraft server. This disconnects players briefly — confirm with the user first. Owner, admin, or an op-level member.", "restarted"))

	reg.Register(mcp.Tool{
		Name: "server_command",
		Description: "Run a Minecraft console command on a RUNNING server (via RCON) and return the console output. " +
			"This is how you inspect or change live world state that the specific tools do not cover — e.g. " +
			"\"time query daytime\", \"time set day\", \"weather clear\", \"gamerule keepInventory true\", " +
			"\"difficulty peaceful\", \"give <player> minecraft:diamond 1\", \"tp <player> 0 100 0\", \"say hello\". " +
			"One command per call; the leading slash is optional. The server must be up (start it first). " +
			"To stop the server use server_stop, not this. Owner, admin, or an op-level member.",
		InputSchema: schema(map[string]any{
			"server":  serverProp,
			"command": map[string]any{"type": "string", "description": `The console command, without the leading slash, e.g. "time query daytime".`},
		}, "command"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, Command string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessControl)
			if err != nil {
				return nil, err
			}
			cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(a.Command), "/"))
			if cmd == "" {
				return nil, errors.New(`which command? give a console command, e.g. "time query daytime"`)
			}
			if len(cmd) > 1200 {
				return nil, errors.New("command too long for a single RCON frame")
			}
			// `stop` exits the JVM outside hosuto's managed lifecycle; the dedicated tool does it cleanly.
			if strings.EqualFold(strings.Fields(cmd)[0], "stop") {
				return nil, errors.New("use server_stop to stop the server")
			}
			replies, ok, err := s.rt.Command(ctx, srv, cmd)
			if err != nil {
				return nil, errors.New("the server rejected the command")
			}
			if !ok {
				return nil, errors.New("the server is not running (or its console is not up yet) — start it first")
			}
			out := ""
			if len(replies) > 0 {
				out = replies[0]
			}
			return map[string]any{"command": cmd, "output": out}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "server_ping",
		Description: "Ping a running server and report its round-trip latency in milliseconds. This is the server's own responsiveness measured from the host (it climbs when the server is lagging), not a remote player's client ping. Call this when the user asks how fast the server responds or whether it is lagging.",
		InputSchema: schema(map[string]any{"server": serverProp}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, argServer(args), accessVisible)
			if err != nil {
				return nil, err
			}
			lat, ok := s.rt.Ping(ctx, srv)
			if !ok {
				return map[string]any{"reachable": false}, nil
			}
			return map[string]any{"reachable": true, "latencyMs": lat.Milliseconds()}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "autostart_set",
		Description: "Turn on or off whether the server comes up automatically with the operating system. This does NOT start or stop the server now. Owner only.",
		InputSchema: schema(map[string]any{
			"server":  serverProp,
			"enabled": map[string]any{"type": "boolean", "description": "true to start with the OS, false to not."},
		}, "enabled"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct {
				Server  string `json:"server"`
				Enabled bool   `json:"enabled"`
			}
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			if err := s.rt.SetAutostart(ctx, srv, a.Enabled); err != nil {
				return nil, err
			}
			return map[string]any{"autostart": a.Enabled}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name: "whitelist_add",
		Description: "Let someone join a server. Give `user` for a member of this holistic instance — only works for someone you are acquainted with (a shared contact group) who has linked a Minecraft account. " +
			"Give `minecraft` to admit a Minecraft account by its in-game name, for someone who has no account here at all; they get to join and nothing else. Exactly one of the two. Owner or admin.",
		InputSchema: schema(map[string]any{
			"server":    serverProp,
			"user":      map[string]any{"type": "string", "description": "The holistic (Linux) username to add."},
			"minecraft": map[string]any{"type": "string", "description": "An in-game Minecraft name, for someone with no holistic account."},
			"level":     map[string]any{"type": "string", "enum": []string{"play", "op"}, "description": "play (default) or op (may start/stop the server)."},
		}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct {
				Server, User, Level, Minecraft string
			}
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			user, mc := strings.TrimSpace(a.User), strings.TrimSpace(a.Minecraft)
			// Naming both is ambiguous about WHO is being added, and naming neither says nothing at
			// all. Refusing beats guessing: the wrong guess writes an access rule.
			if (user == "") == (mc == "") {
				return nil, errors.New("name exactly one of: user (a holistic username) or minecraft (an in-game name)")
			}
			level := a.Level
			if level == "" {
				level = "play"
			}
			if !store.ValidLevel(level) {
				return nil, errors.New("level must be play or op")
			}

			grant := store.Grant{Kind: "adhoc", Level: level, Members: []string{user}, Label: user}
			who := user
			if mc != "" {
				if grant, err = s.admitMinecraft(ctx, srv, mc, level); err != nil {
					return nil, err
				}
				who = grant.Label
			} else if err := s.canAdd(c.user.Username, user, c.user.Can(rights.GroupAdmin)); err != nil {
				return nil, err
			}
			if _, err := s.st.AddGrant(srv.ID, grant); err != nil {
				return nil, err
			}
			fresh, _ := s.st.Server(srv.ID)
			if err := s.applyMembers(ctx, fresh); err != nil {
				return nil, err
			}
			s.notifyAdded(ctx, fresh, c.user.Username)
			return "added " + who + " (" + level + ")", nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "whitelist_remove",
		Description: "Take someone off a server, by holistic username or by in-game Minecraft name — whichever way they were added. Cannot remove someone who is on via a contact or system group. Owner or admin.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"user":   map[string]any{"type": "string", "description": "A holistic (Linux) username, or an in-game Minecraft name."},
		}, "user"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, User string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			if err := s.revokeUser(ctx, srv, a.User); err != nil {
				return nil, err
			}
			return "removed " + a.User, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "join_policy_set",
		Description: "Set who may join a server: \"whitelist\" (only listed members) or \"open\" (anyone). Takes effect after a restart. Owner or admin.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"policy": map[string]any{"type": "string", "enum": []string{"whitelist", "open"}},
		}, "policy"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, Policy string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			srv, err = s.applyPolicy(ctx, srv, a.Policy)
			if err != nil {
				return nil, err
			}
			return map[string]any{"joinPolicy": srv.JoinPolicy, "restartRequired": true}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "mods_list",
		Description: "List the mods installed on a server.",
		InputSchema: schema(map[string]any{"server": serverProp}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, argServer(args), accessVisible)
			if err != nil {
				return nil, err
			}
			mods := srv.Mods
			if mods == nil {
				mods = []store.Mod{}
			}
			return map[string]any{"mods": mods, "loader": srv.Loader, "hasClientMods": store.LoaderHasClientMods(srv.Loader)}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "mods_search",
		Description: "Search Modrinth for mods compatible with a server's Minecraft version and loader. Returns projects with their projectId, which mods_add takes.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"query":  map[string]any{"type": "string", "description": "Search text, e.g. a mod name."},
			"limit":  map[string]any{"type": "integer", "description": "Max results (default 20, max 50)."},
		}, "query"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct {
				Server, Query string
				Limit         int
			}
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessVisible)
			if err != nil {
				return nil, err
			}
			limit := a.Limit
			if limit <= 0 || limit > 50 {
				limit = 20
			}
			hits, err := s.mr.Search(ctx, a.Query, srv.MCVersion, srv.Loader, limit)
			if err != nil {
				return nil, errors.New("could not reach Modrinth")
			}
			return map[string]any{"mods": hits}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "mods_add",
		Description: "Install a mod on a server by its Modrinth project id or slug (from mods_search). The mod is validated against the server's version and loader, and refused if it is client-only. Owner or admin.",
		InputSchema: schema(map[string]any{
			"server":    serverProp,
			"projectId": map[string]any{"type": "string", "description": "Modrinth project id or slug."},
		}, "projectId"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, ProjectID string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			m, deps, err := s.installMod(ctx, srv, a.ProjectID)
			if err != nil {
				return nil, err
			}
			depNames := make([]string, 0, len(deps))
			for _, d := range deps {
				depNames = append(depNames, d.Name)
			}
			return map[string]any{"added": m.Name, "id": m.ID, "filename": m.Filename, "dependencies": depNames}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "mods_remove",
		Description: "Remove an installed mod from a server by its mod id (from mods_list). Owner or admin.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"modId":  map[string]any{"type": "string", "description": "The mod id from mods_list."},
		}, "modId"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, ModID string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			if err := s.uninstallMod(ctx, srv, a.ModID); err != nil {
				return nil, errors.New("no such mod")
			}
			return "removed", nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "files_list",
		Description: "List a directory in a server's files (worlds, configs, mods, logs). Paths are rooted at \"server\", e.g. \"server\", \"server/config\", \"server/logs\". Owner or admin.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"path":   map[string]any{"type": "string", "description": `Directory path, rooted at "server" (default "server").`},
		}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, Path string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			tr, err := files.Open(runtime.Dir(srv.Owner, srv.Slug))
			if err != nil {
				return nil, errors.New("could not open the server's files")
			}
			path := normalizeFilesPath(a.Path)
			entries, err := tr.List(path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"path": path, "entries": entries}, nil
		},
	})

	reg.Register(mcp.Tool{
		Name:        "files_read",
		Description: "Read a text file from a server's files (a config, properties file, or a log). Paths are rooted at \"server\", e.g. \"server/server.properties\". Truncated to 128 KiB. Owner or admin.",
		InputSchema: schema(map[string]any{
			"server": serverProp,
			"path":   map[string]any{"type": "string", "description": `File path, rooted at "server".`},
		}, "path"),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			var a struct{ Server, Path string }
			decodeArgs(args, &a)
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, a.Server, accessOwned)
			if err != nil {
				return nil, err
			}
			tr, err := files.Open(runtime.Dir(srv.Owner, srv.Slug))
			if err != nil {
				return nil, errors.New("could not open the server's files")
			}
			path := normalizeFilesPath(a.Path)
			content, truncated, err := tr.ReadText(path, 128<<10)
			if err != nil {
				return nil, err
			}
			return map[string]any{"path": path, "content": content, "truncated": truncated}, nil
		},
	})

	return reg
}

// lifecycleTool builds one of the start/stop/restart tools — they differ only in the verb.
func (s *Server) lifecycleTool(action, desc, done string) mcp.Tool {
	return mcp.Tool{
		Name:        "server_" + action,
		Description: desc,
		InputSchema: schema(map[string]any{"server": serverProp}),
		Handler: func(ctx context.Context, cAny any, args json.RawMessage) (any, error) {
			c := cAny.(*mcpCaller)
			srv, err := s.mcpTarget(ctx, c, argServer(args), accessControl)
			if err != nil {
				return nil, err
			}
			switch action {
			case "start":
				err = s.rt.Start(ctx, srv)
			case "stop":
				err = s.rt.Stop(ctx, srv)
			case "restart":
				err = s.rt.Restart(ctx, srv)
			}
			if err != nil {
				return nil, err
			}
			return done + " " + srv.Slug, nil
		},
	}
}

// ── MCP token routes ────────────────────────────────────────────────────────────────────

// mintMCPToken issues a bearer token the caller pastes into an external MCP client (or the Ask AI tab
// hands to aigentic). Optionally bound to one server, so a token can be handed out with a narrow scope.
func (s *Server) mintMCPToken(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		ServerID string `json:"serverId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	scope := ""
	if id := strings.TrimSpace(body.ServerID); id != "" {
		srv, ok := s.findServer(id)
		if !ok || !s.hasAccess(r.Context(), srv, u, accessVisible) {
			writeErr(w, http.StatusNotFound, "No such server")
			return
		}
		scope = srv.ID
	}
	days := s.cfg.Int("mcpTokenDays", 30)
	if days <= 0 || days > 365 {
		days = 30
	}
	token, exp, err := s.tok.Mint(u.Username, scope, time.Duration(days)*24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create a token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires": exp.Unix(), "scope": scope, "endpoint": s.mcpEndpoint(r),
	})
}

func (s *Server) mcpTokenStatus(w http.ResponseWriter, r *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tokens": s.tok.Active(u.Username), "endpoint": s.mcpEndpoint(r),
	})
}

func (s *Server) revokeMCPToken(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if err := s.tok.RevokeAll(u.Username); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not revoke tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// mcpEndpoint is the public URL an external MCP client points at. It prefers an explicit config value
// (a host behind a tunnel may not see its own public name in the Host header); otherwise it derives
// it from the request, which is correct for the normal Caddy-fronted deployment.
func (s *Server) mcpEndpoint(r *http.Request) string {
	if u := strings.TrimSpace(s.cfg.String("mcpPublicUrl", "")); u != "" {
		return u
	}
	return "https://" + r.Host + base + "mcp"
}

// ── helpers ───────────────────────────────────────────────────────────────────────────

// serverProp is the shared JSON-schema fragment for the optional "server" argument.
var serverProp = map[string]any{
	"type":        "string",
	"description": "The server's id or slug. Optional when this connection is already bound to one server.",
}

// schema builds a JSON-Schema object from a property map and an optional required list.
func schema(props map[string]any, required ...string) json.RawMessage {
	if props == nil {
		props = map[string]any{}
	}
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	b, _ := json.Marshal(m)
	return json.RawMessage(b)
}

// decodeArgs unmarshals a tool's arguments, tolerating an absent object.
func decodeArgs(args json.RawMessage, v any) {
	if len(args) == 0 {
		return
	}
	_ = json.Unmarshal(args, v)
}

// argServer pulls just the optional "server" field for the many tools that take nothing else.
func argServer(args json.RawMessage) string {
	var a struct {
		Server string `json:"server"`
	}
	decodeArgs(args, &a)
	return a.Server
}

// normalizeFilesPath forgives a model that omits or drops the "server" root: "config" and
// "server/config" both resolve to the same place, and "" lists the server root.
func normalizeFilesPath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return files.RootKey
	}
	if p == files.RootKey || strings.HasPrefix(p, files.RootKey+"/") {
		return p
	}
	return files.RootKey + "/" + p
}
