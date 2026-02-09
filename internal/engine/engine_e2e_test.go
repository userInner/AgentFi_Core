package engine

// End-to-end integration test for the Agent Engine.
// Uses mock LLM + mock MCP to exercise the complete Agent Loop:
//   load state → Fast Path → LLM → tool_call routing → risk control → execute → persist
// Checkpoint task 12: Agent Engine 端到端验证

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/agentfi/agentfi-go-backend/internal/llm"
	"github.com/agentfi/agentfi-go-backend/internal/mcp"
	"github.com/agentfi/agentfi-go-backend/internal/risk"
	"github.com/agentfi/agentfi-go-backend/internal/store"
	"github.com/agentfi/agentfi-go-backend/pkg/config"
)

// TestE2EAgentLoopFullCycle exercises the complete Agent Loop end-to-end:
// 1. Agent wakes up
// 2. Fast Path is not parseable → falls through to LLM
// 3. LLM requests a tool call (get_price)
// 4. MCP returns price data
// 5. LLM receives tool result, decides to trade (execute_trade)
// 6. Risk controller approves the trade
// 7. Trade is logged, state is persisted, cycle audit log is written
func TestE2EAgentLoopFullCycle(t *testing.T) {
	agent := makeTestAgent("Buy BTC when momentum is strong", "BTC-PERP", 120)
	serverID := uuid.New()

	tools := []mcp.ToolDefinition{
		{Name: "get_price", Description: "Get current price", Parameters: map[string]any{"type": "object"}},
	}
	toolsJSON, _ := json.Marshal(tools)

	mockStore := newMockEngineStore()
	mockStore.agents[agent.ID] = agent
	mockStore.mcpServers[agent.ID] = []store.McpServer{
		{ID: serverID, Name: "market-data", Url: "http://localhost:9000", Type: "http", Tools: toolsJSON},
	}

	// LLM sequence:
	// Call 1: returns tool_call for get_price
	// Call 2: receives price data, returns tool_call for execute_trade
	// Call 3: receives trade result, returns final text
	seqLLM := &sequenceLLMClient{
		responses: []*llm.ChatResponse{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc_1", Name: "get_price", Arguments: map[string]any{"symbol": "BTC"}},
				},
				Usage: llm.TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
			},
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc_2", Name: "execute_trade", Arguments: map[string]any{
						"action": "BUY",
						"pair":   "BTC-PERP",
						"amount": float64(50000),
						"price":  float64(4200000),
					}},
				},
				Usage: llm.TokenUsage{PromptTokens: 200, CompletionTokens: 30, TotalTokens: 230},
			},
			{
				Content: "Bought BTC-PERP at 42000.00. Position updated.",
				Usage:   llm.TokenUsage{PromptTokens: 300, CompletionTokens: 20, TotalTokens: 320},
			},
		},
	}

	mockMCP := newMockMCPToolCaller()
	mockMCP.toolDefs[serverID] = tools
	mockMCP.callResults["get_price"] = map[string]any{"value": float64(42000), "symbol": "BTC"}

	mockRisk := &mockRiskValidator{}

	eng := NewEngine(mockStore, seqLLM, mockMCP, mockRisk, config.LLMConfig{
		Model: "gpt-4o", Temperature: 0.7, MaxTokens: 4096,
	})

	state := &AgentState{Holdings: make(map[string]int64)}
	ra := &RunningAgent{Agent: agent, State: state}

	err := eng.RunCycle(context.Background(), ra)
	if err != nil {
		t.Fatalf("RunCycle failed: %v", err)
	}

	// --- Verify the full flow ---

	// 1. LLM was called 3 times (get_price tool_call → execute_trade tool_call → final)
	if seqLLM.callIdx != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", seqLLM.callIdx)
	}

	// 2. MCP get_price was called with correct server
	if len(mockMCP.callLog) == 0 {
		t.Fatal("MCP was never called")
	}
	foundGetPrice := false
	for _, call := range mockMCP.callLog {
		if call.ToolName == "get_price" && call.ServerID == serverID {
			foundGetPrice = true
		}
	}
	if !foundGetPrice {
		t.Fatal("get_price was not routed to the correct MCP server")
	}

	// 3. Risk controller was called for the execute_trade
	if len(mockRisk.calls) != 1 {
		t.Fatalf("expected 1 risk validation call, got %d", len(mockRisk.calls))
	}
	tradeReq := mockRisk.calls[0]
	if tradeReq.Action != "BUY" {
		t.Fatalf("expected BUY trade, got %s", tradeReq.Action)
	}
	if tradeReq.Amount != 50000 {
		t.Fatalf("expected trade amount 50000, got %d", tradeReq.Amount)
	}

	// 4. Trade log was persisted
	if len(mockStore.tradeLogs) != 1 {
		t.Fatalf("expected 1 trade log, got %d", len(mockStore.tradeLogs))
	}
	tl := mockStore.tradeLogs[0]
	if tl.AgentID != agent.ID || tl.Action != "BUY" || !tl.RiskApproved {
		t.Fatalf("trade log mismatch: agent=%v action=%s approved=%v", tl.AgentID, tl.Action, tl.RiskApproved)
	}

	// 5. Agent cycle audit log was persisted (Req 7.4)
	if len(mockStore.cycles) != 1 {
		t.Fatalf("expected 1 cycle log, got %d", len(mockStore.cycles))
	}
	cycle := mockStore.cycles[0]
	if cycle.AgentID != agent.ID {
		t.Fatalf("cycle agent ID mismatch")
	}
	if cycle.CycleNum != 1 {
		t.Fatalf("expected cycle_num 1, got %d", cycle.CycleNum)
	}
	if !cycle.LlmPrompt.Valid || cycle.LlmPrompt.String == "" {
		t.Fatal("cycle audit log missing LLM prompt")
	}
	if !cycle.LlmResponse.Valid {
		t.Fatal("cycle audit log missing LLM response")
	}

	// 6. Agent state was persisted (Req 7.1)
	if len(mockStore.stateUpdates) != 1 {
		t.Fatalf("expected 1 state update, got %d", len(mockStore.stateUpdates))
	}
	if mockStore.stateUpdates[0].ID != agent.ID {
		t.Fatal("state update agent ID mismatch")
	}

	// 7. In-memory state was updated with the trade
	if state.Holdings["BTC-PERP"] != 50000 {
		t.Fatalf("expected holdings BTC-PERP=50000, got %d", state.Holdings["BTC-PERP"])
	}
	if state.DailyTradeCount != 1 {
		t.Fatalf("expected daily trade count 1, got %d", state.DailyTradeCount)
	}
	if state.CycleNumber != 1 {
		t.Fatalf("expected cycle number 1, got %d", state.CycleNumber)
	}
}


// TestE2EAgentLoopFastPathSkip verifies that when a Fast Path condition is
// not met, the LLM is never called and the cycle completes cleanly.
func TestE2EAgentLoopFastPathSkip(t *testing.T) {
	// Strategy with a parseable condition: RSI < 30
	agent := makeTestAgent("Buy when RSI < 30", "BTC-PERP", 120)
	serverID := uuid.New()

	tools := []mcp.ToolDefinition{
		{Name: "get_rsi", Description: "Get RSI value", Parameters: map[string]any{"type": "object"}},
	}
	toolsJSON, _ := json.Marshal(tools)

	mockStore := newMockEngineStore()
	mockStore.agents[agent.ID] = agent
	mockStore.mcpServers[agent.ID] = []store.McpServer{
		{ID: serverID, Name: "indicators", Url: "http://localhost:9001", Type: "http", Tools: toolsJSON},
	}

	// LLM should NOT be called at all.
	mockLLM := &mockLLMClient{
		response: &llm.ChatResponse{Content: "should not be called"},
	}

	mockMCP := newMockMCPToolCaller()
	mockMCP.toolDefs[serverID] = tools
	// RSI = 55 → condition "RSI < 30" is NOT met → skip cycle
	mockMCP.callResults["get_rsi"] = map[string]any{"value": float64(55)}

	mockRisk := &mockRiskValidator{}

	eng := NewEngine(mockStore, mockLLM, mockMCP, mockRisk, config.LLMConfig{
		Model: "gpt-4o", Temperature: 0.7, MaxTokens: 4096,
	})

	state := &AgentState{Holdings: make(map[string]int64)}
	ra := &RunningAgent{Agent: agent, State: state}

	err := eng.RunCycle(context.Background(), ra)
	if err != nil {
		t.Fatalf("RunCycle failed: %v", err)
	}

	// LLM should not have been called (Fast Path condition not met → skip).
	if mockLLM.callCount > 0 {
		t.Fatalf("LLM was called %d times, expected 0 (fast path skip)", mockLLM.callCount)
	}

	// Cycle audit log should still be persisted.
	if len(mockStore.cycles) != 1 {
		t.Fatalf("expected 1 cycle log, got %d", len(mockStore.cycles))
	}
	if !mockStore.cycles[0].FastPathHit {
		t.Fatal("expected fast_path_hit=true in cycle log")
	}

	// No trades should have occurred.
	if len(mockStore.tradeLogs) != 0 {
		t.Fatalf("expected 0 trade logs, got %d", len(mockStore.tradeLogs))
	}
}

// TestE2EAgentLoopRiskRejection verifies that when the risk controller rejects
// a trade, the trade is not executed but the rejection is logged.
func TestE2EAgentLoopRiskRejection(t *testing.T) {
	agent := makeTestAgent("Aggressive BTC scalping", "BTC-PERP", 120)
	serverID := uuid.New()

	tools := []mcp.ToolDefinition{
		{Name: "get_price", Description: "Get price", Parameters: map[string]any{"type": "object"}},
	}
	toolsJSON, _ := json.Marshal(tools)

	mockStore := newMockEngineStore()
	mockStore.agents[agent.ID] = agent
	mockStore.mcpServers[agent.ID] = []store.McpServer{
		{ID: serverID, Name: "market-data", Url: "http://localhost:9000", Type: "http", Tools: toolsJSON},
	}

	// LLM immediately requests a trade.
	seqLLM := &sequenceLLMClient{
		responses: []*llm.ChatResponse{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc_1", Name: "execute_trade", Arguments: map[string]any{
						"action": "BUY",
						"pair":   "BTC-PERP",
						"amount": float64(999999999), // huge amount
						"price":  float64(4200000),
					}},
				},
				Usage: llm.TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
			},
			{
				Content: "Trade was rejected by risk controller. Standing down.",
				Usage:   llm.TokenUsage{PromptTokens: 200, CompletionTokens: 15, TotalTokens: 215},
			},
		},
	}

	mockMCP := newMockMCPToolCaller()
	mockMCP.toolDefs[serverID] = tools

	// Risk controller that rejects the trade.
	rejectingRisk := &rejectingRiskValidator{reason: "trade_amount_exceeded: 999999999 > limit 10000000"}

	eng := NewEngine(mockStore, seqLLM, mockMCP, rejectingRisk, config.LLMConfig{
		Model: "gpt-4o", Temperature: 0.7, MaxTokens: 4096,
	})

	state := &AgentState{Holdings: make(map[string]int64)}
	ra := &RunningAgent{Agent: agent, State: state}

	err := eng.RunCycle(context.Background(), ra)
	if err != nil {
		t.Fatalf("RunCycle failed: %v", err)
	}

	// Trade log should show risk_approved=false.
	if len(mockStore.tradeLogs) != 1 {
		t.Fatalf("expected 1 trade log, got %d", len(mockStore.tradeLogs))
	}
	if mockStore.tradeLogs[0].RiskApproved {
		t.Fatal("expected trade to be rejected (risk_approved=false)")
	}

	// Holdings should NOT have changed.
	if state.Holdings["BTC-PERP"] != 0 {
		t.Fatalf("expected no position change after rejection, got %d", state.Holdings["BTC-PERP"])
	}

	// Cycle log should still be persisted.
	if len(mockStore.cycles) != 1 {
		t.Fatalf("expected 1 cycle log, got %d", len(mockStore.cycles))
	}
}

// rejectingRiskValidator always rejects trades.
type rejectingRiskValidator struct {
	reason string
	calls  []risk.TradeRequest
}

func (r *rejectingRiskValidator) Validate(_ context.Context, trade risk.TradeRequest, _ risk.RiskParams, _ risk.AgentState) (*risk.RiskDecision, error) {
	r.calls = append(r.calls, trade)
	return &risk.RiskDecision{
		Approved: false,
		Reason:   r.reason,
		Rules:    []string{r.reason},
	}, nil
}
