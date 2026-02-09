// Package fastpath provides fast-path evaluation for simple numeric conditions
// extracted from agent strategy prompts. When a strategy contains parseable
// conditions like "RSI < 30", the fast path evaluates them directly against
// MCP data without calling the LLM, reducing latency and token consumption.
//
// Requirements: 5.1, 5.2, 5.3, 5.4
package fastpath

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Supported indicators and their corresponding MCP tool names.
var indicatorTools = map[string]string{
	"RSI":     "get_rsi",
	"MACD":    "get_macd",
	"PRICE":   "get_price",
	"EMA":     "get_ema",
	"SMA":     "get_sma",
	"VOLUME":  "get_volume",
	"ATR":     "get_atr",
	"BBWIDTH": "get_bbwidth",
}

// Supported comparison operators.
var validOperators = map[string]bool{
	"<":  true,
	">":  true,
	"<=": true,
	">=": true,
	"==": true,
}

// conditionPattern matches patterns like "RSI < 30", "Price >= 50000.5",
// "MACD > 0". It is case-insensitive for the indicator name.
// Group 1: indicator, Group 2: operator, Group 3: number (int or float).
var conditionPattern = regexp.MustCompile(
	`(?i)\b(RSI|MACD|PRICE|EMA|SMA|VOLUME|ATR|BBWIDTH)\s*(<=|>=|==|<|>)\s*(-?\d+(?:\.\d+)?)\b`,
)

// Condition represents a single parsed numeric condition from a strategy prompt.
type Condition struct {
	Indicator string         `json:"indicator"` // e.g. "RSI", "MACD", "PRICE"
	Operator  string         `json:"operator"`  // "<", ">", "<=", ">=", "=="
	Threshold float64        `json:"threshold"` // numeric threshold
	ToolName  string         `json:"tool_name"` // MCP tool to call for data
	ToolArgs  map[string]any `json:"tool_args,omitempty"`
}

// ToolCaller is the minimal interface needed to fetch indicator data from MCP.
type ToolCaller interface {
	CallTool(ctx context.Context, serverID any, toolName string, args map[string]any) (any, error)
}

// TryParse extracts all parseable numeric conditions from a strategy prompt.
// Returns an empty slice (not an error) when no conditions can be parsed,
// signalling the caller to fall back to full LLM reasoning (Req 5.4).
func TryParse(strategyPrompt string) []Condition {
	matches := conditionPattern.FindAllStringSubmatch(strategyPrompt, -1)
	if len(matches) == 0 {
		return nil
	}

	conditions := make([]Condition, 0, len(matches))
	for _, m := range matches {
		indicator := strings.ToUpper(m[1])
		operator := m[2]
		threshold, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue // skip unparseable numbers
		}

		toolName, ok := indicatorTools[indicator]
		if !ok {
			continue // unknown indicator
		}

		conditions = append(conditions, Condition{
			Indicator: indicator,
			Operator:  operator,
			Threshold: threshold,
			ToolName:  toolName,
			ToolArgs:  map[string]any{},
		})
	}
	return conditions
}

// Evaluate checks all conditions against live MCP data. Returns true only when
// ALL conditions are met (logical AND). If any MCP call fails, returns an error.
// Requirements: 5.1, 5.2, 5.3
func Evaluate(ctx context.Context, conditions []Condition, callTool func(ctx context.Context, toolName string, args map[string]any) (any, error)) (bool, error) {
	if len(conditions) == 0 {
		return false, nil
	}

	for _, cond := range conditions {
		value, err := callTool(ctx, cond.ToolName, cond.ToolArgs)
		if err != nil {
			return false, fmt.Errorf("fastpath: call %s: %w", cond.ToolName, err)
		}

		numVal, err := toFloat64(value)
		if err != nil {
			return false, fmt.Errorf("fastpath: convert %s result: %w", cond.ToolName, err)
		}

		if !compare(numVal, cond.Operator, cond.Threshold) {
			return false, nil // condition not met → skip cycle (Req 5.3)
		}
	}

	return true, nil // all conditions met → proceed to LLM for execution decision
}

// compare applies the operator to lhs and rhs.
func compare(lhs float64, op string, rhs float64) bool {
	switch op {
	case "<":
		return lhs < rhs
	case ">":
		return lhs > rhs
	case "<=":
		return lhs <= rhs
	case ">=":
		return lhs >= rhs
	case "==":
		return lhs == rhs
	default:
		return false
	}
}

// toFloat64 converts an MCP tool result to a float64. It handles float64,
// int, int64, json.Number, and string representations.
func toFloat64(v any) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case float32:
		return float64(val), nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case string:
		return strconv.ParseFloat(val, 64)
	case map[string]any:
		// MCP tools often return {"value": 28.5} — extract the "value" field.
		if raw, ok := val["value"]; ok {
			return toFloat64(raw)
		}
		return 0, fmt.Errorf("map has no 'value' key")
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}
