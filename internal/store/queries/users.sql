-- name: CreateUser :one
INSERT INTO users (address)
VALUES ($1)
ON CONFLICT (address) DO UPDATE SET updated_at = NOW()
RETURNING *;

-- name: GetUserByAddress :one
SELECT * FROM users WHERE address = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;
