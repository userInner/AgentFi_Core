-- name: CreateMCPServer :one
INSERT INTO mcp_servers (name, url, type, tools, is_public, owner_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMCPServer :one
SELECT * FROM mcp_servers WHERE id = $1;

-- name: ListPublicMCPServers :many
SELECT * FROM mcp_servers WHERE is_public = true AND status = 'active';

-- name: ListMCPServersByOwner :many
SELECT * FROM mcp_servers WHERE owner_id = $1 AND status = 'active';

-- name: UpdateMCPServerStatus :exec
UPDATE mcp_servers SET status = $2 WHERE id = $1;

-- name: BindAgentMCP :exec
INSERT INTO agent_mcp_bindings (agent_id, server_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: UnbindAgentMCP :exec
DELETE FROM agent_mcp_bindings WHERE agent_id = $1 AND server_id = $2;

-- name: ListAgentMCPServers :many
SELECT ms.* FROM mcp_servers ms
JOIN agent_mcp_bindings amb ON ms.id = amb.server_id
WHERE amb.agent_id = $1;
