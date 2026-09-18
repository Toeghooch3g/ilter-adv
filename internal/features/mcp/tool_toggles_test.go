package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/features/mcp/toolpricing"
)

// disabledTestRegistry returns a registry with one server ("srv1") offering
// two tools, with no pre-configured disabled/cost state.
func disabledTestRegistry() *Registry {
	return &Registry{
		servers: map[string]*ServerInfo{
			"srv1": {
				ID:     "srv1",
				Config: config.MCPServerConfig{ID: "srv1", Name: "Srv1", Transport: "sse"},
				Tools: []ToolDefinition{
					{Name: "visible-tool", Description: "stays"},
					{Name: "hidden-tool", Description: "admin-disabled"},
				},
			},
		},
		disabled:  map[string]map[string]bool{},
		toolCosts: map[string]map[string]float64{},
	}
}

// TestListToolsHidesDisabled verifies the ListTools filter hides an
// admin-disabled tool while leaving the rest visible.
func TestListToolsHidesDisabled(t *testing.T) {
	reg := disabledTestRegistry()

	// No state → both visible.
	if got := len(reg.ListTools()); got != 2 {
		t.Fatalf("expected 2 tools with no disabled state, got %d", got)
	}

	reg.disabled["srv1"] = map[string]bool{"hidden-tool": true}
	names := listToolNames(reg.ListTools())
	if len(names) != 1 || names[0] != "visible-tool" {
		t.Fatalf("expected only visible-tool after disabling hidden-tool, got %v", names)
	}
}

// TestToolCostRoundTrip verifies per-tool cost is stored per-call (÷1000)
// and returned via ToolCost.
func TestToolCostRoundTrip(t *testing.T) {
	reg := disabledTestRegistry()
	cost := 0.5 // $0.5 per 1k requests
	reg.toolCosts["srv1"] = map[string]float64{"visible-tool": cost / 1000}

	if got := reg.ToolCost("srv1", "visible-tool"); got != 0.0005 {
		t.Fatalf("ToolCost = %v, want 0.0005", got)
	}
	if got := reg.ToolCost("srv1", "hidden-tool"); got != 0 {
		t.Fatalf("ToolCost for unpriced tool = %v, want 0", got)
	}
	if got := reg.ToolCost("other", "visible-tool"); got != 0 {
		t.Fatalf("ToolCost for unknown server = %v, want 0", got)
	}
}

// TestExecutorRejectsDisabledTool verifies the defense-in-depth execution
// gate rejects a direct call to an admin-disabled tool.
func TestExecutorRejectsDisabledTool(t *testing.T) {
	reg := disabledTestRegistry()
	reg.disabled["srv1"] = map[string]bool{"hidden-tool": true}

	mock := &mockTransportForExecutor{connected: true}
	clients := NewClientManager(nil)
	insertMockClient(clients, mock)
	ex := NewExecutor(reg, clients, nil, nil, nil)

	ctx := context.Background()
	res := ex.ExecuteTool(ctx, &ExecuteToolParams{ToolName: "srv1-hidden-tool", Arguments: json.RawMessage(`{}`)})
	if !res.IsError || len(res.Content) == 0 || res.Content[0].Text != "tool is disabled" {
		t.Fatalf("srv1-hidden-tool must be rejected with 'tool is disabled', got %+v", res)
	}

	// Visible tool proceeds (no disabled message).
	res = ex.ExecuteTool(ctx, &ExecuteToolParams{ToolName: "srv1-visible-tool", Arguments: json.RawMessage(`{}`)})
	for _, c := range res.Content {
		if c.Text == "tool is disabled" {
			t.Fatalf("srv1-visible-tool must not be disabled, got %+v", res)
		}
	}
}

// TestExecutorPerToolCostPrecedence verifies a per-tool registry cost
// overrides a conflicting tool_pricing rule (computeCost is the hook; it
// returns 0.002 from the registry rather than 0.012 from the rule).
func TestExecutorPerToolCostPrecedence(t *testing.T) {
	reg := disabledTestRegistry()
	// Per-tool cost: $2 per 1k requests → 0.002 per call.
	reg.toolCosts["srv1"] = map[string]float64{"visible-tool": 0.002}

	clients := NewClientManager(nil)
	ex := NewExecutor(reg, clients, nil, nil, nil)
	// Conflicting pricing rule would bill 0.012 — the per-tool cost must win.
	ex.SetPricingResolver(func() *toolpricing.Resolver {
		return toolpricing.NewResolver([]toolpricing.Rule{
			{Server: "srv1", Tool: "visible-tool", Unit: toolpricing.UnitCall, CostPerUnit: 0.012},
		})
	})

	if got := ex.computeCost("srv1", "visible-tool", json.RawMessage(`{}`)); got != 0.002 {
		t.Errorf("computeCost = %v, want 0.002 (per-tool cost overrides rule 0.012)", got)
	}
	// Un-priced tool falls through to the rule resolver.
	if got := ex.computeCost("srv1", "hidden-tool", json.RawMessage(`{}`)); got != 0 {
		t.Errorf("computeCost for hidden-tool = %v, want 0 (no rule matches)", got)
	}
}

// TestToolTogglePersistsAcrossSync verifies a disabled+cost tool toggle
// survives a SyncTools delete-then-reinsert cycle and a fresh registry load.
func TestToolTogglePersistsAcrossSync(t *testing.T) {
	store := setupRegistryTestStore(t)
	servers := []config.MCPServerConfig{
		{ID: "srv1", Name: "Server 1", Enabled: true, Transport: "sse"},
	}

	reg, err := NewRegistryFromCache(servers, store)
	if err != nil {
		t.Fatalf("NewRegistryFromCache: %v", err)
	}

	tools := []ToolDefinition{
		{Name: "tool-a", Description: "a"},
		{Name: "tool-b", Description: "b"},
	}
	if syncErr := reg.SyncTools(context.Background(), "srv1", tools); syncErr != nil {
		t.Fatalf("SyncTools: %v", syncErr)
	}

	// Admin disables tool-a and prices tool-b at $0.5/1k.
	reg.SetToolEnabled(context.Background(), "srv1", "tool-a", false)
	cost := 0.5
	reg.SetToolCost(context.Background(), "srv1", "tool-b", &cost)

	if !reg.IsToolDisabled("srv1", "tool-a") {
		t.Fatal("tool-a should be disabled after SetToolEnabled")
	}
	if reg.ToolCost("srv1", "tool-b") != 0.0005 {
		t.Fatalf("tool-b cost = %v, want 0.0005", reg.ToolCost("srv1", "tool-b"))
	}

	// SyncTools deletes + reinserts mcp_tools — the toggles table is
	// untouched, so state must survive.
	if syncErr := reg.SyncTools(context.Background(), "srv1", tools); syncErr != nil {
		t.Fatalf("SyncTools (2): %v", syncErr)
	}

	reg2, err := NewRegistryFromCache(servers, store)
	if err != nil {
		t.Fatalf("NewRegistryFromCache (reload): %v", err)
	}
	if !reg2.IsToolDisabled("srv1", "tool-a") {
		t.Fatal("tool-a disabled state must survive SyncTools and reload")
	}
	if got := reg2.ToolCost("srv1", "tool-b"); got != 0.0005 {
		t.Fatalf("tool-b cost after reload = %v, want 0.0005", got)
	}

	// Re-enable + clear cost → defaults restored.
	reg2.SetToolEnabled(context.Background(), "srv1", "tool-a", true)
	reg2.SetToolCost(context.Background(), "srv1", "tool-b", nil)
	reg3, err := NewRegistryFromCache(servers, store)
	if err != nil {
		t.Fatalf("NewRegistryFromCache (reload 3): %v", err)
	}
	if reg3.IsToolDisabled("srv1", "tool-a") {
		t.Fatal("tool-a should be enabled after re-enable")
	}
	if got := reg3.ToolCost("srv1", "tool-b"); got != 0 {
		t.Fatalf("tool-b cost after clear = %v, want 0", got)
	}
}
