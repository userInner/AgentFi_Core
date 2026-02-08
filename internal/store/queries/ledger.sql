-- name: CreateLedgerEntry :one
INSERT INTO ledger_entries (user_id, agent_id, type, amount, balance)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListLedgerByUserAndAgent :many
SELECT * FROM ledger_entries
WHERE user_id = $1 AND agent_id = $2
ORDER BY created_at DESC;

-- name: ListLedgerByUser :many
SELECT * FROM ledger_entries WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;
