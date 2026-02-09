package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"pgregory.net/rapid"

	"github.com/agentfi/agentfi-go-backend/internal/llm"
	"github.com/agentfi/agentfi-go-backend/internal/mcp"
	"github.com/agentfi/agentfi-go-backend/internal/risk"
	"github.com/agentfi/agentfi-go-backend/internal/store"
	"github.com/agentfi/agentfi-go-backend/pkg/config"
)

// --- Mock implementations ---

// mockEngineStore captures calls for verification.
type mockEngineStore struct {
	agents       map[uuid.UUID]store.Agent
	mcpServers   map[uuid.UUID][]store.McpServer
	cycles       []store.CreateAgentCycleParams
	tradeLogs    []store.CreateTradeLogParams
	stateUpdates []store.UpdateAgentStateParams
	statusUpdates []store.UpdateAgentStatusParams
}

func newMockEngineStore() *mockEngineStore {
	return &mockEngineStore{
		agents:     make(map[uuid.UUID]store.Agent),
		mcpServers: make(map[uuid.UUID][]store.McpServer),
	}
}

func (m *mockEngineStore) GetAgent(_ context.Context, id uuid.UUID) (store.Agent, error) {
	a, ok := m.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("not found")
	}
	return a, nil
}

func (m *mockEngineStore) ListRunningAgents(_ context.Context) ([]store.Agent, error) {
	var result []store.Agent
	for _, a := range m.agents {
		if a.Status == "running" {
			result = append(result, a)
		}
	}
	return result, nil
}

func (m *mockEngineStore) ListAgentMCPServers(_ context.Context, agentID uuid.UUID) ([]store.McpServer, error) {
	return m.mcpServers[agentID], nil
}

func (m *mockEngineStore) UpdateAgentState(_ context.Context, arg store.UpdateAgentStateParams) error {
	m.stateUpdates = append(m.stateUpdates, arg)
	return nil
}

func (m *mockEngineStore) UpdateAgentStatus(_ context.Context, arg store.UpdateAgentStatusParams) (store.Agent, error) {
	m.statusUpdates = append(m.statusUpdates, arg)
	a, ok := m.agents[arg.ID]
	if !ok {
		return store.Agent{}, fmt.Errorf("not found")
	}
	a.Status = arg.Status
	m.agents[arg.ID] = a
	return a, nil
}

func (m *mockEngineStore) CreateAgentCycle(_ context.Context, arg store.CreateAgentCycleParams) (store.AgentCycle, error) {
	m.cycles = append(m.cycles, arg)
	return store.AgentCycle{
		ID:          uuid.New(),
		AgentID:     arg.AgentID,
		CycleNum:    arg.CycleNum,
		StartedAt:   arg.StartedAt,
		FinishedAt:  arg.FinishedAt,
		DurationMs:  arg.DurationMs,
		LlmPrompt:  arg.LlmPrompt,
		LlmResponse: arg.LlmResponse,
		ToolCalls:   arg.ToolCalls,
		TokenUsage:  arg.TokenUsage,
		FastPathHit: arg.FastPathHit,
		Error:       arg.Error,
	}, nil
}

func (m *mockEngineStore) CreateTradeLog(_ context.Context, arg store.CreateTradeLogParams) (store.TradeLog, error) {
	m.tradeLogs = append(m.tradeLogs, arg)
	return store.TradeLog{
		ID:           uuid.New(),
		AgentID:      arg.AgentID,
		CycleNum:     arg.CycleNum,
		Action:       arg.Action,
		Pair:         arg.Pair,
		Amount:       arg.Amount,
		Price:        arg.Price,
		RiskApproved: arg.RiskApproved,
		RiskReason:   arg.RiskReason,
	}, nil
}

// mockLLMClient captures the chat request and returns a configurable response.
type mockLLMClient struct {
	lastRequest *llm.ChatRequest
	response    *llm.ChatResponse
	callCount   int
}

func (m *mockLLMClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.lastRequest = &req
	m.callCount++
	if m.response != nil {
		return m.response, nil
	}
	return &llm.ChatResponse{
		Content: "No action needed.",
		Usage:   llm.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}, nil
}

// mockMCPToolCaller tracks tool calls and returns configurable results.
type mockMCPToolCaller struct {
	toolDefs    map[uuid.UUID][]mcp.ToolDefinition
	callLog     []toolCallEntry
	callResults map[string]any // toolName -> result
}

type toolCallEntry struct {
	ServerID uuid.UUID
	ToolName string
	Args     map[string]any
}

func newMockMCPToolCaller() *mockMCPToolCaller {
	return &mockMCPToolCaller{
		toolDefs:    make(map[uuid.UUID][]mcp.ToolDefinition),
		callResults: make(map[string]any),
	}
}

func (m *mockMCPToolCaller) CallTool(_ context.Context, serverID uuid.UUID, toolName string, args map[string]any) (any, error) {
	m.callLog = append(m.callLog, toolCallEntry{ServerID: serverID, ToolName: toolName, Args: args})
	if result, ok := m.callResults[toolName]; ok {
		return result, nil
	}
	return map[string]any{"status": "ok"}, nil
}

func (m *mockMCPToolCaller) GetToolDefinitions(_ context.Context, serverIDs []uuid.UUID) ([]mcp.ToolDefinition, error) {
	var all []mcp.ToolDefinition
	for _, sid := range serverIDs {
		all = append(all, m.toolDefs[sid]...)
	}
	return all, nil
}

// mockRiskValidator always approves trades.
type mockRiskValidator struct {
	decisions []*risk.RiskDecision
	calls     []risk.TradeRequest
}

func (m *mockRiskValidator) Validate(_ context.Context, trade risk.TradeRequest, _ risk.RiskParams, _ risk.AgentState) (*risk.RiskDecision, error) {
	m.calls = append(m.calls, trade)
	d := &risk.RiskDecision{Approved: true}
	m.decisions = append(m.decisions, d)
	return d, nil
}

// --- Generators ---

func genStrategyPrompt() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		base := rapid.SampledFrom([]string{
			"Buy BTC when market dips",
			"Sell ETH if momentum fades",
			"DCA into SOL weekly",
			"Follow trend on BTC-PERP",
			"Scalp DOGE on volatility spikes",
		}).Draw(t, "base")
		return base
	})
}

func genPair() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{"BTC-PERP", "ETH-PERP", "SOL-PERP", "DOGE-PERP"})
}

func genIntervalSec() *rapid.Generator[int32] {
	return rapid.Int32Range(0, 200000)
}

func genAgentState() *rapid.Generator[*AgentState] {
	return rapid.Custom(func(t *rapid.T) *AgentState {
		numHoldings := rapid.IntRange(0, 5).Draw(t, "numHoldings")
		holdings := make(map[string]int64)
		pairs := []string{"BTC-PERP", "ETH-PERP", "SOL-PERP", "DOGE-PERP", "AVAX-PERP"}
		for i := 0; i < numHoldings; i++ {
			holdings[pairs[i]] = rapid.Int64Range(-1_000_000, 1_000_000).Draw(t, fmt.Sprintf("holding_%d", i))
		}
		return &AgentState{
			Holdings:         holdings,
			CumulativePnL:    rapid.Int64Range(-10_000_000, 10_000_000).Draw(t, "pnl"),
			DailyLoss:        rapid.Int64Range(0, 1_000_000).Draw(t, "dailyLoss"),
			DailyTradeCount:  rapid.IntRange(0, 100).Draw(t, "dailyTradeCount"),
			HourlyTradeCount: rapid.IntRange(0, 50).Draw(t, "hourlyTradeCount"),
			LastCycleAt:      time.Now().UTC().Add(-time.Duration(rapid.IntRange(0, 3600).Draw(t, "lastCycleAgo")) * time.Second),
			CycleNumber:      rapid.Int64Range(0, 10000).Draw(t, "cycleNum"),
		}
	})
}

func genToolDefinitions() *rapid.Generator[[]mcp.ToolDefinition] {
	return rapid.Custom(func(t *rapid.T) []mcp.ToolDefinition {
		n := rapid.IntRange(1, 5).Draw(t, "numTools")
		tools := make([]mcp.ToolDefinition, n)
		for i := 0; i < n; i++ {
			tools[i] = mcp.ToolDefinition{
				Name:        fmt.Sprintf("tool_%d", i),
				Description: fmt.Sprintf("Test tool %d", i),
				Parameters:  map[string]any{"type": "object"},
			}
		}
		return tools
	})
}

// makeTestAgent creates a store.Agent with the given parameters.
func makeTestAgent(prompt, pair string, intervalSec int32) store.Agent {
	riskParams, _ := json.Marshal(risk.RiskParams{
		MaxTradeAmount:   100_000_00,
		MaxDailyLoss:     500_000_00,
		MaxPositionRatio: 0.5,
		MaxTradesPerHour: 10,
	})
	return store.Agent{
		ID:             uuid.New(),
		UserID:         uuid.New(),
		Name:           "test-agent",
		StrategyPrompt: prompt,
		Pair:           pair,
		IntervalSec:    intervalSec,
		Status:         "running",
		RiskParams:     riskParams,
		LlmParams:      json.RawMessage(`{}`),
		State:          json.RawMessage(`{}`),
		Aum:            1_000_000_00,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
}


// Feature: agentfi-go-backend, Property 9: Agent Loop context construction
// For any running Agent, the context message sent to the LLM SHALL contain the
// current timestamp, the Agent's strategy prompt, and all tool definitions from
// bound MCP Servers.
// Validates: Requirements 4.1
func TestPropertyAgentLoopContextConstruction(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		prompt := genStrategyPrompt().Draw(rt, "prompt")
		pair := genPair().Draw(rt, "pair")
		tools := genToolDefinitions().Draw(rt, "tools")

		agent := makeTestAgent(prompt, pair, 120)
		serverID := uuid.New()

		// Set up mocks.
		mockStore := newMockEngineStore()
		mockStore.agents[agent.ID] = agent

		toolsJSON, _ := json.Marshal(tools)
		mockStore.mcpServers[agent.ID] = []store.McpServer{
			{
				ID:    serverID,
				Name:  "test-server",
				Url:   "http://localhost:9000",
				Type:  "http",
				Tools: toolsJSON,
			},
		}

		mockLLM := &mockLLMClient{
			response: &llm.ChatResponse{
				Content: "No action needed.",
				Usage:   llm.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			},
		}

		mockMCP := newMockMCPToolCaller()
		mockMCP.toolDefs[serverID] = tools

		mockRisk := &mockRiskValidator{}

		eng := NewEngine(mockStore, mockLLM, mockMCP, mockRisk, config.LLMConfig{
			Model: "gpt-4o", Temperature: 0.7, MaxTokens: 4096,
		})

		state := &AgentState{Holdings: make(map[string]int64)}
		ra := &RunningAgent{Agent: agent, State: state}

		_ = eng.RunCycle(context.Background(), ra)

		// Verify the LLM was called.
		if mockLLM.lastRequest == nil {
			rt.Fatal("LLM was not called")
		}

		req := mockLLM.lastRequest

		// 1. Messages must contain the strategy prompt.
		foundPrompt := false
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, prompt) {
				foundPrompt = true
				break
			}
		}
		if !foundPrompt {
			rt.Fatalf("LLM messages do not contain strategy prompt %q", prompt)
		}

		// 2. Messages must contain a timestamp (RFC3339 format).
		foundTimestamp := false
		for _, msg := range req.Messages {
			// Check for a year pattern like "2025-" or "2026-" which is part of RFC3339.
			if strings.Contains(msg.Content, time.Now().UTC().Format("2006-01-02")) {
				foundTimestamp = true
				break
			}
		}
		if !foundTimestamp {
			rt.Fatal("LLM messages do not contain current timestamp")
		}

		// 3. Tool definitions must include all bound MCP tools plus execute_trade.
		toolNames := make(map[string]bool)
		for _, td := range req.Tools {
			toolNames[td.Name] = true
		}
		// execute_trade is always included.
		if !toolNames["execute_trade"] {
			rt.Fatal("tool definitions missing execute_trade")
		}
		for _, td := range tools {
			if !toolNames[td.Name] {
				rt.Fatalf("tool definitions missing MCP tool %q", td.Name)
			}
		}
	})
}

// Feature: agentfi-go-backend, Property 10: Agent Loop tool call routing
// For any LLM response containing a tool_call with a tool name matching a bound
// MCP Server's tool, the Agent Loop SHALL route the call to the correct MCP
// Client and return the result to the LLM.
// Validates: Requirements 4.2
func TestPropertyAgentLoopToolCallRouting(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		prompt := genStrategyPrompt().Draw(rt, "prompt")
		pair := genPair().Draw(rt, "pair")

		// Generate 1-3 tools to bind.
		numTools := rapid.IntRange(1, 3).Draw(rt, "numTools")
		tools := make([]mcp.ToolDefinition, numTools)
		for i := 0; i < numTools; i++ {
			tools[i] = mcp.ToolDefinition{
				Name:        fmt.Sprintf("get_data_%d", i),
				Description: fmt.Sprintf("Fetch data %d", i),
				Parameters:  map[string]any{"type": "object"},
			}
		}

		// Pick a random tool to be called by the LLM.
		calledToolIdx := rapid.IntRange(0, numTools-1).Draw(rt, "calledToolIdx")
		calledToolName := tools[calledToolIdx].Name

		agent := makeTestAgent(prompt, pair, 120)
		serverID := uuid.New()

		mockStore := newMockEngineStore()
		mockStore.agents[agent.ID] = agent

		toolsJSON, _ := json.Marshal(tools)
		mockStore.mcpServers[agent.ID] = []store.McpServer{
			{
				ID:    serverID,
				Name:  "data-server",
				Url:   "http://localhost:9000",
				Type:  "http",
				Tools: toolsJSON,
			},
		}

		// LLM returns a tool_call on first request, then a final response.
		callCount := 0
		mockLLM := &mockLLMClient{}
		mockLLM.response = nil // we'll override Chat behavior

		toolCallArgs := map[string]any{"symbol": "BTC"}
		expectedResult := map[string]any{"value": float64(42000)}

		// Custom LLM that returns tool_call first, then final response.
		customLLM := &sequenceLLMClient{
			responses: []*llm.ChatResponse{
				{
					Content: "",
					ToolCalls: []llm.ToolCall{
						{
							ID:        "tc_1",
							Name:      calledToolName,
							Arguments: toolCallArgs,
						},
					},
					Usage: llm.TokenUsage{PromptTokens: 50, CompletionTokens: 20, TotalTokens: 70},
				},
				{
					Content: "Based on the data, no trade needed.",
					Usage:   llm.TokenUsage{PromptTokens: 80, CompletionTokens: 30, TotalTokens: 110},
				},
			},
		}

		mockMCP := newMockMCPToolCaller()
		mockMCP.toolDefs[serverID] = tools
		mockMCP.callResults[calledToolName] = expectedResult

		mockRisk := &mockRiskValidator{}

		eng := NewEngine(mockStore, customLLM, mockMCP, mockRisk, config.LLMConfig{
			Model: "gpt-4o", Temperature: 0.7, MaxTokens: 4096,
		})

		state := &AgentState{Holdings: make(map[string]int64)}
		ra := &RunningAgent{Agent: agent, State: state}

		err := eng.RunCycle(context.Background(), ra)
		if err != nil {
			rt.Fatalf("RunCycle error: %v", err)
		}

		// Verify the MCP tool was called with the correct server ID and tool name.
		if len(mockMCP.callLog) == 0 {
			rt.Fatal("MCP tool was never called")
		}

		found := false
		for _, call := range mockMCP.callLog {
			if call.ServerID == serverID && call.ToolName == calledToolName {
				found = true
				break
			}
		}
		if !found {
			rt.Fatalf("tool call not routed to correct server %s for tool %q; calls: %+v",
				serverID, calledToolName, mockMCP.callLog)
		}

		// Verify the LLM received the tool result (it was called a second time).
		if callCount < 2 && customLLM.callIdx < 2 {
			// The second LLM call should have a tool message with the result.
		}
		if customLLM.callIdx < 2 {
			rt.Fatal("LLM was not called a second time with tool result")
		}

		_ = callCount
	})
}

// sequenceLLMClient returns responses in order.
type sequenceLLMClient struct {
	responses []*llm.ChatResponse
	requests  []llm.ChatRequest
	callIdx   int
}

func (s *sequenceLLMClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.requests = append(s.requests, req)
	idx := s.callIdx
	s.callIdx++
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	return &llm.ChatResponse{Content: "done", Usage: llm.TokenUsage{}}, nil
}

// Feature: agentfi-go-backend, Property 11: Agent interval clamping
// For any user-configured interval, the effective interval SHALL be clamped to
// the range [60 seconds, 86400 seconds].
// Validates: Requirements 4.6
func TestPropertyAgentIntervalClamping(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		input := genIntervalSec().Draw(rt, "interval")
		result := clampInterval(input)

		if result < 60 {
			rt.Fatalf("clamped interval %d is below minimum 60", result)
		}
		if result > 86400 {
			rt.Fatalf("clamped interval %d is above maximum 86400", result)
		}

		// If input is within range, result should equal input.
		if input >= 60 && input <= 86400 && result != input {
			rt.Fatalf("input %d is in range but result %d differs", input, result)
		}
		// If input is below minimum, result should be 60.
		if input < 60 && result != 60 {
			rt.Fatalf("input %d below min but result %d != 60", input, result)
		}
		// If input is above maximum, result should be 86400.
		if input > 86400 && result != 86400 {
			rt.Fatalf("input %d above max but result %d != 86400", input, result)
		}
	})
}

// Feature: agentfi-go-backend, Property 16: Agent state persistence round-trip
// For any Agent state (holdings, PnL, trade count), persisting to the database
// then loading SHALL produce an equivalent state object.
// Validates: Requirements 7.1, 7.2
func TestPropertyAgentStatePersistenceRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		original := genAgentState().Draw(rt, "state")

		// Serialize (what persistAgentState does).
		stateJSON, err := json.Marshal(original)
		if err != nil {
			rt.Fatalf("marshal state: %v", err)
		}

		// Deserialize (what LoadAgentState does).
		loaded := LoadAgentState(json.RawMessage(stateJSON))

		// Compare fields.
		if loaded.CumulativePnL != original.CumulativePnL {
			rt.Fatalf("CumulativePnL mismatch: got %d, want %d", loaded.CumulativePnL, original.CumulativePnL)
		}
		if loaded.DailyLoss != original.DailyLoss {
			rt.Fatalf("DailyLoss mismatch: got %d, want %d", loaded.DailyLoss, original.DailyLoss)
		}
		if loaded.DailyTradeCount != original.DailyTradeCount {
			rt.Fatalf("DailyTradeCount mismatch: got %d, want %d", loaded.DailyTradeCount, original.DailyTradeCount)
		}
		if loaded.HourlyTradeCount != original.HourlyTradeCount {
			rt.Fatalf("HourlyTradeCount mismatch: got %d, want %d", loaded.HourlyTradeCount, original.HourlyTradeCount)
		}
		if loaded.CycleNumber != original.CycleNumber {
			rt.Fatalf("CycleNumber mismatch: got %d, want %d", loaded.CycleNumber, original.CycleNumber)
		}

		// Compare holdings.
		if len(loaded.Holdings) != len(original.Holdings) {
			rt.Fatalf("Holdings length mismatch: got %d, want %d", len(loaded.Holdings), len(original.Holdings))
		}
		for k, v := range original.Holdings {
			if loaded.Holdings[k] != v {
				rt.Fatalf("Holdings[%s] mismatch: got %d, want %d", k, loaded.Holdings[k], v)
			}
		}

		// Compare LastCycleAt (within 1 second tolerance due to JSON time precision).
		timeDiff := loaded.LastCycleAt.Sub(original.LastCycleAt)
		if timeDiff < -time.Second || timeDiff > time.Second {
			rt.Fatalf("LastCycleAt mismatch: got %v, want %v (diff=%v)", loaded.LastCycleAt, original.LastCycleAt, timeDiff)
		}
	})
}

// Feature: agentfi-go-backend, Property 17: Audit log completeness
// For any Agent Loop cycle that invokes the LLM, an audit log entry SHALL be
// created containing the Agent ID, cycle number, prompt, and response.
// Validates: Requirements 7.4
func TestPropertyAuditLogCompleteness(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		prompt := genStrategyPrompt().Draw(rt, "prompt")
		pair := genPair().Draw(rt, "pair")
		llmResponseContent := rapid.SampledFrom([]string{
			"No action needed.",
			"Market is stable, holding position.",
			"Conditions not favorable for trading.",
		}).Draw(rt, "llmResponse")

		agent := makeTestAgent(prompt, pair, 120)
		serverID := uuid.New()

		mockStore := newMockEngineStore()
		mockStore.agents[agent.ID] = agent

		tools := []mcp.ToolDefinition{
			{Name: "get_price", Description: "Get price", Parameters: map[string]any{"type": "object"}},
		}
		toolsJSON, _ := json.Marshal(tools)
		mockStore.mcpServers[agent.ID] = []store.McpServer{
			{ID: serverID, Name: "srv", Url: "http://localhost:9000", Type: "http", Tools: toolsJSON},
		}

		mockLLM := &mockLLMClient{
			response: &llm.ChatResponse{
				Content: llmResponseContent,
				Usage:   llm.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			},
		}

		mockMCP := newMockMCPToolCaller()
		mockMCP.toolDefs[serverID] = tools

		mockRisk := &mockRiskValidator{}

		eng := NewEngine(mockStore, mockLLM, mockMCP, mockRisk, config.LLMConfig{
			Model: "gpt-4o", Temperature: 0.7, MaxTokens: 4096,
		})

		state := &AgentState{Holdings: make(map[string]int64)}
		ra := &RunningAgent{Agent: agent, State: state}

		err := eng.RunCycle(context.Background(), ra)
		if err != nil {
			rt.Fatalf("RunCycle error: %v", err)
		}

		// Verify an audit log (agent_cycle) was created.
		if len(mockStore.cycles) == 0 {
			rt.Fatal("no agent cycle log was created")
		}

		cycle := mockStore.cycles[len(mockStore.cycles)-1]

		// Agent ID must match.
		if cycle.AgentID != agent.ID {
			rt.Fatalf("cycle AgentID %v != agent ID %v", cycle.AgentID, agent.ID)
		}

		// Cycle number must be positive (incremented from 0).
		if cycle.CycleNum <= 0 {
			rt.Fatalf("cycle number should be positive, got %d", cycle.CycleNum)
		}

		// Prompt must be present and non-empty.
		if !cycle.LlmPrompt.Valid || cycle.LlmPrompt.String == "" {
			rt.Fatal("audit log must contain the LLM prompt")
		}

		// Response must be present.
		if !cycle.LlmResponse.Valid || cycle.LlmResponse.String == "" {
			rt.Fatal("audit log must contain the LLM response")
		}
		if cycle.LlmResponse.String != llmResponseContent {
			rt.Fatalf("audit log response %q != expected %q", cycle.LlmResponse.String, llmResponseContent)
		}

		// FinishedAt must be set.
		if !cycle.FinishedAt.Valid {
			rt.Fatal("audit log must have a finished_at timestamp")
		}

		// DurationMs must be non-negative.
		if cycle.DurationMs.Valid && cycle.DurationMs.Int32 < 0 {
			rt.Fatalf("duration_ms should be non-negative, got %d", cycle.DurationMs.Int32)
		}

		// Agent state must also have been persisted.
		if len(mockStore.stateUpdates) == 0 {
			rt.Fatal("agent state was not persisted after cycle")
		}
		stateUpdate := mockStore.stateUpdates[len(mockStore.stateUpdates)-1]
		if stateUpdate.ID != agent.ID {
			rt.Fatalf("state update AgentID %v != agent ID %v", stateUpdate.ID, agent.ID)
		}
	})
}

// Ensure pgtype is used (avoid unused import).
var _ = pgtype.Text{}
