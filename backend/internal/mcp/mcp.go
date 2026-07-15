// Package mcp is a minimal, domain-agnostic Model Context Protocol server over Streamable HTTP.
//
// It speaks JSON-RPC 2.0 (initialize / tools/list / tools/call / ping) and knows nothing about
// hosuto: a caller registers Tools, supplies an Authenticator that turns an HTTP request into an
// opaque caller identity, and this package handles the wire. hosuto's own tools live in package api,
// which gives the opaque caller and the tool arguments their meaning.
//
// The transport is the stateless flavour of Streamable HTTP: the client POSTs a single JSON-RPC
// request and receives one application/json response. There is no session id and no server-initiated
// SSE, because every hosuto tool is synchronous — there is nothing for the server to stream. This is
// exactly what an MCP client (Claude Desktop/Code, or the `claude` binary via --mcp-config) needs to
// list and call tools.
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// protocolVersion is the MCP revision this server advertises.
const protocolVersion = "2025-06-18"

// Tool is one callable operation.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage // a JSON Schema object; defaulted to {"type":"object"} when empty
	// Handler runs the tool. caller is whatever the Authenticator returned; args is the raw
	// "arguments" object. A returned error becomes an MCP tool error (a result with isError:true),
	// NOT a transport error — the model reads the message and adapts, exactly as it would to a failed
	// shell command.
	Handler func(ctx context.Context, caller any, args json.RawMessage) (any, error)
}

// Registry holds the tool set and the server identity.
type Registry struct {
	name    string
	version string
	mu      sync.RWMutex
	order   []string
	tools   map[string]Tool
}

// NewRegistry builds an empty registry that will announce itself as {name, version}.
func NewRegistry(name, version string) *Registry {
	return &Registry{name: name, version: version, tools: map[string]Tool{}}
}

// Register adds (or replaces) a tool. Registration order is preserved for tools/list.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name]; !ok {
		r.order = append(r.order, t.Name)
	}
	r.tools[t.Name] = t
}

// Authenticator turns a request into an opaque caller identity, or an error (→ JSON-RPC unauthorized).
// It is invoked for the methods that act on behalf of a user (tools/list, tools/call), never for the
// initialize handshake.
type Authenticator func(r *http.Request) (caller any, err error)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Handler returns the http.Handler for the single MCP endpoint.
func (r *Registry) Handler(authn Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			// GET would be for a server-initiated SSE stream, which this stateless server does not open.
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var call rpcRequest
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&call); err != nil {
			writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		// A JSON-RPC notification carries no id and expects no body back.
		if len(call.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: call.ID}
		switch call.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": r.name, "version": r.version},
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			if _, err := authn(req); err != nil {
				resp.Error = &rpcError{Code: -32001, Message: "unauthorized: " + err.Error()}
				break
			}
			resp.Result = map[string]any{"tools": r.list()}
		case "tools/call":
			caller, err := authn(req)
			if err != nil {
				resp.Error = &rpcError{Code: -32001, Message: "unauthorized: " + err.Error()}
				break
			}
			resp.Result, resp.Error = r.call(req.Context(), caller, call.Params)
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + call.Method}
		}
		writeRPC(w, resp)
	})
}

func (r *Registry) list() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return out
}

func (r *Registry) call(ctx context.Context, caller any, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
	}
	r.mu.RLock()
	t, ok := r.tools[p.Name]
	r.mu.RUnlock()
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
	res, err := t.Handler(ctx, caller, p.Arguments)
	if err != nil {
		// A tool error is a normal result with isError:true — the model sees it and can adapt, and one
		// bad call never fails the connection.
		return toolResult(err.Error(), true), nil
	}
	return toolResult(res, false), nil
}

// toolResult wraps a value as an MCP tool result. Strings pass through as text; everything else is
// JSON-encoded so the model gets structured data it can reason over.
func toolResult(v any, isErr bool) map[string]any {
	var text string
	switch x := v.(type) {
	case string:
		text = x
	case nil:
		text = "ok"
	default:
		if b, err := json.Marshal(x); err == nil {
			text = string(b)
		} else {
			text = "ok"
		}
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}
