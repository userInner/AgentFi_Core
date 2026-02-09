package fastpath

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// --- Generators ---

// genIndicator draws a random supported indicator name.
func genIndicator() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{"RSI", "MACD", "PRICE", "EMA", "SMA", "VOLUME", "ATR", "BBWIDTH"})
}

// genOperator draws a random supported comparison operator.
func genOperator() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{"<", ">", "<=", ">=", "=="})
}

// genThreshold draws a random numeric threshold (positive, reasonable range).
func genThreshold() *rapid.Generator[float64] {
	return rapid.Float64Range(-10000, 100000)
}

// parseableInput holds a generated strategy prompt and the expected parsed values.
type parseableInput struct {
	Prompt    string
	Indicator string
	Operator  string
	Threshold float64
}

// genParseablePrompt builds a strategy prompt containing one valid condition.
func genParseablePrompt() *rapid.Generator[parseableInput] {
	return rapid.Custom(func(t *rapid.T) parseableInput {
		ind := genIndicator().Draw(t, "indicator")
		op := genOperator().Draw(t, "operator")
		th := genThreshold().Draw(t, "threshold")

		// Format threshold: use integer form when it's a whole number.
		var thStr string
		if th == float64(int64(th)) {
			thStr = fmt.Sprintf("%d", int64(th))
		} else {
			thStr = fmt.Sprintf("%.2f", th)
		}

		// Embed the condition in surrounding text to simulate a real prompt.
		prefix := rapid.SampledFrom([]string{
			"Buy when ",
			"If ",
			"Check that ",
			"When ",
			"",
		}).Draw(t, "prefix")
		suffix := rapid.SampledFrom([]string{
			" then buy BTC",
			" execute trade",
			"",
			", sell immediately",
		}).Draw(t, "suffix")

		prompt := fmt.Sprintf("%s%s %s %s%s", prefix, ind, op, thStr, suffix)
		return parseableInput{
			Prompt:    prompt,
			Indicator: ind,
			Operator:  op,
			Threshold: th,
		}
	})
}

// genUnparseablePrompt generates a strategy prompt with no valid conditions.
func genUnparseablePrompt() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{
		"Buy BTC when the market looks good",
		"Sell everything if sentiment is negative",
		"Hold position and wait for breakout",
		"DCA into ETH every week",
		"Follow the trend and manage risk",
		"",
		"Just do whatever makes sense",
		"UNKNOWN_INDICATOR < 50",
		"FOO > 100",
	})
}

// Feature: agentfi-go-backend, Property 12: Fast Path parse and evaluate
// For any strategy prompt containing a numeric condition in the form
// "[INDICATOR] [OPERATOR] [NUMBER]", the Fast Path evaluator SHALL parse it
// into a Condition struct. For any unparseable strategy, it SHALL return an
// empty condition list.
// Validates: Requirements 5.1, 5.4
func TestPropertyFastPathParseAndEvaluate(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Randomly test either a parseable or unparseable prompt.
		parseable := rapid.Bool().Draw(rt, "parseable")

		if parseable {
			gen := genParseablePrompt().Draw(rt, "input")
			conditions := TryParse(gen.Prompt)

			if len(conditions) == 0 {
				rt.Fatalf("expected at least one condition from prompt %q, got none", gen.Prompt)
			}

			// Find the condition matching our generated indicator.
			found := false
			for _, c := range conditions {
				if c.Indicator == gen.Indicator && c.Operator == gen.Operator {
					found = true

					// Threshold must match (within float tolerance for formatted values).
					diff := c.Threshold - gen.Threshold
					if diff < -0.01 || diff > 0.01 {
						rt.Fatalf("threshold mismatch: got %f, want %f", c.Threshold, gen.Threshold)
					}

					// ToolName must correspond to the indicator.
					expectedTool, ok := indicatorTools[gen.Indicator]
					if !ok {
						rt.Fatalf("indicator %q not in indicatorTools map", gen.Indicator)
					}
					if c.ToolName != expectedTool {
						rt.Fatalf("tool name mismatch: got %q, want %q", c.ToolName, expectedTool)
					}
				}
			}
			if !found {
				rt.Fatalf("parsed conditions %+v do not contain expected indicator=%s operator=%s",
					conditions, gen.Indicator, gen.Operator)
			}
		} else {
			prompt := genUnparseablePrompt().Draw(rt, "prompt")
			conditions := TryParse(prompt)

			if len(conditions) != 0 {
				rt.Fatalf("expected no conditions from unparseable prompt %q, got %+v", prompt, conditions)
			}
		}
	})
}


// Feature: agentfi-go-backend, Property 13: Fast Path skip on unmet condition
// For any Agent with a parseable Fast Path condition, when the condition
// evaluates to false, the Agent Loop SHALL not invoke the LLM. We verify this
// by checking that Evaluate returns false when the MCP data does not satisfy
// the condition.
// Validates: Requirements 5.3
func TestPropertyFastPathSkipOnUnmetCondition(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ind := genIndicator().Draw(rt, "indicator")
		op := genOperator().Draw(rt, "operator")
		threshold := rapid.Float64Range(1, 10000).Draw(rt, "threshold")

		// Build a single condition directly.
		toolName := indicatorTools[ind]
		cond := Condition{
			Indicator: ind,
			Operator:  op,
			Threshold: threshold,
			ToolName:  toolName,
			ToolArgs:  map[string]any{},
		}

		// Generate an MCP value that does NOT satisfy the condition.
		var mcpValue float64
		switch op {
		case "<":
			// condition: value < threshold → violate by value >= threshold
			mcpValue = threshold + rapid.Float64Range(0, 10000).Draw(rt, "offset")
		case ">":
			// condition: value > threshold → violate by value <= threshold
			mcpValue = threshold - rapid.Float64Range(0, 10000).Draw(rt, "offset")
		case "<=":
			// condition: value <= threshold → violate by value > threshold
			mcpValue = threshold + rapid.Float64Range(0.01, 10000).Draw(rt, "offset")
		case ">=":
			// condition: value >= threshold → violate by value < threshold
			mcpValue = threshold - rapid.Float64Range(0.01, 10000).Draw(rt, "offset")
		case "==":
			// condition: value == threshold → violate by value != threshold
			mcpValue = threshold + rapid.Float64Range(1, 10000).Draw(rt, "offset")
		}

		// Mock MCP tool caller that returns our controlled value.
		mockCallTool := func(_ context.Context, toolName string, args map[string]any) (any, error) {
			return mcpValue, nil
		}

		result, err := Evaluate(context.Background(), []Condition{cond}, mockCallTool)
		if err != nil {
			rt.Fatalf("Evaluate returned unexpected error: %v", err)
		}

		if result {
			rt.Fatalf("expected Evaluate=false (skip cycle) for unmet condition: %s %s %f, mcpValue=%f",
				ind, op, threshold, mcpValue)
		}
	})
}
