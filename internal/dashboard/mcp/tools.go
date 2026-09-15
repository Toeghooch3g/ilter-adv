package dashmcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ilter-ai/ilter/internal/model"

	"github.com/go-chi/chi/v5"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
	"github.com/ilter-ai/ilter/internal/features/mcp"
	mcptransport "github.com/ilter-ai/ilter/internal/platform/transport/mcp"
)

var mcpLog = slog.With("component", "mcp")

// resolveMCPServerConfigForTest loads id's server row and builds the
// config.MCPServerConfig TestServer needs to spin up a transport client,
// along with the timeout to allow when OAuth is in play.
func resolveMCPServerConfigForTest(store *db.SQLiteStore, id string) (cfg config.MCPServerConfig, useOAuthTimeout time.Duration, err error) {
	var name, transport, url, command, args, env, authType, authKeyEnv sql.NullString
	var timeoutMs, maxRetries sql.NullInt64
	err = store.DB.QueryRow(`SELECT name, transport, url, command, args, env, timeout_ms, max_retries, auth_type, auth_key_env
		FROM mcp_servers WHERE id = ?`, id).Scan(&name, &transport, &url, &command, &args, &env, &timeoutMs, &maxRetries, &authType, &authKeyEnv)
	if err != nil {
		return cfg, 0, err
	}

	cfg = config.MCPServerConfig{
		Transport:  transport.String,
		URL:        url.String,
		Command:    command.String,
		AuthType:   authType.String,
		AuthKeyEnv: authKeyEnv.String,
		MaxRetries: int(maxRetries.Int64),
	}

	if args.Valid && args.String != "" {
		var parsedArgs []string
		if e := json.Unmarshal([]byte(args.String), &parsedArgs); e == nil {
			cfg.Args = parsedArgs
		}
	}

	if env.Valid && env.String != "" {
		var parsedEnv map[string]string
		if e := json.Unmarshal([]byte(env.String), &parsedEnv); e == nil {
			cfg.Env = parsedEnv
		}
	}

	timeout := 30 * time.Second
	useOAuthTimeout = 2 * time.Minute
	if timeoutMs.Valid && timeoutMs.Int64 > 0 {
		timeout = time.Duration(timeoutMs.Int64) * time.Millisecond
		useOAuthTimeout = timeout
	}
	cfg.Timeout = timeout.String()

	return cfg, useOAuthTimeout, nil
}

// attachOAuthCallback starts a built-in OAuth callback server so the MCP
// server under test can use ilter's callback URL instead of running its
// own, injecting it into cfg.Env as ILTER_OAUTH_CALLBACK_URL. Returns nil
// if the callback server failed to start.
func attachOAuthCallback(cfg *config.MCPServerConfig) *mcptransport.OAuthCallbackServer {
	oauthSrv := mcptransport.NewOAuthCallbackServer("")
	oauthURL, err := oauthSrv.Start()
	if err != nil {
		return nil
	}
	if cfg.Env == nil {
		cfg.Env = make(map[string]string)
	}
	cfg.Env["ILTER_OAUTH_CALLBACK_URL"] = oauthURL
	return oauthSrv
}

// writeMCPTestConnectError writes a TestServer connection-phase error
// response, including an oauth_url when one is discoverable in stderr.
func writeMCPTestConnectError(w http.ResponseWriter, errMsg string, client mcp.TransportClient) {
	stderr := extractStderr(client)
	resp := map[string]any{
		"status":      "error",
		"tools_count": 0,
		"error":       errMsg,
		"stderr":      stderr,
	}
	if u := extractOAuthURL(stderr); u != "" {
		resp["oauth_url"] = u
	}
	model.WriteJSON(w, http.StatusOK, resp)
}

// awaitMCPTestConnection waits for the client's Start(ctx) to finish (or
// for ctx to time out, or for an OAuth authorization URL to appear in its
// stderr), writing an appropriate response to w for every non-proceed
// outcome. Returns true only when Start succeeded and the caller should go
// on to the handshake — in that case the caller owns cleanup (cancel +
// client.Close). In every false case except the timeout/failure ones,
// cleanup has already run here.
func awaitMCPTestConnection(w http.ResponseWriter, ctx context.Context, cancel context.CancelFunc, client mcp.TransportClient, startCh <-chan error) bool {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-startCh:
			if err != nil {
				cancel()
				_ = client.Close()
				writeMCPTestConnectError(w, "Connection failed: "+err.Error(), client)
				return false
			}
			return true
		case <-ticker.C:
			if u := extractOAuthURL(extractStderr(client)); u != "" {
				model.WriteJSON(w, http.StatusOK, map[string]any{
					"status":      "error",
					"tools_count": 0,
					"error":       "OAuth authorization required. Open the link below, authorize, then test again.",
					"oauth_url":   u,
				})
				// Keep client alive — OAuth callback port is in the MCP process.
				// Goroutine with client.Start(ctx) continues; token gets cached
				// after user authorizes. Second test will use cached token.
				return false
			}
		case <-ctx.Done():
			cancel()
			_ = client.Close()
			writeMCPTestConnectError(w, "Connection timed out: "+ctx.Err().Error(), client)
			return false
		}
	}
}

// performMCPHandshakeAndList runs TestServer's initialize + tools/list
// JSON-RPC calls against client, writing an error response to w and
// returning nil if either step fails.
func performMCPHandshakeAndList(w http.ResponseWriter, ctx context.Context, client mcp.TransportClient) *mcp.ListToolsResult {
	initID := json.RawMessage(`"1"`)
	initReq := &mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      &initID,
		Method:  mcp.MethodInitialize,
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"ilter","version":"1.0"}}`),
	}
	if _, err := client.Call(ctx, initReq); err != nil { //nolint:contextcheck // ctx is the intentional Background-derived timeout from TestServer (MCP process must outlive this HTTP request)
		writeMCPTestConnectError(w, "Handshake failed: "+err.Error(), client)
		return nil
	}

	listID := json.RawMessage(`"2"`)
	listReq := &mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      &listID,
		Method:  mcp.MethodToolsList,
	}
	listResp, err := client.Call(ctx, listReq) //nolint:contextcheck // ctx is the intentional Background-derived timeout from TestServer (MCP process must outlive this HTTP request)
	if err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"status":      "error",
			"tools_count": 0,
			"error":       "tools/list failed: " + err.Error(),
			"stderr":      extractStderr(client),
		})
		return nil
	}

	if listResp.Error != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"status":      "error",
			"tools_count": 0,
			"error":       fmt.Sprintf("Server error: %s (code %d)", listResp.Error.Message, listResp.Error.Code),
			"stderr":      extractStderr(client),
		})
		return nil
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &listResult); err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"status":      "error",
			"tools_count": 0,
			"error":       "Failed to parse tools list: " + err.Error(),
			"stderr":      extractStderr(client),
		})
		return nil
	}

	return &listResult
}

// syncMCPToolsAfterTest replaces id's stored tools with tools (best-effort:
// logs, doesn't fail the test response, on any DB error) and bumps the
// server's updated_at.
func (h *MCPHandler) syncMCPToolsAfterTest(id string, tools []mcp.ToolDefinition) {
	tx, err := h.store.DB.Begin()
	if err == nil {
		if _, e := tx.Exec("DELETE FROM mcp_tools WHERE server_id = ?", id); e != nil {
			mcpLog.Warn("failed to clear old tools during sync", "server_id", id, "error", e)
			_ = tx.Rollback()
		} else {
			for _, tool := range tools {
				toolID := tool.Name + "-" + id
				if _, e := tx.Exec(`INSERT OR REPLACE INTO mcp_tools (id, server_id, name, description, schema) VALUES (?, ?, ?, ?, ?)`,
					toolID, id, tool.Name, tool.Description, string(tool.InputSchema)); e != nil {
					mcpLog.Warn("failed to insert tool during sync", "server_id", id, "tool", tool.Name, "error", e)
					_ = tx.Rollback()
					break
				}
			}
			_ = tx.Commit()
		}
	}

	if _, err := h.store.DB.Exec("UPDATE mcp_servers SET updated_at = datetime('now') WHERE id = ?", id); err != nil {
		mcpLog.Warn("failed to update server timestamp", "server_id", id, "error", err)
	}
}

func (h *MCPHandler) TestServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Server ID is required")
		return
	}

	cfg, useOAuthTimeout, err := resolveMCPServerConfigForTest(h.store, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			model.WriteJSONError(w, http.StatusNotFound, "not_found", "Server not found")
			return
		}
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to query server")
		return
	}

	// Start a built-in OAuth callback server so MCP servers can use ilter's
	// callback URL instead of running their own. The URL is injected as
	// ILTER_OAUTH_CALLBACK_URL in the process environment.
	if oauthSrv := attachOAuthCallback(&cfg); oauthSrv != nil {
		//nolint:contextcheck // Stop() intentionally uses its own bounded 5s shutdown context, not the request's (which may already be canceled by the time this deferred call runs)
		defer oauthSrv.Stop()
	}

	serverInfo := &mcp.ServerInfo{
		ID:     id,
		Config: cfg,
	}

	client, err := mcp.NewTransportClient(serverInfo)
	if err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"status":      "error",
			"tools_count": 0,
			"error":       "Failed to create client: " + err.Error(),
		})
		return
	}

	// Use Background context so the MCP process stays alive after the HTTP handler
	// returns — the OAuth callback server runs inside the MCP process.
	ctx, cancel := context.WithTimeout(context.Background(), useOAuthTimeout)

	startCh := make(chan error, 1)
	go func() {
		startCh <- client.Start(ctx)
	}()

	if !awaitMCPTestConnection(w, ctx, cancel, client, startCh) { //nolint:contextcheck // ctx is the intentional Background-derived timeout from above (MCP process must outlive this HTTP request)
		return
	}
	defer func() { cancel(); _ = client.Close() }()

	listResult := performMCPHandshakeAndList(w, ctx, client) //nolint:contextcheck // ctx is the intentional Background-derived timeout from above (MCP process must outlive this HTTP request)
	if listResult == nil {
		return
	}

	h.syncMCPToolsAfterTest(id, listResult.Tools)

	if h.registry != nil {
		if err := h.registry.SyncTools(r.Context(), id, listResult.Tools); err != nil {
			mcpLog.Warn("failed to sync tools to registry after test",
				"server_id", id, "error", err)
		}
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"status":      "online",
		"tools_count": len(listResult.Tools),
	})
}

func (h *MCPHandler) ListServerTools(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Server ID is required")
		return
	}

	rows, err := h.store.DB.Query(`SELECT id, name, description, schema, created_at
		FROM mcp_tools WHERE server_id = ? ORDER BY name`, id)
	if err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to list tools")
		return
	}
	defer func() { _ = rows.Close() }()

	tools := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, description, inputSchema, createdAt sql.NullString
		if err := rows.Scan(&id, &name, &description, &inputSchema, &createdAt); err != nil {
			continue
		}
		tools = append(tools, map[string]any{
			"id":           nullToEmpty(id),
			"name":         nullToEmpty(name),
			"description":  nullToEmpty(description),
			"input_schema": nullToEmpty(inputSchema),
			"created_at":   nullToEmpty(createdAt),
		})
	}
	if err := rows.Err(); err != nil {
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to list tools")
		return
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"server_id": id,
		"tools":     tools,
	})
}

// resolveMCPServerConfigForCall loads id's server row and builds the
// config.MCPServerConfig CallServerTool needs to spin up a transport
// client, along with its configured timeout.
func resolveMCPServerConfigForCall(store *db.SQLiteStore, id string) (cfg config.MCPServerConfig, timeout time.Duration, err error) {
	var name, transport, url, command, args, env, authType, authKeyEnv sql.NullString
	var timeoutMs, maxRetries sql.NullInt64
	err = store.DB.QueryRow(`SELECT name, transport, url, command, args, env, timeout_ms, max_retries, auth_type, auth_key_env
		FROM mcp_servers WHERE id = ?`, id).Scan(&name, &transport, &url, &command, &args, &env, &timeoutMs, &maxRetries, &authType, &authKeyEnv)
	if err != nil {
		return cfg, 0, err
	}

	cfg = config.MCPServerConfig{
		Transport:  transport.String,
		URL:        url.String,
		Command:    command.String,
		AuthType:   authType.String,
		AuthKeyEnv: authKeyEnv.String,
		MaxRetries: int(maxRetries.Int64),
	}
	if args.Valid && args.String != "" {
		var parsedArgs []string
		if e := json.Unmarshal([]byte(args.String), &parsedArgs); e == nil {
			cfg.Args = parsedArgs
		}
	}
	if env.Valid && env.String != "" {
		var parsedEnv map[string]string
		if e := json.Unmarshal([]byte(env.String), &parsedEnv); e == nil {
			cfg.Env = parsedEnv
		}
	}

	timeout = 30 * time.Second
	if timeoutMs.Valid && timeoutMs.Int64 > 0 {
		timeout = time.Duration(timeoutMs.Int64) * time.Millisecond
	}
	cfg.Timeout = timeout.String()

	return cfg, timeout, nil
}

// buildToolCallContent renders a CallToolResult's content items as the
// dashboard API's JSON shape.
func buildToolCallContent(items []mcp.ToolContent) []map[string]any {
	content := make([]map[string]any, len(items))
	for i, c := range items {
		item := map[string]any{"type": c.Type}
		if c.Text != "" {
			item["text"] = c.Text
		}
		if c.Data != "" {
			item["data"] = c.Data
		}
		if c.MIMEType != "" {
			item["mimeType"] = c.MIMEType
		}
		if c.URI != "" {
			item["uri"] = c.URI
		}
		content[i] = item
	}
	return content
}

func (h *MCPHandler) CallServerTool(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Server ID is required")
		return
	}

	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request body")
		return
	}
	if req.Name == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Tool name is required")
		return
	}

	cfg, timeout, err := resolveMCPServerConfigForCall(h.store, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			model.WriteJSONError(w, http.StatusNotFound, "not_found", "Server not found")
			return
		}
		model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "Failed to query server")
		return
	}

	serverInfo := &mcp.ServerInfo{
		ID:     id,
		Config: cfg,
	}

	client, err := mcp.NewTransportClient(serverInfo)
	if err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "Failed to create client: " + err.Error()}},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err = client.Start(ctx); err != nil {
		_ = client.Close()
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "Connection failed: " + err.Error()}},
			"stderr":  extractStderr(client),
		})
		return
	}
	defer func() { _ = client.Close() }()

	initID := json.RawMessage(`"1"`)
	initReq := &mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      &initID,
		Method:  mcp.MethodInitialize,
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"ilter","version":"1.0"}}`),
	}
	_, err = client.Call(ctx, initReq)
	if err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "Handshake failed: " + err.Error()}},
		})
		return
	}

	callParams := mcp.CallToolParams{
		Name:      req.Name,
		Arguments: req.Arguments,
	}
	callBody, _ := json.Marshal(callParams)
	callID := json.RawMessage(`"2"`)
	callReq := &mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      &callID,
		Method:  mcp.MethodToolsCall,
		Params:  callBody,
	}
	resp, err := client.Call(ctx, callReq)
	if err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "Tool call failed: " + err.Error()}},
		})
		return
	}

	if resp.Error != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": resp.Error.Message}},
		})
		return
	}

	var result mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		model.WriteJSON(w, http.StatusOK, map[string]any{
			"isError": false,
			"content": []map[string]any{{"type": "text", "text": string(resp.Result)}},
		})
		return
	}

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"isError": result.IsError,
		"content": buildToolCallContent(result.Content),
	})
}

// resolveMCPServerConfigForSync loads id's server row and builds the
// config.MCPServerConfig syncServerByID needs to spin up a transport
// client, along with its display name and configured timeout.
func resolveMCPServerConfigForSync(ctx context.Context, store *db.SQLiteStore, id string) (name string, cfg config.MCPServerConfig, syncTimeout time.Duration, err error) {
	var nameNS, transport, url, command, args, env, authType, authKeyEnv sql.NullString
	var timeoutMs, maxRetries sql.NullInt64
	err = store.DB.QueryRowContext(ctx, `SELECT name, transport, url, command, args, env, auth_type, auth_key_env, timeout_ms, max_retries
		FROM mcp_servers WHERE id = ?`, id).Scan(&nameNS, &transport, &url, &command, &args, &env, &authType, &authKeyEnv, &timeoutMs, &maxRetries)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", cfg, 0, fmt.Errorf("server %q not found", id)
		}
		return "", cfg, 0, fmt.Errorf("failed to query server: %w", err)
	}

	syncTimeout = 30 * time.Second
	if timeoutMs.Valid && timeoutMs.Int64 > 0 {
		syncTimeout = time.Duration(timeoutMs.Int64) * time.Millisecond
	}

	cfg = config.MCPServerConfig{
		Transport:  transport.String,
		URL:        url.String,
		Command:    command.String,
		AuthType:   authType.String,
		AuthKeyEnv: authKeyEnv.String,
		Timeout:    syncTimeout.String(),
		MaxRetries: int(maxRetries.Int64),
	}

	if args.Valid && args.String != "" {
		var parsedArgs []string
		if e := json.Unmarshal([]byte(args.String), &parsedArgs); e == nil {
			cfg.Args = parsedArgs
		}
	}

	if env.Valid && env.String != "" {
		var parsedEnv map[string]string
		if e := json.Unmarshal([]byte(env.String), &parsedEnv); e == nil {
			cfg.Env = parsedEnv
		}
	}

	return nameNS.String, cfg, syncTimeout, nil
}

// performMCPSyncHandshakeAndList runs syncServerByID's initialize +
// tools/list JSON-RPC calls against client.
func performMCPSyncHandshakeAndList(ctx context.Context, client mcp.TransportClient) (*mcp.ListToolsResult, error) {
	initID := json.RawMessage(`"1"`)
	initReq := &mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      &initID,
		Method:  mcp.MethodInitialize,
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"ilter","version":"1.0"}}`),
	}
	initResp, err := client.Call(ctx, initReq)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize: %w", err)
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("failed to initialize: %s (code %d)", initResp.Error.Message, initResp.Error.Code)
	}

	listID := json.RawMessage(`"2"`)
	listReq := &mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      &listID,
		Method:  mcp.MethodToolsList,
	}
	listResp, err := client.Call(ctx, listReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call tools: %w", err)
	}
	if listResp.Error != nil {
		return nil, fmt.Errorf("failed to call tools: %s (code %d)", listResp.Error.Message, listResp.Error.Code)
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &listResult); err != nil {
		return nil, fmt.Errorf("failed to parse tools list: %w", err)
	}
	return &listResult, nil
}

// writeSyncedTools replaces id's stored tools with tools inside a single
// transaction.
func (h *MCPHandler) writeSyncedTools(ctx context.Context, id string, tools []mcp.ToolDefinition) error {
	tx, err := h.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM mcp_tools WHERE server_id = ?", id); err != nil {
		return fmt.Errorf("failed to clear old tools: %w", err)
	}

	for _, tool := range tools {
		toolID := tool.Name + "-" + id
		if _, err := tx.ExecContext(
			ctx,
			`INSERT OR REPLACE INTO mcp_tools (id, server_id, name, description, schema)
			 VALUES (?, ?, ?, ?, ?)`,
			toolID, id, tool.Name, tool.Description, string(tool.InputSchema),
		); err != nil {
			return fmt.Errorf("failed to insert tool: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

func (h *MCPHandler) syncServerByID(ctx context.Context, id string) error {
	name, cfg, syncTimeout, err := resolveMCPServerConfigForSync(ctx, h.store, id)
	if err != nil {
		return err
	}

	serverInfo := &mcp.ServerInfo{
		ID:     id,
		Config: cfg,
	}

	client, err := mcp.NewTransportClient(serverInfo)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	if err = client.Start(syncCtx); err != nil {
		_ = client.Close()
		return fmt.Errorf("failed to connect to MCP server: %w", err)
	}
	defer func() { _ = client.Close() }()

	listResult, err := performMCPSyncHandshakeAndList(syncCtx, client)
	if err != nil {
		return err
	}

	if err := h.writeSyncedTools(syncCtx, id, listResult.Tools); err != nil {
		return err
	}

	if h.registry != nil {
		if err := h.registry.SyncTools(ctx, id, listResult.Tools); err != nil {
			mcpLog.Warn("failed to sync tools to registry after sync",
				"server_id", id, "error", err)
		}
	}

	mcpLog.Info("synced MCP server",
		"server_id", id, "server_name", name, "tool_count", len(listResult.Tools))
	return nil
}

func (h *MCPHandler) SyncServerTools(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		model.WriteJSONError(w, http.StatusBadRequest, "invalid_request_error", "Server ID is required")
		return
	}

	if err := h.syncServerByID(r.Context(), id); err != nil {
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found"):
			model.WriteJSONError(w, http.StatusNotFound, "not_found", errMsg)
		case strings.Contains(errMsg, "Failed to connect") || strings.Contains(errMsg, "Failed to initialize") || strings.Contains(errMsg, "Failed to call tools"):
			model.WriteJSONError(w, http.StatusBadGateway, "connection_error", errMsg)
		default:
			model.WriteJSONError(w, http.StatusInternalServerError, "internal_error", errMsg)
		}
		return
	}

	var name string
	_ = h.store.DB.QueryRowContext(r.Context(), "SELECT name FROM mcp_servers WHERE id = ?", id).Scan(&name)

	model.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("Synced MCP server %q", name),
	})
}
