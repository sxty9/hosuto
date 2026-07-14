package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeRouter is an in-process stand-in for mc-router's REST API: it holds a route table, applies
// the same create/delete semantics, and records every call so a test can assert on the exact set of
// writes a reconcile made. No test in this package touches the network.
type fakeRouter struct {
	mu     sync.Mutex
	routes map[string]string
	calls  []string // "POST host=backend" | "DELETE host"
	listMu int      // status to answer GET /routes with, 0 = OK
	postMu int      // status to answer POST /routes with, 0 = OK
}

func newFake(routes map[string]string) *fakeRouter {
	if routes == nil {
		routes = map[string]string{}
	}
	return &fakeRouter{routes: routes}
}

func (f *fakeRouter) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeRouter) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/routes":
		if f.listMu != 0 {
			w.WriteHeader(f.listMu)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.routes)

	case r.Method == http.MethodPost && r.URL.Path == "/routes":
		var body struct {
			ServerAddress string `json:"serverAddress"`
			Backend       string `json:"backend"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.ServerAddress == "" || body.Backend == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.calls = append(f.calls, "POST "+body.ServerAddress+"="+body.Backend)
		if f.postMu != 0 {
			w.WriteHeader(f.postMu)
			return
		}
		f.routes[body.ServerAddress] = body.Backend // create is an upsert
		w.WriteHeader(http.StatusCreated)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/routes/"):
		addr := strings.TrimPrefix(r.URL.Path, "/routes/")
		f.calls = append(f.calls, "DELETE "+addr)
		if _, ok := f.routes[addr]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.routes, addr)
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRouter) snapshot() (map[string]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	routes := make(map[string]string, len(f.routes))
	for k, v := range f.routes {
		routes[k] = v
	}
	return routes, append([]string(nil), f.calls...)
}

func TestPut(t *testing.T) {
	f := newFake(nil)
	c := New(f.server(t).URL, nil)

	if err := c.Put(context.Background(), "smp.mc.example.org", "127.0.0.1:25601"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	routes, calls := f.snapshot()
	if got := routes["smp.mc.example.org"]; got != "127.0.0.1:25601" {
		t.Fatalf("route not registered, got %q", got)
	}
	if want := []string{"POST smp.mc.example.org=127.0.0.1:25601"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}

	// Create is an upsert: re-pointing a server whose port moved must not need a delete first.
	if err := c.Put(context.Background(), "smp.mc.example.org", "127.0.0.1:25609"); err != nil {
		t.Fatalf("Put (re-point): %v", err)
	}
	routes, _ = f.snapshot()
	if got := routes["smp.mc.example.org"]; got != "127.0.0.1:25609" {
		t.Fatalf("route not re-pointed, got %q", got)
	}
}

func TestPutCanonicalisesHost(t *testing.T) {
	f := newFake(nil)
	c := New(f.server(t).URL, nil)

	// A DNS name is case-insensitive; the route table is not. Registering the mixed-case form must
	// not create a second route that no lowercased handshake would ever match.
	if err := c.Put(context.Background(), "  SMP.MC.Example.org.  ", "127.0.0.1:25601"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	routes, _ := f.snapshot()
	if _, ok := routes["smp.mc.example.org"]; !ok || len(routes) != 1 {
		t.Fatalf("want exactly the canonical route, got %v", routes)
	}
}

func TestPutRejectsEmpty(t *testing.T) {
	f := newFake(nil)
	c := New(f.server(t).URL, nil)

	for _, tc := range []struct{ name, host, backend string }{
		{"no host", "", "127.0.0.1:25601"},
		{"no backend", "smp.mc.example.org", ""},
		{"blank host", "   ", "127.0.0.1:25601"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Put(context.Background(), tc.host, tc.backend); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
	if _, calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("a rejected Put must not reach mc-router, got %v", calls)
	}
}

func TestPutErrorStatus(t *testing.T) {
	f := newFake(nil)
	f.postMu = http.StatusInternalServerError
	c := New(f.server(t).URL, nil)

	if err := c.Put(context.Background(), "smp.mc.example.org", "127.0.0.1:25601"); err == nil {
		t.Fatal("a 500 from mc-router must surface as an error")
	}
}

func TestDelete(t *testing.T) {
	f := newFake(map[string]string{"smp.mc.example.org": "127.0.0.1:25601"})
	c := New(f.server(t).URL, nil)

	if err := c.Delete(context.Background(), "smp.mc.example.org"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	routes, _ := f.snapshot()
	if len(routes) != 0 {
		t.Fatalf("route not removed, table = %v", routes)
	}
}

func TestDeleteMissingRouteIsSuccess(t *testing.T) {
	f := newFake(nil)
	c := New(f.server(t).URL, nil)

	// Teardown must converge. A 404 means the route is gone, which is exactly what was asked for;
	// failing here would wedge a server deletion on a route an earlier attempt already removed.
	if err := c.Delete(context.Background(), "gone.mc.example.org"); err != nil {
		t.Fatalf("404 on DELETE must be success, got %v", err)
	}
}

func TestRoutes(t *testing.T) {
	f := newFake(map[string]string{
		"smp.mc.example.org":      "127.0.0.1:25601",
		"creative.mc.example.org": "127.0.0.1:25602",
	})
	c := New(f.server(t).URL, nil)

	got, err := c.Routes(context.Background())
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	want := map[string]string{
		"smp.mc.example.org":      "127.0.0.1:25601",
		"creative.mc.example.org": "127.0.0.1:25602",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Routes = %v, want %v", got, want)
	}
}

func TestSyncAddsAndRemovesExactlyTheRightSet(t *testing.T) {
	f := newFake(map[string]string{
		"smp.mc.example.org":      "127.0.0.1:25601", // correct — must not be touched
		"creative.mc.example.org": "127.0.0.1:25602", // wrong backend — must be re-pointed
		"orphan.mc.example.org":   "127.0.0.1:25698", // server is gone — must be removed
	})
	c := New(f.server(t).URL, nil)

	want := map[string]string{
		"smp.mc.example.org":      "127.0.0.1:25601",
		"creative.mc.example.org": "127.0.0.1:25652", // moved port
		"skyblock.mc.example.org": "127.0.0.1:25603", // new server, never routed
	}
	if err := c.Sync(context.Background(), want); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	routes, calls := f.snapshot()
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("table after Sync = %v, want %v", routes, want)
	}
	// The already-correct route must not be rewritten: churn on a live route is a needless splice
	// of every connected player's session.
	wantCalls := []string{
		"POST creative.mc.example.org=127.0.0.1:25652",
		"POST skyblock.mc.example.org=127.0.0.1:25603",
		"DELETE orphan.mc.example.org",
	}
	sort.Strings(calls)
	sort.Strings(wantCalls)
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}

func TestSyncEmptyWantClearsTable(t *testing.T) {
	f := newFake(map[string]string{"orphan.mc.example.org": "127.0.0.1:25601"})
	c := New(f.server(t).URL, nil)

	// hosuto owns the whole table. No servers means no routes — this is the crash-recovery case
	// that keeps a recycled port from being reachable under a deleted server's domain.
	if err := c.Sync(context.Background(), map[string]string{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if routes, _ := f.snapshot(); len(routes) != 0 {
		t.Fatalf("table should be empty, got %v", routes)
	}
}

func TestSyncDoesNotDeleteWhenTableCannotBeRead(t *testing.T) {
	f := newFake(map[string]string{"smp.mc.example.org": "127.0.0.1:25601"})
	f.listMu = http.StatusInternalServerError
	c := New(f.server(t).URL, nil)

	// Fail closed. A table we could not read tells us nothing about which routes are orphaned, and
	// guessing would take every live server offline.
	if err := c.Sync(context.Background(), map[string]string{}); err == nil {
		t.Fatal("Sync must fail when the route table cannot be read")
	}
	if _, calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("Sync must write nothing when the table is unreadable, got %v", calls)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	f := newFake(nil)
	c := New(f.server(t).URL, nil)
	want := map[string]string{"smp.mc.example.org": "127.0.0.1:25601"}

	if err := c.Sync(context.Background(), want); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := c.Sync(context.Background(), want); err != nil {
		t.Fatalf("Sync (second pass): %v", err)
	}
	_, calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("a converged table must take no writes on the second pass, calls = %v", calls)
	}
}

func TestSyncBestEffort(t *testing.T) {
	f := newFake(map[string]string{"orphan.mc.example.org": "127.0.0.1:25698"})
	f.postMu = http.StatusInternalServerError
	c := New(f.server(t).URL, nil)

	// A route that will not register must still not strand the rest of the reconcile: the orphan
	// gets removed, and the failure is reported.
	err := c.Sync(context.Background(), map[string]string{"smp.mc.example.org": "127.0.0.1:25601"})
	if err == nil {
		t.Fatal("Sync must report the failed Put")
	}
	if routes, _ := f.snapshot(); len(routes) != 0 {
		t.Fatalf("the orphan should still have been removed, table = %v", routes)
	}
}

func TestDisabledClientNoOps(t *testing.T) {
	c := New("", nil)

	if c.Enabled() {
		t.Fatal("a client with no base URL must be disabled")
	}
	// Every call must be a silent no-op, so hosuto boots and serves its UI on a host where
	// mc-router is not installed yet.
	if err := c.Put(context.Background(), "smp.mc.example.org", "127.0.0.1:25601"); err != nil {
		t.Fatalf("disabled Put: %v", err)
	}
	if err := c.Delete(context.Background(), "smp.mc.example.org"); err != nil {
		t.Fatalf("disabled Delete: %v", err)
	}
	if err := c.Sync(context.Background(), map[string]string{"smp.mc.example.org": "127.0.0.1:25601"}); err != nil {
		t.Fatalf("disabled Sync: %v", err)
	}
	routes, err := c.Routes(context.Background())
	if err != nil {
		t.Fatalf("disabled Routes: %v", err)
	}
	if routes == nil || len(routes) != 0 {
		t.Fatalf("disabled Routes must be empty and non-nil, got %v", routes)
	}
}

func TestDisabledWhitespaceURL(t *testing.T) {
	if New("   ", nil).Enabled() {
		t.Fatal("a blank base URL must leave the client disabled")
	}
}

func TestHostCanonical(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"smp.mc.example.org", "smp.mc.example.org"},
		{"SMP.MC.Example.org", "smp.mc.example.org"},
		{"  smp.mc.example.org  ", "smp.mc.example.org"},
		{"smp.mc.example.org.", "smp.mc.example.org"}, // fully-qualified form the client may send
		{"", ""},
	} {
		if got := host(tc.in); got != tc.want {
			t.Fatalf("host(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRoutesObjectShape pins the shape a LIVE mc-router (v1.44) actually returns from GET /routes:
// an object per host, not the bare string that POST /routes accepts. The asymmetry is easy to miss
// and was a real bug — Routes() feeds Sync(), so failing to parse it means orphaned routes are never
// reconciled and a stale public domain keeps pointing at a port that has since been reused.
func TestRoutesObjectShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"smp.mc.example.org": {"backend":"127.0.0.1:25601","scalingTarget":"127.0.0.1:25601"},
			"old.mc.example.org": "127.0.0.1:25602"
		}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, srv.Client()).Routes(context.Background())
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	want := map[string]string{
		"smp.mc.example.org": "127.0.0.1:25601", // object form (current mc-router)
		"old.mc.example.org": "127.0.0.1:25602", // bare-string form (older builds)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Routes() = %v, want %v", got, want)
	}
}
