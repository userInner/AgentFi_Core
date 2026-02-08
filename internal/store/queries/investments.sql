-- name: CreateInvestment :one
INSERT INTO investments (user_id, agent_id, amount, share_ratio)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetInvestment :one
SELECT * FROM investments WHERE id = $1;

-- name: ListInvestmentsByUser :many
SELECT * FROM investments WHERE user_id = $1 AND status = 'active'
ORDER BY created_at DESC;

-- name: ListInvestmentsByAgent :many
SELECT * FROM investments WHERE agent_id = $1 AND status = 'active';

-- name: RedeemInvestment :one
UPDATE investments SET status = 'redeemed', redeemed_at = NOW()
WHERE id = $1
RETURNING *;
