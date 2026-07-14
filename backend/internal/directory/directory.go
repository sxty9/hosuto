// Package directory enumerates holistic-managed users (members of the smbusers group, the same set
// privleg and the dashboard admin API list) and resolves each user's Linux groups live from the OS.
// Two players "know each other" exactly when they share an "hc_*" group — those groups are
// materialised by privleg from a group whose ContactVisibility flag is on, and contax computes
// contact visibility this same way. hosuto never asks privleg anything at request time: Linux
// groups are the single source of truth and hosuto reads them directly, so privleg stays out of the
// request path (parity with how every service enforces hp_* rights). Group lookups are cached
// briefly since resolving a server's grants can touch every member.
package directory

import (
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// ContactGroupPrefix marks a Linux group as a contact-visibility group. privleg materialises one
// such group per contact-enabled group definition; membership means "participates in that web".
const ContactGroupPrefix = "hc_"

// Directory enumerates managed users and resolves their groups.
type Directory struct {
	enumGroup string

	// The OS is reached only through these two funcs, so a test can inject a fake resolver instead
	// of depending on the host's passwd/group database. New always wires them to the real readers.
	groupsOf  func(username string) []string
	membersOf func(group string) []string

	mu    sync.Mutex
	cache map[string]groupEntry
}

type groupEntry struct {
	groups []string
	at     time.Time
}

// New builds a directory. enumGroup defaults to "smbusers".
func New(enumGroup string) *Directory {
	if enumGroup == "" {
		enumGroup = "smbusers"
	}
	return &Directory{
		enumGroup: enumGroup,
		groupsOf:  osGroups,
		membersOf: osGroupMembers,
		cache:     make(map[string]groupEntry),
	}
}

// Members returns the sorted usernames of holistic-managed accounts (empty if the group is
// missing — a host with no managed users).
func (d *Directory) Members() []string { return d.membersOf(d.enumGroup) }

// GroupMembers returns the members of an arbitrary Linux group.
//
// hosuto uses it to expand a "holistic"-kind grant — an admin sharing a server with a whole OS group
// (typically an hc_* contact group). The group is read live, so a member who leaves the group loses
// access on the next whitelist sync without hosuto storing anything.
func (d *Directory) GroupMembers(group string) []string {
	if group == "" {
		return nil
	}
	return d.membersOf(group)
}

// Groups returns a user's Linux groups, read live from the OS and cached for 30s.
func (d *Directory) Groups(username string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.cache[username]; ok && time.Since(e.at) < 30*time.Second {
		return e.groups
	}
	groups := d.groupsOf(username)
	d.cache[username] = groupEntry{groups: groups, at: time.Now()}
	return groups
}

// ContactGroups returns the set of a user's contact-visibility groups (the hc_* subset).
func (d *Directory) ContactGroups(username string) map[string]bool {
	m := map[string]bool{}
	for _, g := range d.Groups(username) {
		if strings.HasPrefix(g, ContactGroupPrefix) {
			m[g] = true
		}
	}
	return m
}

// Knows reports whether a and b share at least one hc_* contact group (symmetric). This is the only
// meaning "player a and player b know each other" has in hosuto — the contact web privleg
// materialises into Linux groups is the whole answer, so an unknown user, a user with no contact
// groups, and two users whose only shared groups are non-contact ones all read as strangers.
func (d *Directory) Knows(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ga := d.ContactGroups(a)
	if len(ga) == 0 {
		return false
	}
	for g := range d.ContactGroups(b) {
		if ga[g] {
			return true
		}
	}
	return false
}

// IsManaged reports whether a username is a holistic-managed account. Deliberately uncached: it is
// asked when a grant is written, never on the read path.
func (d *Directory) IsManaged(name string) bool {
	if name == "" {
		return false
	}
	for _, m := range d.Members() {
		if m == name {
			return true
		}
	}
	return false
}

// ContactGroupsOf returns the hc_* subset of an already-resolved group list (used for the caller,
// whose groups the session verifier resolved once).
func ContactGroupsOf(groups []string) map[string]bool {
	m := map[string]bool{}
	for _, g := range groups {
		if strings.HasPrefix(g, ContactGroupPrefix) {
			m[g] = true
		}
	}
	return m
}

// osGroups resolves a user's Linux groups from the OS. An unknown user yields no groups, not an
// error: callers treat "no groups" and "no such user" alike.
func osGroups(username string) []string {
	out, err := exec.Command("id", "-nG", username).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// osGroupMembers reads a group's members from the OS, sorted and deduplicated.
func osGroupMembers(group string) []string {
	out, err := exec.Command("getent", "group", group).Output()
	if err != nil {
		return nil
	}
	// getent group line: name:passwd:gid:member1,member2,...
	fields := strings.SplitN(strings.TrimSpace(string(out)), ":", 4)
	if len(fields) < 4 || fields[3] == "" {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range strings.Split(fields[3], ",") {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			seen[m] = true
			names = append(names, m)
		}
	}
	sort.Strings(names)
	return names
}
