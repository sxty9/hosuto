package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// post drives one JSON-RPC call against a handler and returns the decoded response.
func post(t *testing.T, h http.Handler, header, body string) rpcResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if header != "" {
		req.Header.Set("X-Test", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp rpcResponse
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return resp
}

// testAuth accepts the caller iff X-Test: ok.
func testAuth(r *http.Request) (any, error) {
	if r.Header.Get("X-Test") == "ok" {
		return "alice", nil
	}
	return nil, errors.New("no")
}

func TestProtocol(t *testing.T) {
	reg := NewRegistry("hosuto", "0.1.0")
	reg.Register(Tool{
		Name:        "echo",
		Description: "echo",
		Handler: func(_ context.Context, caller any, args json.RawMessage) (any, error) {
			return map[string]any{"caller": caller, "args": string(args)}, nil
		},
	})
	reg.Register(Tool{
		Name: "boom",
		Handler: func(_ context.Context, _ any, _ json.RawMessage) (any, error) {
			return nil, errors.New("kaboom")
		},
	})
	h := reg.Handler(testAuth)

	// initialize needs no auth and echoes the protocol version + server info.
	init := post(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if init.Error != nil {
		t.Fatalf("initialize errored: %+v", init.Error)
	}
	res, _ := json.Marshal(init.Result)
	if !strings.Contains(string(res), protocolVersion) || !strings.Contains(string(res), `"name":"hosuto"`) {
		t.Fatalf("initialize result missing version/name: %s", res)
	}

	// tools/list without auth is rejected.
	if got := post(t, h, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); got.Error == nil {
		t.Fatal("tools/list without auth should error")
	}

	// tools/list with auth returns both tools.
	listed := post(t, h, "ok", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if listed.Error != nil {
		t.Fatalf("tools/list errored: %+v", listed.Error)
	}
	body, _ := json.Marshal(listed.Result)
	if !strings.Contains(string(body), `"echo"`) || !strings.Contains(string(body), `"boom"`) {
		t.Fatalf("tools/list missing tools: %s", body)
	}
	// every tool must carry an inputSchema, even the one that declared none.
	if strings.Count(string(body), `"inputSchema"`) != 2 {
		t.Fatalf("expected an inputSchema per tool: %s", body)
	}

	// tools/call runs the tool and passes the caller identity through.
	call := post(t, h, "ok", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)
	if call.Error != nil {
		t.Fatalf("tools/call errored: %+v", call.Error)
	}
	got, _ := json.Marshal(call.Result)
	if !strings.Contains(string(got), `alice`) || strings.Contains(string(got), `"isError":true`) {
		t.Fatalf("echo result wrong: %s", got)
	}

	// a tool that returns an error is a result with isError:true, NOT a transport error.
	bang := post(t, h, "ok", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"boom"}}`)
	if bang.Error != nil {
		t.Fatalf("tool error must not be a transport error: %+v", bang.Error)
	}
	bs, _ := json.Marshal(bang.Result)
	if !strings.Contains(string(bs), `"isError":true`) || !strings.Contains(string(bs), "kaboom") {
		t.Fatalf("boom should surface as an isError result: %s", bs)
	}

	// an unknown tool is a protocol error.
	if got := post(t, h, "ok", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope"}}`); got.Error == nil {
		t.Fatal("unknown tool should error")
	}

	// a notification (no id) gets 202 and no body.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("notification should be 202 with empty body, got %d %q", rec.Code, rec.Body.String())
	}

	// GET is rejected (no server-initiated stream).
	greq := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, greq)
	if grec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET should be 405, got %d", grec.Code)
	}
}

func TestTokenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ts, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	tok, exp, err := ts.Mint("alice", "srv-123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "hmcp_") || exp.Before(time.Now()) {
		t.Fatalf("bad mint: %q %v", tok, exp)
	}

	// a valid token resolves to its subject + scope.
	sub, scope, ok := ts.Lookup(tok)
	if !ok || sub != "alice" || scope != "srv-123" {
		t.Fatalf("lookup: %q %q %v", sub, scope, ok)
	}
	// a wrong token does not.
	if _, _, ok := ts.Lookup("hmcp_deadbeef"); ok {
		t.Fatal("unknown token must not resolve")
	}

	// tokens survive a reopen (only the hash is persisted, so the clear token still validates).
	ts2, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ts2.Lookup(tok); !ok {
		t.Fatal("token should survive reopen")
	}
	// and the file must not contain the clear token.
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), tok) {
		t.Fatal("clear token leaked into the token file")
	}

	// an expired token does not resolve.
	dead, _, err := ts2.Mint("bob", "", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ts2.Lookup(dead); ok {
		t.Fatal("expired token must not resolve")
	}

	// Active lists only live tokens; RevokeAll clears them.
	if got := ts2.Active("alice"); len(got) != 1 || got[0].Scope != "srv-123" {
		t.Fatalf("active alice: %+v", got)
	}
	if err := ts2.RevokeAll("alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ts2.Lookup(tok); ok {
		t.Fatal("revoked token must not resolve")
	}
}
