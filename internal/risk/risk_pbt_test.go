package risk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/agentfi/agentfi-go-backend/internal/store"
)

// --- Generators ---

// genTradeRequest generates a random TradeRequest with positive amounts.
func genTradeRequest() *rapid.Generator[TradeRequest] {
	return rapid.Custom(func(t *rapid.T) TradeRequest {
		return TradeRequest{
			AgentID:  uuid.New(),
			Action:   rapid.SampledFrom([]string{"BUY", "SELL"}).Draw(t, "action"),
			Pair:     rapid.SampledFrom([]string{"BTC-PERP", "ETH-PERP", "SOL-PERP"}).Draw(t, "pair"),
			Amount:   rapid.Int64Range(1, 1_000_000_00).Draw(t, "amount"),
			Price:    rapid.Int64Range(1, 100_000_00).Draw(t, "price"),
			CycleNum: rapid.Int64Range(1, 10000).Draw(t, "cycle_num"),
		}
	})
}

// genRiskParams generates random risk parameters with positive limits.
func genRiskParams() *rapid.Generator[RiskParams] {
	return rapid.Custom(func(t *rapid.T) RiskParams {
		return RiskParams{
			MaxTradeAmount:   rapid.Int64Range(100, 500_000_00).Draw(t, "max_trade"),
			MaxDailyLoss:     rapid.Int64Range(100, 1_000_000_00).Draw(t, "max_daily_loss"),
			MaxPositionRatio: rapid.Float64Range(0.01, 1.0).Draw(t, "max_pos_ratio"),
			MaxTradesPerHour: rapid.IntRange(1, 100).Draw(t, "max_trades_hr"),
		}
	})
}

// --- Mock logger for Property 15 ---

type capturedEvent struct {
	Params store.CreateRiskEventParams
}

type mockRiskEventLogger struct {
	events []capturedEvent
}

func (m *mockRiskEventLogger) CreateRiskEvent(_ context.Context, arg store.CreateRiskEventParams) (store.RiskEvent, error) {
	m.events = append(m.events, capturedEvent{Params: arg})
	return store.RiskEvent{
		ID:        uuid.New(),
		AgentID:   arg.AgentID,
		EventType: arg.EventType,
		Details:   arg.Details,
	}, nil
}

// Feature: agentfi-go-backend, Property 14: Risk controller rejects limit violations
// For any trade where the amount exceeds the single-trade limit, OR the cumulative
// daily loss exceeds the daily limit, OR the position would exceed the max ratio,
// OR the hourly trade count exceeds the max, the Risk Controller SHALL reject the trade.
// Validates: Requirements 6.2, 6.3, 6.4, 6.5
func TestPropertyRiskControllerRejectsLimitViolations(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		params := genRiskParams().Draw(rt, "params")

		// Pick which rule to violate.
		violation := rapid.SampledFrom([]string{
			"max_trade_amount",
			"daily_loss",
			"position_ratio",
			"trade_frequency",
		}).Draw(rt, "violation")

		trade := genTradeRequest().Draw(rt, "trade")

		// Start with a clean state that would normally pass all rules.
		state := AgentState{
			CumulativeDailyLoss: 0,
			HourlyTradeCount:    0,
			CurrentPositionAmt:  0,
			AUM:                 10_000_000_00, // large AUM so position ratio is small
		}

		// Ensure the trade amount is within limits by default.
		if trade.Amount > params.MaxTradeAmount {
			trade.Amount = params.MaxTradeAmount - 1
		}
		if trade.Amount <= 0 {
			trade.Amount = 1
		}

		switch violation {
		case "max_trade_amount":
			// Make trade exceed single-trade limit (Req 6.2).
			excess := rapid.Int64Range(1, 1_000_000).Draw(rt, "excess")
			trade.Amount = params.MaxTradeAmount + excess

		case "daily_loss":
			// Set cumulative daily loss at or above the limit (Req 6.3).
			state.CumulativeDailyLoss = params.MaxDailyLoss + rapid.Int64Range(0, 1_000_000).Draw(rt, "over")

		case "position_ratio":
			// Set current position so adding trade.Amount exceeds ratio (Req 6.4).
			// newPosition / AUM > MaxPositionRatio
			// We need: currentPos + trade.Amount > MaxPositionRatio * AUM
			threshold := int64(params.MaxPositionRatio * float64(state.AUM))
			state.CurrentPositionAmt = threshold // already at limit
			trade.Amount = rapid.Int64Range(1, 1_000_000).Draw(rt, "push_over")
			// Ensure trade amount doesn't trigger max_trade_amount rule first.
			if trade.Amount > params.MaxTradeAmount {
				trade.Amount = params.MaxTradeAmount
			}

		case "trade_frequency":
			// Set hourly count at or above the limit (Req 6.5).
			state.HourlyTradeCount = params.MaxTradesPerHour + rapid.IntRange(0, 50).Draw(rt, "over")
		}

		decision := ValidatePure(trade, params, state)

		if decision.Approved {
			rt.Fatalf("expected rejection for %s violation, but trade was approved.\n"+
				"trade=%+v\nparams=%+v\nstate=%+v", violation, trade, params, state)
		}

		if decision.Reason == "" {
			rt.Fatal("rejected decision must have a non-empty reason")
		}

		if len(decision.Rules) == 0 {
			rt.Fatal("rejected decision must list at least one triggered rule")
		}

		// Daily loss violation must signal PauseAgent (Req 6.3).
		if violation == "daily_loss" && !decision.PauseAgent {
			rt.Fatal("daily_loss violation must set PauseAgent=true")
		}

		// Non-daily-loss violations must NOT signal PauseAgent.
		if violation != "daily_loss" && decision.PauseAgent {
			rt.Fatalf("%s violation should not set PauseAgent", violation)
		}
	})
}

// Feature: agentfi-go-backend, Property 15: Risk decision audit logging
// For any trade submitted to the Risk Controller (approved or rejected), a log
// entry SHALL be created containing the timestamp, Agent ID, trade parameters,
// and decision reason.
// Validates: Requirements 6.6
func TestPropertyRiskDecisionAuditLogging(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		mock := &mockRiskEventLogger{}
		ctrl := NewController(mock)

		params := genRiskParams().Draw(rt, "params")
		trade := genTradeRequest().Draw(rt, "trade")

		// Randomly decide whether the trade should pass or fail.
		shouldFail := rapid.Bool().Draw(rt, "should_fail")

		state := AgentState{
			CumulativeDailyLoss: 0,
			HourlyTradeCount:    0,
			CurrentPositionAmt:  0,
			AUM:                 10_000_000_00,
		}

		if shouldFail {
			// Force a max_trade_amount violation.
			trade.Amount = params.MaxTradeAmount + rapid.Int64Range(1, 1_000_000).Draw(rt, "excess")
		} else {
			// Ensure trade passes all rules.
			if trade.Amount > params.MaxTradeAmount {
				trade.Amount = params.MaxTradeAmount - 1
			}
			if trade.Amount <= 0 {
				trade.Amount = 1
			}
		}

		decision, err := ctrl.Validate(context.Background(), trade, params, state)
		if err != nil {
			rt.Fatalf("Validate returned error: %v", err)
		}

		// Exactly one risk event must be logged per Validate call.
		if len(mock.events) != 1 {
			rt.Fatalf("expected 1 logged event, got %d", len(mock.events))
		}

		evt := mock.events[0].Params

		// Agent ID must match.
		if evt.AgentID != trade.AgentID {
			rt.Fatalf("logged AgentID %v != trade AgentID %v", evt.AgentID, trade.AgentID)
		}

		// Event type must reflect the decision.
		if decision.Approved && evt.EventType != "trade_approved" {
			rt.Fatalf("approved trade should log event_type=trade_approved, got %q", evt.EventType)
		}
		if !decision.Approved && !decision.PauseAgent && evt.EventType != "trade_rejected" {
			rt.Fatalf("rejected trade should log event_type=trade_rejected, got %q", evt.EventType)
		}

		// Details must contain trade parameters and decision.
		var details riskEventDetails
		if err := json.Unmarshal(evt.Details, &details); err != nil {
			rt.Fatalf("failed to unmarshal logged details: %v", err)
		}

		if details.Trade.AgentID != trade.AgentID {
			rt.Fatalf("logged trade AgentID mismatch")
		}
		if details.Trade.Amount != trade.Amount {
			rt.Fatalf("logged trade Amount %d != %d", details.Trade.Amount, trade.Amount)
		}
		if details.Trade.Action != trade.Action {
			rt.Fatalf("logged trade Action %q != %q", details.Trade.Action, trade.Action)
		}
		if details.Trade.Pair != trade.Pair {
			rt.Fatalf("logged trade Pair %q != %q", details.Trade.Pair, trade.Pair)
		}
		if details.Decision == nil {
			rt.Fatal("logged details must contain the decision")
		}
		if details.Decision.Approved != decision.Approved {
			rt.Fatalf("logged decision.Approved %v != %v", details.Decision.Approved, decision.Approved)
		}
		if details.Time.IsZero() {
			rt.Fatal("logged details must contain a non-zero timestamp")
		}
	})
}
