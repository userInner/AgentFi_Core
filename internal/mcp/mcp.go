// Package mcp provides MCP client connections, manager, and circuit breaker.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentfi/agentfi-go-backend/internal/store"
)

// Errors returned by the MCP package.
var (
	ErrServerUnavailable = errors.New("mcp: server unavailable (circuit open)")
	ErrHealthCheck       = errors.New("mcp: health check failed")
	ErrInvalidURL        = errors.New("mcp: invalid server URL")
	ErrToolNotFound      = errors.New("mcp: tool not found")
	ErrCallFailed        = errors.New("mcp: tool call failed")
	ErrNotFound          = errors.New("mcp: server not found")
	ErrValidation        = errors.New("mcp: validation error")
)

// ToolDefinition describes a tool exposed by an MCP Server.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// --- MCP Client Interface ---

// Client defines the interface for communicating with a single MCP Server.
type Client interface {
	// CallTool invokes a named tool on the MCP Server.
	CallTool(ctx context.Context, toolName string, args map[string]any) (any, error)
	// ListTools returns the tool definitions advertised by the server.
	ListTools(ctx context.Context) ([]ToolDefinition, error)
	// HealthCheck pings the server; returns nil if healthy.
	HealthCheck(ctx context.Context) error
}

// --- JSON-RPC types ---

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- HTTP MCP Client ---

// HTTPClient implements Client using JSON-RPC over HTTP.
type HTTPClient struct {
	serverURL  string
	httpClient *http.Client
}

// NewHTTPClient creates a new HTTP-based MCP Client.
func NewHTTPClient(serverURL string) *HTTPClient {
	return &HTTPClient{
		serverURL: serverURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// CallTool invokes a tool via JSON-RPC POST.
func (c *HTTPClient) CallTool(ctx context.Context, toolName string, args map[string]any) (any, error) {
	params := map[string]any{
		"name":      toolName,
		"arguments": args,
	}
	resp, err := c.rpc(ctx, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCallFailed, err)
	}
	var result any
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal tool result: %w", err)
	}
	return result, nil
}

// ListTools retrieves tool definitions via JSON-RPC.
func (c *HTTPClient) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	resp, err := c.rpc(ctx, "tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}
	var wrapper struct {
		Tools []ToolDefinition `json:"tools"`
	}
	if err := json.Unmarshal(resp, &wrapper); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal tools: %w", err)
	}
	return wrapper.Tools, nil
}

// HealthCheck sends a ping JSON-RPC call.
func (c *HTTPClient) HealthCheck(ctx context.Context) error {
	_, err := c.rpc(ctx, "ping", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHealthCheck, err)
	}
	return nil
}

// rpc sends a JSON-RPC request and returns the result bytes.
func (c *HTTPClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("mcp: invalid JSON-RPC response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("mcp: rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// --- Circuit Breaker ---

// CircuitState represents the state of a circuit breaker.
type CircuitState int32

const (
	CircuitClosed   CircuitState = iota // healthy
	CircuitOpen                         // unavailable — reject calls
	CircuitHalfOpen                     // probing — allow one call to test recovery
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

const (
	failureThreshold = 3 // consecutive failures before opening
)

// circuitBreaker tracks health for a single MCP Server.
type circuitBreaker struct {
	state    atomic.Int32 // CircuitState
	failures atomic.Int32
}

func newCircuitBreaker() *circuitBreaker {
	cb := &circuitBreaker{}
	cb.state.Store(int32(CircuitClosed))
	return cb
}

// State returns the current circuit state.
func (cb *circuitBreaker) State() CircuitState {
	return CircuitState(cb.state.Load())
}

// RecordSuccess resets failures and closes the circuit.
func (cb *circuitBreaker) RecordSuccess() {
	cb.failures.Store(0)
	cb.state.Store(int32(CircuitClosed))
}

// RecordFailure increments failures; opens the circuit after threshold.
func (cb *circuitBreaker) RecordFailure() CircuitState {
	n := cb.failures.Add(1)
	if n >= int32(failureThreshold) {
		cb.state.Store(int32(CircuitOpen))
	}
	return CircuitState(cb.state.Load())
}

// TryHalfOpen transitions from open to half-open for a probe attempt.
// Returns true if the transition succeeded.
func (cb *circuitBreaker) TryHalfOpen() bool {
	return cb.state.CompareAndSwap(int32(CircuitOpen), int32(CircuitHalfOpen))
}

// --- MCP Manager ---

// Manager manages MCP Server connections, circuit breakers, and health checks.
type Manager struct {
	store    *store.Store
	mu       sync.RWMutex
	clients  map[uuid.UUID]Client          // serverID -> client
	breakers map[uuid.UUID]*circuitBreaker // serverID -> breaker
	stopCh   chan struct{}
}

// NewManager creates a new MCP Manager.
func NewManager(s *store.Store) *Manager {
	return &Manager{
		store:    s,
		clients:  make(map[uuid.UUID]Client),
		breakers: make(map[uuid.UUID]*circuitBreaker),
		stopCh:   make(chan struct{}),
	}
}

// Register stores a new MCP Server in the database and initialises its client.
func (m *Manager) Register(ctx context.Context, req RegisterRequest) (*store.McpServer, error) {
	if err := ValidateURL(req.URL); err != nil {
		return nil, err
	}

	toolsJSON, err := json.Marshal(req.Tools)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal tools: %w", err)
	}

	srv, err := m.store.CreateMCPServer(ctx, store.CreateMCPServerParams{
		Name:     req.Name,
		Url:      req.URL,
		Type:     req.Type,
		Tools:    toolsJSON,
		IsPublic: req.IsPublic,
		OwnerID:  req.OwnerID,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: create server: %w", err)
	}

	m.mu.Lock()
	m.clients[srv.ID] = NewHTTPClient(req.URL)
	m.breakers[srv.ID] = newCircuitBreaker()
	m.mu.Unlock()

	return &srv, nil
}

// Connect ensures a client exists for the given server and verifies health.
func (m *Manager) Connect(ctx context.Context, serverID uuid.UUID) (Client, error) {
	m.mu.RLock()
	client, ok := m.clients[serverID]
	m.mu.RUnlock()

	if !ok {
		// Load from DB and create client.
		srv, err := m.store.GetMCPServer(ctx, serverID)
		if err != nil {
			return nil, fmt.Errorf("mcp: get server: %w", err)
		}
		client = NewHTTPClient(srv.Url)
		m.mu.Lock()
		m.clients[serverID] = client
		if _, exists := m.breakers[serverID]; !exists {
			m.breakers[serverID] = newCircuitBreaker()
		}
		m.mu.Unlock()
	}

	// Check circuit breaker.
	m.mu.RLock()
	cb := m.breakers[serverID]
	m.mu.RUnlock()

	if cb != nil && cb.State() == CircuitOpen {
		return nil, ErrServerUnavailable
	}

	// Verify health with 5s timeout (Req 3.2).
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.HealthCheck(healthCtx); err != nil {
		if cb != nil {
			cb.RecordFailure()
		}
		return nil, fmt.Errorf("mcp: health check on connect: %w", err)
	}
	if cb != nil {
		cb.RecordSuccess()
	}

	return client, nil
}

// Disconnect removes the client for a server.
func (m *Manager) Disconnect(_ context.Context, serverID uuid.UUID) {
	m.mu.Lock()
	delete(m.clients, serverID)
	m.mu.Unlock()
}

// CallTool routes a tool call through the appropriate client, respecting the circuit breaker.
func (m *Manager) CallTool(ctx context.Context, serverID uuid.UUID, toolName string, args map[string]any) (any, error) {
	m.mu.RLock()
	cb := m.breakers[serverID]
	client := m.clients[serverID]
	m.mu.RUnlock()

	if client == nil {
		return nil, ErrNotFound
	}
	if cb != nil && cb.State() == CircuitOpen {
		return nil, ErrServerUnavailable
	}

	result, err := client.CallTool(ctx, toolName, args)
	if err != nil {
		if cb != nil {
			cb.RecordFailure()
		}
		return nil, err
	}
	if cb != nil {
		cb.RecordSuccess()
	}
	return result, nil
}

// GetToolDefinitions returns merged tool definitions for the given server IDs.
func (m *Manager) GetToolDefinitions(ctx context.Context, serverIDs []uuid.UUID) ([]ToolDefinition, error) {
	var all []ToolDefinition
	for _, sid := range serverIDs {
		srv, err := m.store.GetMCPServer(ctx, sid)
		if err != nil {
			return nil, fmt.Errorf("mcp: get server %s: %w", sid, err)
		}
		var tools []ToolDefinition
		if err := json.Unmarshal(srv.Tools, &tools); err != nil {
			slog.Warn("mcp: unmarshal tools", "server_id", sid, "error", err)
			continue
		}
		all = append(all, tools...)
	}
	return all, nil
}

// GetBreaker returns the circuit breaker for a server (exposed for testing).
func (m *Manager) GetBreaker(serverID uuid.UUID) *circuitBreaker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.breakers[serverID]
}

// StartHealthChecks launches a background goroutine that checks all registered
// servers every 30 seconds. When a server fails 3 consecutive checks the circuit
// opens and dependent agents should be paused (Req 3.3). When a previously open
// circuit's probe succeeds, the circuit closes (Req 3.4).
func (m *Manager) StartHealthChecks(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.runHealthChecks(ctx)
			}
		}
	}()
}

// Stop signals the health check goroutine to exit.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// runHealthChecks iterates over all known servers and probes them.
func (m *Manager) runHealthChecks(ctx context.Context) {
	m.mu.RLock()
	ids := make([]uuid.UUID, 0, len(m.clients))
	for id := range m.clients {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	for _, id := range ids {
		m.mu.RLock()
		client := m.clients[id]
		cb := m.breakers[id]
		m.mu.RUnlock()
		if client == nil || cb == nil {
			continue
		}

		state := cb.State()

		// For open circuits, try half-open probe.
		if state == CircuitOpen {
			if !cb.TryHalfOpen() {
				continue
			}
		}

		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := client.HealthCheck(checkCtx)
		cancel()

		if err != nil {
			newState := cb.RecordFailure()
			if newState == CircuitOpen {
				slog.Warn("mcp: circuit opened", "server_id", id)
				// Update DB status to unavailable.
				_ = m.store.UpdateMCPServerStatus(ctx, store.UpdateMCPServerStatusParams{
					ID:     id,
					Status: "unavailable",
				})
			}
		} else {
			if state == CircuitHalfOpen || state == CircuitOpen {
				slog.Info("mcp: circuit recovered", "server_id", id)
				_ = m.store.UpdateMCPServerStatus(ctx, store.UpdateMCPServerStatusParams{
					ID:     id,
					Status: "active",
				})
			}
			cb.RecordSuccess()
		}
	}
}

// ValidateURL checks that a string is a valid HTTP(S) URL.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme must be http or https", ErrInvalidURL)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: host is required", ErrInvalidURL)
	}
	return nil
}

// --- Request / Response types ---

// RegisterRequest is the input for registering a new MCP Server.
type RegisterRequest struct {
	Name     string             `json:"name"`
	URL      string             `json:"url"`
	Type     string             `json:"type"` // "http" | "stdio" | "sse"
	Tools    []ToolDefinition   `json:"tools"`
	IsPublic bool               `json:"is_public"`
	OwnerID  pgtype.UUID        `json:"-"` // set by handler from auth claims
}
