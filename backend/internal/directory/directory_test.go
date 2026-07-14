package directory

import "testing"

// fake builds a directory whose OS lookups are answered from a fixture, so the tests never touch
// the host's group database.
func fake(groups map[string][]string, members ...string) *Directory {
	d := New("smbusers")
	d.groupsOf = func(u string) []string { return groups[u] }
	d.membersOf = func(string) []string { return members }
	return d
}

func TestKnows(t *testing.T) {
	groups := map[string][]string{
		// alice and bob share hc_family; carol shares only ordinary groups with them; dave is in no
		// contact group at all; erin's contact web does not overlap anyone's.
		"alice": {"alice", "smbusers", "hp_hosuto_play", "hc_family", "hc_dnd"},
		"bob":   {"bob", "smbusers", "hp_hosuto_play", "hc_family"},
		"carol": {"carol", "smbusers", "hp_hosuto_play", "hc_work"},
		"dave":  {"dave", "smbusers", "hp_hosuto_host"},
		"erin":  {"erin", "smbusers", "hc_work"},
	}

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"shared hc_ group", "alice", "bob", true},
		{"shared hc_ group, arguments swapped", "bob", "alice", true},
		{"only non-hc groups shared", "alice", "carol", false},
		{"no overlap at all", "alice", "dave", false},
		{"neither side has a contact group", "dave", "dave", false},
		{"contact groups that do not intersect", "bob", "erin", false},
		{"unknown user", "alice", "mallory", false},
		{"empty username", "alice", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := fake(groups)
			if got := d.Knows(tc.a, tc.b); got != tc.want {
				t.Fatalf("Knows(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Knowing is symmetric by definition; every case must hold both ways round.
			if got := d.Knows(tc.b, tc.a); got != tc.want {
				t.Fatalf("Knows(%q, %q) = %v, want %v (not symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestIsManaged(t *testing.T) {
	d := fake(nil, "alice", "bob")
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"alice", true},
		{"bob", true},
		{"mallory", false},
		{"", false},
	} {
		if got := d.IsManaged(tc.name); got != tc.want {
			t.Fatalf("IsManaged(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestContactGroups(t *testing.T) {
	d := fake(map[string][]string{"alice": {"alice", "smbusers", "hc_family", "hp_hosuto_play"}})
	got := d.ContactGroups("alice")
	if len(got) != 1 || !got["hc_family"] {
		t.Fatalf("ContactGroups = %v, want only hc_family", got)
	}
}

// The 30s cache must survive the resolver refactor: resolving the same user twice hits the OS once.
func TestGroupsCached(t *testing.T) {
	calls := 0
	d := New("smbusers")
	d.groupsOf = func(string) []string {
		calls++
		return []string{"hc_family"}
	}
	d.Groups("alice")
	d.Groups("alice")
	if calls != 1 {
		t.Fatalf("resolved groups %d times, want 1 (cached)", calls)
	}
	d.Groups("bob")
	if calls != 2 {
		t.Fatalf("resolved groups %d times, want 2 (per-user cache)", calls)
	}
}
