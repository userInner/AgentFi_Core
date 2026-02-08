-- name: CreateRiskEvent :one
INSERT INTO risk_events (agent_id, event_type, details)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListRiskEventsByAgent :many
SELECT * FROM risk_events WHERE agent_id = $1
ORDER BY created_at DESC
LIMIT $2;
