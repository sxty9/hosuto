// Package mcapi resolves a Minecraft name to a Mojang profile. It is the only place in hosuto that
// knows a player's game identity comes from outside the landscape.
//
// hosuto links a Linux user to a game account by name: the user types their in-game name, this
// package resolves it, and store.LinkAccount records the uuid. There is NO proof of ownership —
// claiming a name you do not own is possible, and that is a deliberate, accepted trade (the
// landscape is a household, not a public host; store.Account.Verified exists so that bolting on a
// Microsoft OAuth flow later is a policy change, not a migration).
//
// The invariant this package holds: **every uuid that leaves it is DASHED**. Mojang's API returns a
// bare 32-hex id, but the server reads whitelist.json and ops.json with a strict UUID parser and
// drops any entry whose uuid is not in 8-4-4-4-12 form. A dropped entry does not error anywhere —
// the player simply cannot join, with no message. So Lookup, LookupBulk and Dash all emit the
// dashed form, and a uuid that cannot be converted is an error, never a best-effort passthrough.
//
// There is no cache here on purpose: a lookup happens once, when a user links their account, and
// the resolved uuid then lives in the store forever. Nothing on a request path calls Mojang. That
// matters because Mojang's budget is roughly 200 requests per 2 minutes per IP — a per-request
// lookup would exhaust it for the whole host.
package mcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the current, unauthenticated profile API. The old api.mojang.com
	// name→uuid endpoints are gone; this is the one that answers today.
	DefaultBaseURL = "https://api.minecraftservices.com"

	// lookupPath resolves one name.  GET  → 200 {"id":"<32-hex>","name":"jeb_"} · 404 if unknown.
	lookupPath = "/minecraft/profile/lookup/name/"
	// bulkPath resolves many.        POST ["a","b"] → 200 [{...}] · misses are silently omitted.
	bulkPath = "/minecraft/profile/lookups/bulk/byname"

	// bulkMax is Mojang's hard cap: an 11th name in one body is rejected wholesale with
	// 400 CONSTRAINT_VIOLATION, so LookupBulk chunks rather than trusting the caller.
	bulkMax = 10

	// timeout bounds a link attempt; the user is waiting on it in the UI.
	timeout = 8 * time.Second

	// maxBody caps what we will read from Mojang. Ten profiles is well under a kilobyte; anything
	// approaching this is a broken or hostile endpoint, not an answer.
	maxBody = 1 << 20

	userAgent = "hosuto/0.1 (+holistic)"
)

// ErrNoSuchPlayer means Mojang has no account by that name. It is a normal outcome (the user made
// a typo), not a failure of the service, and the API layer renders it as such.
var ErrNoSuchPlayer = errors.New("no such minecraft account")

// nameRe bounds a Minecraft name: 3-16 characters, letters, digits and underscore. Checking it here
// is not pedantry — a name that cannot exist must not spend one of the ~200 requests per 2 minutes
// that the whole host shares.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// ValidName reports whether a name could name a Minecraft account at all.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Profile is a resolved game account. UUID is always dashed (see the package doc).
type Profile struct {
	UUID string `json:"uuid"`
	Name string `json:"name"` // spelled as Mojang spells it, which may differ in case from the query
}

// Client talks to the Mojang profile API.
type Client struct {
	base string
	hc   *http.Client
}

// New builds a client. An empty baseURL means the real Mojang endpoint; tests inject an
// httptest.Server. A nil http.Client gets one with the package timeout.
func New(baseURL string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{base: strings.TrimSuffix(baseURL, "/"), hc: hc}
}

// Lookup resolves one name to a profile, returning ErrNoSuchPlayer if Mojang does not know it.
//
// A syntactically impossible name also yields ErrNoSuchPlayer, without a request: to the caller
// "17 characters long" and "nobody is called that" are the same answer, and the request is not
// worth spending.
func (c *Client) Lookup(ctx context.Context, name string) (Profile, error) {
	if !ValidName(name) {
		return Profile{}, fmt.Errorf("%w: %q is not a minecraft name (3-16 chars, letters, digits, underscore)", ErrNoSuchPlayer, name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+lookupPath+url.PathEscape(name), nil)
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("mcapi: lookup %q: %w", name, err)
	}
	defer resp.Body.Close()

	// 404 is the documented miss. 204 was the legacy endpoint's way of saying the same thing and
	// costs one comparison to keep accepting.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return Profile{}, fmt.Errorf("%w: %q", ErrNoSuchPlayer, name)
	}
	if resp.StatusCode != http.StatusOK {
		return Profile{}, statusErr("lookup "+name, resp)
	}

	var raw struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
		return Profile{}, fmt.Errorf("mcapi: lookup %q: malformed response: %w", name, err)
	}
	return toProfile(raw.ID, raw.Name)
}

// LookupBulk resolves many names in as few requests as possible, keyed by LOWERCASED name so the
// caller can look up what it asked for regardless of how Mojang spells it back.
//
// Names Mojang does not know are simply absent from the map — that is the endpoint's own behaviour,
// and the caller must handle a missing key anyway, so an unresolvable name is not an error here.
//
// It fails closed: any transport or status error aborts the whole call and returns nil rather than
// a partial map. A partial map would be indistinguishable from "those players have no accounts",
// and the caller's next move is to write whitelist.json — quietly dropping players from it because
// Mojang rate-limited us is exactly the silent failure this package exists to prevent.
func (c *Client) LookupBulk(ctx context.Context, names []string) (map[string]Profile, error) {
	// Dedupe on the lowercased name: Mojang matches case-insensitively, so "Jeb_" and "jeb_" are
	// one lookup, and a duplicate would only burn rate-limit budget. Impossible names are dropped
	// here for the same reason — they can only ever be misses.
	seen := make(map[string]bool, len(names))
	want := make([]string, 0, len(names))
	for _, n := range names {
		k := strings.ToLower(n)
		if !ValidName(n) || seen[k] {
			continue
		}
		seen[k] = true
		want = append(want, n)
	}

	out := make(map[string]Profile, len(want))
	for i := 0; i < len(want); i += bulkMax {
		chunk := want[i:min(i+bulkMax, len(want))]
		got, err := c.bulk(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for _, p := range got {
			out[strings.ToLower(p.Name)] = p
		}
	}
	return out, nil
}

// bulk is one request for at most bulkMax names.
func (c *Client) bulk(ctx context.Context, names []string) ([]Profile, error) {
	body, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+bulkPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcapi: bulk lookup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("bulk lookup", resp)
	}

	var raw []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("mcapi: bulk lookup: malformed response: %w", err)
	}
	out := make([]Profile, 0, len(raw))
	for _, r := range raw {
		p, err := toProfile(r.ID, r.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// toProfile converts one wire record. An id that will not dash is fatal: shipping it would put a
// uuid on the whitelist that the server silently ignores.
func toProfile(id, name string) (Profile, error) {
	if name == "" {
		return Profile{}, errors.New("mcapi: mojang returned a profile with no name")
	}
	uuid, err := Dash(id)
	if err != nil {
		return Profile{}, fmt.Errorf("mcapi: profile %q: %w", name, err)
	}
	return Profile{UUID: uuid, Name: name}, nil
}

// statusErr renders a non-200 for the API layer. 429 is called out separately because it is the one
// a real host actually hits, and "try again in a minute" is the only useful thing to tell the user.
func statusErr(op string, resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("mcapi: %s: rate limited by mojang, try again in a minute", op)
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if s := strings.TrimSpace(string(snippet)); s != "" {
		return fmt.Errorf("mcapi: %s: mojang returned %s: %s", op, resp.Status, s)
	}
	return fmt.Errorf("mcapi: %s: mojang returned %s", op, resp.Status)
}

// Dash converts Mojang's bare 32-hex id into the dashed 8-4-4-4-12 uuid that whitelist.json and
// ops.json require. It is exported because the client-export package writes those files too and
// must not reimplement this.
//
// An already-dashed id is accepted and normalised (lowercased) rather than rejected, so a caller
// holding a uuid of unknown provenance — one from the store, say — can pass it through blindly.
// Everything else is an error: no case, ever, produces a uuid that is not exactly 36 characters of
// lowercase hex and hyphens, because a wrong uuid is not a visible failure, it is a player who
// cannot join and nobody knows why.
func Dash(id string) (string, error) {
	s := strings.TrimSpace(id)

	if len(s) == 36 {
		u, ok := undash(s)
		if !ok {
			return "", fmt.Errorf("mcapi: %q is not a uuid", id)
		}
		s = u
	}
	if len(s) != 32 {
		return "", fmt.Errorf("mcapi: %q is not a 32-hex minecraft id (got %d chars)", id, len(s))
	}

	out := make([]byte, 0, 36)
	for i := 0; i < 32; i++ {
		c, ok := hexDigit(s[i])
		if !ok {
			return "", fmt.Errorf("mcapi: %q is not hex", id)
		}
		out = append(out, c)
		// 8-4-4-4-12: the hyphens fall after the 8th, 12th, 16th and 20th nibble.
		if i == 7 || i == 11 || i == 15 || i == 19 {
			out = append(out, '-')
		}
	}
	return string(out), nil
}

// undash strips the hyphens from a 36-char candidate, but only if they are in the four places a
// uuid puts them. Caller guarantees len(s) == 36.
func undash(s string) (string, bool) {
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return "", false
	}
	return s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36], true
}

// hexDigit validates one nibble and folds it to lowercase, the canonical form on disk.
func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		return c, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 'a', true
	}
	return 0, false
}
