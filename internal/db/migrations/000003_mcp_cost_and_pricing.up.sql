-- +goose Up

-- USD cost of a single MCP tool call for budget/usage accounting. Priced
-- tools (runtime_config section "tool_pricing") record their cost here so
-- the MCP audit view can show spend per call and the stats handler can sum
-- total tool spend. Unpriced calls keep the default 0.
ALTER TABLE mcp_audit_log ADD COLUMN cost REAL DEFAULT 0;
