package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ilter-ai/ilter/internal/features/mcp/protocol"
)

// streamableLog is scoped to the Streamable HTTP transport operations.
var streamableLog = slogWithSub("streamable_http")

// slogWithSub builds a package-scoped logger with a transport sub-component,
// mirroring the sseLog pattern in client_sse.go.
func slogWithSub(sub string) *slogLogger { return &slogLogger{sub: sub} }

// slogLogger is a minimal structured logger wrapper around the package's
// mcpLog. Keeping it local avoids a direct import of log/slog at each site
// while preserving the "component=mcp sub=<transport>" convention.
type slogLogger struct{ sub string }

func (l *slogLogger) Debug(msg string, args ...any) {
	mcpLog.Debug(msg, append([]any{"sub", l.sub}, args...)...)
}

func (l *slogLogger) Warn(msg string, args ...any) {
	mcpLog.Warn(msg, append([]any{"sub", l.sub}, args...)...)
}

// StreamableHTTPClient implements TransportClient for the MCP Streamable HTTP
// transport (the 2025-03-26+ replacement for legacy SSE). Unlike SSE there is
// no GET stream and no endpoint event: every JSON-RPC message is POSTed to
// the server's URL with Accept: application/json, text/event-stream, and the
// response is either a plain JSON body or an SSE stream whose first "message"
// event carries the JSON-RPC response. The server's Mcp-Session-Id response
// header is captured and sent back on subsequent requests; a 404 with an
// invalid session id triggers one re-Start (fresh session) and a single retry.
//
// Authorization (bearer/basic) is attached to every request, including the
// initialize handshake and notifications — this is the transport that fixes
// bearer 403s on modern "HTTP MCP" servers.
type StreamableHTTPClient struct {
	server *ServerInfo

	mu              sync.RWMutex
	sessionID       string
	connected       bool
	negotiatedVer   protocol.ID
	discoveredTools []ToolDefinition
}

// NewStreamableHTTPClient creates a Streamable HTTP transport client.
func NewStreamableHTTPClient(server *ServerInfo) *StreamableHTTPClient {
	return &StreamableHTTPClient{server: server}
}

func (c *StreamableHTTPClient) Start(ctx context.Context) error {
	c.mu.Lock()
	already := c.connected
	c.mu.Unlock()
	if already {
		return nil
	}
	if c.server.Config.URL == "" {
		return fmt.Errorf("streamable http server %q has no url configured", c.server.ID)
	}

	// Handshake (initialize) + tools/list via negotiateOutbound, exactly like
	// the SSE client. c.Call attaches auth + session headers.
	rawCall := func(ctx context.Context, method string, params json.RawMessage) (*JSONRPCResponse, error) {
		id := json.RawMessage(`"handshake"`)
		return c.Call(ctx, &JSONRPCRequest{JSONRPC: JSONRPCVersion, ID: &id, Method: method, Params: params})
	}
	sendNotification := func(method string, params json.RawMessage) {
		notifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, _ = c.post(notifyCtx, &JSONRPCRequest{JSONRPC: JSONRPCVersion, Method: method, Params: params})
	}

	ver, err := negotiateOutbound(ctx, c.server, rawCall, sendNotification)
	if err != nil {
		return fmt.Errorf("streamable http handshake: %w", err)
	}

	c.mu.Lock()
	c.negotiatedVer = ver.ID()
	c.connected = true
	c.mu.Unlock()

	tools, discErr := c.discoverTools(ctx)
	if discErr != nil {
		streamableLog.Warn("tools/list failed, tools will be unavailable",
			"server_id", c.server.ID, "error", discErr)
	} else {
		c.mu.Lock()
		c.discoveredTools = tools
		c.mu.Unlock()
		streamableLog.Debug("discovered tools",
			"server_id", c.server.ID, "count", len(tools))
	}
	return nil
}

func (c *StreamableHTTPClient) Call(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if req.ID == nil {
		return nil, fmt.Errorf("streamable http requires request IDs (notifications not supported here)")
	}

	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}

	// Session-expiry resilience: a 404 with an invalid Mcp-Session-Id means
	// the server dropped our session; re-establish it once and retry.
	if resp != nil && resp.Error != nil && resp.Error.Code == ErrorCodeNotInitialized {
		c.mu.Lock()
		c.connected = false
		c.sessionID = ""
		c.mu.Unlock()
		if err := c.Start(ctx); err != nil {
			return nil, fmt.Errorf("streamable http session expired and re-init failed: %w", err)
		}
		return c.post(ctx, req)
	}
	return resp, nil
}

func (c *StreamableHTTPClient) Close() error {
	// Streamable HTTP holds no persistent connection; the session simply
	// lapses server-side.
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
	return nil
}

func (c *StreamableHTTPClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *StreamableHTTPClient) NegotiatedVersion() protocol.ID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.negotiatedVer
}

func (c *StreamableHTTPClient) Tools() []ToolDefinition {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ToolDefinition, len(c.discoveredTools))
	copy(out, c.discoveredTools)
	return out
}

// post sends one JSON-RPC message and returns the JSON-RPC response, or nil
// for an accepted notification (202). Auth and the Mcp-Session-Id (when known)
// are attached to every request.
func (c *StreamableHTTPClient) post(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server.Config.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create post request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	setStreamableAuthHeaders(httpReq, c.server)

	c.mu.RLock()
	sid := c.sessionID
	c.mu.RUnlock()
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusAccepted {
		return nil, nil // notification accepted; no response body
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("post message: HTTP %d", resp.StatusCode)
	}

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return parseSSEMessageResponse(resp.Body)
	}

	var rpcResp JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &rpcResp, nil
}

// setStreamableAuthHeaders attaches the configured Authorization header
// (bearer/basic) to a Streamable HTTP request.
func setStreamableAuthHeaders(req *http.Request, server *ServerInfo) {
	switch server.Config.AuthType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+server.Config.AuthKeyEnv)
	case "basic":
		req.Header.Set("Authorization", "Basic "+server.Config.AuthKeyEnv)
	}
}

// parseSSEMessageResponse reads an SSE body and returns the first "message"
// event's JSON-RPC payload. Used when a streamable HTTP server responds to a
// POST with an SSE stream instead of a plain JSON body.
func parseSSEMessageResponse(body io.Reader) (*JSONRPCResponse, error) {
	scanner := bufio.NewScanner(body)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "":
			if data.Len() == 0 {
				continue
			}
			var rpcResp JSONRPCResponse
			if err := json.Unmarshal([]byte(data.String()), &rpcResp); err == nil {
				return &rpcResp, nil
			}
			data.Reset()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sse response: %w", err)
	}
	if data.Len() > 0 {
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal([]byte(data.String()), &rpcResp); err == nil {
			return &rpcResp, nil
		}
	}
	return nil, fmt.Errorf("no JSON-RPC message in SSE response")
}

// discoverTools calls tools/list with pagination via the streamable endpoint.
func (c *StreamableHTTPClient) discoverTools(ctx context.Context) ([]ToolDefinition, error) {
	var allTools []ToolDefinition
	cursor := ""
	const maxPages = 100

	for range maxPages {
		params := json.RawMessage("{}")
		if cursor != "" {
			params = json.RawMessage(`{"cursor":"` + cursor + `"}`)
		}

		listID := json.RawMessage(`"tools/list"`)
		req := &JSONRPCRequest{
			JSONRPC: JSONRPCVersion,
			ID:      &listID,
			Method:  MethodToolsList,
			Params:  params,
		}

		resp, err := c.Call(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("tools/list call: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list error (code %d): %s", resp.Error.Code, resp.Error.Message)
		}

		var result ListToolsResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("parse tools/list result: %w", err)
		}

		allTools = append(allTools, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	return allTools, nil
}
