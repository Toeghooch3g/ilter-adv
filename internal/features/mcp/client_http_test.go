package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/mcp/protocol"
)

// newFakeStreamableServer runs a real HTTP server implementing the MCP
// Streamable HTTP transport for one session: initialize, notifications/
// initialized (202), tools/list. It records the Authorization header on every
// request so tests can assert the bearer token is attached to all of them.
func newFakeStreamableServer(t *testing.T, authHeader string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var authSeen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authSeen = append(authSeen, r.Header.Get("Authorization"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case MethodInitialize, protocol.MethodServerDiscover:
			result, _ := protocol.MarshalDiscoverResult(protocol.ImplementationInfo{Name: "fake", Version: "1"})
			// Streamable HTTP initialize returns protocolVersion + serverInfo.
			// MarshalDiscoverResult emits DiscoverResult (protocolVersions+serverInfo),
			// which ParseServerHandshake accepts (protocolVersion check passes for
			// empty protocolVersion). To be faithful to the 2025-03-26 shape, build
			// the initializeResult explicitly.
			initResult, _ := json.Marshal(map[string]any{
				"protocolVersion": string(protocol.V20250326),
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})
			_ = initResult
			_ = result
			writeRPCResult(w, &req, map[string]any{
				"protocolVersion": string(protocol.V20250326),
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case MethodToolsList:
			writeRPCResult(w, &req, map[string]any{"tools": []map[string]any{{"name": "echo", "description": "echo"}}})
		case MethodToolsCall:
			writeRPCResult(w, &req, map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))

	_ = authHeader
	return srv, &authSeen
}

func writeRPCResult(w http.ResponseWriter, req *JSONRPCRequest, result any) {
	resp := JSONRPCResponse{JSONRPC: JSONRPCVersion, ID: req.ID}
	b, _ := json.Marshal(result)
	resp.Result = b
	_ = json.NewEncoder(w).Encode(resp)
}

// TestStreamableHTTPClient_AuthorizationEveryRequest verifies the bearer token
// is attached to the initialize handshake, tools/list, and a tools/call — the
// exact gap the user hit (403s on HTTP MCP servers).
func TestStreamableHTTPClient_AuthorizationEveryRequest(t *testing.T) {
	srv, authSeen := newFakeStreamableServer(t, "Bearer test-token")
	defer srv.Close()

	server := &ServerInfo{
		ID: "fake-http",
		Config: config.MCPServerConfig{
			ID:              "fake-http",
			Transport:       "http",
			URL:             srv.URL,
			AuthType:        "bearer",
			AuthKeyEnv:      "test-token",
			ProtocolVersion: "auto",
		},
	}
	c := NewStreamableHTTPClient(server)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("expected connected after Start")
	}

	// Exercise a tool call (the 4th request type after handshake + notifications).
	id := json.RawMessage(`"call-1"`)
	_, err := c.Call(ctx, &JSONRPCRequest{JSONRPC: JSONRPCVersion, ID: &id, Method: MethodToolsCall, Params: json.RawMessage(`{"name":"echo","arguments":{}}`)})
	if err != nil {
		t.Fatalf("Call() error: %v", err)
	}

	mu := &sync.Mutex{}
	mu.Lock()
	defer mu.Unlock()
	if len(*authSeen) < 3 {
		t.Fatalf("expected auth on initialize, tools/list, tools/call; saw %d requests", len(*authSeen))
	}
	for i, got := range *authSeen {
		if got != "Bearer test-token" {
			t.Errorf("request %d Authorization = %q, want %q", i, got, "Bearer test-token")
		}
	}
}

// TestStreamableHTTPClient_SessionHeaderPropagation verifies the server's
// Mcp-Session-Id is captured on the handshake and sent back on later calls.
func TestStreamableHTTPClient_SessionHeaderPropagation(t *testing.T) {
	var mu sync.Mutex
	var sawSessionHeader []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawSessionHeader = append(sawSessionHeader, r.Header.Get("Mcp-Session-Id"))
		mu.Unlock()
		w.Header().Set("Mcp-Session-Id", "sess-123")
		w.Header().Set("Content-Type", "application/json")
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case MethodInitialize, protocol.MethodServerDiscover:
			writeRPCResult(w, &req, map[string]any{
				"protocolVersion": string(protocol.V20250326),
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})
		case MethodToolsList:
			writeRPCResult(w, &req, map[string]any{"tools": []map[string]any{}})
		default:
			writeRPCResult(w, &req, map[string]any{"ok": true})
		}
	}))
	defer srv.Close()

	server := &ServerInfo{
		ID:     "fake-http-session",
		Config: config.MCPServerConfig{ID: "fake-http-session", Transport: "http", URL: srv.URL, ProtocolVersion: "auto"},
	}
	c := NewStreamableHTTPClient(server)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	id := json.RawMessage(`"call-1"`)
	if _, err := c.Call(ctx, &JSONRPCRequest{JSONRPC: JSONRPCVersion, ID: &id, Method: MethodToolsCall}); err != nil {
		t.Fatalf("Call() error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Request 0 = initialize (no session yet), later ones carry sess-123.
	// tools/list and tools/call should both send the captured session id.
	if len(sawSessionHeader) < 3 {
		t.Fatalf("expected >= 3 requests, saw %d", len(sawSessionHeader))
	}
	if sawSessionHeader[0] != "" {
		t.Errorf("initialize should not yet carry a session id, got %q", sawSessionHeader[0])
	}
	for i := 1; i < len(sawSessionHeader); i++ {
		if sawSessionHeader[i] != "sess-123" {
			t.Errorf("request %d Mcp-Session-Id = %q, want sess-123", i, sawSessionHeader[i])
		}
	}
}
