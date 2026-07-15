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
