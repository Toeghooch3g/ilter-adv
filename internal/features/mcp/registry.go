package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ilter-ai/ilter/internal/config"
	"github.com/ilter-ai/ilter/internal/db"
)

// ToolInfo associates a tool definition with the server that provides it.
type ToolInfo struct {
	Tool       ToolDefinition
	ServerID   string
	ServerName string
}

// ServerInfo holds runtime state for a single MCP server registration.
type ServerInfo struct {
	ID       string
	Config   config.MCPServerConfig
	Tools    []ToolDefinition
	Healthy  bool
	LastSync time.Time
}

// Registry manages discovered and configured MCP servers and their tools.
type Registry struct {
	mu      sync.RWMutex
	servers map[string]*ServerInfo // server ID → info

	// disabled maps serverID → toolName → true when that server's tool is
	// disabled by an admin (dashboard toggle). Consulted in ListTools and
	// Executor, loaded from mcp_tool_toggles at construction and updated by
	// SetToolEnabled. Missing entries mean enabled.
	disabled map[string]map[string]bool
	// toolCosts maps serverID → toolName → USD cost per call (= cost_per_1k/1000).
	// Missing entries mean unpriced; this overrides the tool_pricing runtime
	// rules for the exact (server, tool).
	toolCosts map[string]map[string]float64

	store *db.SQLiteStore

	// blockedToolsFn returns the current set of server-prefixed exposed tool
	// names hidden from every surface (see SetBlockedToolsFn). Nil disables
	// filtering.
	blockedToolsFn func() []string

	changeMu sync.Mutex
	onChange []func()
}

// SetBlockedToolsFn installs the closure the registry consults to hide
// blocked tools. The closure is invoked per ListTools call and reads the
// live config snapshot, so updates take effect without restart. Nil (or a
// closure returning an empty list) disables filtering.
func (r *Registry) SetBlockedToolsFn(fn func() []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockedToolsFn = fn
}

// blockedSet returns the blocked-tool lookup set for the current snapshot,
// or nil when no filtering is active.
func (r *Registry) blockedSet() map[string]bool {
	if r.blockedToolsFn == nil {
		return nil
	}
	blocked := r.blockedToolsFn()
	if len(blocked) == 0 {
		return nil
	}
	set := make(map[string]bool, len(blocked))
	for _, b := range blocked {
		set[b] = true
	}
	return set
}

// OnToolsChanged registers fn to be called whenever this registry's tool
// set changes (SyncTools, RegisterServer, UnregisterServer) — the real
// event source behind the 2026-07-28 `subscriptions/listen` toolsListChanged
// notification (see SubscriptionBroker), so a genuine downstream change
// drives the notification rather than a synthetic trigger.
func (r *Registry) OnToolsChanged(fn func()) {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	r.onChange = append(r.onChange, fn)
}

func (r *Registry) fireToolsChanged() {
	r.changeMu.Lock()
	fns := make([]func(), len(r.onChange))
	copy(fns, r.onChange)
	r.changeMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// NewRegistryFromCache creates a Registry populated from the given server
// configs (sourced from ConfigCache) and loads supplemental servers and tools
// from the database.
func NewRegistryFromCache(servers []config.MCPServerConfig, store *db.SQLiteStore) (*Registry, error) {
	r := &Registry{
		servers:   make(map[string]*ServerInfo, len(servers)+4),
		disabled:  make(map[string]map[string]bool),
		toolCosts: make(map[string]map[string]float64),
		store:     store,
	}

	for _, sc := range servers {
		if !sc.Enabled {
			continue
		}
		r.servers[sc.ID] = &ServerInfo{
			ID:     sc.ID,
			Config: sc,
		}
	}

	// Load supplemental servers from the mcp_servers table (admin CRUD).
	if err := r.loadServersFromDB(); err != nil {
		mcpLog.Warn("failed to load servers from database", "error", err)
	}

	for id := range r.servers {
		if err := r.loadToolsFromDB(id); err != nil {
			mcpLog.Warn("failed to load tools for server", "server_id", id, "error", err)
		}
	}

	r.loadToolTogglesFromDB()

	r.mu.RLock()
	serverCount := len(r.servers)
	toolCount := 0
	for _, s := range r.servers {
		toolCount += len(s.Tools)
	}
	r.mu.RUnlock()
	slog.Info(
		"registry initialized from cache",
		"servers", serverCount,
		"tools", toolCount,
	)

	return r, nil
}

// InitFromCache reloads the registry from a ConfigCache servers list.
// It clears existing servers, loads the new list, then merges DB records.
// Safe for concurrent use (acquires write lock).
func (r *Registry) InitFromCache(servers []config.MCPServerConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.servers = make(map[string]*ServerInfo, len(servers)+4)
	r.disabled = make(map[string]map[string]bool)
	r.toolCosts = make(map[string]map[string]float64)
	for _, sc := range servers {
		if !sc.Enabled {
			continue
		}
		r.servers[sc.ID] = &ServerInfo{
			ID:     sc.ID,
			Config: sc,
		}
	}

	if err := r.loadServersFromDB(); err != nil {
		mcpLog.Warn("failed to load servers from database on refresh", "error", err)
	}

	for id := range r.servers {
		if err := r.loadToolsFromDB(id); err != nil {
			mcpLog.Warn("failed to load tools for server on refresh", "server_id", id, "error", err)
		}
	}

	r.loadToolTogglesFromDB()

	serverCount := len(r.servers)
	toolCount := 0
	for _, s := range r.servers {
		toolCount += len(s.Tools)
	}
	slog.Info(
		"MCP registry reloaded from cache",
		"servers", serverCount,
		"tools", toolCount,
	)
	return nil
}

// loadToolTogglesFromDB loads per-tool disable/cost state from the
// mcp_tool_toggles table into the in-memory maps. Called at construction and
// on registry refresh; must hold r.mu.
func (r *Registry) loadToolTogglesFromDB() {
	if r.store == nil {
		return
	}
	rows, err := r.store.ListMCPToolToggles()
	if err != nil {
		mcpLog.Warn("failed to load MCP tool toggles from DB", "error", err)
		return
	}
	for _, row := range rows {
		if !row.Enabled {
			if r.disabled[row.ServerID] == nil {
				r.disabled[row.ServerID] = make(map[string]bool)
			}
			r.disabled[row.ServerID][row.ToolName] = true
		}
		if row.CostPer1k != nil {
			if r.toolCosts[row.ServerID] == nil {
				r.toolCosts[row.ServerID] = make(map[string]float64)
			}
			r.toolCosts[row.ServerID][row.ToolName] = *row.CostPer1k / 1000
		}
	}
}

// SetToolEnabled enables or disables a server's tool, persisting to the
// toggles table and updating the in-memory disabled map. A missing DB row
// with enabled=true is a no-op for persistence (defaults are already
// enabled). Fires onToolsChanged only when the state actually flips.
func (r *Registry) SetToolEnabled(ctx context.Context, serverID, toolName string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	wasDisabled := r.disabled[serverID] != nil && r.disabled[serverID][toolName]
	if !wasDisabled && enabled {
		// No change: already enabled.
		return
	}

	// Persist: read current cost so we preserve it.
	if r.store != nil {
		_, costPer1k, _, _ := r.store.GetMCPToolToggle(ctx, serverID, toolName)
		if err := r.store.UpsertMCPToolToggle(ctx, db.MCPToolToggleRow{
			ServerID:  serverID,
			ToolName:  toolName,
			Enabled:   enabled,
			CostPer1k: costPer1k,
		}); err != nil {
			mcpLog.Warn("failed to persist tool toggle", "server_id", serverID, "tool", toolName, "error", err)
		}
	}

	if enabled {
		if r.disabled[serverID] != nil {
			delete(r.disabled[serverID], toolName)
			if len(r.disabled[serverID]) == 0 {
				delete(r.disabled, serverID)
			}
		}
	} else {
		if r.disabled[serverID] == nil {
			r.disabled[serverID] = make(map[string]bool)
		}
		r.disabled[serverID][toolName] = true
	}

	r.fireToolsChanged()
}

// SetToolCost sets (or clears, when costPer1k is nil) the per-1k-request
// cost of a server's tool, persisting to the toggles table and updating the
// in-memory cost map.
func (r *Registry) SetToolCost(ctx context.Context, serverID, toolName string, costPer1k *float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.store != nil {
		// Read current enabled state so it's preserved across the upsert.
		enabled, _, _, _ := r.store.GetMCPToolToggle(ctx, serverID, toolName)

		switch {
		case costPer1k == nil && enabled:
			// Clearing cost on an enabled tool: the row carries no information
			// anymore, so delete it entirely.
			_ = r.store.DeleteMCPToolToggle(ctx, serverID, toolName)
		case costPer1k == nil && !enabled:
			// Tool is disabled; keep that state but drop the cost.
			_ = r.store.UpsertMCPToolToggle(ctx, db.MCPToolToggleRow{
				ServerID:  serverID,
				ToolName:  toolName,
				Enabled:   false,
				CostPer1k: nil,
			})
		default:
			_ = r.store.UpsertMCPToolToggle(ctx, db.MCPToolToggleRow{
				ServerID:  serverID,
				ToolName:  toolName,
				Enabled:   enabled,
				CostPer1k: costPer1k,
			})
		}
	}

	if costPer1k != nil {
		if r.toolCosts[serverID] == nil {
			r.toolCosts[serverID] = make(map[string]float64)
		}
		r.toolCosts[serverID][toolName] = *costPer1k / 1000
	} else if r.toolCosts[serverID] != nil {
		delete(r.toolCosts[serverID], toolName)
		if len(r.toolCosts[serverID]) == 0 {
			delete(r.toolCosts, serverID)
		}
	}
}

// ToolCost returns the per-call USD cost configured for serverID/toolName
// (0 when unset). This is the executor's pricing hook; the per-tool cost
// overrides any matching tool_pricing runtime rule.
func (r *Registry) ToolCost(serverID, toolName string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if m := r.toolCosts[serverID]; m != nil {
		return m[toolName]
	}
	return 0
}

// IsToolDisabled reports whether serverID/toolName is admin-disabled. Cheap
// per-call lookup (no allocation), used by the executor's defense-in-depth
// gate behind the ListTools filter.
func (r *Registry) IsToolDisabled(serverID, toolName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.disabled[serverID] != nil && r.disabled[serverID][toolName]
}

// ListServers returns all registered servers (read-only snapshot), sorted by
// ID for deterministic ordering across calls (Go map iteration is randomized).
func (r *Registry) ListServers() []*ServerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ServerInfo, 0, len(r.servers))
	for _, s := range r.servers {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListTools returns all tools across all servers, sorted by (ServerID, tool
// name) for deterministic ordering across calls — clients rely on stable
// tools/list ordering for LLM prompt-cache hit rates. Tools matched by the
// configured blocked-tools list are hidden here, which makes them invisible
// to every client surface at once (chat injection, native gateway
// tools/list, hub tools/list) since all three read from this single source.
func (r *Registry) ListTools() []ToolInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	blocked := r.blockedSet()
	var out []ToolInfo
	for id, s := range r.servers {
		for _, t := range s.Tools {
			if blocked != nil && blocked[ExposedToolName(s.Config.Name, id, t.Name)] {
				continue
			}
			if r.disabled[id] != nil && r.disabled[id][t.Name] {
				continue
			}
			out = append(out, ToolInfo{Tool: t, ServerID: id, ServerName: s.Config.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ServerID != out[j].ServerID {
			return out[i].ServerID < out[j].ServerID
		}
		return out[i].Tool.Name < out[j].Tool.Name
	})
	return out
}

// ResolveTool looks up a tool by its server-prefixed exposed name
// (ExposedToolName) and returns the tool info along with its server.
// Only the prefixed form is accepted; bare tool names and the legacy
// "server__tool" form are rejected. Servers are iterated in sorted-ID
// order so a name collision between servers resolves deterministically
// (lowest server ID wins).
func (r *Registry) ResolveTool(name string) (*ToolDefinition, *ServerInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.servers))
	for id := range r.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		s := r.servers[id]
		for _, t := range s.Tools {
			if ExposedToolName(s.Config.Name, id, t.Name) == name {
				return &t, s, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("tool %q not found; use the server-prefixed name shown by tools/list", name)
}

// RegisterServer adds or updates a server in the in-memory registry.
func (r *Registry) RegisterServer(id string, cfg config.MCPServerConfig, tools []ToolDefinition) {
	if err := ValidateMCPServerID(id); err != nil {
		mcpLog.Error("server registration rejected", "server_id", id, "error", err)
		return
	}
	r.mu.Lock()
	r.servers[id] = &ServerInfo{
		ID:      id,
		Config:  cfg,
		Tools:   tools,
		Healthy: false,
	}
	r.mu.Unlock()
	mcpLog.Debug("server registered in registry", "server_id", id)
	r.fireToolsChanged()
}

// UnregisterServer removes a server from the in-memory registry.
func (r *Registry) UnregisterServer(id string) {
	r.mu.Lock()
	delete(r.servers, id)
	r.mu.Unlock()
	mcpLog.Debug("server unregistered from registry", "server_id", id)
	r.fireToolsChanged()
}

// SyncTools updates the in-memory tool list for a server and persists to DB.
// This is called after a successful tools/list discovery.
func (r *Registry) SyncTools(ctx context.Context, serverID string, tools []ToolDefinition) error {
	r.mu.Lock()
	s, ok := r.servers[serverID]
	if ok {
		s.Tools = tools
		s.LastSync = time.Now()
	}
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("server %q not found in registry", serverID)
	}

	// Persist to DB.
	if r.store != nil {
		if err := r.saveToolsToDB(ctx, serverID, tools); err != nil {
			return fmt.Errorf("persist tools: %w", err)
		}
	}

	r.fireToolsChanged()
	return nil
}

func (r *Registry) saveToolsToDB(ctx context.Context, serverID string, tools []ToolDefinition) error {
	inputs := make([]db.MCPToolInput, 0, len(tools))
	for _, tool := range tools {
		inputs = append(inputs, db.MCPToolInput{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return r.store.SaveMCPTools(ctx, serverID, inputs)
}

func (r *Registry) loadServersFromDB() error {
	if r.store == nil {
		return nil
	}

	rows, err := r.store.ListMCPServers()
	if err != nil {
		return fmt.Errorf("query servers: %w", err)
	}

	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		r.registerServerFromDBRow(row)
	}

	return nil
}

// registerServerFromDBRow converts a DB server row into a ServerInfo and
// adds it to the registry, unless a server with the same ID is already
// registered (config takes precedence over DB-persisted rows).
func (r *Registry) registerServerFromDBRow(row db.MCPServerRow) {
	if _, exists := r.servers[row.ID]; exists {
		return
	}
	r.servers[row.ID] = &ServerInfo{
		ID:     row.ID,
		Config: buildServerConfigFromDBRow(row),
	}
}

// buildServerConfigFromDBRow builds a config.MCPServerConfig from a DB
// server row, unmarshaling its JSON-encoded args/env.
func buildServerConfigFromDBRow(row db.MCPServerRow) config.MCPServerConfig {
	timeout := fmt.Sprintf("%dms", row.TimeoutMs)
	if row.TimeoutMs <= 0 {
		timeout = "30s"
	}

	sc := config.MCPServerConfig{
		ID:              row.ID,
		Name:            row.Name,
		Description:     row.Description,
		Transport:       row.Transport,
		URL:             row.URL,
		Command:         row.Command,
		Handler:         row.Handler,
		Enabled:         true,
		Timeout:         timeout,
		MaxRetries:      row.MaxRetries,
		AuthType:        row.AuthType,
		AuthKeyEnv:      row.AuthKeyEnv,
		ProtocolVersion: row.ProtocolVersion,
	}

	if row.Args != "" {
		if err := json.Unmarshal([]byte(row.Args), &sc.Args); err != nil {
			mcpLog.Warn("failed to unmarshal args for server", "server_id", row.ID, "error", err)
		}
	}
	if row.Env != "" {
		if err := json.Unmarshal([]byte(row.Env), &sc.Env); err != nil {
			mcpLog.Warn("failed to unmarshal env for server", "server_id", row.ID, "error", err)
		}
	}

	return sc
}

func (r *Registry) loadToolsFromDB(serverID string) error {
	if r.store == nil {
		return nil
	}

	rows, err := r.store.ListMCPTools(serverID)
	if err != nil {
		return fmt.Errorf("query tools for server %s: %w", serverID, err)
	}

	var tools []ToolDefinition
	for _, row := range rows {
		t := ToolDefinition{
			Name:        row.Name,
			Description: row.Description,
		}
		if len(row.Schema) > 0 {
			t.InputSchema = row.Schema
		}
		tools = append(tools, t)
	}

	mcpLog.Debug(
		"loaded tools from DB",
		"server_id", serverID,
		"count", len(tools),
	)

	server, ok := r.servers[serverID]
	if ok {
		server.Tools = tools
	}
	return nil
}
