package main

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// The corpus is deliberately adversarial about the two things this seam gets
// wrong most easily: content is measured in runes while the tool-call fields are
// measured in bytes, and Parameters is measured as the byte length of Go's JSON
// encoding, not as a re-serialization of the value.

// ------------------------------------------------------------------ messages --

// contentSamples covers every UTF-8 width, plus the cases where a rune count and
// a grapheme count disagree (combining marks, ZWJ sequences).
var contentSamples = []struct {
	name string
	text string
}{
	{"empty", ""},
	{"ascii", "hello world"},
	{"ascii_long", repeat("the quick brown fox ", 40)},
	{"cjk_3byte", repeat("行", 37)},
	{"hangul_3byte", repeat("한", 19)},
	{"cyrillic_2byte", repeat("Ж", 23)},
	{"emoji_4byte", repeat("🦐", 11)},
	{"zwj_family", "👨‍👩‍👧‍ family"},
	{"combining", "e\u0301tail"},
	{"combining_long", repeat("a\u0308\u0301", 15)},
	{"rtl", "مرحبا بالعالم"},
	{"mixed_widths", "a行🦐Ж e\u0301 ١٢٣"},
	{"newlines", "a\nb\n\n\nc\n"},
	{"crlf", "a\r\nb\r\n"},
	{"tabs", "\ta\t \t b"},
	{"u2028_sep", "a\u2028b\u2029c"},
	{"quotes_backslash", "he said \"hi\" \\ \t\n"},
	{"nbsp", "a\u00a0b"},
	{"bom", "\ufeffheader"},
	{"nul", "a\x00b"},
	{"long_single_rune", repeat("𠀀", 500)},
}

var systemPartSamples = [][]string{
	nil,
	{"only part"},
	{"first", "second"},
	{"a", "b", "c", "d", "e"},
	{"行行行", "🦐🦐"},
	{"", ""},
	{repeat("x", 1000), "short"},
}

var toolCallSamples = []struct {
	name     string
	calls    []protocoltypes.ToolCall
	toolCall string
}{
	{"no_calls", nil, ""},
	{"function_ascii", []protocoltypes.ToolCall{{
		ID: "call_1", Type: "function",
		Function: &protocoltypes.FunctionCall{Name: "read", Arguments: `{"path":"a.txt"}`},
	}}, ""},
	{"function_multibyte", []protocoltypes.ToolCall{{
		ID: "call_行", Type: "function",
		Function: &protocoltypes.FunctionCall{Name: "读取", Arguments: `{"路径":"文件.txt"}`},
	}}, ""},
	{"nil_function_uses_name", []protocoltypes.ToolCall{{
		ID: "call_2", Type: "function", Name: "top_level_行",
	}}, ""},
	{"nil_function_and_type", []protocoltypes.ToolCall{{
		ID: "call_3", Name: "n",
	}}, ""},
	{"all_empty", []protocoltypes.ToolCall{{}}, ""},
	{"three_calls", []protocoltypes.ToolCall{
		{ID: "a", Type: "function", Function: &protocoltypes.FunctionCall{Name: "f", Arguments: "{}"}},
		{ID: "b", Type: "function", Function: &protocoltypes.FunctionCall{Name: "g", Arguments: `{"k":1}`}},
		{ID: "c", Type: "function", Name: "h"},
	}, ""},
	{"tool_role_with_id", []protocoltypes.ToolCall{}, "call_行_1"},
	{"tool_role_ascii_id", nil, "call-abcdef-12345"},
	{"arguments_emoji", []protocoltypes.ToolCall{{
		ID: "1", Type: "function",
		Function: &protocoltypes.FunctionCall{Name: "f", Arguments: `{"e":"🦐🦐🦐"}`},
	}}, ""},
}

func messageCases(seed int64, fuzz int) []fixtureCase {
	out := make([]fixtureCase, 0,
		len(contentSamples)+2*len(systemPartSamples)+len(toolCallSamples)+5+fuzz)

	// Curated: every content sample against a fixed remainder, then the
	// system-part and tool-call matrices against a fixed content.
	for _, c := range contentSamples {
		out = append(out, messageCase("content_"+c.name, c.text, "", nil, nil, ""))
	}
	for i, parts := range systemPartSamples {
		msg := messageCase(fmt.Sprintf("system_parts_%d", i), "baseline content", "", parts, nil, "")
		out = append(out, msg)
		// Same parts, longer content: exercises max(content, systemParts).
		out = append(out, messageCase(
			fmt.Sprintf("system_parts_%d_long_content", i),
			repeat("long ", 200), "", parts, nil, ""))
	}
	for _, tc := range toolCallSamples {
		out = append(out, messageCase("calls_"+tc.name, "content", "", nil, tc.calls, tc.toolCall))
	}
	for _, media := range []int{0, 1, 2, 5} {
		out = append(out, messageCase(fmt.Sprintf("media_%d", media), "content", "reasoning", nil, nil, "", media))
	}
	// Everything at once, so the interaction is pinned too.
	out = append(out, messageCase("everything",
		repeat("行🦐", 60), repeat("reason", 30),
		[]string{"part one", "part two"},
		[]protocoltypes.ToolCall{{
			ID: "id_行", Type: "function",
			Function: &protocoltypes.FunctionCall{Name: "工具", Arguments: `{"a":1}`},
		}}, "call_行", 3))

	out = append(out, fuzzMessageCases(seed, fuzz)...)
	return out
}

func messageCase(
	name, content, reasoning string,
	parts []string,
	calls []protocoltypes.ToolCall,
	toolCallID string,
	mediaCount ...int,
) fixtureCase {
	media := 0
	if len(mediaCount) > 0 {
		media = mediaCount[0]
	}

	msg := protocoltypes.Message{
		Role:             "user",
		Content:          content,
		ReasoningContent: reasoning,
		ToolCalls:        calls,
		ToolCallID:       toolCallID,
	}
	for _, p := range parts {
		msg.SystemParts = append(msg.SystemParts, protocoltypes.ContentBlock{Type: "text", Text: p})
	}
	for i := 0; i < media; i++ {
		msg.Media = append(msg.Media, fmt.Sprintf("/tmp/img-%d.png", i))
	}

	return fixtureCase{Name: name, Input: messageInput(msg, media), message: msg}
}

// messageInput is the portable form. Only the fields the seam reads are
// emitted; media is reduced to a count because the estimator never looks at the
// paths, and inventing plausible-looking ones would imply they matter.
func messageInput(msg protocoltypes.Message, mediaCount int) map[string]any {
	parts := make([]any, 0, len(msg.SystemParts))
	for _, p := range msg.SystemParts {
		parts = append(parts, map[string]any{"type": p.Type, "text": p.Text})
	}
	calls := make([]any, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		entry := map[string]any{"id": tc.ID, "type": tc.Type, "name": tc.Name}
		if tc.Function != nil {
			entry["function"] = map[string]any{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			}
		} else {
			entry["function"] = nil
		}
		calls = append(calls, entry)
	}
	return map[string]any{
		"content":           msg.Content,
		"reasoning_content": msg.ReasoningContent,
		"system_parts":      parts,
		"tool_calls":        calls,
		"tool_call_id":      msg.ToolCallID,
		"media_count":       mediaCount,
	}
}

// fuzzAlphabet mixes all four UTF-8 widths so a byte/rune slip shows up
// immediately rather than only on non-ASCII input.
var fuzzAlphabet = []rune("aA9 _\n\t行한Ж🦐é\u0301\u2028\ufeff")

func fuzzMessageCases(seed int64, n int) []fixtureCase {
	rng := rand.New(rand.NewSource(seed))
	out := make([]fixtureCase, 0, n)
	for i := 0; i < n; i++ {
		msg := protocoltypes.Message{
			Role:             "user",
			Content:          randText(rng, 0, 300),
			ReasoningContent: randText(rng, 0, 60),
			ToolCallID:       randToken(rng),
		}
		for p := rng.Intn(4); p > 0; p-- {
			msg.SystemParts = append(msg.SystemParts, protocoltypes.ContentBlock{
				Type: "text", Text: randText(rng, 0, 120),
			})
		}
		for c := rng.Intn(4); c > 0; c-- {
			tc := protocoltypes.ToolCall{
				ID:   randToken(rng),
				Type: randType(rng),
				Name: randToken(rng),
			}
			if rng.Intn(4) != 0 {
				tc.Function = &protocoltypes.FunctionCall{
					Name:      randToken(rng),
					Arguments: randText(rng, 0, 80),
				}
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
		}
		for m := rng.Intn(4); m > 0; m-- {
			msg.Media = append(msg.Media, "m")
		}
		out = append(out, fixtureCase{
			Name:    fmt.Sprintf("fuzz_%05d", i),
			Input:   messageInput(msg, len(msg.Media)),
			message: msg,
		})
	}
	return out
}

// paramValueCases feeds the encoding/json.Marshal fixture. It is the same value
// set the tool-defs seam wraps in a definition, plus a fuzz tail, so a byte-level
// encoder difference and a token-count difference are pinned by the same corpus.
func paramValueCases(seed int64, fuzz int) []fixtureCase {
	var out []fixtureCase
	for _, p := range parameterSamples {
		out = append(out, paramCase("params_"+p.name, p.value))
	}
	out = append(out,
		paramCase("params_absent", nil),
		paramCase("params_empty_map", map[string]any{}),
		paramCase("params_scalar", "just a string"),
		paramCase("params_int", 42),
		paramCase("params_float", 1.25),
		paramCase("params_bool_true", true),
		paramCase("params_bool_false", false),
		paramCase("params_null", nil),
		paramCase("params_array_root", []any{1, "two", nil, map[string]any{"k": "v"}}),
		paramCase("params_array_empty", []any{}),
		paramCase("params_array_nested_empty", []any{[]any{}, map[string]any{}}),
		paramCase("params_deep", map[string]any{"a": []any{map[string]any{"b": []any{map[string]any{"c": 1.5}}}}}),
	)

	rng := rand.New(rand.NewSource(seed ^ 0x50A4))
	for i := 0; i < fuzz; i++ {
		var v any
		if rng.Intn(8) == 0 {
			v = randValue(rng, 0) // non-object roots exercise the same encoder
		} else {
			v = randParams(rng, 0)
		}
		out = append(out, paramCase(fmt.Sprintf("fuzz_%05d", i), v))
	}
	return out
}

func paramCase(name string, v any) fixtureCase {
	return fixtureCase{
		Name:   name,
		Input:  map[string]any{"parameters": portable(v)},
		params: v,
	}
}

func randType(rng *rand.Rand) string {
	switch rng.Intn(3) {
	case 0:
		return ""
	case 1:
		return "function"
	default:
		return randToken(rng)
	}
}

func randToken(rng *rand.Rand) string {
	if rng.Intn(5) == 0 {
		return ""
	}
	return randText(rng, 1, 16)
}

func randText(rng *rand.Rand, lo, hi int) string {
	n := lo
	if hi > lo {
		n += rng.Intn(hi - lo + 1)
	}
	b := make([]rune, 0, n)
	for i := 0; i < n; i++ {
		b = append(b, fuzzAlphabet[rng.Intn(len(fuzzAlphabet))])
	}
	return string(b)
}

// --------------------------------------------------------------- tool defs --

// parameterSamples pins len(json.Marshal(Parameters)) against the parts of Go's
// encoder a hand-written serializer gets wrong: key ordering, HTML escaping,
// float formatting, and values that fail to marshal at all.
var parameterSamples = []struct {
	name  string
	value map[string]any
}{
	{"nil", nil},
	{"empty", map[string]any{}},
	{"scalar_types", map[string]any{"s": "x", "b": true, "n": nil, "i": 7}},
	{"key_order", map[string]any{"z": 1, "a": 2, "M": 3, "0": 4, "行": 5, "_": 6}},
	{"key_order_nested", map[string]any{"outer": map[string]any{"z": 1, "a": 2}}},
	{"html_in_values", map[string]any{"v": "a<b>c&d"}},
	{"html_in_keys", map[string]any{"a<b>c&d": 1}},
	{"u2028_u2029", map[string]any{"v": "a\u2028b\u2029c"}},
	{"non_ascii_unescaped", map[string]any{"行": "行", "emoji": "🦐"}},
	{"control_chars", map[string]any{"v": "a\x00b\x1fc\td\ne"}},
	{"control_chars_bs_ff", map[string]any{"v": "a\bb\fc"}},
	{"quotes_backslash", map[string]any{"v": "a\"b\\c"}},
	{"float_int_valued", map[string]any{"v": 1.0}},
	{"float_third", map[string]any{"v": 1.0 / 3.0}},
	{"float_tiny", map[string]any{"v": 1e-7}},
	{"float_huge", map[string]any{"v": 1e21}},
	{"float_max", map[string]any{"v": math.MaxFloat64}},
	{"float_neg_zero", map[string]any{"v": math.Copysign(0, -1)}},
	{"float_large_int", map[string]any{"v": float64(1 << 53)}},
	// Go switches between 'f' and 'e' rendering at 1e-6 and 1e21, and its
	// shortest-representation digits have to be matched exactly. These sit on
	// both sides of each boundary and at the extremes.
	{"float_boundary_low_in", map[string]any{"v": 1e-6}},
	{"float_boundary_low_out", map[string]any{"v": 9.999e-7}},
	{"float_boundary_high_in", map[string]any{"v": 1e20}},
	{"float_boundary_high_out", map[string]any{"v": 1e21}},
	{"float_digits_boundary", map[string]any{"v": 1e15}},
	{"float_digits_boundary_2", map[string]any{"v": 1e16}},
	{"float_digits_boundary_3", map[string]any{"v": 1e17}},
	{"float_int_exact", map[string]any{"v": 1234567890123456789.0}},
	{"float_denormal", map[string]any{"v": 5e-324}},
	{"float_tiny_exp", map[string]any{"v": 1e-300}},
	{"float_huge_exp", map[string]any{"v": 1e300}},
	{"float_pi", map[string]any{"v": math.Pi}},
	{"float_halves", map[string]any{"v": 0.5, "w": 2.5, "x": -0.125}},
	{"int64_max", map[string]any{"v": int64(9007199254740991)}},
	{"negative", map[string]any{"v": -42}},
	{"slice", map[string]any{"v": []any{1, "two", nil, true}}},
	{"slice_empty", map[string]any{"v": []any{}}},
	{"nested_deep", map[string]any{
		"a": map[string]any{"b": map[string]any{"c": []any{map[string]any{"d": "e"}}}},
	}},
	{"many_keys", func() map[string]any {
		m := make(map[string]any, 60)
		for i := 0; i < 60; i++ {
			m[fmt.Sprintf("k%02d", i)] = i
		}
		return m
	}()},
	// These make json.Marshal return an error, and the estimator then skips the
	// whole Parameters value -- it does not fall back to a partial count.
	{"unmarshalable_nan", map[string]any{"v": math.NaN()}},
	{"unmarshalable_inf", map[string]any{"v": math.Inf(1)}},
	{"unmarshalable_neg_inf", map[string]any{"v": math.Inf(-1)}},
	{"unmarshalable_mixed", map[string]any{"ok": "yes", "bad": math.Inf(-1)}},
	{"unmarshalable_nested", map[string]any{"ok": "yes", "bad": map[string]any{"deeper": []any{math.NaN()}}}},
	{"unmarshalable_complex", map[string]any{"v": complex128(1 + 2i)}},
	{"unmarshalable_func", map[string]any{"v": func() {}}},
	{"unmarshalable_chan", map[string]any{"v": make(chan int)}},
}

func toolDefCases(seed int64, fuzz int) []fixtureCase {
	var out []fixtureCase

	for _, p := range parameterSamples {
		out = append(out, toolDefCase("params_"+p.name, []protocoltypes.ToolDefinition{
			def("tool", "desc", p.value),
		}))
	}
	// The nil-map branch is separate from the marshal-error branch.
	out = append(out, toolDefCase("params_absent", []protocoltypes.ToolDefinition{
		def("tool", "desc", nil),
	}))
	out = append(out,
		toolDefCase("empty_list", nil),
		toolDefCase("one_empty_def", []protocoltypes.ToolDefinition{{}}),
		toolDefCase("multibyte_name_desc", []protocoltypes.ToolDefinition{
			def("读取文件", "读取 🦐 文件", map[string]any{"a": 1}),
		}),
		toolDefCase("many_defs", func() []protocoltypes.ToolDefinition {
			defs := make([]protocoltypes.ToolDefinition, 0, 25)
			for i := 0; i < 25; i++ {
				defs = append(defs, def(fmt.Sprintf("tool_%02d", i), repeat("d", i), map[string]any{"i": i}))
			}
			return defs
		}()),
		// A failing Parameters in one definition must not affect the others.
		toolDefCase("mixed_marshalable", []protocoltypes.ToolDefinition{
			def("a", "ok", map[string]any{"k": "v"}),
			def("b", "bad", map[string]any{"k": math.NaN()}),
			def("c", "ok", nil),
		}),
	)

	out = append(out, fuzzToolDefCases(seed^0xD1CE, fuzz/2)...)
	return out
}

// def builds a ToolDefinition. Parameters is a map[string]any on the real type,
// so the nil case is a nil map -- the estimator's `!= nil` guard treats it
// differently from a map that fails to marshal.
func def(name, description string, parameters map[string]any) protocoltypes.ToolDefinition {
	return protocoltypes.ToolDefinition{
		Type: "function",
		Function: protocoltypes.ToolFunctionDefinition{
			Name:        name,
			Description: description,
			Parameters:  parameters,
		},
	}
}

func toolDefCase(name string, defs []protocoltypes.ToolDefinition) fixtureCase {
	entries := make([]any, 0, len(defs))
	for _, d := range defs {
		entries = append(entries, map[string]any{
			"name":        d.Function.Name,
			"description": d.Function.Description,
			"parameters":  portable(d.Function.Parameters),
		})
	}
	return fixtureCase{
		Name:  name,
		Input: map[string]any{"defs": entries},
		defs:  defs,
	}
}

// ErrorUnmarshalable is the key a fixture uses to stand in for a value Go's
// json.Marshal refuses. The estimator's behavior for those is to skip the whole
// Parameters value, so the Rust side only needs the marker's presence, not the
// specific type.
const ErrorUnmarshalable = "$go_marshal_error"

// portable rewrites a Parameters value into something the fixture file itself can
// hold. json.Marshal refuses NaN, infinities, funcs, chans and complex numbers --
// which are exactly the interesting cases here, because the estimator then skips
// the value entirely -- so they become an explicit marker instead of an error.
func portable(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = portable(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, portable(val))
		}
		return out
	case float64:
		if math.IsNaN(x) {
			return map[string]any{ErrorUnmarshalable: "NaN"}
		}
		if math.IsInf(x, 1) {
			return map[string]any{ErrorUnmarshalable: "+Inf"}
		}
		if math.IsInf(x, -1) {
			return map[string]any{ErrorUnmarshalable: "-Inf"}
		}
		return x
	case float32:
		return portable(float64(x))
	case complex128:
		return map[string]any{ErrorUnmarshalable: "complex"}
	case func(), chan int:
		return map[string]any{ErrorUnmarshalable: "unsupported_type"}
	default:
		return x
	}
}

func fuzzToolDefCases(seed int64, n int) []fixtureCase {
	rng := rand.New(rand.NewSource(seed))
	out := make([]fixtureCase, 0, n)
	for i := 0; i < n; i++ {
		var defs []protocoltypes.ToolDefinition
		for d := rng.Intn(4); d > 0; d-- {
			def := protocoltypes.ToolDefinition{}
			def.Function.Name = randToken(rng)
			def.Function.Description = randText(rng, 0, 40)
			if rng.Intn(5) != 0 {
				def.Function.Parameters = randParams(rng, 0)
			}
			defs = append(defs, def)
		}
		out = append(out, toolDefCase(fmt.Sprintf("fuzz_%05d", i), defs))
	}
	return out
}

func randParams(rng *rand.Rand, depth int) map[string]any {
	m := make(map[string]any, 4)
	for k := rng.Intn(5); k > 0; k-- {
		m[randText(rng, 1, 6)] = randValue(rng, depth)
	}
	return m
}

func randValue(rng *rand.Rand, depth int) any {
	if depth > 2 {
		return randText(rng, 0, 10)
	}
	switch rng.Intn(9) {
	case 0:
		return nil
	case 1:
		return rng.Intn(1000)
	case 2:
		// Span the whole exponent range, not just the norm distribution: the
		// point is to cross Go's 1e-6 / 1e21 format boundaries and land on
		// denormals, not to look like realistic data.
		switch rng.Intn(3) {
		case 0:
			return rng.NormFloat64() * 1e3
		case 1:
			return math.Pow(10, float64(rng.Intn(615)-308)) * (1 + rng.Float64())
		default:
			return rng.NormFloat64() * math.Pow(10, float64(rng.Intn(40)-20))
		}
	case 3:
		return rng.Intn(2) == 0
	case 4:
		return randText(rng, 0, 20)
	case 5:
		return randParams(rng, depth+1)
	case 6:
		out := make([]any, 0, 3)
		for i := rng.Intn(4); i > 0; i-- {
			out = append(out, randValue(rng, depth+1))
		}
		return out
	case 7:
		return math.Inf(rng.Intn(3) - 1)
	default:
		return map[string]any{"nested": randValue(rng, depth+1)}
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
