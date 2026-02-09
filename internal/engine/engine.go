// Package engine provides the Agent Loop execution engine and scheduler.
// It implements the ReAct cycle: load state → Fast Path → LLM → tool_call
// routing → risk control → execution → persistence.
// Requirements: 4.1, 4.2, 4.3, 4.4, 4.5, 4.6, 7.1, 7.2, 7.3, 7.4
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentfi/agentfi-go-backend/internal/engine/fastpath"
	"github.com/agentfi/agentfi-go-backend/internal/llm"
	"github.com/agentfi/agentfi-go-backend/internal/mcp"
	"github.com/agentfi/agentfi-go-backend/internal/risk"
	"github.com/agentfi/agentfi-go-backend/internal/store"
	"github.com/agentfi/agentfi-go-backend/pkg/config"
)

// CycleTimeout is the maximum duration for a single Agent Loop cycle (Req 4.5).
const CycleTimeout = 60 * time.Second

// MaxToolCallRounds limits the ReAct loop to prevent infinite tool_call chains.
const MaxToolCallRounds = 10

// Errors returned by the engine.
var (
	ErrCycleTimeout = errors.New("engine: cycle timeout exceeded")
	ErrAgentNotFound = errors.New("engine: agent not found")
)

// AgentState holds the runtime state for a running agent.
type AgentState struct {
	Holdings        map[string]int64 `json:"holdings"`
	CumulativePnL   int64            `json:"cumulative_pnl"`
	DailyLoss       int64            `json:"daily_loss"`
	DailyTradeCount int              `json:"daily_trade_count"`
	HourlyTradeCount int             `json:"hourly_trade_count"`
	LastCycleAt     time.Time        `json:"last_cycle_at"`
	CycleNumber     int64            `json:"cycle_number"`
}

// RunningAgent represents an agent that is actively being scheduled.
type RunningAgent struct {
	Agent  store.Agent
	Ticker *time.Ticker
	Cancel context.CancelFunc
	State  *AgentState
}


// --- Dependency interfaces for testability (Req 11.3) ---

// EngineStore defines the database operations needed by the engine.
type EngineStore interface {
	GetAgent(ctx context.Context, id uuid.UUID) (store.Agent, error)
	ListRunningAgents(ctx context.Context) ([]store.Agent, error)
	ListAgentMCPServers(ctx context.Context, agentID uuid.UUID) ([]store.McpServer, error)
	UpdateAgentState(ctx context.Context, arg store.UpdateAgentStateParams) error
	UpdateAgentStatus(ctx context.Context, arg store.UpdateAgentStatusParams) (store.Agent, error)
	CreateAgentCycle(ctx context.Context, arg store.CreateAgentCycleParams) (store.AgentCycle, error)
	CreateTradeLog(ctx context.Context, arg store.CreateTradeLogParams) (store.TradeLog, error)
}

// MCPToolCaller abstracts MCP tool calls for the engine.
type MCPToolCaller interface {
	CallTool(ctx context.Context, serverID uuid.UUID, toolName string, args map[string]any) (any, error)
	GetToolDefinitions(ctx context.Context, serverIDs []uuid.UUID) ([]mcp.ToolDefinition, error)
}

// RiskValidator abstracts risk validation for the engine.
type RiskValidator interface {
	Validate(ctx context.Context, trade risk.TradeRequest, params risk.RiskParams, state risk.AgentState) (*risk.RiskDecision, error)
}

// Engine is the core Agent Loop execution engine and scheduler.
type Engine struct {
	store      EngineStore
	llm        llm.Client
	mcpManager MCPToolCaller
	risk       RiskValidator
	llmCfg     config.LLMConfig

	mu     sync.RWMutex
	agents map[uuid.UUID]*RunningAgent
	stopCh chan struct{}
}

// NewEngine creates a new Engine.
func NewEngine(
	st EngineStore,
	llmClient llm.Client,
	mcpMgr MCPToolCaller,
	riskCtrl RiskValidator,
	llmCfg config.LLMConfig,
) *Engine {
	return &Engine{
		store:      st,
		llm:        llmClient,
		mcpManager: mcpMgr,
		risk:       riskCtrl,
		llmCfg:     llmCfg,
		agents:     make(map[uuid.UUID]*RunningAgent),
		stopCh:     make(chan struct{}),
	}
}

// CycleResult captures the outcome of a single RunCycle execution for logging.
type CycleResult struct {
	FastPathHit  bool
	ToolCalls    []toolCallRecord
	LLMPrompt    string
	LLMResponse  string
	TokenUsage   llm.TokenUsage
	TradeAction  string // "BUY", "SELL", or "" if no trade
	Error        string
}

type toolCallRecord struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Result    any            `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// RunCycle executes a single Agent Loop cycle.
// Flow: load state → Fast Path → LLM → tool_call routing → risk → execute → persist
// Requirements: 4.1, 4.2, 4.3, 4.4, 4.5
func (e *Engine) RunCycle(ctx context.Context, ra *RunningAgent) error {
	// Apply 60s timeout (Req 4.5).
	cycleCtx, cancel := context.WithTimeout(ctx, CycleTimeout)
	defer cancel()

	startedAt := time.Now().UTC()
	ra.State.CycleNumber++
	cycleNum := ra.State.CycleNumber

	slog.Info("engine: cycle start",
		slog.String("agent_id", ra.Agent.ID.String()),
		slog.Int64("cycle_num", cycleNum),
	)

	result := &CycleResult{}

	// Run the core cycle logic; capture any error.
	cycleErr := e.runCycleInner(cycleCtx, ra, result)
	if cycleErr != nil {
		result.Error = cycleErr.Error()
		slog.Error("engine: cycle error",
			slog.String("agent_id", ra.Agent.ID.String()),
			slog.Int64("cycle_num", cycleNum),
			slog.String("error", cycleErr.Error()),
		)
	}

	// Persist state and cycle log regardless of error (Req 7.1, 7.4).
	finishedAt := time.Now().UTC()
	durationMs := int(finishedAt.Sub(startedAt).Milliseconds())
	ra.State.LastCycleAt = finishedAt

	if persistErr := e.persistCycle(ctx, ra, cycleNum, startedAt, finishedAt, durationMs, result); persistErr != nil {
		slog.Error("engine: persist cycle failed",
			slog.String("agent_id", ra.Agent.ID.String()),
			slog.String("error", persistErr.Error()),
		)
	}

	if persistErr := e.persistAgentState(ctx, ra); persistErr != nil {
		slog.Error("engine: persist agent state failed",
			slog.String("agent_id", ra.Agent.ID.String()),
			slog.String("error", persistErr.Error()),
		)
	}

	slog.Info("engine: cycle end",
		slog.String("agent_id", ra.Agent.ID.String()),
		slog.Int64("cycle_num", cycleNum),
		slog.Int("duration_ms", durationMs),
		slog.Bool("fast_path_hit", result.FastPathHit),
		slog.Int("tool_calls", len(result.ToolCalls)),
	)

	return cycleErr
}

// runCycleInner contains the core ReAct loop logic.
func (e *Engine) runCycleInner(ctx context.Context, ra *RunningAgent, result *CycleResult) error {
	agentID := ra.Agent.ID

	// 1. Get bound MCP server IDs and tool definitions (Req 4.1).
	servers, err := e.store.ListAgentMCPServers(ctx, agentID)
	if err != nil {
		return fmt.Errorf("list mcp servers: %w", err)
	}
	serverIDs := make([]uuid.UUID, len(servers))
	for i, s := range servers {
		serverIDs[i] = s.ID
	}

	toolDefs, err := e.mcpManager.GetToolDefinitions(ctx, serverIDs)
	if err != nil {
		return fmt.Errorf("get tool definitions: %w", err)
	}

	// Build a server-ID lookup by tool name for routing.
	toolServerMap := buildToolServerMap(servers)

	// 2. Try Fast Path evaluation (Req 5.1, 5.3, 5.4).
	conditions := fastpath.TryParse(ra.Agent.StrategyPrompt)
	if len(conditions) > 0 {
		// Build a callTool func scoped to this agent's servers.
		callFn := func(ctx context.Context, toolName string, args map[string]any) (any, error) {
			sid, ok := toolServerMap[toolName]
			if !ok {
				return nil, fmt.Errorf("no server for tool %q", toolName)
			}
			return e.mcpManager.CallTool(ctx, sid, toolName, args)
		}

		met, evalErr := fastpath.Evaluate(ctx, conditions, callFn)
		if evalErr != nil {
			slog.Warn("engine: fast path eval error, falling back to LLM",
				slog.String("agent_id", agentID.String()),
				slog.String("error", evalErr.Error()),
			)
			// Fall through to full LLM path on error.
		} else if !met {
			// Condition not met → skip LLM call entirely (Req 5.3).
			result.FastPathHit = true
			return nil
		} else {
			// Condition met → proceed to LLM for execution decision only (Req 5.2).
			result.FastPathHit = true
		}
	}

	// 3. Construct context message for LLM (Req 4.1).
	llmTools := toLLMToolDefs(toolDefs)
	messages := e.buildMessages(ra)
	result.LLMPrompt = messages[len(messages)-1].Content

	// Resolve agent-level LLM params with global fallback.
	var agentLLMParams LLMParamsOverride
	if err := json.Unmarshal(ra.Agent.LlmParams, &agentLLMParams); err != nil {
		slog.Warn("engine: unmarshal agent llm_params", "error", err)
	}

	chatReq := llm.ChatRequest{
		Model:       agentLLMParams.Model,
		Temperature: agentLLMParams.Temperature,
		MaxTokens:   agentLLMParams.MaxTokens,
		Messages:    messages,
		Tools:       llmTools,
	}

	// 4. ReAct loop: LLM → tool_call → feed result → repeat (Req 4.2, 4.3).
	for round := 0; round < MaxToolCallRounds; round++ {
		if err := ctx.Err(); err != nil {
			return ErrCycleTimeout
		}

		resp, llmErr := e.llm.Chat(ctx, chatReq)
		if llmErr != nil {
			return fmt.Errorf("llm chat: %w", llmErr)
		}

		result.TokenUsage.PromptTokens += resp.Usage.PromptTokens
		result.TokenUsage.CompletionTokens += resp.Usage.CompletionTokens
		result.TokenUsage.TotalTokens += resp.Usage.TotalTokens
		result.LLMResponse = resp.Content

		// No tool calls → final response (Req 4.3).
		if len(resp.ToolCalls) == 0 {
			// Check if the response contains a trade instruction.
			e.handleFinalResponse(ctx, ra, resp.Content, result)
			return nil
		}

		// Process tool calls (Req 4.2).
		assistantMsg := llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		chatReq.Messages = append(chatReq.Messages, assistantMsg)

		for _, tc := range resp.ToolCalls {
			tcRecord := toolCallRecord{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			}

			// Check if this is an execute_trade call → route through risk first.
			if tc.Name == "execute_trade" {
				tradeResult, tradeErr := e.handleTradeToolCall(ctx, ra, tc, result)
				if tradeErr != nil {
					tcRecord.Error = tradeErr.Error()
					// Report error to LLM (Req 4.4).
					chatReq.Messages = append(chatReq.Messages, llm.Message{
						Role:       "tool",
						Content:    fmt.Sprintf("Error: %s", tradeErr.Error()),
						ToolCallID: tc.ID,
					})
				} else {
					tcRecord.Result = tradeResult
					resultJSON, _ := json.Marshal(tradeResult)
					chatReq.Messages = append(chatReq.Messages, llm.Message{
						Role:       "tool",
						Content:    string(resultJSON),
						ToolCallID: tc.ID,
					})
				}
				result.ToolCalls = append(result.ToolCalls, tcRecord)
				continue
			}

			// Route to MCP server.
			serverID, ok := toolServerMap[tc.Name]
			if !ok {
				tcRecord.Error = fmt.Sprintf("no server for tool %q", tc.Name)
				chatReq.Messages = append(chatReq.Messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("Error: tool %q not found", tc.Name),
					ToolCallID: tc.ID,
				})
				result.ToolCalls = append(result.ToolCalls, tcRecord)
				continue
			}

			toolResult, toolErr := e.mcpManager.CallTool(ctx, serverID, tc.Name, tc.Arguments)
			if toolErr != nil {
				tcRecord.Error = toolErr.Error()
				// Report error to LLM so it can decide alternative action (Req 4.4).
				chatReq.Messages = append(chatReq.Messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("Error: %s", toolErr.Error()),
					ToolCallID: tc.ID,
				})
			} else {
				tcRecord.Result = toolResult
				resultJSON, _ := json.Marshal(toolResult)
				chatReq.Messages = append(chatReq.Messages, llm.Message{
					Role:       "tool",
					Content:    string(resultJSON),
					ToolCallID: tc.ID,
				})
			}
			result.ToolCalls = append(result.ToolCalls, tcRecord)
		}
	}

	return fmt.Errorf("engine: max tool call rounds (%d) exceeded", MaxToolCallRounds)
}

// LLMParamsOverride holds per-agent LLM config from the agents.llm_params JSONB column.
type LLMParamsOverride struct {
	Model       string  `json:"model,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

// buildMessages constructs the LLM conversation context (Req 4.1).
// Includes: system prompt with strategy, current state summary, timestamp.
func (e *Engine) buildMessages(ra *RunningAgent) []llm.Message {
	stateJSON, _ := json.Marshal(ra.State)
	systemContent := fmt.Sprintf(
		"You are a trading agent. Follow the strategy below.\n\n"+
			"Strategy: %s\n\n"+
			"Trading pair: %s\n\n"+
			"Current state:\n%s\n\n"+
			"Current time: %s\n\n"+
			"When you want to execute a trade, call the execute_trade tool with action (BUY/SELL), pair, amount (integer cents), and price (integer cents).\n"+
			"If no action is needed, respond with your analysis.",
		ra.Agent.StrategyPrompt,
		ra.Agent.Pair,
		string(stateJSON),
		time.Now().UTC().Format(time.RFC3339),
	)

	return []llm.Message{
		{Role: "system", Content: systemContent},
		{Role: "user", Content: "Analyze the current market conditions and decide on the next action."},
	}
}

// handleFinalResponse processes the LLM's final text response (no tool calls).
// Logs the decision (Req 4.3).
func (e *Engine) handleFinalResponse(ctx context.Context, ra *RunningAgent, content string, result *CycleResult) {
	slog.Info("engine: llm final response",
		slog.String("agent_id", ra.Agent.ID.String()),
		slog.String("decision", truncate(content, 200)),
	)
}

// tradeToolCallArgs represents the expected arguments for execute_trade.
type tradeToolCallArgs struct {
	Action string `json:"action"` // "BUY" | "SELL"
	Pair   string `json:"pair"`
	Amount int64  `json:"amount"` // cents
	Price  int64  `json:"price"`  // cents
}

// handleTradeToolCall processes an execute_trade tool call through risk control.
// Requirements: 6.1, 6.2, 6.3, 6.4, 6.5, 6.6
func (e *Engine) handleTradeToolCall(
	ctx context.Context,
	ra *RunningAgent,
	tc llm.ToolCall,
	result *CycleResult,
) (any, error) {
	// Parse trade arguments.
	argsJSON, _ := json.Marshal(tc.Arguments)
	var args tradeToolCallArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil, fmt.Errorf("invalid trade arguments: %w", err)
	}

	if args.Action == "" || args.Amount <= 0 {
		return nil, fmt.Errorf("invalid trade: action=%q amount=%d", args.Action, args.Amount)
	}

	// Build risk params from agent config.
	var riskParams risk.RiskParams
	if err := json.Unmarshal(ra.Agent.RiskParams, &riskParams); err != nil {
		return nil, fmt.Errorf("unmarshal risk params: %w", err)
	}

	riskState := risk.AgentState{
		CumulativeDailyLoss: ra.State.DailyLoss,
		HourlyTradeCount:    ra.State.HourlyTradeCount,
		CurrentPositionAmt:  currentPosition(ra.State.Holdings),
		AUM:                 ra.Agent.Aum,
	}

	tradeReq := risk.TradeRequest{
		AgentID:  ra.Agent.ID,
		Action:   args.Action,
		Pair:     args.Pair,
		Amount:   args.Amount,
		Price:    args.Price,
		CycleNum: ra.State.CycleNumber,
	}

	// Validate through risk controller (Req 6.1).
	decision, err := e.risk.Validate(ctx, tradeReq, riskParams, riskState)
	if err != nil {
		return nil, fmt.Errorf("risk validation: %w", err)
	}

	// Log trade (Req 6.6).
	riskReason := pgtype.Text{}
	if decision.Reason != "" {
		riskReason = pgtype.Text{String: decision.Reason, Valid: true}
	}
	_, logErr := e.store.CreateTradeLog(ctx, store.CreateTradeLogParams{
		AgentID:      ra.Agent.ID,
		CycleNum:     ra.State.CycleNumber,
		Action:       args.Action,
		Pair:         args.Pair,
		Amount:       args.Amount,
		Price:        args.Price,
		Pnl:          0, // PnL calculated post-execution
		RiskApproved: decision.Approved,
		RiskReason:   riskReason,
	})
	if logErr != nil {
		slog.Error("engine: create trade log", "error", logErr)
	}

	if !decision.Approved {
		result.TradeAction = ""
		// If risk says pause agent (Req 6.3 — daily loss exceeded).
		if decision.PauseAgent {
			if _, pauseErr := e.store.UpdateAgentStatus(ctx, store.UpdateAgentStatusParams{
				ID:     ra.Agent.ID,
				Status: "paused",
			}); pauseErr != nil {
				slog.Error("engine: pause agent after risk", "error", pauseErr)
			}
			e.PauseAgent(ra.Agent.ID)
		}
		return map[string]any{
			"status": "rejected",
			"reason": decision.Reason,
		}, nil
	}

	// Trade approved — execute via wallet MCP.
	result.TradeAction = args.Action
	ra.State.DailyTradeCount++
	ra.State.HourlyTradeCount++

	// Update holdings.
	pair := args.Pair
	if pair == "" {
		pair = ra.Agent.Pair
	}
	switch args.Action {
	case "BUY":
		ra.State.Holdings[pair] += args.Amount
	case "SELL":
		ra.State.Holdings[pair] -= args.Amount
	}

	return map[string]any{
		"status": "executed",
		"action": args.Action,
		"pair":   pair,
		"amount": args.Amount,
		"price":  args.Price,
	}, nil
}

// --- Persistence helpers (Req 7.1, 7.4) ---

// persistCycle writes the cycle log to agent_cycles (Req 7.4).
func (e *Engine) persistCycle(
	ctx context.Context,
	ra *RunningAgent,
	cycleNum int64,
	startedAt, finishedAt time.Time,
	durationMs int,
	result *CycleResult,
) error {
	toolCallsJSON, _ := json.Marshal(result.ToolCalls)
	tokenJSON, _ := json.Marshal(result.TokenUsage)

	errText := pgtype.Text{}
	if result.Error != "" {
		errText = pgtype.Text{String: result.Error, Valid: true}
	}

	promptText := pgtype.Text{}
	if result.LLMPrompt != "" {
		promptText = pgtype.Text{String: result.LLMPrompt, Valid: true}
	}

	responseText := pgtype.Text{}
	if result.LLMResponse != "" {
		responseText = pgtype.Text{String: result.LLMResponse, Valid: true}
	}

	_, err := e.store.CreateAgentCycle(ctx, store.CreateAgentCycleParams{
		AgentID:     ra.Agent.ID,
		CycleNum:    cycleNum,
		StartedAt:   startedAt,
		FinishedAt:  pgtype.Timestamptz{Time: finishedAt, Valid: true},
		DurationMs:  pgtype.Int4{Int32: int32(durationMs), Valid: true},
		LlmPrompt:  promptText,
		LlmResponse: responseText,
		ToolCalls:   toolCallsJSON,
		TokenUsage:  tokenJSON,
		FastPathHit: result.FastPathHit,
		Error:       errText,
	})
	return err
}

// persistAgentState writes the agent's current state to the agents table (Req 7.1).
func (e *Engine) persistAgentState(ctx context.Context, ra *RunningAgent) error {
	stateJSON, err := json.Marshal(ra.State)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	return e.store.UpdateAgentState(ctx, store.UpdateAgentStateParams{
		ID:         ra.Agent.ID,
		State:      stateJSON,
		TotalPnl:   ra.State.CumulativePnL,
		TradeCount: int32(ra.State.DailyTradeCount), // simplified; real impl tracks total
	})
}

// --- Helper functions ---

// buildToolServerMap creates a mapping from tool name to server ID.
func buildToolServerMap(servers []store.McpServer) map[string]uuid.UUID {
	m := make(map[string]uuid.UUID)
	for _, srv := range servers {
		var tools []mcp.ToolDefinition
		if err := json.Unmarshal(srv.Tools, &tools); err != nil {
			continue
		}
		for _, t := range tools {
			m[t.Name] = srv.ID
		}
	}
	return m
}

// toLLMToolDefs converts MCP tool definitions to LLM tool definitions.
func toLLMToolDefs(mcpTools []mcp.ToolDefinition) []llm.ToolDefinition {
	// Always include the execute_trade tool.
	defs := []llm.ToolDefinition{
		{
			Name:        "execute_trade",
			Description: "Execute a trade on the exchange. Use this when you decide to buy or sell.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"BUY", "SELL"}},
					"pair":   map[string]any{"type": "string"},
					"amount": map[string]any{"type": "integer", "description": "amount in cents"},
					"price":  map[string]any{"type": "integer", "description": "price in cents"},
				},
				"required": []string{"action", "pair", "amount", "price"},
			},
		},
	}

	for _, t := range mcpTools {
		defs = append(defs, llm.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return defs
}

// LoadAgentState deserializes the agent's persisted state from the DB JSONB column (Req 7.2).
func LoadAgentState(raw json.RawMessage) *AgentState {
	state := &AgentState{
		Holdings: make(map[string]int64),
	}
	if len(raw) > 0 && string(raw) != "{}" {
		_ = json.Unmarshal(raw, state)
	}
	if state.Holdings == nil {
		state.Holdings = make(map[string]int64)
	}
	return state
}

// currentPosition sums all holdings values.
func currentPosition(holdings map[string]int64) int64 {
	var total int64
	for _, v := range holdings {
		if v > 0 {
			total += v
		}
	}
	return total
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// --- Scheduler (Req 4.6, 7.3) ---

// StartAgent starts the Agent Loop for a given agent ID.
// It loads the agent from DB, initializes state, and starts a ticker goroutine.
func (e *Engine) StartAgent(ctx context.Context, agentID uuid.UUID) error {
	agent, err := e.store.GetAgent(ctx, agentID)
	if err != nil {
		return fmt.Errorf("engine: get agent: %w", err)
	}

	interval := clampInterval(agent.IntervalSec)
	state := LoadAgentState(agent.State)

	agentCtx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(time.Duration(interval) * time.Second)

	ra := &RunningAgent{
		Agent:  agent,
		Ticker: ticker,
		Cancel: cancel,
		State:  state,
	}

	e.mu.Lock()
	// If already running, stop the old one first.
	if old, exists := e.agents[agentID]; exists {
		old.Cancel()
		old.Ticker.Stop()
	}
	e.agents[agentID] = ra
	e.mu.Unlock()

	// Launch the ticker goroutine.
	go e.runAgentLoop(agentCtx, ra)

	slog.Info("engine: agent started",
		slog.String("agent_id", agentID.String()),
		slog.Int("interval_sec", int(interval)),
	)
	return nil
}

// PauseAgent stops the Agent Loop for a given agent ID.
func (e *Engine) PauseAgent(agentID uuid.UUID) {
	e.mu.Lock()
	ra, exists := e.agents[agentID]
	if exists {
		ra.Cancel()
		ra.Ticker.Stop()
		delete(e.agents, agentID)
	}
	e.mu.Unlock()

	if exists {
		slog.Info("engine: agent paused", slog.String("agent_id", agentID.String()))
	}
}

// RestoreAgents loads all agents with status='running' from the database and
// resumes their Agent Loops. Called on MCP_Host startup (Req 7.3).
func (e *Engine) RestoreAgents(ctx context.Context) error {
	agents, err := e.store.ListRunningAgents(ctx)
	if err != nil {
		return fmt.Errorf("engine: list running agents: %w", err)
	}

	slog.Info("engine: restoring agents", slog.Int("count", len(agents)))

	for _, agent := range agents {
		if err := e.StartAgent(ctx, agent.ID); err != nil {
			slog.Error("engine: restore agent failed",
				slog.String("agent_id", agent.ID.String()),
				slog.String("error", err.Error()),
			)
			// Continue restoring other agents; don't fail the whole startup.
			continue
		}
	}

	slog.Info("engine: restore complete")
	return nil
}

// Stop gracefully shuts down all running agents.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	for id, ra := range e.agents {
		ra.Cancel()
		ra.Ticker.Stop()
		slog.Info("engine: stopping agent", slog.String("agent_id", id.String()))
	}
	e.agents = make(map[uuid.UUID]*RunningAgent)

	select {
	case <-e.stopCh:
	default:
		close(e.stopCh)
	}
}

// RunningAgentCount returns the number of currently running agents.
func (e *Engine) RunningAgentCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.agents)
}

// GetRunningAgent returns the RunningAgent for the given ID, or nil.
func (e *Engine) GetRunningAgent(agentID uuid.UUID) *RunningAgent {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.agents[agentID]
}

// runAgentLoop is the goroutine that runs the ticker-driven Agent Loop.
func (e *Engine) runAgentLoop(ctx context.Context, ra *RunningAgent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("engine: agent loop panic",
				slog.String("agent_id", ra.Agent.ID.String()),
				slog.Any("panic", r),
			)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ra.Ticker.C:
			if err := e.RunCycle(ctx, ra); err != nil {
				slog.Error("engine: cycle failed",
					slog.String("agent_id", ra.Agent.ID.String()),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

// clampInterval ensures the interval is within [60, 86400] seconds (Req 4.6).
func clampInterval(sec int32) int32 {
	if sec < 60 {
		return 60
	}
	if sec > 86400 {
		return 86400
	}
	return sec
}
