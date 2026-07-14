// Package router is hosuto's client for mc-router (github.com/itzg/mc-router), the TCP proxy that
// gives every Minecraft server its own domain on one shared public port 25565.
//
// mc-router peeks the client's handshake packet, reads the "Server Address" field the player's
// client sent, and splices the raw TCP stream to the matching backend. Nothing is decrypted or
// re-encoded, so the backends stay online-mode=true and authenticate against Mojang themselves —
// hosuto never sits in the auth path. hosuto's job is only to keep the route table honest:
// <slug>.<zone> → 127.0.0.1:<port>.
//
// # The REST API is the only writer
//
// mc-router runs with -routes-config, and its API persists to that file itself (api_server.go calls
// configLoader.SaveRoutes() after every create and delete). The API is therefore a durable single
// source of truth, and hosuto must NOT also write the routes file — two writers, one file, and the
// last one to flush wins.
//
// We deliberately do NOT run mc-router with -routes-config-watch. Its file loader is ADDITIVE:
// Load() does a BulkRegister with no Reset(), and only SIGHUP takes the Reload() → Reset() path. A
// route deleted from the file under a watcher would therefore keep being served — a deleted server
// would stay reachable. Writing through the API instead makes deletion actually delete.
//
// # The default route is OFF unless an admin asks for it
//
// By default hosuto sets no default route: an unknown hostname is REFUSED, not silently spliced into
// whichever server happens to be the fallback — a stranger guessing a hostname would otherwise land
// on a member's world.
//
// The one deployment that needs it is a host with no port forward, reached through a tunnel
// (playit.gg and friends). Such a tunnel terminates the connection under ITS OWN hostname, so the
// handshake never carries our domain and mc-router has nothing to match on. A fallback server is
// then the only way anyone reaches anything — and it is only safe because that deployment has one
// server to reach.
//
// It is therefore an explicit admin setting (`defaultServer` in the central Configuration tab), not
// a default. Turning it on where several members host servers would hand every stray connection to
// whoever is named in it.
//
// # No authentication
//
// mc-router's API has none. It is bound to 127.0.0.1 and must never be exposed; anyone who can
// reach it can re-point every domain on the host.
//
// A client built with an empty base URL is disabled: every call is a silent no-op and Enabled()
// reports false, so hosuto still boots and serves its UI on a host where mc-router is not installed
// yet. Servers created there simply have no domain until an admin sets routerApi and the next
// daemon start syncs them in.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// timeout bounds a call to mc-router. It is a loopback service; if it cannot answer in this
	// long it is wedged, and a server create must fail rather than hang the request.
	timeout = 8 * time.Second
	// maxBody bounds the route table we will decode, so a wedged or hostile endpoint on the other
	// end of the socket cannot balloon the daemon's heap.
	maxBody = 1 << 20
)

// Client talks to one mc-router REST API.
type Client struct {
	base string
	http *http.Client
}

// New builds a client for mc-router's API base URL (e.g. http://127.0.0.1:25580). An empty base URL
// leaves the client disabled. hc is injectable for tests; nil gets a client with the standard
// timeout.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{base: strings.TrimRight(strings.TrimSpace(baseURL), "/"), http: hc}
}

// Enabled reports whether an mc-router API is configured. A disabled client no-ops.
func (c *Client) Enabled() bool { return c.base != "" }

// host canonicalises a server address. mc-router keys its table by the string it is handed, and DNS
// names are case-insensitive, so hosuto always registers and deletes the same lowercase form —
// otherwise "SMP.mc.example.org" and "smp.mc.example.org" would be two routes for one server.
func host(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// Put registers (or re-points) the route host → backend, where backend is a dialable address such
// as "127.0.0.1:25601". mc-router's create is an upsert, so this is safe to call on a server whose
// port changed.
func (c *Client) Put(ctx context.Context, h string, backend string) error {
	if !c.Enabled() {
		return nil
	}
	h, backend = host(h), strings.TrimSpace(backend)
	if h == "" || backend == "" {
		return errors.New("router: host and backend are required")
	}
	body, err := json.Marshal(map[string]string{"serverAddress": h, "backend": backend})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/routes", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("router: POST /routes %s: status %d", h, resp.StatusCode)
	}
	return nil
}

// Delete removes the route for host. A route that is already gone is success: deletion is the
// cleanup half of a server teardown, and it must converge rather than wedge the teardown on a route
// some earlier attempt already removed.
func (c *Client) Delete(ctx context.Context, h string) error {
	if !c.Enabled() {
		return nil
	}
	return c.del(ctx, host(h))
}

// del deletes by the exact key. Sync passes keys straight from the live table, which may have been
// hand-created in a form that host() would not reproduce; canonicalising those would delete the
// wrong key, or nothing at all.
func (c *Client) del(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("router: host is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/routes/"+url.PathEscape(key), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("router: DELETE /routes/%s: status %d", key, resp.StatusCode)
	}
	return nil
}

// Routes returns mc-router's live route table, host → backend. A disabled client has no routes,
// which is not an error: the caller ranges over the result either way.
func (c *Client) Routes(ctx context.Context) (map[string]string, error) {
	if !c.Enabled() {
		return map[string]string{}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/routes", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("router: GET /routes: status %d", resp.StatusCode)
	}
	// mc-router's response shape is NOT the {"host":"backend"} that POST /routes takes. Verified
	// against a live mc-router v1.44:
	//
	//     {"smp.mc.example.org": {"backend": "127.0.0.1:25601", "scalingTarget": "127.0.0.1:25601"}}
	//
	// Decode into json.RawMessage first and accept BOTH shapes: older builds answered a bare string,
	// and the flat form is what a reader would reasonably expect. Getting this wrong is quiet and
	// nasty — Routes() feeds Sync(), so a parse error at boot means orphaned routes are never
	// reconciled and a stale public domain keeps pointing at a recycled port.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("router: GET /routes: %w", err)
	}
	out := make(map[string]string, len(raw))
	for host, v := range raw {
		var flat string
		if json.Unmarshal(v, &flat) == nil {
			out[host] = flat
			continue
		}
		var obj struct {
			Backend string `json:"backend"`
		}
		if err := json.Unmarshal(v, &obj); err != nil {
			return nil, fmt.Errorf("router: GET /routes: route %q: %w", host, err)
		}
		out[host] = obj.Backend
	}
	return out, nil
}

// Sync reconciles mc-router's live table with the routes hosuto knows it should have: it adds what
// is missing, re-points what disagrees, and removes routes for hosts hosuto no longer has. The
// daemon calls it on start, so a crash between "delete the server" and "delete the route" can never
// leave a domain pointing at a port that has been recycled to somebody else's server.
//
// Sync is the reason hosuto needs no route bookkeeping of its own: the store is the intent, the
// mc-router API is the state, and this is the loop that closes the gap.
func (c *Client) Sync(ctx context.Context, want map[string]string) error {
	if !c.Enabled() {
		return nil
	}
	have, err := c.Routes(ctx)
	if err != nil {
		return err // fail closed: never reconcile — and above all never delete — against a table we could not read
	}

	// Canonicalise intent once, so a want key and a table key compare on equal terms.
	canon := make(map[string]string, len(want))
	for h, backend := range want {
		canon[host(h)] = strings.TrimSpace(backend)
	}

	// Best effort: one unroutable server must not strand the others. Report the first failure and
	// let the next start retry — reconciliation is idempotent, so a partial pass loses nothing.
	var firstErr error
	fail := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	for _, h := range sorted(canon) {
		if have[h] == canon[h] {
			continue // already correct; do not churn a live route
		}
		fail(c.Put(ctx, h, canon[h]))
	}
	for _, key := range sorted(have) {
		if _, keep := canon[host(key)]; !keep {
			fail(c.del(ctx, key))
		}
	}
	return firstErr
}

// do issues the request. Every call is one round trip to a loopback service; there is no retry,
// because the caller (a create, a delete, or the start-up sync) is the retry.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("router: %s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, nil
}

// drain returns the connection to the pool rather than leaving it to be re-dialed.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// sorted gives the reconcile a deterministic order, so a failing route fails the same way twice and
// the tests can assert on the calls it made.
func sorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetDefault points mc-router's fallback at a backend — every connection whose handshake hostname
// matches no route lands there. An empty backend clears it.
//
// Verified against a live mc-router v1.44: POST /defaultRoute takes {"backend": "..."}, persists it
// into the routes file as "default-server", and an EMPTY backend is how you remove it. There is no
// DELETE (it answers 405), which is the sort of thing worth writing down rather than rediscovering.
func (c *Client) SetDefault(ctx context.Context, backend string) error {
	if !c.Enabled() {
		return nil
	}
	body, err := json.Marshal(map[string]string{"backend": backend})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/defaultRoute", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("router: POST /defaultRoute: status %d", resp.StatusCode)
	}
	return nil
}
