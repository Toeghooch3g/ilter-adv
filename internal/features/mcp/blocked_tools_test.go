package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/mcp/protocol"
)

// blockedTestRegistry returns a Registry with two servers offering the same
// bare tool name so per-server and per-surface blocking can be asserted:
//   - "kagi":         search, get_info
//   - "anginxbrowser": search, browse
func blockedTestRegistry() *Registry {
	servers := map[string]*ServerInfo{
		"kagi": {
			ID:     "kagi",
			Config: config.MCPServerConfig{ID: "kagi", Name: "Kagi", Transport: "sse"},
			Tools: []ToolDefinition{
				{Name: "search", Description: "kagi search"},
				{Name: "get_info", Description: "kagi info"},
			},
		},
		"anginxbrowser": {
			ID:     "anginxbrowser",
			Config: config.MCPServerConfig{ID: "anginxbrowser", Name: "AnginxBrowser", Transport: "sse"},
			Tools: []ToolDefinition{
				{Name: "search", Description: "web search"},
				{Name: "browse", Description: "browse page"},
			},
		},
	}
	return &Registry{servers: servers}
}

// listToolNames extracts the bare names from a ListTools snapshot.
func listToolNames(all []ToolInfo) []string {
	names := make([]string, 0, len(all))
	for _, ti := range all {
		names = append(names, ti.Tool.Name)
	}
	return names
}

// TestListToolsBlocked covers the single tool-list source filter: an entry
// matching a server's prefixed exposed name blocks that one server's tool,
// a bare-name entry blocks nothing, and a nil closure disables filtering.
func TestListToolsBlocked(t *testing.T) {
	// nil fn → no filtering (existing behavior preserved).
	reg := blockedTestRegistry()
	if got := len(reg.ListTools()); got != 4 {
		t.Fatalf("expected 4 tools with nil blocked fn, got %d", got)
	}

	// Bare name no longer blocks anything (prefixed-only matching).
	reg.SetBlockedToolsFn(func() []string { return []string{"search"} })
	got := listToolNames(reg.ListTools())
	if len(got) != 4 || !contains("search", got) {
		t.Fatalf("bare 'search' must not hide any tool under prefixed-only matching, got %v", got)
	}

	// Prefixed entry blocks only the named server's tool.
	reg.SetBlockedToolsFn(func() []string { return []string{ExposedToolName("Kagi", "kagi", "search")} })
	got = listToolNames(reg.ListTools())
	if !contains("search", got) {
		// After kagi's copy is filtered, anginxbrowser's search is the only
		// one left — it must still be present.
		t.Fatalf("anginxbrowser's search must survive a kagi-scoped block, got %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 tools after kagi-scoped block, got %v", got)
	}

	// Empty list → no filtering.
	reg.SetBlockedToolsFn(func() []string { return nil })
	if got := len(reg.ListTools()); got != 4 {
		t.Fatalf("expected 4 tools with empty blocked list, got %d", got)
	}
}

// TestInjectorExcludesBlockedTool verifies the chat-request injection surface
// inherits the registry filter (a blocked tool never reaches the LLM).
func TestInjectorExcludesBlockedTool(t *testing.T) {
	reg := blockedTestRegistry()
	reg.SetBlockedToolsFn(func() []string { return []string{ExposedToolName("Kagi", "kagi", "search")} })
	auth := NewAuthorizer(nil, []config.MCPAccessRule{{Tools: []string{"*/*"}}}, "deny")
	inj := NewInjector(reg, auth, nil)

	out := inj.GetAuthorizedOpenAITools("", nil)
	names := oaToolNames(out)
	if contains("kagi-search", names) {
		t.Fatalf("blocked kagi-search must be invisible at injection, got %v", names)
	}
	if !contains("kagi-get_info", names) || !contains("anginxbrowser-search", names) || !contains("anginxbrowser-browse", names) {
		t.Fatalf("unblocked tools must stay injected, got %v", names)
	}
}

// TestGatewayToolsListExcludesBlocked verifies the native MCP gateway
// tools/list surface inherits the filter.
func TestGatewayToolsListExcludesBlocked(t *testing.T) {
	reg := blockedTestRegistry()
	reg.SetBlockedToolsFn(func() []string { return []string{ExposedToolName("AnginxBrowser", "anginxbrowser", "search")} })
	auth := NewAuthorizer(nil, []config.MCPAccessRule{{Tools: []string{"*/*"}}}, "deny")
	gw := NewGateway(reg, auth, nil, nil, &config.MCPConfig{Endpoint: "/mcp"}, nil)

	resp := gw.Dispatch(&JSONRPCRequest{
		JSONRPC: JSONRPCVersion,
		ID:      testID("1"),
		Method:  MethodToolsList,
	}, &RequestContext{})
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	var list ListToolsResult
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatal(err)
	}
	names := toolNames(list.Tools)
	if contains("anginxbrowser-search", names) {
		t.Fatalf("anginxbrowser-search must be invisible in gateway tools/list, got %v", names)
	}
	if !contains("kagi-search", names) {
		t.Fatalf("kagi's search must stay visible in gateway tools/list, got %v", names)
	}
}

// TestHubToolsListExcludesBlocked verifies the Hub tools/list surface
// inherits the filter.
func TestHubToolsListExcludesBlocked(t *testing.T) {
	reg := blockedTestRegistry()
	reg.SetBlockedToolsFn(func() []string { return []string{ExposedToolName("AnginxBrowser", "anginxbrowser", "browse")} })
	auth := NewAuthorizer(nil, []config.MCPAccessRule{{Tools: []string{"*/*"}}}, "deny")
	hub := NewHub(reg, auth, nil, nil, &config.MCPConfig{})
	sm := NewSessionManager()
	session := sm.Create("", "")
	defer sm.Delete(session.ID)
	session.ProtocolVersion = protocol.V20250326

	resp := hub.Dispatch(&JSONRPCRequest{
		JSONRPC: JSONRPCVersion,
		ID:      testID("1"),
		Method:  MethodToolsList,
	}, session)
	if resp.Error != nil {
		t.Fatalf("hub tools/list error: %+v", resp.Error)
	}
	var list ListToolsResult
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatal(err)
	}
	names := toolNames(list.Tools)
	// The hub exposes tools under their server-prefixed names.
	browseExposed := ExposedToolName("AnginxBrowser", "anginxbrowser", "browse")
	if contains(browseExposed, names) {
		t.Fatalf("%s must be invisible in hub tools/list, got %v", browseExposed, names)
	}
	if !contains(ExposedToolName("Kagi", "kagi", "search"), names) || !contains(ExposedToolName("Kagi", "kagi", "get_info"), names) {
		t.Fatalf("unblocked tools must stay visible in hub tools/list, got %v", names)
	}
}

// TestExecutorRejectsBlockedTool verifies the defense-in-depth execution
// gate: even a client that guesses a blocked tool's name is rejected.
func TestExecutorRejectsBlockedTool(t *testing.T) {
	reg := blockedTestRegistry()
	reg.SetBlockedToolsFn(func() []string {
		return []string{
			ExposedToolName("AnginxBrowser", "anginxbrowser", "browse"),
			ExposedToolName("Kagi", "kagi", "search"),
		}
	})
	clients := NewClientManager(reg)
	ex := NewExecutor(reg, clients, nil, nil, nil)
	ex.SetBlockedToolsFn(func() []string {
		return []string{
			ExposedToolName("AnginxBrowser", "anginxbrowser", "browse"),
			ExposedToolName("Kagi", "kagi", "search"),
		}
	})

	ctx := context.Background()

	// Prefixed block.
	res := ex.ExecuteTool(ctx, &ExecuteToolParams{ToolName: ExposedToolName("AnginxBrowser", "anginxbrowser", "browse"), Arguments: json.RawMessage(`{}`)})
	if !res.IsError || len(res.Content) == 0 || res.Content[0].Text != "tool blocked by gateway policy" {
		t.Fatalf("anginxbrowser-browse must be rejected with blocked message, got %+v", res)
	}

	// Prefixed block on the other server.
	res = ex.ExecuteTool(ctx, &ExecuteToolParams{ToolName: ExposedToolName("Kagi", "kagi", "search"), Arguments: json.RawMessage(`{}`)})
	if !res.IsError || len(res.Content) == 0 || res.Content[0].Text != "tool blocked by gateway policy" {
		t.Fatalf("kagi-search must be rejected with blocked message, got %+v", res)
	}

	// Unblocked tool is not rejected with the blocked message (it proceeds;
	// the "blocked" message must not appear).
	res = ex.ExecuteTool(ctx, &ExecuteToolParams{ToolName: ExposedToolName("Kagi", "kagi", "get_info"), Arguments: json.RawMessage(`{}`)})
	for _, c := range res.Content {
		if c.Text == "tool blocked by gateway policy" {
			t.Fatalf("kagi-get_info must not be blocked, got %+v", res)
		}
	}
}
