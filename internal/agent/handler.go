package agent

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/agentfi/agentfi-go-backend/internal/auth"
)

// Handler provides HTTP handlers for Agent CRUD endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a new agent handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Routes returns a chi.Router with all agent routes mounted.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.HandleCreate)
	r.Get("/", h.HandleList)
	r.Get("/{id}", h.HandleGet)
	r.Put("/{id}", h.HandleUpdate)
	r.Delete("/{id}", h.HandleDelete)
	r.Post("/{id}/start", h.HandleStart)
	r.Post("/{id}/pause", h.HandlePause)
	return r
}

// HandleCreate handles POST /api/agents.
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	resp, err := h.svc.Create(r.Context(), claims.Address, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// HandleGet handles GET /api/agents/{id}.
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid agent id")
		return
	}

	resp, err := h.svc.Get(r.Context(), id)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleList handles GET /api/agents.
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	cursor := r.URL.Query().Get("cursor")
	limit := int32(20)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = int32(n)
		}
	}

	agents, nextCursor, err := h.svc.List(r.Context(), claims.Address, cursor, limit)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, listResponse{
		Data:       agents,
		NextCursor: nextCursor,
	})
}

// HandleUpdate handles PUT /api/agents/{id}.
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid agent id")
		return
	}

	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	resp, err := h.svc.Update(r.Context(), id, claims.Address, req)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleDelete handles DELETE /api/agents/{id}.
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid agent id")
		return
	}

	if err := h.svc.Delete(r.Context(), id, claims.Address); err != nil {
		handleServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleStart handles POST /api/agents/{id}/start.
func (h *Handler) HandleStart(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid agent id")
		return
	}

	resp, err := h.svc.Start(r.Context(), id, claims.Address)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandlePause handles POST /api/agents/{id}/pause.
func (h *Handler) HandlePause(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid agent id")
		return
	}

	resp, err := h.svc.Pause(r.Context(), id, claims.Address)
	if err != nil {
		handleServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- response types ---

type listResponse struct {
	Data       []AgentResponse `json:"data"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
		writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not authorized to access this agent")
	case errors.Is(err, ErrInvalidStatus):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, ErrValidation):
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	default:
		slog.Error("agent handler error", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}
