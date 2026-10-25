package protocoltypes

import (
	"encoding/json"
	"testing"
	"time"
)

// Characterization tests for the wire format that picoclaw persists to disk.
//
// protocoltypes.Message is written verbatim into every
// `<workspace>/sessions/<key>.jsonl` line, so these shapes are a stored-data
// contract, not an internal detail: a reimplementation that serializes the same
// value differently produces files the other implementation cannot read
// faithfully, and there is no migration step that can detect it.
//
// The traps pinned here are the ones a hand-written serde port is most likely
// to get wrong, because each is a place where Go's encoding/json does something
// other than "omit if empty":
//
//   - Message.role and Message.content have no omitempty: they are always
//     present, even when empty.
//   - LLMResponse.reasoning and LLMResponse.reasoning_details have no omitempty
//     either, so a nil slice serializes as JSON null, not as an absent key.
//   - ToolDefinition.function.parameters has no omitempty: null, not absent.
//   - ToolCall.Name, ToolCall.Arguments and ToolCall.ThoughtSignature are
//     json:"-": in-memory only, silently dropped on write.
//   - Message/ContentBlock/ToolDefinition prompt metadata is json:"-".
//   - Message.CreatedAt is *time.Time, which encoding/json writes as
//     RFC 3339 with nanoseconds and trailing zeros removed.
//   - Decoding is case-insensitive on field names (encoding/json), so real
//     world files carry "Role" and "CONTENT".

func TestCompat_MessageWireFormat(t *testing.T) {
	tests := []struct {
		name string
		in   Message
		want string
	}{
		{
			name: "zero value still emits role and content",
			in:   Message{},
			want: `{"role":"","content":""}`,
		},
		{
			name: "user message",
			in:   Message{Role: "user", Content: "hi"},
			want: `{"role":"user","content":"hi"}`,
		},
		{
			name: "field order is struct declaration order",
			in: Message{
				Role:             "assistant",
				Content:          "c",
				ModelName:        "gpt-x",
				ReasoningContent: "thinking",
				ToolCallID:       "tc-1",
			},
			want: `{"role":"assistant","content":"c","model_name":"gpt-x",` +
				`"reasoning_content":"thinking","tool_call_id":"tc-1"}`,
		},
		{
			name: "empty slices are omitted, not emitted as []",
			in:   Message{Role: "user", Content: "x", Media: []string{}, Attachments: []Attachment{}, ToolCalls: []ToolCall{}},
			want: `{"role":"user","content":"x"}`,
		},
		{
			name: "empty tool call is emitted as an element with its own empty strings",
			in:   Message{Role: "assistant", ToolCalls: []ToolCall{{}}},
			want: `{"role":"assistant","content":"","tool_calls":[{"id":""}]}`,
		},
		{
			name: "tool call flattening fields never reach the wire",
			in: Message{Role: "assistant", ToolCalls: []ToolCall{{
				ID:               "tc-1",
				Type:             "function",
				Name:             "exec",
				Arguments:        map[string]any{"cmd": "ls"},
				ThoughtSignature: "sig",
				Function:         &FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
			}}},
			want: `{"role":"assistant","content":"","tool_calls":[{"id":"tc-1","type":"function",` +
				`"function":{"name":"exec","arguments":"{\"cmd\":\"ls\"}"}}]}`,
		},
		{
			name: "function thought signature is emitted, tool call thought signature is not",
			in: Message{ToolCalls: []ToolCall{{
				ID:               "tc-1",
				ThoughtSignature: "dropped",
				Function:         &FunctionCall{Name: "f", Arguments: "{}", ThoughtSignature: "kept"},
			}}},
			want: `{"role":"","content":"","tool_calls":[{"id":"tc-1",` +
				`"function":{"name":"f","arguments":"{}","thought_signature":"kept"}}]}`,
		},
		{
			name: "prompt metadata is in-memory only",
			in: Message{
				Role:         "system",
				Content:      "p",
				PromptLayer:  "core",
				PromptSlot:   "identity",
				PromptSource: "AGENT.md",
			},
			want: `{"role":"system","content":"p"}`,
		},
		{
			name: "system parts carry cache control but not prompt metadata",
			in: Message{Role: "system", SystemParts: []ContentBlock{{
				Type:         "text",
				Text:         "a",
				CacheControl: &CacheControl{Type: "ephemeral"},
				PromptLayer:  "core",
			}}},
			want: `{"role":"system","content":"","system_parts":[{"type":"text","text":"a",` +
				`"cache_control":{"type":"ephemeral"}}]}`,
		},
		{
			name: "attachments omit every unset field",
			in:   Message{Attachments: []Attachment{{Ref: "media://abc"}, {Type: "image", URL: "https://x/y"}}},
			want: `{"role":"","content":"","attachments":[{"ref":"media://abc"},` +
				`{"type":"image","url":"https://x/y"}]}`,
		},
		{
			name: "extra_content google thought signature",
			in: Message{ToolCalls: []ToolCall{{
				ID:           "tc-1",
				ExtraContent: &ExtraContent{Google: &GoogleExtra{ThoughtSignature: "gs"}},
			}}},
			want: `{"role":"","content":"","tool_calls":[{"id":"tc-1",` +
				`"extra_content":{"google":{"thought_signature":"gs"}}}]}`,
		},
		{
			name: "extra_content tool feedback explanation",
			in: Message{ToolCalls: []ToolCall{{
				ID:           "tc-1",
				ExtraContent: &ExtraContent{ToolFeedbackExplanation: "reading file"},
			}}},
			want: `{"role":"","content":"","tool_calls":[{"id":"tc-1",` +
				`"extra_content":{"tool_feedback_explanation":"reading file"}}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("wire mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestCompat_LLMResponseWireFormat(t *testing.T) {
	got, err := json.Marshal(LLMResponse{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// reasoning and reasoning_details have no omitempty: a nil slice is JSON
	// null, and both keys are always present.
	want := `{"content":"","finish_reason":"","reasoning":"","reasoning_details":null}`
	if string(got) != want {
		t.Errorf("zero-value LLMResponse\n got: %s\nwant: %s", got, want)
	}

	got, err = json.Marshal(LLMResponse{
		Content:      "ok",
		FinishReason: "stop",
		Usage:        &UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want = `{"content":"ok","finish_reason":"stop","usage":{"prompt_tokens":1,` +
		`"completion_tokens":2,"total_tokens":3},"reasoning":"","reasoning_details":null}`
	if string(got) != want {
		t.Errorf("LLMResponse with usage\n got: %s\nwant: %s", got, want)
	}

	// An empty (non-nil) slice is distinct from nil: [] rather than null.
	got, err = json.Marshal(LLMResponse{ReasoningDetails: []ReasoningDetail{}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want = `{"content":"","finish_reason":"","reasoning":"","reasoning_details":[]}`; string(got) != want {
		t.Errorf("empty vs nil reasoning_details\n got: %s\nwant: %s", got, want)
	}
}

func TestCompat_ToolDefinitionWireFormat(t *testing.T) {
	got, err := json.Marshal(ToolDefinition{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// parameters has no omitempty, so an unset schema is null rather than absent.
	// Providers that validate the tools array reject a missing parameters key,
	// which is why this must stay null and not be skipped.
	want := `{"type":"","function":{"name":"","description":"","parameters":null}}`
	if string(got) != want {
		t.Errorf("zero-value ToolDefinition\n got: %s\nwant: %s", got, want)
	}

	got, err = json.Marshal(ToolDefinition{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:        "exec",
			Description: "run",
			Parameters:  map[string]any{"type": "object"},
		},
		PromptLayer: "capability",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want = `{"type":"function","function":{"name":"exec","description":"run",` +
		`"parameters":{"type":"object"}}}`
	if string(got) != want {
		t.Errorf("ToolDefinition\n got: %s\nwant: %s", got, want)
	}
}

func TestCompat_CreatedAtTimestampFormat(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "UTC whole second has no fractional part",
			in:   time.Date(2026, 10, 25, 12, 30, 0, 0, time.UTC),
			want: `"2026-10-25T12:30:00Z"`,
		},
		{
			name: "nanoseconds are kept with trailing zeros trimmed",
			in:   time.Date(2026, 10, 25, 12, 30, 0, 123456700, time.UTC),
			want: `"2026-10-25T12:30:00.1234567Z"`,
		},
		{
			name: "exactly one millisecond is three digits, not nine",
			in:   time.Date(2026, 10, 25, 12, 30, 0, 1_000_000, time.UTC),
			want: `"2026-10-25T12:30:00.001Z"`,
		},
		{
			name: "non-UTC offsets are preserved, not normalized to Z",
			in:   time.Date(2026, 10, 25, 12, 30, 0, 0, time.FixedZone("CST", 8*3600)),
			want: `"2026-10-25T12:30:00+08:00"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := tt.in
			got, err := json.Marshal(Message{Role: "user", Content: "x", CreatedAt: &ts})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want := `{"role":"user","content":"x","created_at":` + tt.want + `}`
			if string(got) != want {
				t.Errorf("\n got: %s\nwant: %s", got, want)
			}

			// Round trip must preserve the instant even when the textual offset
			// is not reproduced byte for byte.
			var back Message
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back.CreatedAt == nil || !back.CreatedAt.Equal(ts) {
				t.Errorf("round trip lost the instant: %v", back.CreatedAt)
			}
		})
	}
}

func TestCompat_DecodingIsCaseInsensitive(t *testing.T) {
	// encoding/json matches field names case-insensitively, so files written by
	// other tools (and by hand-edited configs) legitimately use Go-style keys.
	// serde is case-sensitive by default; a port needs explicit aliases.
	raw := `{"Role":"user","CONTENT":"hi","Model_Name":"m","Tool_Call_ID":"tc"}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m.Role != "user" || m.Content != "hi" || m.ModelName != "m" || m.ToolCallID != "tc" {
		t.Errorf("case-insensitive decode changed: %+v", m)
	}
}

func TestCompat_UnknownFieldsAreIgnored(t *testing.T) {
	// Forward compatibility: newer writers add keys, older readers must not fail.
	raw := `{"role":"user","content":"hi","a_key_from_the_future":{"nested":[1,2]}}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("Unmarshal must not fail on unknown keys: %v", err)
	}
	if m.Role != "user" || m.Content != "hi" {
		t.Errorf("decoded wrong: %+v", m)
	}
}

func TestCompat_SentinelAndNullHandling(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":null,"content":null}`), &m); err != nil {
		t.Fatalf("null into string must succeed: %v", err)
	}
	if m.Role != "" || m.Content != "" {
		t.Errorf("JSON null must decode to the empty string, got %+v", m)
	}

	// A null created_at is indistinguishable from an absent one.
	if err := json.Unmarshal([]byte(`{"created_at":null}`), &m); err != nil {
		t.Fatalf("null into *time.Time: %v", err)
	}
	if m.CreatedAt != nil {
		t.Errorf("null created_at must decode to nil, got %v", m.CreatedAt)
	}
}

func TestCompat_RoundTripIsStable(t *testing.T) {
	// decode(encode(x)) must reproduce x for every shape a session file can
	// hold, otherwise a second save rewrites history.
	original := Message{
		Role:             "assistant",
		Content:          "done",
		ModelName:        "claude-x",
		ReasoningContent: "hm",
		Media:            []string{"media://a"},
		Attachments:      []Attachment{{Type: "file", Ref: "media://b", Filename: "n.txt"}},
		SystemParts:      []ContentBlock{{Type: "text", Text: "s", CacheControl: &CacheControl{Type: "ephemeral"}}},
		ToolCalls: []ToolCall{{
			ID:       "tc-1",
			Type:     "function",
			Function: &FunctionCall{Name: "exec", Arguments: `{"cmd":"ls -la"}`},
		}},
	}

	first, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	second, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("round trip is not idempotent\nfirst:  %s\nsecond: %s", first, second)
	}
}
