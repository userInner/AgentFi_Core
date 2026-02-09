package mcp

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentfi/agentfi-go-backend/internal/auth"
	"github.com/agentfi/agentfi-go-backend/internal/store"
)

// Handler provides HTTP handlers for MCP Server management endpoints.
type Handler struct {
	mgr   *Manager
	store *store.Store
}

// NewHandler creates a new MCP handler.
func NewHandler(mgr *Manager, s *store.Store) *Handler {
	return &Handler{mgr: mgr, store: s}
}

// HandleList handles GET /api/mcp-servers — returns public servers plus the
// authenticated user's private servers.
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	public, err := h.store.ListPublicMCPServers(ctx)
	if err != nil {
		slog.Error("mcp: list public servers", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list servers")
		return
	}

	var private []store.McpServer
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		user, userErr := h.store.GetUserByAddress(ctx, claims.Address)
		if userErr == nil {
			private, err = h.store.ListMCPServersByOwner(ctx, pgtype.UUID{Bytes: user.ID, Valid: true})
			if err != nil {
				slog.Error("mcp: list private servers", "error", err)
				// non-fatal — still return public list
				private = nil
			}
		}
	}

	servers := make([]serverResponse, 0, len(public)+len(private))
	seen := make(map[uuid.UUID]bool)
	for _, s := range public {
		servers = append(servers, toServerResponse(s))
		seen[s.ID] = true
	}
	for _, s := range private {
		if !seen[s.ID] {
			servers = append(servers, toServerResponse(s))
		}
	}

	writeJSON(w, http.StatusOK, listServersResponse{Data: servers})
}

// HandleRegister handles POST /api/mcp-servers — registers a new private MCP Server.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing auth")
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if req.Name == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "name and url are required")
		return
	}

	if err := ValidateURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	// Resolve user ID.
	user, err := h.store.GetUserByAddress(r.Context(), claims.Address)
	if err != nil {
		slog.Error("mcp: get user", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to resolve user")
		return
	}

	srvType := req.Type
	if srvType == "" {
		srvType = "http"
	}

	srv, err := h.mgr.Register(r.Context(), RegisterRequest{
		Name:     req.Name,
		URL:      req.URL,
		Type:     srvType,
		Tools:    req.Tools,
		IsPublic: false, // user-registered servers are always private
		OwnerID:  pgtype.UUID{Bytes: user.ID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, ErrInvalidURL) {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
			return
		}
		slog.Error("mcp: register server", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to register server")
		return
	}

	writeJSON(w, http.StatusCreated, toServerResponse(*srv))
}

// --- request / response types ---

type registerRequest struct {
	Name  string           `json:"name"`
	URL   string           `json:"url"`
	Type  string           `json:"type,omitempty"`
	Tools []ToolDefinition `json:"tools,omitempty"`
}

type serverResponse struct {
	ID       uuid.UUID        `json:"id"`
	Name     string           `json:"name"`
	URL      string           `json:"url"`
	Type     string           `json:"type"`
	Tools    []ToolDefinition `json:"tools"`
	IsPublic bool             `json:"is_public"`
	Status   string           `json:"status"`
}

type listServersResponse struct {
	Data []serverResponse `json:"data"`
}

func toServerResponse(s store.McpServer) serverResponse {
	var tools []ToolDefinition
	_ = json.Unmarshal(s.Tools, &tools)
	if tools == nil {
		tools = []ToolDefinition{}
	}
	return serverResponse{
		ID:       s.ID,
		Name:     s.Name,
		URL:      s.Url,
		Type:     s.Type,
		Tools:    tools,
		IsPublic: s.IsPublic,
		Status:   s.Status,
	}
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
