// Package risk provides the risk controller for trade validation.
// It implements a rule chain that validates trades against per-agent
// risk parameters before execution (Requirements 6.1–6.6).
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/agentfi/agentfi-go-backend/internal/store"
)

// TradeRequest represents a trade to be validated by the risk controller.
type TradeRequest struct {
	AgentID  uuid.UUID `json:"agent_id"`
	Action   string    `json:"action"` // "BUY" | "SELL"
	Pair     string    `json:"pair"`
	Amount   int64     `json:"amount"` // cents
	Price    int64     `json:"price"`  // cents
	CycleNum int64     `json:"cycle_num"`
}

// RiskDecision is the outcome of risk validation.
type RiskDecision struct {
	Approved   bool     `json:"approved"`
	Reason     string   `json:"reason"`
	Rules      []string `json:"rules"`
	PauseAgent bool     `json:"pause_agent"`
}

// RiskParams holds per-agent risk control parameters.
type RiskParams struct {
	MaxTradeAmount   int64   `json:"max_trade_amount"`
	MaxDailyLoss     int64   `json:"max_daily_loss"`
	MaxPositionRatio float64 `json:"max_position_ratio"`
	MaxTradesPerHour int     `json:"max_trades_per_hour"`
}

// AgentState holds the runtime state needed for risk evaluation.
type AgentState struct {
	CumulativeDailyLoss int64 `json:"daily_loss"`
	HourlyTradeCount    int   `json:"hourly_trade_count"`
	CurrentPositionAmt  int64 `json:"current_position_amt"` // current position value in cents
	AUM                 int64 `json:"aum"`                  // total assets under management in cents
}

// RiskEventLogger abstracts the database call for writing risk events,
// enabling the controller to be tested without a real database.
type RiskEventLogger interface {
	CreateRiskEvent(ctx context.Context, arg store.CreateRiskEventParams) (store.RiskEvent, error)
}

// Controller validates trades against a chain of risk rules.
// Each rule is an independent function; they execute sequentially.
// Every decision (approved or rejected) is persisted as a risk_event (Req 6.6).
type Controller struct {
	logger RiskEventLogger
}

// NewController creates a new risk controller.
func NewController(logger RiskEventLogger) *Controller {
	return &Controller{logger: logger}
}

// ruleFunc is the signature for an individual risk rule.
// It returns ("", false) when the rule passes, or (reason, pauseAgent) on violation.
type ruleFunc func(trade TradeRequest, params RiskParams, state AgentState) (reason string, pauseAgent bool)

// rules returns the ordered chain of risk rules.
func rules() []ruleFunc {
	return []ruleFunc{
		checkMaxTradeAmount,
		checkDailyLoss,
		checkPositionRatio,
		checkTradeFrequency,
	}
}

// Validate runs the trade through the risk rule chain and logs the decision.
// Requirements: 6.1, 6.2, 6.3, 6.4, 6.5, 6.6
func (c *Controller) Validate(
	ctx context.Context,
	trade TradeRequest,
	params RiskParams,
	state AgentState,
) (*RiskDecision, error) {
	decision := &RiskDecision{Approved: true}

	for _, rule := range rules() {
		reason, pause := rule(trade, params, state)
		if reason != "" {
			decision.Approved = false
			decision.Reason = reason
			decision.PauseAgent = pause
			decision.Rules = append(decision.Rules, reason)
			break // first failing rule rejects the trade
		}
	}

	// Log every decision to risk_events (Req 6.6).
	if err := c.logDecision(ctx, trade, decision); err != nil {
		return decision, fmt.Errorf("risk: log decision: %w", err)
	}

	return decision, nil
}

// --- Individual risk rules ---

// checkMaxTradeAmount rejects trades exceeding the single-trade limit (Req 6.2).
func checkMaxTradeAmount(trade TradeRequest, params RiskParams, _ AgentState) (string, bool) {
	if params.MaxTradeAmount > 0 && trade.Amount > params.MaxTradeAmount {
		return fmt.Sprintf("trade_amount_exceeded: %d > limit %d", trade.Amount, params.MaxTradeAmount), false
	}
	return "", false
}

// checkDailyLoss rejects trades when cumulative daily loss exceeds the limit
// and signals that the agent should be paused (Req 6.3).
func checkDailyLoss(_ TradeRequest, params RiskParams, state AgentState) (string, bool) {
	if params.MaxDailyLoss > 0 && state.CumulativeDailyLoss >= params.MaxDailyLoss {
		return fmt.Sprintf("daily_loss_exceeded: %d >= limit %d", state.CumulativeDailyLoss, params.MaxDailyLoss), true
	}
	return "", false
}

// checkPositionRatio rejects trades that would push the position beyond the
// maximum allowed ratio of AUM (Req 6.4).
func checkPositionRatio(trade TradeRequest, params RiskParams, state AgentState) (string, bool) {
	if params.MaxPositionRatio <= 0 || state.AUM <= 0 {
		return "", false
	}
	newPosition := state.CurrentPositionAmt + trade.Amount
	ratio := float64(newPosition) / float64(state.AUM)
	if ratio > params.MaxPositionRatio {
		return fmt.Sprintf("position_ratio_exceeded: %.4f > limit %.4f", ratio, params.MaxPositionRatio), false
	}
	return "", false
}

// checkTradeFrequency rejects trades when the hourly trade count exceeds the
// configured maximum (Req 6.5).
func checkTradeFrequency(_ TradeRequest, params RiskParams, state AgentState) (string, bool) {
	if params.MaxTradesPerHour > 0 && state.HourlyTradeCount >= params.MaxTradesPerHour {
		return fmt.Sprintf("rate_limit_exceeded: %d >= limit %d trades/hour", state.HourlyTradeCount, params.MaxTradesPerHour), false
	}
	return "", false
}

// --- Logging ---

// riskEventDetails is the JSON payload stored in risk_events.details.
type riskEventDetails struct {
	Trade    TradeRequest  `json:"trade"`
	Decision *RiskDecision `json:"decision"`
	Time     time.Time     `json:"time"`
}

// logDecision persists the risk decision to the risk_events table.
func (c *Controller) logDecision(ctx context.Context, trade TradeRequest, decision *RiskDecision) error {
	eventType := "trade_approved"
	if !decision.Approved {
		eventType = "trade_rejected"
		if decision.PauseAgent {
			eventType = "agent_paused"
		}
	}

	details, err := json.Marshal(riskEventDetails{
		Trade:    trade,
		Decision: decision,
		Time:     time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("marshal details: %w", err)
	}

	_, err = c.logger.CreateRiskEvent(ctx, store.CreateRiskEventParams{
		AgentID:   trade.AgentID,
		EventType: eventType,
		Details:   details,
	})
	return err
}

// ValidatePure runs the risk rule chain without logging.
// Useful for testing the pure rule logic in isolation.
func ValidatePure(trade TradeRequest, params RiskParams, state AgentState) *RiskDecision {
	decision := &RiskDecision{Approved: true}
	for _, rule := range rules() {
		reason, pause := rule(trade, params, state)
		if reason != "" {
			decision.Approved = false
			decision.Reason = reason
			decision.PauseAgent = pause
			decision.Rules = append(decision.Rules, reason)
			break
		}
	}
	return decision
}
