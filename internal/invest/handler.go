package invest

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/agentfi/agentfi-go-backend/internal/auth"
)

// Handler provides HTTP handlers for investment endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a new investment handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// --- request/response types ---

type depositRequest struct {
	AgentID string `json:"agent_id"`
	Amount  int64  `json:"amount"`
}

type redeemRequest struct {
	InvestmentID string `json:"investment_id"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HandleDeposit handles POST /api/invest — deposit funds into an Agent.
func (h *Handler) HandleDeposit(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	var req depositRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	agentID, err := uuid.Parse(req.AgentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid agent_id")
		return
	}

	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "amount must be positive")
		return
	}

	inv, err := h.svc.Deposit(r.Context(), claims.Address, agentID, req.Amount)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

// HandleRedeem handles POST /api/redeem — redeem an investment.
func (h *Handler) HandleRedeem(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	var req redeemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	investmentID, err := uuid.Parse(req.InvestmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid investment_id")
		return
	}

	redemption, err := h.svc.Redeem(r.Context(), claims.Address, investmentID)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, redemption)
}

// HandlePortfolio handles GET /api/portfolio — list user's investments.
func (h *Handler) HandlePortfolio(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	portfolio, err := h.svc.GetPortfolio(r.Context(), claims.Address)
	if err != nil {
		slog.Error("portfolio error", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, portfolio)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "investment not found")
	case errors.Is(err, ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
	case errors.Is(err, ErrAlreadyRedeemed):
		writeError(w, http.StatusConflict, "CONFLICT", "investment already redeemed")
	case errors.Is(err, ErrInvalidAmount):
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	default:
		slog.Error("invest handler error", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}
