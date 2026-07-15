// Package aigentic is hosuto's thin client for aigentic's internal machine-to-machine run endpoint
// (POST internal/run, shared-secret auth). It lets hosuto run one agentic turn ON BEHALF OF an
// operator — for the in-game "!ai" CLI, where there is no browser to carry the operator's session —
// billed to that operator's own aigentic credential. The subject is named in the body; aigentic
// resolves it to a live OS identity and holds it to exactly the same rights gates as the cookie-authed
// path, so hosuto never escalates anyone. A disabled client (no URL or secret) leaves the whole
// in-game AI feature inert rather than taking the daemon down — the same degrade-silently contract as
// the notify/contax clients.
package aigentic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrDisabled means the client has no base URL or secret configured, so the M2M channel is off.
var ErrDisabled = errors.New("aigentic: client not configured")

// ErrNoCredential means aigentic turned the run away because the subject has no Claude credential
// linked (subscription token or API key). aigentic reports this as a 503 (the engine is unavailable
// for this user); for the forced claude-cli engine that is overwhelmingly the actionable cause, so
// the in-game CLI can tell the operator to link one in aigentic.
var ErrNoCredential = errors.New("aigentic: the operator has no Claude credential linked in aigentic")

// MCPRef attaches one MCP server to the run. Name selects a provider aigentic has been configured to
// allow — the URL lives server-side, never on the wire — and Token is the caller's scoped bearer for
// that provider (here: a hosuto MCP token minted for the operator + one server).
type MCPRef struct {
	Name  string `json:"name"`
	Token string `json:"token,omitempty"`
}

// Req is the subset of aigentic's request that the in-game CLI needs. Kind selects the engine
// ("claude-cli" for the agentic MCP loop); System binds the turn to one server; MCP attaches hosuto's
// tools.
type Req struct {
	Kind   string   // aigentic engine kind: "claude-cli" | "claude-api" | "ollama" | "choose"
	Prompt string   // the user turn (typically the rendered transcript)
	System string   // extra system guidance appended by the engine (server-binding context)
	Model  string   // optional model override; "" => the engine's default
	MCP    []MCPRef // MCP servers to attach for this run
}

// Res is the subset of aigentic's result the CLI renders and persists.
type Res struct {
	Output string // the model's answer
	Engine string // leaf kind that actually ran
	Model  string // concrete model id used
}

// Client posts to aigentic's internal/run endpoint with the shared internal secret.
type Client struct {
	base   string // aigentic base, e.g. http://127.0.0.1:8780
	secret string
	http   *http.Client
}

// New builds a client. baseURL is e.g. http://127.0.0.1:8780; secret is the shared aigentic internal
// secret. An empty base URL or secret leaves the client disabled (Run returns ErrDisabled). The
// timeout is generous: an agentic claude-cli turn with MCP tool calls can take tens of seconds, and
// the caller further bounds it with the request context.
func New(baseURL, secret string) *Client {
	return &Client{
		base:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		secret: strings.TrimSpace(secret),
		http:   &http.Client{Timeout: 5 * time.Minute},
	}
}

// Enabled reports whether the client is configured to run turns.
func (c *Client) Enabled() bool { return c.base != "" && c.secret != "" }

// wireData mirrors the fields of aigentic.Request that the CLI sets. Marshaled into the opaque prizm
// Data₀ the endpoint decodes for the selected engine.
type wireData struct {
	Prompt string   `json:"prompt"`
	System string   `json:"system,omitempty"`
	Model  string   `json:"model,omitempty"`
	MCP    []MCPRef `json:"mcp,omitempty"`
}

// wireReq is the internal/run body: {subject, header, data}. Only header.kind matters for routing;
// aigentic stamps the authoritative subject itself from the top-level field.
type wireReq struct {
	Subject string `json:"subject"`
	Header  struct {
		Kind string `json:"kind"`
	} `json:"header"`
	Data json.RawMessage `json:"data"`
}

// Run executes one turn on behalf of subject and returns the engine's answer. It maps a missing
// Claude credential (aigentic 503) to ErrNoCredential and any other non-2xx to a generic error
// carrying aigentic's detail; a transport error (including the context deadline) is returned as-is so
// the caller can distinguish a timeout.
func (c *Client) Run(ctx context.Context, subject string, req Req) (Res, error) {
	if !c.Enabled() {
		return Res{}, ErrDisabled
	}
	data, err := json.Marshal(wireData{Prompt: req.Prompt, System: req.System, Model: req.Model, MCP: req.MCP})
	if err != nil {
		return Res{}, err
	}
	var body wireReq
	body.Subject = subject
	body.Header.Kind = req.Kind
	body.Data = data
	buf, err := json.Marshal(body)
	if err != nil {
		return Res{}, err
	}

	endpoint := c.base + "/api/services/aigentic/internal/run"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return Res{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Aigentic-Internal-Secret", c.secret)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Res{}, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return Res{}, fmt.Errorf("%w: %s", ErrNoCredential, detailOf(resp.Body))
	}
	if resp.StatusCode >= 300 {
		return Res{}, fmt.Errorf("aigentic: run failed (%d): %s", resp.StatusCode, detailOf(resp.Body))
	}

	// 200: a prizm response {header, data} whose data is the engine's Result.
	var out struct {
		Data struct {
			Output string `json:"output"`
			Engine string `json:"engine"`
			Model  string `json:"model"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Res{}, fmt.Errorf("aigentic: bad response: %w", err)
	}
	return Res{Output: out.Data.Output, Engine: out.Data.Engine, Model: out.Data.Model}, nil
}

// detailOf pulls the {"detail": …} message out of an aigentic error body, falling back to the raw
// bytes when the body is not the expected shape.
func detailOf(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(b, &e) == nil && e.Detail != "" {
		return e.Detail
	}
	return strings.TrimSpace(string(b))
}
