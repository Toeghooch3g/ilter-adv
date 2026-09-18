package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/mcp/protocol"
)

// newFakeSSEServer starts a real HTTP server implementing just enough of
// the legacy SSE MCP transport to exercise SSEClient end-to-end: GET opens
// an SSE stream and emits an "endpoint" event; POST dispatches a JSON-RPC
// request through handler, and the result is pushed back as a "message"
// event on the GET stream (mirroring how a real MCP SSE server responds —
// the POST itself only ever returns 202 Accepted).
func newFakeSSEServer(t *testing.T, handler func(method string, params json.RawMessage) (json.RawMessage, *RPCError)) *httptest.Server {
	t.Helper()
	respCh := make(chan []byte, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseRecorder-less httptest server must support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: http://%s/sse/post\n\n", r.Host) //nolint:gosec // SSE data frame in a test fixture, not HTML
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-respCh:
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/sse/post", func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result, rpcErr := handler(req.Method, req.Params)
		resp := &JSONRPCResponse{JSONRPC: JSONRPCVersion, ID: req.ID, Result: result, Error: rpcErr}
		b, _ := json.Marshal(resp)
		respCh <- b
		w.WriteHeader(http.StatusAccepted)
	})
	return httptest.NewServer(mux)
}

// runStartWithTimeout guards against the exact self-deadlock bug that was
// fixed in SSEClient.Start() (it held a write lock for its whole body via
// defer while also calling RLock/Lock again from within that same call
// stack) — if that bug were ever reintroduced, this test would hang
// forever instead of failing cleanly, so it runs Start() on a goroutine
// and fails with a clear message on timeout rather than blocking the test
// suite.
func runStartWithTimeout(t *testing.T, c *SSEClient) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Start(context.Background())
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("SSEClient.Start() did not return within timeout — likely the self-deadlock bug is back (c.mu.Lock() held while RLock/Lock is called again on the same goroutine)")
		return nil
	}
}

func TestSSEClient_Start_NegotiatesNewestByDefault(t *testing.T) {
	srv := newFakeSSEServer(t, func(method string, _ json.RawMessage) (json.RawMessage, *RPCError) {
		switch method {
		case protocol.MethodServerDiscover:
			result, _ := protocol.MarshalDiscoverResult(protocol.ImplementationInfo{Name: "fake", Version: "1"})
			return result, nil
		case MethodToolsList:
			return json.RawMessage(`{"tools":[]}`), nil
		}
		return nil, &RPCError{Code: ErrorCodeMethodNotFound, Message: "not found"}
	})
	defer srv.Close()

	server := &ServerInfo{
		ID:     "fake-sse",
		Config: config.MCPServerConfig{ID: "fake-sse", Transport: "sse", URL: srv.URL + "/sse", ProtocolVersion: "auto"},
	}
	c := NewSSEClient(server)
	defer func() { _ = c.Close() }()

	if err := runStartWithTimeout(t, c); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("expected connected after successful Start")
	}
	if c.NegotiatedVersion() != protocol.V20260728 {
		t.Errorf("NegotiatedVersion() = %q, want newest %q", c.NegotiatedVersion(), protocol.V20260728)
	}
}

func TestSSEClient_Start_FallsBackToOlderVersion(t *testing.T) {
	srv := newFakeSSEServer(t, func(method string, params json.RawMessage) (json.RawMessage, *RPCError) {
		switch method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(params, &p)
			b, _ := json.Marshal(map[string]any{"protocolVersion": p.ProtocolVersion})
			return b, nil
		case MethodToolsList:
			return json.RawMessage(`{"tools":[]}`), nil
		}
		// server/discover unsupported — simulates a 2025-03-26-only server.
		return nil, &RPCError{Code: ErrorCodeMethodNotFound, Message: "not found"}
	})
	defer srv.Close()

	server := &ServerInfo{
		ID:     "fake-sse-old",
		Config: config.MCPServerConfig{ID: "fake-sse-old", Transport: "sse", URL: srv.URL + "/sse", ProtocolVersion: "auto"},
	}
	c := NewSSEClient(server)
	defer func() { _ = c.Close() }()

	if err := runStartWithTimeout(t, c); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if c.NegotiatedVersion() != protocol.V20250326 {
		t.Errorf("NegotiatedVersion() = %q, want %q", c.NegotiatedVersion(), protocol.V20250326)
	}
}

func TestSSEClient_Start_NoHandshakeAcceptedFails(t *testing.T) {
	srv := newFakeSSEServer(t, func(_ string, _ json.RawMessage) (json.RawMessage, *RPCError) {
		return nil, &RPCError{Code: ErrorCodeMethodNotFound, Message: "not found"}
	})
	defer srv.Close()

	server := &ServerInfo{
		ID:     "fake-sse-broken",
		Config: config.MCPServerConfig{ID: "fake-sse-broken", Transport: "sse", URL: srv.URL + "/sse", ProtocolVersion: "auto"},
	}
	c := NewSSEClient(server)
	defer func() { _ = c.Close() }()

	err := runStartWithTimeout(t, c)
	if err == nil {
		t.Fatal("expected Start() to fail when no protocol version's handshake is accepted")
	}
}

// TestSSEClient_RelativeEndpointResolved verifies the relative-endpoint fix:
// when a server emits a relative endpoint (data: /sse/post) instead of an
// absolute URL, the client resolves it against the dial URL so POSTs
// (initialize, tools/list) actually reach the server — previously they died
// with "unsupported protocol scheme". Also asserts the bearer token is
// attached to both the GET dial and the POSTs.
func TestSSEClient_RelativeEndpointResolved(t *testing.T) {
	var mu sync.Mutex
	var authSeen []string
	respCh := make(chan []byte, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authSeen = append(authSeen, "GET /sse -> "+r.Header.Get("Authorization"))
		mu.Unlock()
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("no flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Relative endpoint — the bug: the client used this verbatim as a URL.
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /sse/post\n\n") //nolint:gosec // SSE fixture
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-respCh:
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/sse/post", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authSeen = append(authSeen, "POST /sse/post -> "+r.Header.Get("Authorization"))
		mu.Unlock()
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": string(protocol.V20250326),
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case MethodToolsList:
			result = map[string]any{"tools": []map[string]any{}}
		default:
			result = map[string]any{}
		}
		b, _ := json.Marshal(&JSONRPCResponse{JSONRPC: JSONRPCVersion, ID: req.ID, Result: mustRaw(result)})
		respCh <- b
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	server := &ServerInfo{
		ID:     "fake-sse-relative",
		Config: config.MCPServerConfig{ID: "fake-sse-relative", Transport: "sse", URL: srv.URL + "/sse", AuthType: "bearer", AuthKeyEnv: "tok-123", ProtocolVersion: "auto"},
	}
	c := NewSSEClient(server)
	defer func() { _ = c.Close() }()

	if err := runStartWithTimeout(t, c); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("expected connected")
	}

	// Give the async readLoop a moment to process handshake + tools/list.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	var gotDial, gotPost bool
	for _, s := range authSeen {
		if strings.Contains(s, "GET /sse -> Bearer tok-123") {
			gotDial = true
		}
		if strings.Contains(s, "POST /sse/post -> Bearer tok-123") {
			gotPost = true
		}
	}
	if !gotDial {
		t.Errorf("expected Authorization on the GET /sse dial; saw %v", authSeen)
	}
	if !gotPost {
		t.Errorf("expected Authorization on POST /sse/post (resolved relative endpoint); saw %v", authSeen)
	}
}

// mustRaw marshals v to raw JSON bytes, panicking on failure (test helper).
func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
