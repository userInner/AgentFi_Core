// Package invest provides investment deposit, redemption, and profit sharing.
package invest

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentfi/agentfi-go-backend/internal/store"
)

// Errors returned by the investment service.
var (
	ErrNotFound       = errors.New("invest: not found")
	ErrAlreadyRedeemed = errors.New("invest: already redeemed")
	ErrInvalidAmount  = errors.New("invest: invalid amount")
	ErrAgentNotFound  = errors.New("invest: agent not found")
)

// Investment is the API-facing representation of an investment record.
type Investment struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"user_id"`
	AgentID    uuid.UUID `json:"agent_id"`
	Amount     int64     `json:"amount"`
	ShareRatio string    `json:"share_ratio"`
	Status     string    `json:"status"`
	CreatedAt  string    `json:"created_at"`
}

// Redemption holds the result of a redeem operation.
type Redemption struct {
	InvestmentID uuid.UUID `json:"investment_id"`
	ReturnedValue int64   `json:"returned_value"`
	ProfitShare   int64   `json:"profit_share"`
}

// InvestmentSummary is a portfolio entry for GET /api/portfolio.
type InvestmentSummary struct {
	InvestmentID uuid.UUID `json:"investment_id"`
	AgentID      uuid.UUID `json:"agent_id"`
	AgentName    string    `json:"agent_name"`
	Deposited    int64     `json:"deposited"`
	CurrentValue int64     `json:"current_value"`
	ShareRatio   string    `json:"share_ratio"`
	Status       string    `json:"status"`
}

// Service provides investment deposit, redemption, and profit sharing.
type Service struct {
	store *store.Store
	// profitSharePct is the trader's profit share percentage (0-100).
	// For simplicity, we use a fixed 20% default. In production this would
	// be per-agent configurable.
	profitSharePct int64
}

// NewService creates a new investment service.
func NewService(s *store.Store) *Service {
	return &Service{store: s, profitSharePct: 20}
}

// Deposit records an investment, updates the Agent's AUM and investor count,
// calculates the share ratio, and writes a ledger entry.
// All operations run inside a single database transaction (Req 12.2).
func (s *Service) Deposit(ctx context.Context, userAddr string, agentID uuid.UUID, amount int64) (*Investment, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", ErrInvalidAmount)
	}

	// Ensure user exists.
	user, err := s.store.CreateUser(ctx, userAddr)
	if err != nil {
		return nil, fmt.Errorf("invest: ensure user: %w", err)
	}

	// Verify agent exists.
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAgentNotFound
		}
		return nil, fmt.Errorf("invest: get agent: %w", err)
	}

	// Calculate share ratio: amount / (aum + amount).
	newAUM := agent.Aum + amount
	shareRatio := new(big.Rat).SetFrac(
		new(big.Int).SetInt64(amount),
		new(big.Int).SetInt64(newAUM),
	)

	// Convert to pgtype.Numeric for storage.
	shareNumeric := ratToNumeric(shareRatio)

	var inv store.Investment
	err = s.store.Tx(ctx, func(q *store.Queries) error {
		var txErr error

		// 1. Create investment record.
		inv, txErr = q.CreateInvestment(ctx, store.CreateInvestmentParams{
			UserID:     user.ID,
			AgentID:    agentID,
			Amount:     amount,
			ShareRatio: shareNumeric,
		})
		if txErr != nil {
			return fmt.Errorf("create investment: %w", txErr)
		}

		// 2. Update Agent AUM and investor count.
		txErr = q.UpdateAgentAUM(ctx, store.UpdateAgentAUMParams{
			ID:            agentID,
			Aum:           newAUM,
			InvestorCount: agent.InvestorCount + 1,
		})
		if txErr != nil {
			return fmt.Errorf("update agent aum: %w", txErr)
		}

		// 3. Write ledger entry (deposit = positive amount).
		_, txErr = q.CreateLedgerEntry(ctx, store.CreateLedgerEntryParams{
			UserID:  user.ID,
			AgentID: agentID,
			Type:    "deposit",
			Amount:  amount,
			Balance: amount, // new balance for this user-agent pair
		})
		if txErr != nil {
			return fmt.Errorf("create ledger entry: %w", txErr)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("invest: deposit: %w", err)
	}

	return &Investment{
		ID:         inv.ID,
		UserID:     inv.UserID,
		AgentID:    inv.AgentID,
		Amount:     inv.Amount,
		ShareRatio: numericToString(inv.ShareRatio),
		Status:     inv.Status,
		CreatedAt:  inv.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}, nil
}

// Redeem calculates the investor's current value based on share ratio,
// updates AUM, writes a ledger entry, and marks the investment as redeemed.
// All operations run inside a single database transaction (Req 12.2).
func (s *Service) Redeem(ctx context.Context, userAddr string, investmentID uuid.UUID) (*Redemption, error) {
	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("invest: get user: %w", err)
	}

	inv, err := s.store.GetInvestment(ctx, investmentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("invest: get investment: %w", err)
	}

	if inv.UserID != user.ID {
		return nil, ErrNotFound // don't leak existence
	}
	if inv.Status != "active" {
		return nil, ErrAlreadyRedeemed
	}

	agent, err := s.store.GetAgent(ctx, inv.AgentID)
	if err != nil {
		return nil, fmt.Errorf("invest: get agent: %w", err)
	}

	// Calculate current value: share_ratio * current AUM.
	shareRat := numericToRat(inv.ShareRatio)
	currentValue := new(big.Rat).Mul(shareRat, new(big.Rat).SetInt64(agent.Aum))
	returnedValue := ratToInt64(currentValue)

	// Calculate profit share: only on positive returns (Req 8.3).
	var profitShareAmount int64
	profit := returnedValue - inv.Amount
	if profit > 0 {
		profitShareAmount = profit * s.profitSharePct / 100
		returnedValue -= profitShareAmount
	}

	// New AUM after redemption.
	newAUM := agent.Aum - returnedValue
	if profitShareAmount > 0 {
		newAUM -= profitShareAmount
	}
	if newAUM < 0 {
		newAUM = 0
	}

	newInvestorCount := agent.InvestorCount - 1
	if newInvestorCount < 0 {
		newInvestorCount = 0
	}

	err = s.store.Tx(ctx, func(q *store.Queries) error {
		// 1. Mark investment as redeemed.
		_, txErr := q.RedeemInvestment(ctx, investmentID)
		if txErr != nil {
			return fmt.Errorf("redeem investment: %w", txErr)
		}

		// 2. Update Agent AUM.
		txErr = q.UpdateAgentAUM(ctx, store.UpdateAgentAUMParams{
			ID:            inv.AgentID,
			Aum:           newAUM,
			InvestorCount: newInvestorCount,
		})
		if txErr != nil {
			return fmt.Errorf("update agent aum: %w", txErr)
		}

		// 3. Write withdrawal ledger entry.
		_, txErr = q.CreateLedgerEntry(ctx, store.CreateLedgerEntryParams{
			UserID:  user.ID,
			AgentID: inv.AgentID,
			Type:    "withdrawal",
			Amount:  -returnedValue,
			Balance: 0,
		})
		if txErr != nil {
			return fmt.Errorf("create withdrawal ledger: %w", txErr)
		}

		// 4. Write profit share ledger entry if applicable.
		if profitShareAmount > 0 {
			_, txErr = q.CreateLedgerEntry(ctx, store.CreateLedgerEntryParams{
				UserID:  user.ID,
				AgentID: inv.AgentID,
				Type:    "profit_share",
				Amount:  -profitShareAmount,
				Balance: 0,
			})
			if txErr != nil {
				return fmt.Errorf("create profit share ledger: %w", txErr)
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("invest: redeem: %w", err)
	}

	return &Redemption{
		InvestmentID:  investmentID,
		ReturnedValue: returnedValue,
		ProfitShare:   profitShareAmount,
	}, nil
}

// GetPortfolio returns all active investments for a user with current values.
func (s *Service) GetPortfolio(ctx context.Context, userAddr string) ([]InvestmentSummary, error) {
	user, err := s.store.GetUserByAddress(ctx, userAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []InvestmentSummary{}, nil
		}
		return nil, fmt.Errorf("invest: get user: %w", err)
	}

	investments, err := s.store.ListInvestmentsByUser(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("invest: list investments: %w", err)
	}

	results := make([]InvestmentSummary, 0, len(investments))
	for _, inv := range investments {
		agent, err := s.store.GetAgent(ctx, inv.AgentID)
		if err != nil {
			continue // skip if agent was deleted
		}

		shareRat := numericToRat(inv.ShareRatio)
		currentValue := new(big.Rat).Mul(shareRat, new(big.Rat).SetInt64(agent.Aum))

		results = append(results, InvestmentSummary{
			InvestmentID: inv.ID,
			AgentID:      inv.AgentID,
			AgentName:    agent.Name,
			Deposited:    inv.Amount,
			CurrentValue: ratToInt64(currentValue),
			ShareRatio:   numericToString(inv.ShareRatio),
			Status:       inv.Status,
		})
	}

	return results, nil
}

// CalculateProfitShare calculates and distributes profit shares for all
// active investments in an agent. Only positive returns trigger profit sharing (Req 8.3).
func (s *Service) CalculateProfitShare(ctx context.Context, agentID uuid.UUID) error {
	investments, err := s.store.ListInvestmentsByAgent(ctx, agentID)
	if err != nil {
		return fmt.Errorf("invest: list investments: %w", err)
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return fmt.Errorf("invest: get agent: %w", err)
	}

	for _, inv := range investments {
		shareRat := numericToRat(inv.ShareRatio)
		currentValue := new(big.Rat).Mul(shareRat, new(big.Rat).SetInt64(agent.Aum))
		cv := ratToInt64(currentValue)

		profit := cv - inv.Amount
		if profit <= 0 {
			continue // no profit share on zero or negative returns
		}

		profitShareAmount := profit * s.profitSharePct / 100
		if profitShareAmount <= 0 {
			continue
		}

		err = s.store.Tx(ctx, func(q *store.Queries) error {
			_, txErr := q.CreateLedgerEntry(ctx, store.CreateLedgerEntryParams{
				UserID:  inv.UserID,
				AgentID: agentID,
				Type:    "profit_share",
				Amount:  -profitShareAmount,
				Balance: 0,
			})
			return txErr
		})
		if err != nil {
			return fmt.Errorf("invest: profit share for %s: %w", inv.ID, err)
		}
	}

	return nil
}

// --- numeric helpers ---

// ratToNumeric converts a *big.Rat to pgtype.Numeric.
func ratToNumeric(r *big.Rat) pgtype.Numeric {
	// Use 8 decimal places of precision.
	// Multiply numerator by 10^8, divide by denominator, store with exp=-8.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil)
	num := new(big.Int).Mul(r.Num(), scale)
	num.Div(num, r.Denom())

	return pgtype.Numeric{
		Int:              num,
		Exp:              -8,
		NaN:              false,
		InfinityModifier: pgtype.Finite,
		Valid:            true,
	}
}

// numericToRat converts a pgtype.Numeric to *big.Rat.
func numericToRat(n pgtype.Numeric) *big.Rat {
	if !n.Valid || n.Int == nil {
		return new(big.Rat)
	}
	r := new(big.Rat).SetInt(n.Int)
	if n.Exp < 0 {
		denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n.Exp)), nil)
		r.SetFrac(n.Int, denom)
	} else if n.Exp > 0 {
		mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
		r.SetInt(new(big.Int).Mul(n.Int, mul))
	}
	return r
}

// numericToString converts a pgtype.Numeric to a decimal string.
func numericToString(n pgtype.Numeric) string {
	if !n.Valid || n.Int == nil {
		return "0"
	}
	r := numericToRat(n)
	return r.FloatString(8)
}

// ratToInt64 truncates a *big.Rat to int64 (floor).
func ratToInt64(r *big.Rat) int64 {
	result := new(big.Int).Div(r.Num(), r.Denom())
	return result.Int64()
}
