package mcp

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/agentfi/agentfi-go-backend/internal/store"
)

// --- Generators ---

// genToolDefinition generates a random ToolDefinition.
func genToolDefinition() *rapid.Generator[ToolDefinition] {
	return rapid.Custom(func(t *rapid.T) ToolDefinition {
		return ToolDefinition{
			Name:        rapid.StringMatching(`[a-z_]{3,20}`).Draw(t, "tool_name"),
			Description: rapid.StringMatching(`[A-Za-z0-9 ]{5,50}`).Draw(t, "tool_desc"),
			Parameters:  nil, // keep simple — JSON round-trip of nil is fine
		}
	})
}

// genMcpServer generates a random store.McpServer with valid tool JSON.
func genMcpServer() *rapid.Generator[store.McpServer] {
	return rapid.Custom(func(t *rapid.T) store.McpServer {
		toolCount := rapid.IntRange(0, 5).Draw(t, "tool_count")
		tools := make([]ToolDefinition, toolCount)
		for i := range tools {
			tools[i] = genToolDefinition().Draw(t, "tool")
		}
		toolsJSON, _ := json.Marshal(tools)

		return store.McpServer{
			ID:       uuid.New(),
			Name:     rapid.StringMatching(`[A-Za-z0-9-]{3,30}`).Draw(t, "name"),
			Url:      rapid.StringMatching(`https://[a-z]{3,10}\.example\.com/mcp`).Draw(t, "url"),
			Type:     rapid.SampledFrom([]string{"http", "stdio", "sse"}).Draw(t, "type"),
			Tools:    toolsJSON,
			IsPublic: rapid.Bool().Draw(t, "is_public"),
			Status:   rapid.SampledFrom([]string{"active", "unavailable"}).Draw(t, "status"),
		}
	})
}

// Feature: agentfi-go-backend, Property 6: MCP Server registration round-trip
// For any valid MCP Server config (name, URL, tools), registering then listing
// SHALL include the new server with matching tool definitions.
// Validates: Requirements 3.1
func TestPropertyMCPServerRegistrationRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		srv := genMcpServer().Draw(rt, "server")

		// Convert store.McpServer → serverResponse (simulates what the list endpoint returns).
		resp := toServerResponse(srv)

		// Round-trip: all fields must match.
		if resp.ID != srv.ID {
			rt.Fatalf("ID mismatch: got %v, want %v", resp.ID, srv.ID)
		}
		if resp.Name != srv.Name {
			rt.Fatalf("Name mismatch: got %q, want %q", resp.Name, srv.Name)
		}
		if resp.URL != srv.Url {
			rt.Fatalf("URL mismatch: got %q, want %q", resp.URL, srv.Url)
		}
		if resp.Type != srv.Type {
			rt.Fatalf("Type mismatch: got %q, want %q", resp.Type, srv.Type)
		}
		if resp.IsPublic != srv.IsPublic {
			rt.Fatalf("IsPublic mismatch: got %v, want %v", resp.IsPublic, srv.IsPublic)
		}
		if resp.Status != srv.Status {
			rt.Fatalf("Status mismatch: got %q, want %q", resp.Status, srv.Status)
		}

		// Tool definitions round-trip through JSON.
		var originalTools []ToolDefinition
		if err := json.Unmarshal(srv.Tools, &originalTools); err != nil {
			rt.Fatalf("unmarshal original tools: %v", err)
		}

		if len(resp.Tools) != len(originalTools) {
			rt.Fatalf("Tools length mismatch: got %d, want %d", len(resp.Tools), len(originalTools))
		}
		for i, tool := range originalTools {
			if resp.Tools[i].Name != tool.Name {
				rt.Fatalf("Tools[%d].Name mismatch: got %q, want %q", i, resp.Tools[i].Name, tool.Name)
			}
			if resp.Tools[i].Description != tool.Description {
				rt.Fatalf("Tools[%d].Description mismatch: got %q, want %q", i, resp.Tools[i].Description, tool.Description)
			}
		}

		// Verify that re-marshalling the response tools produces equivalent JSON.
		respToolsJSON, err := json.Marshal(resp.Tools)
		if err != nil {
			rt.Fatalf("marshal response tools: %v", err)
		}
		var roundTripped []ToolDefinition
		if err := json.Unmarshal(respToolsJSON, &roundTripped); err != nil {
			rt.Fatalf("unmarshal round-tripped tools: %v", err)
		}
		if len(roundTripped) != len(originalTools) {
			rt.Fatalf("round-trip tools length mismatch: got %d, want %d", len(roundTripped), len(originalTools))
		}
	})
}

// Feature: agentfi-go-backend, Property 7: Circuit breaker state transitions
// For any MCP Server, after 3 consecutive health check failures the circuit
// SHALL be open (unavailable), and after a successful health check in half-open
// state the circuit SHALL return to closed (available).
// Validates: Requirements 3.3, 3.4
func TestPropertyCircuitBreakerStateTransitions(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cb := newCircuitBreaker()

		// Initial state must be closed.
		if cb.State() != CircuitClosed {
			rt.Fatalf("initial state should be closed, got %v", cb.State())
		}

		// Record fewer than threshold failures — circuit stays closed.
		prefailures := rapid.IntRange(0, failureThreshold-1).Draw(rt, "prefailures")
		for i := 0; i < prefailures; i++ {
			cb.RecordFailure()
		}
		if prefailures < failureThreshold {
			if cb.State() != CircuitClosed {
				rt.Fatalf("after %d failures (< threshold %d), state should be closed, got %v",
					prefailures, failureThreshold, cb.State())
			}
		}

		// A success at any point resets failures and closes the circuit.
		cb.RecordSuccess()
		if cb.State() != CircuitClosed {
			rt.Fatalf("after RecordSuccess, state should be closed, got %v", cb.State())
		}

		// Now drive to open: exactly threshold consecutive failures.
		for i := 0; i < failureThreshold; i++ {
			cb.RecordFailure()
		}
		if cb.State() != CircuitOpen {
			rt.Fatalf("after %d consecutive failures, state should be open, got %v",
				failureThreshold, cb.State())
		}

		// Transition to half-open.
		if !cb.TryHalfOpen() {
			rt.Fatal("TryHalfOpen should succeed from open state")
		}
		if cb.State() != CircuitHalfOpen {
			rt.Fatalf("after TryHalfOpen, state should be half-open, got %v", cb.State())
		}

		// A second TryHalfOpen from half-open should fail (already transitioned).
		if cb.TryHalfOpen() {
			rt.Fatal("TryHalfOpen should fail from half-open state")
		}

		// Success in half-open → closed.
		cb.RecordSuccess()
		if cb.State() != CircuitClosed {
			rt.Fatalf("after success in half-open, state should be closed, got %v", cb.State())
		}
		// Failures should be reset.
		if cb.failures.Load() != 0 {
			rt.Fatalf("failures should be 0 after recovery, got %d", cb.failures.Load())
		}
	})
}

// Feature: agentfi-go-backend, Property 8: MCP URL validation
// For any string that is not a valid URL, the MCP_Host SHALL reject the binding
// attempt. For any valid URL format, the MCP_Host SHALL attempt a connection.
// Validates: Requirements 3.5
func TestPropertyMCPURLValidation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		strategy := rapid.SampledFrom([]string{"valid_http", "valid_https", "no_scheme", "bad_scheme", "no_host", "garbage"}).Draw(rt, "strategy")

		switch strategy {
		case "valid_http":
			host := rapid.StringMatching(`[a-z]{3,10}\.[a-z]{2,5}`).Draw(rt, "host")
			url := "http://" + host
			if err := ValidateURL(url); err != nil {
				rt.Fatalf("valid http URL %q rejected: %v", url, err)
			}

		case "valid_https":
			host := rapid.StringMatching(`[a-z]{3,10}\.[a-z]{2,5}`).Draw(rt, "host")
			url := "https://" + host
			if err := ValidateURL(url); err != nil {
				rt.Fatalf("valid https URL %q rejected: %v", url, err)
			}

		case "no_scheme":
			host := rapid.StringMatching(`[a-z]{3,10}\.[a-z]{2,5}`).Draw(rt, "host")
			url := host + "/path"
			err := ValidateURL(url)
			if err == nil {
				rt.Fatalf("URL without scheme %q should be rejected", url)
			}

		case "bad_scheme":
			scheme := rapid.SampledFrom([]string{"ftp", "ws", "wss", "tcp", "file", "ssh"}).Draw(rt, "scheme")
			host := rapid.StringMatching(`[a-z]{3,10}\.[a-z]{2,5}`).Draw(rt, "host")
			url := scheme + "://" + host
			err := ValidateURL(url)
			if err == nil {
				rt.Fatalf("URL with scheme %q (%q) should be rejected", scheme, url)
			}

		case "no_host":
			url := rapid.SampledFrom([]string{"http://", "https://"}).Draw(rt, "scheme_only")
			err := ValidateURL(url)
			if err == nil {
				rt.Fatalf("URL with no host %q should be rejected", url)
			}

		case "garbage":
			garbage := rapid.StringMatching(`[^a-zA-Z0-9:/]{1,20}`).Draw(rt, "garbage")
			err := ValidateURL(garbage)
			if err == nil {
				rt.Fatalf("garbage string %q should be rejected", garbage)
			}
		}
	})
}
