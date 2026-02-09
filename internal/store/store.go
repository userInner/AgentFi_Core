// Package store provides database access layer using sqlc-generated code.
package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Store wraps sqlc Queries and provides transaction support.
type Store struct {
	pool DBTX
	*Queries
}

// NewStore creates a new Store wrapping the given connection pool.
func NewStore(pool DBTX) *Store {
	return &Store{
		pool:    pool,
		Queries: New(pool),
	}
}

// Tx executes fn inside a database transaction. If fn returns an error the
// transaction is rolled back; otherwise it is committed.
// This satisfies Requirement 12.2 (transaction atomicity).
func (s *Store) Tx(ctx context.Context, fn func(q *Queries) error) error {
	// The pool must implement pgx.Tx-capable interface. We try to begin a tx
	// on the underlying pool. If the pool is already a tx (e.g. nested), we
	// just run fn directly.
	beginner, ok := s.pool.(interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fn(s.Queries)
	}

	tx, err := beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Exec exposes raw exec for ad-hoc queries (used sparingly).
func (s *Store) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	return s.pool.Exec(ctx, sql, args...)
}

// Pool returns the underlying DBTX for cases where direct access is needed.
func (s *Store) Pool() DBTX {
	return s.pool
}

// AgentStore defines the interface for agent-related database operations.
// Used for dependency injection and testing (Req 11.3).
type AgentStore interface {
	CreateAgent(ctx context.Context, arg CreateAgentParams) (Agent, error)
	GetAgent(ctx context.Context, id uuid.UUID) (Agent, error)
	ListAgentsByUser(ctx context.Context, arg ListAgentsByUserParams) ([]Agent, error)
	ListAgentsByUserCursor(ctx context.Context, arg ListAgentsByUserCursorParams) ([]Agent, error)
	UpdateAgent(ctx context.Context, arg UpdateAgentParams) (Agent, error)
	DeleteAgent(ctx context.Context, id uuid.UUID) error
	UpdateAgentStatus(ctx context.Context, arg UpdateAgentStatusParams) (Agent, error)
	GetUserByAddress(ctx context.Context, address string) (User, error)
	CreateUser(ctx context.Context, address string) (User, error)
	BindAgentMCP(ctx context.Context, arg BindAgentMCPParams) error
	UnbindAgentMCP(ctx context.Context, arg UnbindAgentMCPParams) error
	ListAgentMCPServers(ctx context.Context, agentID uuid.UUID) ([]McpServer, error)
}
