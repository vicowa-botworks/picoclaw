package tokenizer

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// These are characterization tests: they pin the exact integers the estimator
// produces today so that a reimplementation in another language can be checked
// against the same numbers instead of against a re-derivation of the formula.
//
// The estimator is not a pure "characters / 4" heuristic and the deviations are
// load-bearing. They decide when the agent compacts context, so a port that
// "simplifies" them changes when every long conversation truncates:
//
//   - Content, ReasoningContent and SystemParts[].Text are counted in RUNES.
//   - ToolCall.ID, ToolCall.Type, Function.Name, Function.Arguments and
//     ToolCallID are counted in BYTES (Go len()).
//   - messageOverhead (12) is added to the character total BEFORE scaling.
//   - Scaling is integer arithmetic: chars*2/5, i.e. floor, not round.
//   - Media is added AFTER scaling, as a flat 256 per item.
//   - SystemParts are an ALTERNATIVE to Content (max of the two), never additive,
//     and carry a 20-per-part structural overhead.

func TestCompat_EstimateMessageTokens_ExactValues(t *testing.T) {
	tests := []struct {
		name string
		msg  providers.Message
		want int
	}{
		{
			name: "empty message still carries the 12 char envelope",
			msg:  providers.Message{},
			// (0 + 12) * 2 / 5 = 4
			want: 4,
		},
		{
			name: "plain content",
			msg:  providers.Message{Role: "user", Content: "hello world"},
			// (11 + 12) * 2 / 5 = 9
			want: 9,
		},
		{
			name: "scaling truncates downward",
			msg:  providers.Message{Role: "user", Content: strings.Repeat("x", 13)},
			// (13 + 12) * 2 / 5 = 10 (25*2/5 = 10)
			want: 10,
		},
		{
			name: "one ASCII char below a truncation boundary",
			msg:  providers.Message{Role: "user", Content: strings.Repeat("x", 11)},
			// (11 + 12) * 2 / 5 = 9 (23*2/5 = 9.2 -> 9)
			want: 9,
		},
		{
			name: "content is counted in runes, not bytes",
			msg:  providers.Message{Role: "user", Content: "你好世界"},
			// 4 runes but 12 UTF-8 bytes: (4 + 12) * 2 / 5 = 6
			want: 6,
		},
		{
			name: "astral plane emoji count as one rune each",
			msg:  providers.Message{Role: "user", Content: "🦐🦀"},
			// 2 runes, 8 bytes: (2 + 12) * 2 / 5 = 5
			want: 5,
		},
		{
			name: "reasoning content is additive with content",
			msg:  providers.Message{Role: "assistant", Content: "abc", ReasoningContent: "defg"},
			// (3 + 4 + 12) * 2 / 5 = 7
			want: 7,
		},
		{
			name: "tool call id and type are counted in bytes",
			msg: providers.Message{
				Role: "assistant",
				ToolCalls: []protocoltypes.ToolCall{{
					ID:   "call_abc",
					Type: "function",
					Function: &protocoltypes.FunctionCall{
						Name:      "exec",
						Arguments: `{"cmd":"ls"}`,
					},
				}},
			},
			// len("call_abc")=8 + len("function")=8 + len("exec")=4
			// + len(`{"cmd":"ls"}`)=12 = 32; (32 + 12) * 2 / 5 = 17
			want: 17,
		},
		{
			name: "function name wins over the flattened name, never both",
			msg: providers.Message{
				Role: "assistant",
				ToolCalls: []protocoltypes.ToolCall{{
					ID:       "call_abc",
					Type:     "function",
					Name:     "this_name_is_long_and_must_not_be_counted",
					Function: &protocoltypes.FunctionCall{Name: "exec", Arguments: "{}"},
				}},
			},
			// 8 + 8 + len("exec")=4 + len("{}")=2 = 22; (22 + 12) * 2 / 5 = 13
			// Counting the flattened Name as well would give (67+12)*2/5 = 31.
			want: 13,
		},
		{
			name: "flattened name is used when Function is absent",
			msg: providers.Message{
				Role: "assistant",
				ToolCalls: []protocoltypes.ToolCall{{
					ID:   "call_abc",
					Type: "function",
					Name: "exec",
				}},
			},
			// 8 + 8 + len("exec")=4 = 20; (20 + 12) * 2 / 5 = 12
			want: 12,
		},
		{
			name: "tool call id is counted once on the message envelope",
			msg: providers.Message{
				Role:       "tool",
				Content:    "result",
				ToolCallID: "call_abc",
			},
			// len("result")=6 + len("call_abc")=8 = 14; (14 + 12) * 2 / 5 = 10
			want: 10,
		},
		{
			name: "media is a flat per-item cost added after scaling",
			msg: providers.Message{
				Role:  "user",
				Media: []string{"media://11111111-1111-1111-1111-111111111111", "media://22222222-2222-2222-2222-222222222222"},
			},
			// (0 + 12) * 2 / 5 = 4, then + 2*256 = 516
			want: 516,
		},
		{
			name: "system parts replace content rather than adding to it",
			msg: providers.Message{
				Role:    "system",
				Content: "short",
				SystemParts: []protocoltypes.ContentBlock{
					{Type: "text", Text: strings.Repeat("a", 100)},
					{Type: "text", Text: strings.Repeat("b", 100)},
				},
			},
			// parts = 200 + 2*20 = 240 > content 5 -> chars = 240
			// (240 + 12) * 2 / 5 = 100
			want: 100,
		},
		{
			name: "content wins when it is larger than the system parts",
			msg: providers.Message{
				Role:        "system",
				Content:     strings.Repeat("a", 100),
				SystemParts: []protocoltypes.ContentBlock{{Type: "text", Text: "short"}},
			},
			// parts = 5 + 20 = 25 < content 100 -> chars = 100
			// (100 + 12) * 2 / 5 = 44
			want: 44,
		},
		{
			name: "system parts are rune counted too",
			msg: providers.Message{
				Role:        "system",
				SystemParts: []protocoltypes.ContentBlock{{Type: "text", Text: "你好世界"}},
			},
			// parts = 4 runes + 20 = 24; (24 + 12) * 2 / 5 = 14
			want: 14,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateMessageTokens(tt.msg); got != tt.want {
				t.Errorf("EstimateMessageTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestCompat_EstimateMessageTokens_RuneVsByteAsymmetry is the single most
// likely place for a port to diverge: the same function counts one field in
// runes and the next in bytes. A port that normalizes either direction changes
// every multibyte conversation's compaction point.
func TestCompat_EstimateMessageTokens_RuneVsByteAsymmetry(t *testing.T) {
	// 8 multibyte runes = 24 bytes. Rune counting gives 8, byte counting 24.
	content := "你好世界你好世界"
	if got, want := len(content), 24; got != want {
		t.Fatalf("fixture changed: len(%q) = %d bytes, want %d", content, got, want)
	}

	contentOnly := EstimateMessageTokens(providers.Message{Content: content})
	// Rune counted: (8 + 12) * 2 / 5 = 8. Byte counted would be (24+12)*2/5 = 14.
	if want := 8; contentOnly != want {
		t.Errorf("Content must be RUNE counted: got %d tokens, want %d", contentOnly, want)
	}

	// The identical 24 bytes inside a tool-call argument are BYTE counted.
	args := `{"q":"你好世界你好世界"}`
	withArgs := EstimateMessageTokens(providers.Message{
		ToolCalls: []protocoltypes.ToolCall{{
			Function: &protocoltypes.FunctionCall{Name: "s", Arguments: args},
		}},
	})
	// len(args) = 29 bytes (24 for the payload + 5 for {"q":""} ... see below),
	// + len("s") = 1. Asserting the byte count directly keeps the test honest
	// if the fixture is ever edited.
	want := (len(args) + 1 + 12) * 2 / 5
	if withArgs != want {
		t.Errorf("Function.Arguments must be BYTE counted: got %d tokens, want %d", withArgs, want)
	}

	if contentOnly == withArgs {
		t.Error("rune/byte asymmetry has disappeared: content and arguments now count identically")
	}
}

func TestCompat_EstimateToolDefsTokens_ExactValues(t *testing.T) {
	tests := []struct {
		name string
		defs []providers.ToolDefinition
		want int
	}{
		{
			name: "no definitions is exactly zero",
			defs: nil,
			want: 0,
		},
		{
			name: "name, description and serialized parameters plus 20 overhead",
			defs: []providers.ToolDefinition{{
				Type: "function",
				Function: providers.ToolFunctionDefinition{
					Name:        "exec",
					Description: "run a command",
					Parameters:  map[string]any{"type": "object"},
				},
			}},
			// len("exec")=4 + len("run a command")=13
			// + len(`{"type":"object"}`)=17 + 20 = 54; 54*2/5 = 21
			want: 21,
		},
		{
			name: "nil parameters contribute nothing",
			defs: []providers.ToolDefinition{{
				Function: providers.ToolFunctionDefinition{Name: "a", Description: "b"},
			}},
			// 1 + 1 + 20 = 22; 22*2/5 = 8
			want: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateToolDefsTokens(tt.defs); got != tt.want {
				t.Errorf("EstimateToolDefsTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestCompat_EstimateToolDefsTokens_MapKeyOrder documents that the estimate is
// stable for a given schema. encoding/json sorts map keys, so the serialized
// length — and therefore the token estimate — does not depend on insertion
// order. A port using an order-preserving JSON map must sort explicitly or the
// same schema will estimate differently on two runs.
func TestCompat_EstimateToolDefsTokens_MapKeyOrder(t *testing.T) {
	a := providers.ToolDefinition{Function: providers.ToolFunctionDefinition{
		Name:       "t",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"z": 1, "a": 2, "m": 3}},
	}}
	b := providers.ToolDefinition{Function: providers.ToolFunctionDefinition{
		Name:       "t",
		Parameters: map[string]any{"properties": map[string]any{"m": 3, "a": 2, "z": 1}, "type": "object"},
	}}

	ga, gb := EstimateToolDefsTokens([]providers.ToolDefinition{a}), EstimateToolDefsTokens([]providers.ToolDefinition{b})
	if ga != gb {
		t.Errorf("estimate depends on map insertion order: %d vs %d", ga, gb)
	}
}
