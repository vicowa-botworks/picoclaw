package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// This file builds the corpus for the provider wire types.
//
// The seam is json.Marshal over protocoltypes structs, because that output is
// what providers receive and what session files contain. Pinning it matters for
// a reason that is easy to miss: these structs mix `omitempty` with bare fields,
// so "absent", `null`, `[]` and `""` are four different bytes and each is
// produced by a different Go value. A port that collapses any two of them
// produces JSON a provider still accepts -- and session files the next release
// reads back differently.
//
// The input is therefore NOT the wire format. It is a description of the Go
// value, written so that nil and empty stay distinguishable: json.Marshal drops
// both `nil` and `[]` for an `omitempty` slice, so a fixture expressed as wire
// JSON could not tell a port which of the two a case was testing.

// wireCase is one (described value, marshalled output) pair.
func wireCase[T any](name, kind string, value T, describe func(T) any) fixtureCase {
	return fixtureCase{
		Name:  name,
		Input: map[string]any{"kind": kind, "value": describe(value)},
		wire:  value,
	}
}

// ---------------------------------------------------------------- descriptions --

func describeTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	// Unix seconds plus the sub-second nanoseconds plus the zone's offset,
	// rather than a pre-formatted string. Go marshals time.Time as RFC3339Nano,
	// which trims trailing zeros from the fractional second -- .100000000 becomes
	// .1, and an exact second gets no fraction at all. Handing over the finished
	// string would let a port copy it and never exercise the formatting that most
	// often differs.
	//
	// Seconds and nanoseconds are separate rather than one nanosecond count
	// because UnixNano is only defined between 1678 and 2262, and the zero
	// time.Time -- which session files do contain, from records written before a
	// timestamp was populated -- is year 1. Unix() and Nanosecond() have no such
	// limit; Nanosecond() is always [0, 1e9) and the instant is
	// seconds + nanoseconds, including before 1970.
	//
	// Offset seconds rather than a name: the name is not reproducible without a
	// tz database, and MarshalJSON writes the offset, never the name.
	_, offset := t.Zone()
	return map[string]any{
		"unix_seconds":   t.Unix(),
		"nanos":          t.Nanosecond(),
		"offset_seconds": offset,
	}
}

func describeMessage(m protocoltypes.Message) any {
	return map[string]any{
		"role":              m.Role,
		"content":           m.Content,
		"model_name":        m.ModelName,
		"created_at":        describeTime(m.CreatedAt),
		"media":             describeStrings(m.Media),
		"attachments":       describeSlice(m.Attachments, describeAttachment),
		"reasoning_content": m.ReasoningContent,
		"system_parts":      describeSlice(m.SystemParts, describeContentBlock),
		"tool_calls":        describeSlice(m.ToolCalls, describeToolCall),
		"tool_call_id":      m.ToolCallID,
		// json:"-" fields. They must never reach the output, so they are set to
		// values that would be obvious if they did.
		"prompt_layer":  m.PromptLayer,
		"prompt_slot":   m.PromptSlot,
		"prompt_source": m.PromptSource,
	}
}

func describeToolCall(tc protocoltypes.ToolCall) any {
	return map[string]any{
		"id":   tc.ID,
		"type": tc.Type,
		// Arguments and Name are json:"-" even though the provider wire has
		// near-identical fields; a port that maps them by name leaks them.
		"arguments":         portable(tc.Arguments),
		"name":              tc.Name,
		"thought_signature": tc.ThoughtSignature,
		"function":          describePointer(tc.Function, describeFunctionCall),
		"extra_content":     describePointer(tc.ExtraContent, describeExtraContent),
	}
}

func describeFunctionCall(f protocoltypes.FunctionCall) any {
	return map[string]any{
		"name":              f.Name,
		"arguments":         f.Arguments,
		"thought_signature": f.ThoughtSignature,
	}
}

func describeExtraContent(e protocoltypes.ExtraContent) any {
	return map[string]any{
		"google": describePointer(e.Google, func(g protocoltypes.GoogleExtra) any {
			return map[string]any{"thought_signature": g.ThoughtSignature}
		}),
		"tool_feedback_explanation": e.ToolFeedbackExplanation,
	}
}

func describeContentBlock(b protocoltypes.ContentBlock) any {
	return map[string]any{
		"type": b.Type,
		"text": b.Text,
		"cache_control": describePointer(b.CacheControl, func(c protocoltypes.CacheControl) any {
			return map[string]any{"type": c.Type}
		}),
		"prompt_layer":  b.PromptLayer,
		"prompt_slot":   b.PromptSlot,
		"prompt_source": b.PromptSource,
	}
}

func describeAttachment(a protocoltypes.Attachment) any {
	return map[string]any{
		"type":         a.Type,
		"ref":          a.Ref,
		"url":          a.URL,
		"filename":     a.Filename,
		"content_type": a.ContentType,
	}
}

func describeLLMResponse(r protocoltypes.LLMResponse) any {
	return map[string]any{
		"content":           r.Content,
		"reasoning_content": r.ReasoningContent,
		"tool_calls":        describeSlice(r.ToolCalls, describeToolCall),
		"finish_reason":     r.FinishReason,
		"usage": describePointer(r.Usage, func(u protocoltypes.UsageInfo) any {
			return map[string]any{
				"prompt_tokens":     u.PromptTokens,
				"completion_tokens": u.CompletionTokens,
				"total_tokens":      u.TotalTokens,
			}
		}),
		// Reasoning and ReasoningDetails carry no omitempty, so they are always
		// emitted -- and a nil ReasoningDetails emits `null`, not `[]`.
		"reasoning":         r.Reasoning,
		"reasoning_details": describeSliceAny(r.ReasoningDetails),
	}
}

func describeToolDefinition(d protocoltypes.ToolDefinition) any {
	return map[string]any{
		"type": d.Type,
		"function": map[string]any{
			"name":        d.Function.Name,
			"description": d.Function.Description,
			// No omitempty: a nil map is `null`, an empty map is `{}`.
			"parameters": portable(d.Function.Parameters),
		},
		"prompt_layer":  d.PromptLayer,
		"prompt_slot":   d.PromptSlot,
		"prompt_source": d.PromptSource,
	}
}

func describeStreamChunk(c protocoltypes.StreamChunk) any {
	// StreamChunk has no json tags at all, so its keys are the Go field names
	// with their original capitalisation.
	return map[string]any{
		"Content":          c.Content,
		"ReasoningContent": c.ReasoningContent,
	}
}

func describeReasoningDetail(d protocoltypes.ReasoningDetail) any {
	return map[string]any{
		"format": d.Format,
		"index":  d.Index,
		"type":   d.Type,
		"text":   d.Text,
	}
}

// describeStrings keeps nil distinct from empty. Both are `null`-vs-`[]` in the
// description because that is exactly the distinction the wire type has to make
// again on the other side.
func describeStrings(s []string) any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func describeSlice[T any](s []T, describe func(T) any) any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = describe(v)
	}
	return out
}

// describeSliceAny is describeSlice for a field whose element type is already
// described by a method value above; it exists so nil vs empty survives for
// ReasoningDetails without a second helper name.
func describeSliceAny[T any](s []T) any {
	return describeSlice(s, func(v T) any { return portable(v) })
}

func describePointer[T any](p *T, describe func(T) any) any {
	if p == nil {
		return nil
	}
	return describe(*p)
}

// ----------------------------------------------------------------------- cases --

// wireCases threads the generator's -seed and -fuzz the way every other seam
// does: the seed is XORed with a per-seam constant so the four corpora do not
// fuzz in lockstep, and the fuzz count is scaled from -fuzz rather than ignored.
// The scale is 200/1500 -- the wire seam's share of the default -fuzz 1500 -- so
// the default run is byte-identical to the committed fixture while -seed and
// -fuzz actually change this file. It must be threaded: the envelope records
// `seed` for the whole run, and a file generated from a hardcoded seed while
// claiming the flag's value is a provenance field that lies.
func wireCases(seed int64, fuzz int) []fixtureCase {
	var out []fixtureCase

	msg := func(name string, m protocoltypes.Message) {
		out = append(out, wireCase(name, "message", m, describeMessage))
	}
	resp := func(name string, r protocoltypes.LLMResponse) {
		out = append(out, wireCase(name, "llm_response", r, describeLLMResponse))
	}
	def := func(name string, d protocoltypes.ToolDefinition) {
		out = append(out, wireCase(name, "tool_definition", d, describeToolDefinition))
	}

	// -- the zero value of every type. Not filler: with omitempty everywhere,
	// the zero Message is the one document shape that shows which fields are
	// mandatory on the wire.
	msg("message_zero", protocoltypes.Message{})
	resp("response_zero", protocoltypes.LLMResponse{})
	def("tooldef_zero", protocoltypes.ToolDefinition{})
	out = append(out, wireCase("chunk_zero", "stream_chunk", protocoltypes.StreamChunk{}, describeStreamChunk))
	out = append(out, wireCase("block_zero", "content_block", protocoltypes.ContentBlock{}, describeContentBlock))
	out = append(out, wireCase("attach_zero", "attachment", protocoltypes.Attachment{}, describeAttachment))
	out = append(out, wireCase("detail_zero", "reasoning_detail", protocoltypes.ReasoningDetail{}, describeReasoningDetail))

	// -- nil versus empty, for every slice and map.
	msg("message_media_nil", protocoltypes.Message{Role: "user", Content: "x"})
	msg("message_media_empty", protocoltypes.Message{Role: "user", Content: "x", Media: []string{}})
	msg("message_media_one", protocoltypes.Message{Role: "user", Content: "x", Media: []string{"/tmp/a.png"}})
	msg("message_parts_empty", protocoltypes.Message{Role: "system", SystemParts: []protocoltypes.ContentBlock{}})
	msg("message_parts_nil", protocoltypes.Message{Role: "system"})
	msg("message_calls_empty", protocoltypes.Message{Role: "assistant", ToolCalls: []protocoltypes.ToolCall{}})
	msg("message_attach_empty", protocoltypes.Message{Role: "user", Attachments: []protocoltypes.Attachment{}})
	def("tooldef_params_nil", protocoltypes.ToolDefinition{Type: "function"})
	def("tooldef_params_empty", protocoltypes.ToolDefinition{
		Type:     "function",
		Function: protocoltypes.ToolFunctionDefinition{Parameters: map[string]any{}},
	})
	// Both carry content so the pair differs by exactly the field under test.
	// LLMResponse{} on its own is byte-identical to response_zero, which made
	// the "nil vs empty" case a duplicate that pinned nothing.
	resp("response_details_nil", protocoltypes.LLMResponse{Content: "x"})
	resp("response_details_empty", protocoltypes.LLMResponse{Content: "x", ReasoningDetails: []protocoltypes.ReasoningDetail{}})
	resp("response_details_one", protocoltypes.LLMResponse{
		ReasoningDetails: []protocoltypes.ReasoningDetail{{Format: "plain", Index: 0, Type: "text", Text: "thinking"}},
	})

	// -- json:"-" fields populated. If any of these appear in the output, the
	// port mapped by field name instead of by tag.
	msg("message_prompt_meta_hidden", protocoltypes.Message{
		Role: "system", Content: "s",
		PromptLayer: "LAYER_INTERNAL", PromptSlot: "SLOT_INTERNAL", PromptSource: "src.go:1",
	})
	msg("message_toolcall_hidden_fields", protocoltypes.Message{
		Role: "assistant",
		ToolCalls: []protocoltypes.ToolCall{{
			ID: "call_1", Type: "function",
			Function:         &protocoltypes.FunctionCall{Name: "lookup", Arguments: `{"q":"x"}`},
			Name:             "HIDDEN_NAME",
			Arguments:        map[string]any{"HIDDEN": "ARGUMENTS"},
			ThoughtSignature: "HIDDEN_SIG",
		}},
	})
	msg("message_block_hidden_fields", protocoltypes.Message{
		Role: "system",
		SystemParts: []protocoltypes.ContentBlock{{
			Type: "text", Text: "t",
			PromptLayer: "LAYER_HIDDEN", PromptSlot: "SLOT_HIDDEN", PromptSource: "HIDDEN",
		}},
	})
	def("tooldef_prompt_meta_hidden", protocoltypes.ToolDefinition{
		Type:        "function",
		Function:    protocoltypes.ToolFunctionDefinition{Name: "n", Description: "d"},
		PromptLayer: "LAYER_HIDDEN", PromptSlot: "SLOT_HIDDEN", PromptSource: "HIDDEN",
	})

	// -- pointers: absent, present-and-empty, present-and-set.
	msg("message_created_at_nil", protocoltypes.Message{Role: "user"})
	msg("message_toolcall_function_empty", protocoltypes.Message{
		Role:      "assistant",
		ToolCalls: []protocoltypes.ToolCall{{ID: "c", Function: &protocoltypes.FunctionCall{}}},
	})
	msg("message_extra_content", protocoltypes.Message{
		Role:      "assistant",
		ToolCalls: []protocoltypes.ToolCall{{ID: "c", ExtraContent: &protocoltypes.ExtraContent{}}},
	})
	msg("message_extra_content_google", protocoltypes.Message{
		Role: "assistant",
		ToolCalls: []protocoltypes.ToolCall{{ID: "c", ExtraContent: &protocoltypes.ExtraContent{
			Google:                  &protocoltypes.GoogleExtra{ThoughtSignature: "sig"},
			ToolFeedbackExplanation: "why",
		}}},
	})
	msg("message_cache_control", protocoltypes.Message{
		Role: "system",
		SystemParts: []protocoltypes.ContentBlock{{
			Type: "text", Text: "t", CacheControl: &protocoltypes.CacheControl{Type: "ephemeral"},
		}},
	})
	resp("response_usage", protocoltypes.LLMResponse{
		Content: "hi", FinishReason: "stop",
		Usage: &protocoltypes.UsageInfo{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
	})
	resp("response_usage_zero", protocoltypes.LLMResponse{
		Usage: &protocoltypes.UsageInfo{},
	})

	// -- time formatting. RFC3339Nano trims trailing zeros; a fixed-width
	// formatter is wrong here in a way that survives every happy-path test.
	at := func(nanos int64, offset int) *time.Time {
		t := time.Unix(0, nanos).In(time.FixedZone("x", offset))
		return &t
	}
	msg("message_time_whole_second_utc", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second), 0)})
	msg("message_time_milli", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second)+1e6, 0)})
	msg("message_time_nano_trailing_zeros", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second)+100e6, 0)})
	msg("message_time_nano_full", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second)+123456789, 0)})
	// A whole-hour zone, which is what the name says. It was +05:30, byte-identical
	// to message_time_offset_half_hour below it.
	msg("message_time_offset_positive", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second), 3600)})
	msg("message_time_offset_negative", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second), -25200)})
	msg("message_time_offset_half_hour", protocoltypes.Message{CreatedAt: at(1700000000*int64(time.Second), 19800)})
	msg("message_time_before_epoch", protocoltypes.Message{CreatedAt: at(-86400*int64(time.Second), 0)})
	// The zero time.Time. Session files carry it: it is what a record written
	// before CreatedAt was populated looks like. It is also outside the range
	// UnixNano can express, so a port that derives the instant from a nanosecond
	// count cannot even represent the input.
	zero := time.Time{}
	msg("message_time_zero_value", protocoltypes.Message{CreatedAt: &zero})
	msg("message_time_zero_value_offset", protocoltypes.Message{
		CreatedAt: func() *time.Time { z := zero.In(time.FixedZone("z", 3600)); return &z }(),
	})
	msg("message_time_far_future", protocoltypes.Message{
		CreatedAt: func() *time.Time { t := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC); return &t }(),
	})

	// -- field order is part of the contract: it is declaration order, and
	// session files diffed by hand (and by tests that compare strings) depend
	// on it. One case with everything set pins the whole sequence.
	msg("message_all_fields", protocoltypes.Message{
		Role: "assistant", Content: "hello", ModelName: "claude", CreatedAt: at(1700000000*int64(time.Second), 0),
		Media:            []string{"m1", "m2"},
		Attachments:      []protocoltypes.Attachment{{Type: "image", Ref: "r", URL: "u", Filename: "f", ContentType: "image/png"}},
		ReasoningContent: "because",
		SystemParts:      []protocoltypes.ContentBlock{{Type: "text", Text: "p"}},
		ToolCalls: []protocoltypes.ToolCall{{
			ID: "c1", Type: "function",
			Function: &protocoltypes.FunctionCall{Name: "fn", Arguments: "{}", ThoughtSignature: "s"},
		}},
		ToolCallID: "tc1",
	})
	resp("response_all_fields", protocoltypes.LLMResponse{
		Content: "c", ReasoningContent: "rc",
		ToolCalls:    []protocoltypes.ToolCall{{ID: "c", Type: "function"}},
		FinishReason: "tool_calls",
		Usage:        &protocoltypes.UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		Reasoning:    "r",
		ReasoningDetails: []protocoltypes.ReasoningDetail{
			{Format: "plain", Index: 1, Type: "text", Text: "a"},
			{Format: "html", Index: 2, Type: "summary", Text: "b"},
		},
	})

	// -- values that stress the encoder inside these structs.
	msg("message_html_in_strings", protocoltypes.Message{
		Role: "user", Content: "<b>&</b>",
		SystemParts: []protocoltypes.ContentBlock{{Type: "text", Text: "a\u2028b\u2029c"}},
	})
	msg("message_unicode", protocoltypes.Message{
		Role: "user", Content: "한行🦐é \x07\x08\x0c",
	})
	msg("message_function_arguments_raw_json", protocoltypes.Message{
		Role: "assistant",
		ToolCalls: []protocoltypes.ToolCall{{
			ID:       "c",
			Function: &protocoltypes.FunctionCall{Name: "f", Arguments: `{"nested":{"a":[1,2,{"b":null}]}}`},
		}},
	})

	// -- StreamChunk: no tags, so capitalised keys.
	out = append(out, wireCase("chunk_populated", "stream_chunk",
		protocoltypes.StreamChunk{Content: "c", ReasoningContent: "r"}, describeStreamChunk))

	// -- a nested map inside Parameters, to reach the encoder through a wire type.
	def("tooldef_params_nested", protocoltypes.ToolDefinition{
		Type: "function",
		Function: protocoltypes.ToolFunctionDefinition{
			Name: "f", Description: "d",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string", "description": "a <b> & c"}},
				"required":   []any{"q"},
			},
		},
	})

	out = append(out, wireFuzz(seed^0x17E, fuzz*200/1500)...)

	return out
}

// wireFuzz fills the shapes the curated cases cannot enumerate: every optional
// field present or absent at once, in combinations nobody would think to write
// by hand. Strings come from the same multi-width alphabet as the tokenizer
// corpus, because the encoder's escaping rules have to hold inside wire types
// too.
func wireFuzz(seed int64, n int) []fixtureCase {
	rng := rand.New(rand.NewSource(seed))
	out := make([]fixtureCase, 0, n)
	for i := 0; i < n; i++ {
		m := protocoltypes.Message{
			Role:             randChoice(rng, []string{"user", "assistant", "system", "tool", randText(rng, 0, 12)}),
			Content:          randText(rng, 0, 12),
			ReasoningContent: maybe(rng, func() string { return randText(rng, 0, 12) }),
			ToolCallID:       maybe(rng, func() string { return fmt.Sprintf("tc_%d", rng.Intn(1000)) }),
			ModelName:        maybe(rng, func() string { return randChoice(rng, []string{"claude", "gpt", randText(rng, 0, 12)}) }),
		}
		switch rng.Intn(3) {
		case 1:
			m.Media = []string{}
		case 2:
			m.Media = []string{randText(rng, 0, 12), randText(rng, 0, 12)}
		}
		if rng.Intn(4) == 0 {
			t := time.Unix(0, int64(rng.Int63n(4e18))-2e18).In(time.FixedZone("f", (rng.Intn(29)-14)*1800))
			m.CreatedAt = &t
		}
		if rng.Intn(3) == 0 {
			m.SystemParts = randBlocks(rng)
		}
		if rng.Intn(3) == 0 {
			m.ToolCalls = randCalls(rng)
		}
		if rng.Intn(5) == 0 {
			m.Attachments = []protocoltypes.Attachment{{
				Type: randText(rng, 0, 12), Ref: randText(rng, 0, 12), URL: randText(rng, 0, 12),
				Filename: randText(rng, 0, 12), ContentType: randText(rng, 0, 12),
			}}
		}
		if rng.Intn(6) == 0 {
			m.PromptLayer, m.PromptSlot, m.PromptSource = "L", "S", "P"
		}
		out = append(out, wireCase(fmt.Sprintf("fuzz_%05d", i), "message", m, describeMessage))
	}
	return out
}

func randChoice(rng *rand.Rand, opts []string) string {
	return opts[rng.Intn(len(opts))]
}

func maybe[T any](rng *rand.Rand, f func() T) T {
	var zero T
	if rng.Intn(2) == 0 {
		return zero
	}
	return f()
}

func randBlocks(rng *rand.Rand) []protocoltypes.ContentBlock {
	n := rng.Intn(3)
	out := make([]protocoltypes.ContentBlock, n)
	for i := range out {
		out[i] = protocoltypes.ContentBlock{
			Type: randChoice(rng, []string{"text", randText(rng, 0, 12)}),
			Text: randText(rng, 0, 12),
		}
		if rng.Intn(3) == 0 {
			out[i].CacheControl = &protocoltypes.CacheControl{Type: "ephemeral"}
		}
	}
	return out
}

func randCalls(rng *rand.Rand) []protocoltypes.ToolCall {
	n := rng.Intn(3)
	out := make([]protocoltypes.ToolCall, n)
	for i := range out {
		out[i] = protocoltypes.ToolCall{
			ID:   fmt.Sprintf("c%d", rng.Intn(100)),
			Type: maybe(rng, func() string { return "function" }),
		}
		switch rng.Intn(3) {
		case 0:
			out[i].Function = &protocoltypes.FunctionCall{}
		case 1:
			out[i].Function = &protocoltypes.FunctionCall{
				Name: randText(rng, 0, 12), Arguments: randText(rng, 0, 12),
				ThoughtSignature: maybe(rng, func() string { return "sig" }),
			}
		}
		if rng.Intn(5) == 0 {
			out[i].ExtraContent = &protocoltypes.ExtraContent{
				ToolFeedbackExplanation: randText(rng, 0, 12),
			}
			if rng.Intn(2) == 0 {
				out[i].ExtraContent.Google = &protocoltypes.GoogleExtra{ThoughtSignature: randText(rng, 0, 12)}
			}
		}
	}
	return out
}

// marshalWire runs the seam: the real encoder over the real struct.
func marshalWire(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}
