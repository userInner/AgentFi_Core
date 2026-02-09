// Package agent provides Agent CRUD and lifecycle management.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/agentfi/agentfi-go-backend/internal/store"
	"github.com/agentfi/agentfi-go-backend/pkg/config"
)

// Errors returned by the agent service.
var (
	ErrNotFound       = errors.New("agent: not found")
	ErrForbidden      = errors.New("agent: forbidden")
	ErrInvalidStatus  = errors.New("agent: invalid status transition")
	ErrValidation     = errors.New("agent: validation error")
)

// Valid agent statuses.
const (
	StatusCreated = "created"
	StatusRunning = "running"
	StatusPaused  = "paused"
	StatusError   = "error"
)

// Interval bounds (seconds).
const (
	MinIntervalSec = 60
	MaxIntervalSec = 86400
)

// RiskParams holds per-agent risk control parameters.
type RiskParams struct {
	MaxTradeAmount   int64   `json:"max_trade_amount"`
	MaxDailyLoss     int64   `json:"max_daily_loss"`
	MaxPositionRatio float64 `json:"max_position_ratio"`
	MaxTradesPerHour int     `json:"max_trades_per_hour"`
}

// LLMParams holds optional per-agent LLM configuration.
// Zero values mean "use global default".
type LLMParams struct {
	Model       string  `json:"model,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

// CreateRequest is the input for creating a new Agent.
type CreateRequest struct {
	Name           string     `json:"name"`
	StrategyPrompt string     `json:"strategy_prompt"`
	Pair           string     `json:"pair"`
	IntervalSec    int32      `json:"interval_sec"`
	MCPServerIDs   []string   `json:"mcp_server_ids"`
	RiskParams     RiskParams `json:"risk_params"`
	LLMParams      *LLMParams `json:"llm_params,omitempty"`
}

// UpdateRequest is the input for updating an existing Agent.
type UpdateRequest struct {
	Name           string     `json:"name"`
	StrategyPrompt string     `json:"strategy_prompt"`
	Pair           string     `json:"pair"`
	IntervalSec    int32      `json:"interval_sec"`
	MCPServerIDs   []string   `json:"mcp_server_ids"`
	RiskParams     RiskParams `json:"risk_params"`
	LLMParams      *LLMParams `json:"llm_params,omitempty"`
}

// AgentResponse is the API-facing representation of an Agent.
type AgentResponse struct {
	ID             uuid.UUID       `json:"id"`
	UserID         uuid.UUID       `json:"user_id"`
	Name           string          `json:"name"`
	StrategyPrompt string          `json:"strategy_prompt"`
	Pair           string          `json:"pair"`
	IntervalSec    int32           `json:"interval_sec"`
	Status         string          `json:"status"`
	RiskParams     RiskParams      `json:"risk_params"`
	LLMParams      LLMParams       `json:"llm_params"`
	Aum            int64           `json:"aum"`
	InvestorCount  int32           `json:"investor_count"`
	TotalPnl       int64           `json:"total_pnl"`
	TradeCount     int32           `json:"trade_count"`
	MCPServerIDs   []uuid.UUID     `json:"mcp_server_ids"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Service provides Agent CRUD and lifecycle operations.
type Service struct {
	store     *store.Store
	llmCfg    config.LLMConfig
}

// NewService creates a new agent service.
func NewService(s *store.Store, llmCfg config.LLMConfig) *Service {
	return &Service{store: s, llmCfg: llmCfg}
}

// clampInterval ensures the interval is within [MinIntervalSec, MaxIntervalSec].
func clampInterval(sec int32) int32 {
	if sec < MinIntervalSec {
		return MinIntervalSec
	}
	if sec > MaxIntervalSec {
		return MaxIntervalSec
	}
	return sec
}

// resolveLLMParams merges agent-level LLM params with global defaults.
func (s *Service) resolveLLMParams(p *LLMParams) LLMParams {
	if p == nil {
		return LLMParams{}
	}
	return *p
}

// toResponse converts a store.Agent to an AgentResponse.
func toResponse(a store.Agent, mcpIDs []uuid.UUID) AgentResponse {
	var rp RiskParams
	json.Unmarshal(a.RiskParams, &rp) //nolint:errcheck

	var lp LLMParams
	json.Unmarshal(a.LlmParams, &lp) //nolint:errcheck

	if mcpIDs == nil {
		mcpIDs = []uuid.UUID{}
	}

	return AgentResponse{
		ID:             a.ID,
		UserID:         a.UserID,
		Name:           a.Name,
		StrategyPrompt: a.StrategyPrompt,
		Pair:           a.Pair,
		IntervalSec:    a.IntervalSec,
		Status:         a.Status,
		RiskParams:     rp,
		LLMParams:      lp,
		Aum:            a.Aum,
		InvestorCount:  a.InvestorCount,
		TotalPnl:       a.TotalPnl,
		TradeCount:     a.TradeCount,
		MCPServerIDs:   mcpIDs,
		CreatedAt:      a.CreatedAt,
		UpdatedAt:      a.UpdatedAt,
	}
}

// Create creates a new Agent and binds MCP servers.
func (s *Service) Create(ctx context.Context, userAddr string, req CreateRequest) (*AgentResponse, error) {
	if err := validateCreate(req); err != nil {
		return nil, err
	}

	// Ensure user exists.
	user, err := s.store.CreateUser(ctx, userAddr)
	if err != nil {
		return nil, fmt.Errorf("agent: ensure user: %w", err)
	}

	riskJSON, _ := json.Marshal(req.RiskParams)
	llmJSON, _ := json.Marshal(s.resolveLLMParams(req.LLMParams))

	var agent store.Agent
	err = s.store.Tx(ctx, func(q *store.Queries) error {
		var txErr error
		agent, txErr = q.CreateAgent(ctx, store.CreateAgentParams{
			UserID:         user.ID,
			Name:           req.Name,
			StrategyPrompt: req.StrategyPrompt,
			Pair:           req.Pair,
			IntervalSec:    clampInterval(req.IntervalSec),
			RiskParams:     riskJSON,
			LlmParams:      llmJSON,
		})
		if txErr != nil {
			return txErr
		}

		// Bind MCP servers.
		for _, sid := range req.MCPServerIDs {
			serverID, parseErr := uuid.Parse(sid)
			if parseErr != nil {
				return fmt.Errorf("agent: invalid mcp server id %q: %w", sid, ErrValidation)
			}
			if txErr = q.BindAgentMCP(ctx, store.BindAgentMCPParams{
				AgentID:  agent.ID,
				ServerID: serverID,
			}); txErr != nil {
				return txErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agent: create: %w", err)
	}

	mcpIDs := parseMCPIDs(req.MCPServerIDs)
	resp := toResponse(agent, mcpIDs)
	return &resp, nil
}

// Get retrieves an Agent by ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*AgentResponse, error) {
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("agent: get: %w", err)
	}

	servers, err := s.store.ListAgentMCPServers(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("agent: list mcp servers: %w", err)
	}
	mcpIDs := make([]uuid.UUID, len(servers))
	for i, srv := range servers {
		mcpIDs[i] = srv.ID
	}

	resp := toResponse(agent, mcpIDs)
	return &resp, nil
}

// List returns agents for a user with cursor-based pagination.
func (s *Service) List(ctx context.Context, userAddr string, cursor string, limit int32) ([]AgentResponse, string, error) {
	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []AgentResponse{}, "", nil
		}
		return nil, "", fmt.Errorf("agent: get user: %w", err)
	}

	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var agents []store.Agent
	if cursor != "" {
		cursorTime, parseErr := time.Parse(time.RFC3339Nano, cursor)
		if parseErr != nil {
			return nil, "", fmt.Errorf("agent: invalid cursor: %w", ErrValidation)
		}
		agents, err = s.store.ListAgentsByUserCursor(ctx, store.ListAgentsByUserCursorParams{
			UserID:    user.ID,
			CreatedAt: cursorTime,
			Limit:     limit + 1, // fetch one extra to determine next cursor
		})
	} else {
		agents, err = s.store.ListAgentsByUser(ctx, store.ListAgentsByUserParams{
			UserID: user.ID,
			Limit:  limit + 1,
		})
	}
	if err != nil {
		return nil, "", fmt.Errorf("agent: list: %w", err)
	}

	var nextCursor string
	if int32(len(agents)) > limit {
		nextCursor = agents[limit].CreatedAt.Format(time.RFC3339Nano)
		agents = agents[:limit]
	}

	results := make([]AgentResponse, len(agents))
	for i, a := range agents {
		results[i] = toResponse(a, nil) // skip MCP IDs in list for performance
	}

	return results, nextCursor, nil
}

// Update modifies an Agent's configuration. Rebinds MCP servers.
func (s *Service) Update(ctx context.Context, id uuid.UUID, userAddr string, req UpdateRequest) (*AgentResponse, error) {
	if err := validateUpdate(req); err != nil {
		return nil, err
	}

	// Verify ownership.
	existing, err := s.store.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("agent: get for update: %w", err)
	}

	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		return nil, fmt.Errorf("agent: get user: %w", err)
	}
	if existing.UserID != user.ID {
		return nil, ErrForbidden
	}

	riskJSON, _ := json.Marshal(req.RiskParams)
	llmJSON, _ := json.Marshal(s.resolveLLMParams(req.LLMParams))

	var agent store.Agent
	err = s.store.Tx(ctx, func(q *store.Queries) error {
		var txErr error
		agent, txErr = q.UpdateAgent(ctx, store.UpdateAgentParams{
			ID:             id,
			Name:           req.Name,
			StrategyPrompt: req.StrategyPrompt,
			Pair:           req.Pair,
			IntervalSec:    clampInterval(req.IntervalSec),
			RiskParams:     riskJSON,
			LlmParams:      llmJSON,
		})
		if txErr != nil {
			return txErr
		}

		// Rebind MCP servers: remove old, add new.
		oldServers, txErr := q.ListAgentMCPServers(ctx, id)
		if txErr != nil {
			return txErr
		}
		for _, srv := range oldServers {
			if txErr = q.UnbindAgentMCP(ctx, store.UnbindAgentMCPParams{
				AgentID:  id,
				ServerID: srv.ID,
			}); txErr != nil {
				return txErr
			}
		}
		for _, sid := range req.MCPServerIDs {
			serverID, parseErr := uuid.Parse(sid)
			if parseErr != nil {
				return fmt.Errorf("agent: invalid mcp server id %q: %w", sid, ErrValidation)
			}
			if txErr = q.BindAgentMCP(ctx, store.BindAgentMCPParams{
				AgentID:  id,
				ServerID: serverID,
			}); txErr != nil {
				return txErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agent: update: %w", err)
	}

	mcpIDs := parseMCPIDs(req.MCPServerIDs)
	resp := toResponse(agent, mcpIDs)
	return &resp, nil
}

// Delete removes an Agent. MCP bindings are cascade-deleted by the DB.
func (s *Service) Delete(ctx context.Context, id uuid.UUID, userAddr string) error {
	existing, err := s.store.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("agent: get for delete: %w", err)
	}

	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		return fmt.Errorf("agent: get user: %w", err)
	}
	if existing.UserID != user.ID {
		return ErrForbidden
	}

	if err := s.store.DeleteAgent(ctx, id); err != nil {
		return fmt.Errorf("agent: delete: %w", err)
	}
	return nil
}

// Start transitions an Agent to running status.
// Only agents in "created" or "paused" status can be started.
func (s *Service) Start(ctx context.Context, id uuid.UUID, userAddr string) (*AgentResponse, error) {
	existing, err := s.store.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("agent: get for start: %w", err)
	}

	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		return nil, fmt.Errorf("agent: get user: %w", err)
	}
	if existing.UserID != user.ID {
		return nil, ErrForbidden
	}

	if existing.Status != StatusCreated && existing.Status != StatusPaused {
		return nil, fmt.Errorf("%w: can only start from created or paused, current=%s", ErrInvalidStatus, existing.Status)
	}

	agent, err := s.store.UpdateAgentStatus(ctx, store.UpdateAgentStatusParams{
		ID:     id,
		Status: StatusRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: start: %w", err)
	}

	resp := toResponse(agent, nil)
	return &resp, nil
}

// Pause transitions an Agent to paused status.
// Only agents in "running" status can be paused.
func (s *Service) Pause(ctx context.Context, id uuid.UUID, userAddr string) (*AgentResponse, error) {
	existing, err := s.store.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("agent: get for pause: %w", err)
	}

	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		return nil, fmt.Errorf("agent: get user: %w", err)
	}
	if existing.UserID != user.ID {
		return nil, ErrForbidden
	}

	if existing.Status != StatusRunning {
		return nil, fmt.Errorf("%w: can only pause from running, current=%s", ErrInvalidStatus, existing.Status)
	}

	agent, err := s.store.UpdateAgentStatus(ctx, store.UpdateAgentStatusParams{
		ID:     id,
		Status: StatusPaused,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: pause: %w", err)
	}

	resp := toResponse(agent, nil)
	return &resp, nil
}

// --- helpers ---

func parseMCPIDs(ids []string) []uuid.UUID {
	result := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		if id, err := uuid.Parse(s); err == nil {
			result = append(result, id)
		}
	}
	return result
}

func validateCreate(req CreateRequest) error {
	if req.Name == "" {
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	if req.StrategyPrompt == "" {
		return fmt.Errorf("%w: strategy_prompt is required", ErrValidation)
	}
	if req.Pair == "" {
		return fmt.Errorf("%w: pair is required", ErrValidation)
	}
	return nil
}

func validateUpdate(req UpdateRequest) error {
	if req.Name == "" {
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	if req.StrategyPrompt == "" {
		return fmt.Errorf("%w: strategy_prompt is required", ErrValidation)
	}
	if req.Pair == "" {
		return fmt.Errorf("%w: pair is required", ErrValidation)
	}
	return nil
}
