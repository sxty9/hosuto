package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmit(t *testing.T) {
	var got map[string]string
	var secret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret = r.Header.Get("X-Notify-Internal-Secret")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	c := New(srv.URL, "sekret")
	if !c.Enabled() {
		t.Fatal("client with a URL and secret should be enabled")
	}
	in := EmitInput{
		User:    "alice",
		Service: "hosuto",
		Title:   "Server stopped",
		Body:    "creeper-world exited",
		URL:     "/app/hosuto",
		Icon:    "server",
		Level:   "warning",
		Dedupe:  "hosuto:srv-1:stopped",
	}
	if err := c.Emit(in); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if secret != "sekret" {
		t.Fatalf("secret header = %q, want %q", secret, "sekret")
	}
	for field, want := range map[string]string{
		"user":    "alice",
		"service": "hosuto",
		"title":   "Server stopped",
		"body":    "creeper-world exited",
		"url":     "/app/hosuto",
		"icon":    "server",
		"level":   "warning",
		"dedupe":  "hosuto:srv-1:stopped",
	} {
		if got[field] != want {
			t.Errorf("payload[%q] = %q, want %q", field, got[field], want)
		}
	}
}

// Without a URL or secret the whole feature degrades to nothing — Emit must not error, so a caller
// never has to know whether notify is deployed.
func TestEmitDisabledIsNoOp(t *testing.T) {
	for _, c := range []*Client{New("", ""), New("http://127.0.0.1:1", ""), New("", "s")} {
		if c.Enabled() {
			t.Fatal("client missing a URL or secret should be disabled")
		}
		if err := c.Emit(EmitInput{User: "alice", Title: "hi"}); err != nil {
			t.Fatalf("disabled Emit should be a silent no-op, got %v", err)
		}
	}
}

func TestEmitErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if err := New(srv.URL, "s").Emit(EmitInput{User: "alice"}); err == nil {
		t.Fatal("expected an error on a non-2xx response")
	}
}
