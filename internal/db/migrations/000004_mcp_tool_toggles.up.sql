-- +goose Up

-- Per-tool server-level state: enable/disable and per-call pricing.
-- Separate from mcp_tools so Test/Sync DELETE+INSERT cycles never wipe it.
CREATE TABLE IF NOT EXISTS mcp_tool_toggles (
    server_id TEXT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    enabled   INTEGER NOT NULL DEFAULT 1,
    -- USD cost per 1000 requests (same semantics as toolpricing.Rule
    -- CostPerUnit: $12/1k = 0.012 per call); NULL = not priced.
    cost_per_1k REAL,
    PRIMARY KEY (server_id, tool_name)
);

-- +goose Down

DROP TABLE IF EXISTS mcp_tool_toggles;
