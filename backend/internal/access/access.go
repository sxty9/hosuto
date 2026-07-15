// Package access is hosuto's ONE authorisation rule for a server: who may see it, control it, or
// own it, plus the reverse index from a Minecraft identity back to a Linux user.
//
// It exists so both the HTTP surface (package api) and the in-game "!ai" CLI (package ingame) enforce
// the SAME rule from the SAME code. Copying the owner/op/member logic into a second place would be
// exactly the parallel data path the Single Source of Truth maxim forbids — and the two would drift.
package access

import (
	"context"
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
		}
	}
	return out
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
