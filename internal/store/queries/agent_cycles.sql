-- name: CreateAgentCycle :one
INSERT INTO agent_cycles (agent_id, cycle_num, started_at, finished_at, duration_ms, llm_prompt, llm_response, tool_calls, token_usage, fast_path_hit, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: ListAgentCycles :many
SELECT * FROM agent_cycles WHERE agent_id = $1
ORDER BY cycle_num DESC
LIMIT $2;
