-- +goose Up

-- Per-key opt-in flag for server-side MCP/OpenAPI tool injection. Defaults
-- to 0 for every key (including existing rows) so injection is off unless a
-- key explicitly opts in.
ALTER TABLE api_keys ADD COLUMN mcp_injection_enabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE api_keys DROP COLUMN mcp_injection_enabled;
