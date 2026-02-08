-- name: CreateTradeLog :one
INSERT INTO trade_logs (agent_id, cycle_num, action, pair, amount, price, pnl, risk_approved, risk_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListTradeLogsByAgent :many
SELECT * FROM trade_logs WHERE agent_id = $1
ORDER BY created_at DESC
LIMIT $2;
