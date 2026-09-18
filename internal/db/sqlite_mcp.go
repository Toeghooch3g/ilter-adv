package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/ilter-ai/ilter/internal/db/sqlc"
)

// MCPServerRow is a mcp_servers row, for loading supplemental (admin-created)
// servers into the runtime registry.
type MCPServerRow struct {
	ID          string
	Name        string
	Description string
	Transport   string
	URL         string
	Command     string
	Args        string // JSON-encoded []string
	Env         string // JSON-encoded map[string]string
	Handler     string
	Enabled     bool
	TimeoutMs   int
	MaxRetries  int
	AuthType    string
	AuthKeyEnv  string
	// ProtocolVersion is the outbound MCP protocol version negotiation
	// pin for this server: "auto" (default) negotiates newest-first with
	// fallback, or an exact version string forces that version.
	ProtocolVersion string
}

// ListMCPServers returns every row in mcp_servers, enabled or not; callers
// filter by Enabled themselves (registry only wants enabled servers not
// already present from static config).
func (s *SQLiteStore) ListMCPServers() ([]MCPServerRow, error) {
	rows, err := s.queries.ListMCPServers(context.Background())
	if err != nil {
		return nil, err
	}
	result := make([]MCPServerRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, MCPServerRow{
			ID:              r.ID,
			Name:            r.Name,
			Description:     strDeref(r.Description),
			Transport:       r.Transport,
			URL:             strDeref(r.Url),
			Command:         strDeref(r.Command),
			Args:            strDeref(r.Args),
			Env:             strDeref(r.Env),
			Handler:         strDeref(r.Handler),
			Enabled:         r.Enabled != 0,
			TimeoutMs:       int(int64Deref(r.TimeoutMs)),
			MaxRetries:      int(r.MaxRetries),
			AuthType:        strDeref(r.AuthType),
			AuthKeyEnv:      strDeref(r.AuthKeyEnv),
			ProtocolVersion: r.ProtocolVersion,
		})
	}
	return result, nil
}

// MCPToolRow is a mcp_tools row for a single server.
type MCPToolRow struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ListMCPTools returns the tools discovered for serverID, ordered by name.
func (s *SQLiteStore) ListMCPTools(serverID string) ([]MCPToolRow, error) {
	rows, err := s.queries.ListMCPTools(context.Background(), serverID)
	if err != nil {
		return nil, err
	}
	result := make([]MCPToolRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, MCPToolRow{
			Name:        r.Name,
			Description: strDeref(r.Description),
			Schema:      r.Schema,
		})
	}
	return result, nil
}

// MCPToolInput is a single discovered tool to persist via SaveMCPTools.
type MCPToolInput struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// SaveMCPTools replaces all persisted tools for serverID with tools, in a
// single transaction (delete-then-bulk-insert, matching a tools/list
// discovery result exactly).
func (s *SQLiteStore) SaveMCPTools(ctx context.Context, serverID string, tools []MCPToolInput) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	qtx := s.queries.WithTx(tx)

	if err := qtx.DeleteMCPToolsByServer(ctx, serverID); err != nil {
		return err
	}

	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = json.RawMessage("")
		}
		if err := qtx.UpsertMCPTool(ctx, sqlc.UpsertMCPToolParams{
			ID:          serverID + ":" + t.Name,
			ServerID:    serverID,
			Name:        t.Name,
			Description: new(t.Description),
			Schema:      schema,
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// MCPToolToggleRow is a per-tool server-level state row (enable/disable and
// per-call pricing).
type MCPToolToggleRow struct {
	ServerID  string
	ToolName  string
	Enabled   bool
	CostPer1k *float64 // USD per 1000 requests; nil = not priced
}

// ListMCPToolToggles returns every per-tool toggle row.
func (s *SQLiteStore) ListMCPToolToggles() ([]MCPToolToggleRow, error) {
	rows, err := s.queries.ListMCPToolToggles(context.Background())
	if err != nil {
		return nil, err
	}
	result := make([]MCPToolToggleRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, MCPToolToggleRow{
			ServerID:  r.ServerID,
			ToolName:  r.ToolName,
			Enabled:   r.Enabled != 0,
			CostPer1k: r.CostPer1k,
		})
	}
	return result, nil
}

// UpsertMCPToolToggle inserts or updates one tool's toggle state.
func (s *SQLiteStore) UpsertMCPToolToggle(ctx context.Context, row MCPToolToggleRow) error {
	enabled := 0
	if row.Enabled {
		enabled = 1
	}
	return s.queries.UpsertMCPToolToggle(ctx, sqlc.UpsertMCPToolToggleParams{
		ServerID:  row.ServerID,
		ToolName:  row.ToolName,
		Enabled:   int64(enabled),
		CostPer1k: row.CostPer1k,
	})
}

// DeleteMCPToolToggle removes one tool's toggle row (resets to defaults:
// enabled + unpriced).
func (s *SQLiteStore) DeleteMCPToolToggle(ctx context.Context, serverID, toolName string) error {
	return s.queries.DeleteMCPToolToggle(ctx, sqlc.DeleteMCPToolToggleParams{
		ServerID: serverID,
		ToolName: toolName,
	})
}

// GetMCPToolToggle reads one tool's toggle state; returns (defaults, false)
// when no row exists.
func (s *SQLiteStore) GetMCPToolToggle(ctx context.Context, serverID, toolName string) (enabled bool, costPer1k *float64, ok bool, err error) {
	row, err := s.queries.GetMCPToolToggle(ctx, sqlc.GetMCPToolToggleParams{
		ServerID: serverID,
		ToolName: toolName,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil, false, nil
		}
		return false, nil, false, err
	}
	return row.Enabled != 0, row.CostPer1k, true, nil
}
