-- MCP server registry: servers config and their discovered tools.

-- name: ListMCPServers :many
SELECT id, name, description, transport, url, command, args, env,
       handler, enabled, timeout_ms, max_retries, auth_type, auth_key_env,
       protocol_version
FROM mcp_servers;

-- name: ListMCPTools :many
SELECT name, description, schema
FROM mcp_tools
WHERE server_id = ?
ORDER BY name;

-- name: DeleteMCPToolsByServer :exec
DELETE FROM mcp_tools WHERE server_id = ?;

-- name: UpsertMCPTool :exec
INSERT OR REPLACE INTO mcp_tools (id, server_id, name, description, schema)
VALUES (?, ?, ?, ?, ?);

-- name: ListMCPToolToggles :many
SELECT server_id, tool_name, enabled, cost_per_1k
FROM mcp_tool_toggles;

-- name: UpsertMCPToolToggle :exec
INSERT INTO mcp_tool_toggles (server_id, tool_name, enabled, cost_per_1k)
VALUES (?, ?, ?, ?)
ON CONFLICT(server_id, tool_name) DO UPDATE SET
  enabled = excluded.enabled,
  cost_per_1k = excluded.cost_per_1k;

-- name: DeleteMCPToolToggle :exec
DELETE FROM mcp_tool_toggles WHERE server_id = ? AND tool_name = ?;

-- name: GetMCPToolToggle :one
SELECT enabled, cost_per_1k
FROM mcp_tool_toggles
WHERE server_id = ? AND tool_name = ?;
