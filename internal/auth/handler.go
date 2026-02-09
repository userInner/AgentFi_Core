package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Handler provides HTTP handlers for auth endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a new auth handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// nonceResponse is the JSON response for POST /api/auth/nonce.
type nonceResponse struct {
	Nonce string `json:"nonce"`
}

// verifyRequest is the JSON request body for POST /api/auth/verify.
type verifyRequest struct {
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

// verifyResponse is the JSON response for POST /api/auth/verify.
type verifyResponse struct {
	Token string `json:"token"`
}

// HandleNonce handles POST /api/auth/nonce — generates and returns a nonce.
func (h *Handler) HandleNonce(w http.ResponseWriter, r *http.Request) {
	nonce, err := h.svc.GenerateNonce(r.Context())
	if err != nil {
		slog.Error("failed to generate nonce", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to generate nonce")
		return
	}
	writeJSON(w, http.StatusOK, nonceResponse{Nonce: nonce})
}

// HandleVerify handles POST /api/auth/verify — verifies SIWE signature and returns JWT.
func (h *Handler) HandleVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if req.Message == "" || req.Signature == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "message and signature are required")
		return
	}

	token, err := h.svc.VerifySIWE(r.Context(), req.Message, req.Signature)
	if err != nil {
		switch err {
		case ErrInvalidSignature, ErrNonceNotFound:
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		default:
			slog.Error("SIWE verification failed", "error", err)
			writeError(w, http.StatusInternalServerError, "INTERNAL", "verification failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, verifyResponse{Token: token})
}

// --- helpers ---

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}
