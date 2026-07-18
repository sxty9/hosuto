package api

import (
	"testing"
	"time"
)

// The face route leans on this to tell "an account we just resolved for a search" from "any UUID a
// member cares to name", so both halves matter: it must remember, and it must forget.
func TestSeenUUIDsExpire(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newSeenUUIDs(15 * time.Minute)
	s.now = func() time.Time { return now }

	s.mark("853C80EF-3C37-49FD-AA49-938B674ADAE6")
	if !s.has("853c80ef-3c37-49fd-aa49-938b674adae6") {
		t.Error("a freshly resolved account should render, whatever the case it was marked in")
	}
	if s.has("11111111-2222-3333-4444-555555555555") {
		t.Error("an account nobody looked up must not render")
	}
	if s.has("") {
		t.Error("the empty UUID must not render")
	}

	now = now.Add(16 * time.Minute)
	if s.has("853c80ef-3c37-49fd-aa49-938b674adae6") {
		t.Error("the permit should have aged out")
	}

	// Marking prunes, so a burst of searches cannot grow the set without bound.
	s.mark("11111111-2222-3333-4444-555555555555")
	if len(s.at) != 1 {
		t.Errorf("set holds %d entries, want 1 — stale ones should be pruned on write", len(s.at))
	}
}
