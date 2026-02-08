-- name: CreateAgent :one
INSERT INTO agents (user_id, name, strategy_prompt, pair, interval_sec, status, risk_params, llm_params)
VALUES ($1, $2, $3, $4, $5, 'created', $6, $7)
RETURNING *;

-- name: GetAgent :one
SELECT * FROM agents WHERE id = $1;

-- name: ListAgentsByUser :many
SELECT * FROM agents
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: ListAgentsByUserCursor :many
SELECT * FROM agents
WHERE user_id = $1 AND created_at < $2
ORDER BY created_at DESC
LIMIT $3;

-- name: UpdateAgent :one
UPDATE agents
SET name = $2,
    strategy_prompt = $3,
    pair = $4,
    interval_sec = $5,
    risk_params = $6,
    llm_params = $7,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteAgent :exec
DELETE FROM agents WHERE id = $1;

-- name: UpdateAgentStatus :one
UPDATE agents SET status = $2, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateAgentState :exec
UPDATE agents
SET state = $2,
    total_pnl = $3,
    trade_count = $4,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateAgentAUM :exec
UPDATE agents
SET aum = $2,
    investor_count = $3,
    updated_at = NOW()
WHERE id = $1;

-- name: ListRunningAgents :many
SELECT * FROM agents WHERE status = 'running';
