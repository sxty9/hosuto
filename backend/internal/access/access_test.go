package access

import (
	"path/filepath"
	"testing"

	"hosuto/internal/store"
)

// newStore builds a store backed by a throwaway file with the given accounts linked.
func newStore(t *testing.T, links map[string][2]string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for user, ug := range links {
		if _, err := st.LinkAccount(user, ug[0], ug[1]); err != nil {
			t.Fatalf("link %s: %v", user, err)
		}
	}
	return st
}

func TestUserByUUIDNormalises(t *testing.T) {
	st := newStore(t, map[string][2]string{
		"nanu":  {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
		"other": {"11111111-2222-3333-4444-555555555555", "Someone"},
	})
	r := New(st, nil, nil, nil)

	// Dashed and undashed, any case, all resolve to the same user.
	for _, q := range []string{
		"069a79f4-44e9-4726-a5be-fca90e38aaf5",
		"069A79F444E94726A5BEFCA90E38AAF5",
		"069a79f444e94726a5befca90e38aaf5",
	} {
		u, ok := r.UserByUUID(q)
		if !ok || u != "nanu" {
			t.Errorf("UserByUUID(%q) = (%q,%v), want (nanu,true)", q, u, ok)
		}
	}
	if _, ok := r.UserByUUID("deadbeef-0000-0000-0000-000000000000"); ok {
		t.Error("unknown UUID should not resolve")
	}
	if _, ok := r.UserByUUID(""); ok {
		t.Error("empty UUID should not resolve")
	}
}

func TestUserByNameCaseInsensitive(t *testing.T) {
	st := newStore(t, map[string][2]string{
		"nanu": {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
	})
	r := New(st, nil, nil, nil)

	if u, ok := r.UserByName("ichbinshenry"); !ok || u != "nanu" {
		t.Errorf("UserByName(lower) = (%q,%v), want (nanu,true)", u, ok)
	}
	if u, ok := r.UserByName("  IchBinsHenry "); !ok || u != "nanu" {
		t.Errorf("UserByName(padded) = (%q,%v), want (nanu,true)", u, ok)
	}
	if _, ok := r.UserByName("Nobody"); ok {
		t.Error("unknown name should not resolve")
	}
}

// srv builds a server owned by `owner` carrying the given grants.
func srv(owner string, grants ...store.Grant) store.Server {
	return store.Server{ID: "srv-test", Owner: owner, Grants: grants}
}

// byName indexes a player list the way a caller reads it.
func byName(ps []Player) map[string]Player {
	out := map[string]Player{}
	for _, p := range ps {
		out[p.Name] = p
	}
	return out
}

// A Minecraft account admitted directly becomes a player with no Linux user behind it: it is on the
// whitelist, and that is the whole of what the grant buys.
func TestPlayersAdmitsDirectMinecraftGrant(t *testing.T) {
	st := newStore(t, map[string][2]string{
		"nanu": {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
	})
	r := New(st, nil, nil, nil)
	s := srv("nanu", store.Grant{
		ID: "grn-1", Kind: "minecraft", Ref: "11111111-2222-3333-4444-555555555555",
		Label: "Notch", Level: "play",
	})

	ps := byName(r.Players(t.Context(), s))
	if len(ps) != 2 {
		t.Fatalf("Players() = %d entries, want 2 (owner + guest)", len(ps))
	}
	guest, ok := ps["Notch"]
	if !ok {
		t.Fatal("the admitted account is not on the player list")
	}
	if guest.User != "" {
		t.Errorf("guest.User = %q, want empty — nobody here stands behind that account", guest.User)
	}
	if guest.UUID != "11111111-2222-3333-4444-555555555555" || guest.Level != "play" {
		t.Errorf("guest = %+v, want the granted uuid at play level", guest)
	}
	if ps["IchBinsHenry"].Level != "op" {
		t.Errorf("owner level = %q, want op", ps["IchBinsHenry"].Level)
	}

	// Joining is all it grants: with no holistic user behind it, it cannot reach the dashboard.
	if got := r.Resolve(t.Context(), s); len(got) != 0 {
		t.Errorf("Resolve() = %v, want nobody — an unlinked account is no member", got)
	}
}

// The "link later" story: the same person links the account this grant already names, and the grant
// starts covering the member too — one player, not two, and now with dashboard access.
func TestPlayersMergesGrantWithLaterLink(t *testing.T) {
	uuid := "11111111-2222-3333-4444-555555555555"
	st := newStore(t, map[string][2]string{
		"nanu": {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
		"bob":  {uuid, "Notch"}, // bob has since linked the very account that was admitted by name
	})
	r := New(st, nil, nil, nil)
	s := srv("nanu", store.Grant{ID: "grn-1", Kind: "minecraft", Ref: uuid, Label: "Notch", Level: "play"})

	ps := r.Players(t.Context(), s)
	if len(ps) != 2 {
		t.Fatalf("Players() = %d entries, want 2 — the grant and the member are one player", len(ps))
	}
	got := byName(ps)["Notch"]
	if got.User != "bob" {
		t.Errorf("Notch.User = %q, want bob — the link should have been picked up live", got.User)
	}
	if _, member := r.Resolve(t.Context(), s)["bob"]; !member {
		t.Error("bob linked the admitted account, so the grant should now make him a member")
	}
}

// A player reachable at two levels is one player at the higher one — whichever grant said so.
func TestPlayersOpWinsAcrossGrants(t *testing.T) {
	uuid := "11111111-2222-3333-4444-555555555555"
	st := newStore(t, map[string][2]string{
		"nanu": {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
		"bob":  {uuid, "Notch"},
	})
	r := New(st, nil, nil, nil)
	s := srv("nanu",
		store.Grant{ID: "grn-1", Kind: "adhoc", Members: []string{"bob"}, Level: "play"},
		store.Grant{ID: "grn-2", Kind: "minecraft", Ref: uuid, Label: "Notch", Level: "op"},
	)

	ps := r.Players(t.Context(), s)
	if len(ps) != 2 {
		t.Fatalf("Players() = %d entries, want 2", len(ps))
	}
	if got := byName(ps)["Notch"]; got.Level != "op" {
		t.Errorf("Notch.Level = %q, want op — the higher grant wins", got.Level)
	}
}

// A member who has not linked an account still lists (the UI warns about them), but carries no UUID
// — so applyMembers has nothing to write and skips them rather than writing a broken entry.
func TestPlayersKeepsUnlinkedMember(t *testing.T) {
	st := newStore(t, map[string][2]string{
		"nanu": {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
	})
	r := New(st, nil, nil, nil)
	s := srv("nanu", store.Grant{ID: "grn-1", Kind: "adhoc", Members: []string{"bob"}, Level: "play"})

	var bob *Player
	for _, p := range r.Players(t.Context(), s) {
		if p.User == "bob" {
			bob = &p
		}
	}
	if bob == nil {
		t.Fatal("an unlinked member should still appear on the list")
	}
	if bob.UUID != "" {
		t.Errorf("bob.UUID = %q, want empty — he has linked nothing", bob.UUID)
	}
}

// The face route renders only players this landscape knows — a linked member or an admitted account
// — so it cannot be used to spend Mojang's shared budget on arbitrary UUIDs.
func TestAdmittedUUIDOnlyKnownPlayers(t *testing.T) {
	admitted := "11111111-2222-3333-4444-555555555555"
	st := newStore(t, map[string][2]string{
		"nanu": {"069a79f4-44e9-4726-a5be-fca90e38aaf5", "IchBinsHenry"},
	})
	if _, err := st.CreateServer(store.Server{
		Slug: "test", Name: "Test", Owner: "nanu", Game: "minecraft", MCVersion: "1.21.1",
		Loader: "vanilla", JoinPolicy: "whitelist",
		Grants: []store.Grant{{ID: "grn-1", Kind: "minecraft", Ref: admitted, Label: "Notch", Level: "play"}},
	}); err != nil {
		t.Fatal(err)
	}
	r := New(st, nil, nil, nil)

	if !r.AdmittedUUID("069A79F4-44E9-4726-A5BE-FCA90E38AAF5") {
		t.Error("a linked member's account should render")
	}
	if !r.AdmittedUUID(admitted) {
		t.Error("a directly-admitted account should render")
	}
	if r.AdmittedUUID("99999999-8888-7777-6666-555555555555") {
		t.Error("a stranger's UUID must not render — that is an open Mojang proxy")
	}
	if r.AdmittedUUID("") {
		t.Error("empty UUID must not render")
	}
}
