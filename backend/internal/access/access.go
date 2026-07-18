// Package access is hosuto's ONE authorisation rule for a server: who may see it, control it, or
// own it, plus the reverse index from a Minecraft identity back to a Linux user.
//
// It exists so both the HTTP surface (package api) and the in-game "!ai" CLI (package ingame) enforce
// the SAME rule from the SAME code. Copying the owner/op/member logic into a second place would be
// exactly the parallel data path the Single Source of Truth maxim forbids — and the two would drift.
package access

import (
	"context"
	"sort"
	"strings"

	"hosuto/internal/auth"
	"hosuto/internal/contax"
	"hosuto/internal/directory"
	"hosuto/internal/rights"
	"hosuto/internal/store"
)

// Level is how much access an operation requires.
type Level int

const (
	Visible Level = iota // owner, admin, or any resolved member
	Control              // owner, admin, or an op-level member
	Owned                // owner or admin only
)

// Resolver answers access questions for a server and maps a game identity to a Linux user.
type Resolver struct {
	st  *store.Store
	cx  *contax.Client
	dir *directory.Directory
	v   *auth.Verifier
}

// New builds a resolver from the same singletons the daemon already holds.
func New(st *store.Store, cx *contax.Client, dir *directory.Directory, v *auth.Verifier) *Resolver {
	return &Resolver{st: st, cx: cx, dir: dir, v: v}
}

// Resolve expands a server's grants into the Linux usernames that may join, and at what level.
//
// Membership is resolved LIVE on every call. A contax group's members and an OS group's members are
// never copied into hosuto's store: contax owns its groups, the OS owns its groups, and hosuto owns
// only the grant that points at them. "op" wins over "play" when a user is reachable through more
// than one grant. The owner is excluded (they are implicitly op).
func (r *Resolver) Resolve(ctx context.Context, srv store.Server) map[string]string {
	out := map[string]string{}
	set := func(user, level string) {
		if user == "" || user == srv.Owner {
			return
		}
		if out[user] == "op" {
			return
		}
		out[user] = level
	}
	for _, g := range srv.Grants {
		switch g.Kind {
		case "adhoc":
			for _, m := range g.Members {
				set(m, g.Level)
			}
		case "contax":
			// A contax lookup that fails (contax down, secret unset) resolves to NO members rather than
			// to stale ones — failing closed means a member briefly loses access; failing open would let
			// someone removed from a group keep it. The former is recoverable.
			members, _ := r.cx.Members(g.Ref)
			for _, m := range members {
				set(m, g.Level)
			}
		case "holistic":
			for _, m := range r.dir.GroupMembers(g.Ref) {
				set(m, g.Level)
			}
		case "minecraft":
			// A game account admitted directly usually has nobody here behind it, and then it
			// contributes no Linux user — it may join, it may not see the dashboard.
			//
			// But the mapping is looked up live, so the day that player links the same account to a
			// holistic user, this grant starts resolving to them: their access "upgrades" from
			// join-only to full membership without anyone editing the server. That is why the lookup
			// belongs here rather than being frozen into the grant when it is created.
			if user, ok := r.UserByUUID(g.Ref); ok {
				set(user, g.Level)
			}
		}
	}
	return out
}

// Player is one entry of a server's player list: a game identity, the level it may join at, and the
// Linux user behind it — empty for a Minecraft account admitted directly.
//
// UUID is empty when a member has not linked an account: they are a player on paper with nothing to
// write to the whitelist yet, and both callers need to tell that apart from having no player at all.
type Player struct {
	User  string
	UUID  string
	Name  string
	Level string
}

// Players is the whole player list of a server, in one pass: resolved members through their linked
// accounts, plus the Minecraft accounts admitted directly.
//
// It exists because there are exactly two things to do with that list — show it and write it to
// whitelist.json — and doing them from two separate expansions of the same grants is how the file
// and the screen come to disagree. Both callers read this.
//
// A player reachable more than once appears once: "op" wins over "play", and a directly-admitted
// account that has since been linked merges with its holistic member rather than showing twice.
func (r *Resolver) Players(ctx context.Context, srv store.Server) []Player {
	byKey := map[string]*Player{}
	var order []string

	// UUID is the identity; a member with no account yet can only be keyed by their username.
	add := func(p Player) {
		key := normUUID(p.UUID)
		if key == "" {
			if p.User == "" {
				return
			}
			key = "user:" + p.User
		}
		cur, seen := byKey[key]
		if !seen {
			cp := p
			byKey[key] = &cp
			order = append(order, key)
			return
		}
		if p.Level == "op" {
			cur.Level = "op"
		}
		if cur.User == "" {
			cur.User = p.User
		}
		if cur.Name == "" {
			cur.Name = p.Name
		}
	}
	addUser := func(user, level string) {
		acc, _ := r.st.Account(user) // a member with no linked account still lists, without a UUID
		add(Player{User: user, UUID: acc.UUID, Name: acc.Name, Level: level})
	}

	addUser(srv.Owner, "op") // the owner is implicitly op and never appears in Resolve
	for user, level := range r.Resolve(ctx, srv) {
		addUser(user, level)
	}
	for _, g := range srv.Grants {
		if g.Kind == "minecraft" {
			add(Player{UUID: g.Ref, Name: g.Label, Level: g.Level})
		}
	}

	out := make([]Player, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.Slice(out, func(i, j int) bool { return playerLabel(out[i]) < playerLabel(out[j]) })
	return out
}

// playerLabel is what a player is called on screen, and therefore what they sort by: their in-game
// name, or their username while they have no account to take a name from.
func playerLabel(p Player) string {
	if p.Name != "" {
		return strings.ToLower(p.Name)
	}
	return strings.ToLower(p.User)
}

// HasAccess is the one authorisation rule. Membership is resolved live (never copied).
func (r *Resolver) HasAccess(ctx context.Context, srv store.Server, u *auth.User, level Level) bool {
	owner := srv.Owner == u.Username || u.Can(rights.GroupAdmin)
	switch level {
	case Owned:
		return owner
	case Control:
		if owner {
			return true
		}
		return r.Resolve(ctx, srv)[u.Username] == "op"
	default: // Visible
		if owner {
			return true
		}
		_, member := r.Resolve(ctx, srv)[u.Username]
		return member
	}
}

// Account returns a user's linked game account.
func (r *Resolver) Account(user string) (store.Account, bool) { return r.st.Account(user) }

// UserAsAuth resolves a Linux username to a full identity (live OS groups), for a caller whose
// identity was established out of band (an in-game player, not a session cookie).
func (r *Resolver) UserAsAuth(user string) (*auth.User, bool) { return r.v.Resolve(user) }

// UserByUUID maps a Minecraft UUID (dashed or not) back to the Linux user who linked it. UUID is the
// rename-stable key — prefer it over UserByName.
func (r *Resolver) UserByUUID(uuid string) (string, bool) {
	want := normUUID(uuid)
	if want == "" {
		return "", false
	}
	for user, acc := range r.st.Accounts() {
		if normUUID(acc.UUID) == want {
			return user, true
		}
	}
	return "", false
}

// AdmittedUUID reports whether a Minecraft account is one this landscape has anything to do with:
// linked by a member, or admitted directly on some server.
//
// It exists to keep the face renderer from becoming an open proxy. A face is fetched by UUID for a
// directly-admitted account (there is no username to fetch it by), and without this any member could
// spend hosuto's shared Mojang session-server budget rendering faces for UUIDs nobody here has ever
// heard of. hosuto renders the faces of its own players and no others.
func (r *Resolver) AdmittedUUID(uuid string) bool {
	want := normUUID(uuid)
	if want == "" {
		return false
	}
	if _, ok := r.UserByUUID(want); ok {
		return true
	}
	for _, srv := range r.st.Servers() {
		for _, g := range srv.Grants {
			if g.Kind == "minecraft" && normUUID(g.Ref) == want {
				return true
			}
		}
	}
	return false
}

// UserByName maps an in-game name back to a Linux user. The name is "at link time" and goes stale on
// a Mojang rename, so it is only a fallback when no UUID is known.
func (r *Resolver) UserByName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	for user, acc := range r.st.Accounts() {
		if strings.EqualFold(acc.Name, name) {
			return user, true
		}
	}
	return "", false
}

func normUUID(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}
