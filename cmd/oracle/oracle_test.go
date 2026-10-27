package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// TestCorpusNamesAreUnique checks the emitter's de-duplication: a duplicate case
// name in a fixture would silently overwrite a Rust test id.
func TestCorpusNamesAreUnique(t *testing.T) {
	for _, tc := range []struct {
		seam  string
		cases []fixtureCase
	}{
		{"messages", messageCases(0x5EED, 50)},
		{"tooldefs", toolDefCases(0x5EED, 50)},
		{"wire", wireCases(0x5EED, 50)},
		{"memory", memoryCases()},
	} {
		seen := map[string]bool{}
		for _, c := range tc.cases {
			if c.Name == "" {
				t.Fatalf("%s: case with empty name", tc.seam)
			}
			if seen[c.Name] {
				t.Errorf("%s: duplicate case name %q", tc.seam, c.Name)
			}
			seen[c.Name] = true
		}
	}
}

// TestCorpusInputIsJSONEncodable is the real constraint on `portable`: the
// fixture file itself has to marshal, including the cases whose whole point is
// that Go's encoder refuses them.
func TestCorpusInputIsJSONEncodable(t *testing.T) {
	for _, tc := range []struct {
		seam  string
		cases []fixtureCase
	}{
		{"messages", messageCases(1, 200)},
		{"tooldefs", toolDefCases(1, 200)},
		{"wire", wireCases(1, 200)},
		{"memory", memoryCases()},
	} {
		for _, c := range tc.cases {
			if _, err := json.Marshal(c.Input); err != nil {
				t.Errorf("%s/%s: input not encodable: %v", tc.seam, c.Name, err)
			}
		}
	}
}

// TestPortableMarksUnmarshalableValues pins the marker contract the Rust side
// reads: any non-finite float becomes {"$go_marshal_error": ...} at any depth.
func TestPortableMarksUnmarshalableValues(t *testing.T) {
	in := map[string]any{"a": []any{1.0, map[string]any{"b": math.NaN()}}}
	out, ok := portable(in).(map[string]any)
	if !ok {
		t.Fatalf("portable returned %T, want map", portable(in))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshaling portable output: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	inner := back["a"].([]any)[1].(map[string]any)["b"].(map[string]any)
	if _, ok := inner[ErrorUnmarshalable]; !ok {
		t.Errorf("nested NaN not marked: %s", raw)
	}
}

// TestPortableMarksEverythingGoRefuses covers the values the exact-type switch
// does not name -- chan string, func(int), []float64 holding NaN. Before default
// probed the encoder these fell through to the fixture's own json.Marshal, which
// failed the run after some of the four files had already been rewritten.
func TestPortableMarksEverythingGoRefuses(t *testing.T) {
	for name, v := range map[string]any{
		"chan string":   make(chan string),
		"func(int)":     func(int) {},
		"[]float64/NaN": []float64{math.NaN()},
		"struct/chan":   struct{ C chan int }{},
	} {
		if _, err := json.Marshal(v); err == nil {
			t.Errorf("%s: the test expects Go to refuse this value, but it marshals", name)
			continue
		}
		out := portable(map[string]any{"v": v})
		raw, err := json.Marshal(out)
		if err != nil {
			t.Errorf("%s: portable left an unmarshalable value through: %v", name, err)
			continue
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		marker, ok := back["v"].(map[string]any)[ErrorUnmarshalable]
		if !ok {
			t.Errorf("%s: not marked: %s", name, raw)
			continue
		}
		if marker != "unsupported_type" {
			t.Errorf("%s: marked as %v, want unsupported_type", name, marker)
		}
	}
}

// TestPortablePreservesFiniteValues guards the other direction: a rewrite that
// loses real data would quietly shrink the corpus.
func TestPortablePreservesFiniteValues(t *testing.T) {
	in := map[string]any{"s": "x", "f": 1.5, "i": 3, "b": true, "n": nil, "l": []any{1, "two"}}
	got := portable(in)
	if !reflect.DeepEqual(got, map[string]any{"s": "x", "f": 1.5, "i": 3, "b": true, "n": nil, "l": []any{1, "two"}}) {
		t.Errorf("portable changed finite values: %#v", got)
	}
	if portable(map[string]any(nil)) != nil {
		t.Error("a nil map must stay nil: the estimator distinguishes it from an empty map")
	}
}

// TestWireKindsAreTheSevenDocumented pins the dispatch keys. The Rust reader
// switches on input.kind, so a kind that stops being emitted means the port
// silently stops covering that type -- wireFuzz only ever produces "message",
// so six of the seven ride on the curated block alone.
func TestWireKindsAreTheSevenDocumented(t *testing.T) {
	kinds := map[string]int{}
	for _, c := range wireCases(0x5EED, 1500) {
		kind, _ := c.Input["kind"].(string)
		kinds[kind]++
	}
	for _, want := range []string{
		"message", "llm_response", "tool_definition", "stream_chunk",
		"content_block", "attachment", "reasoning_detail",
	} {
		if kinds[want] == 0 {
			t.Errorf("kind %q is no longer emitted; the Rust reader dispatches on it", want)
		}
	}
	if len(kinds) != 7 {
		t.Errorf("emitted %d distinct kinds, want 7: %v", len(kinds), kinds)
	}
}

// TestWireFixtureEnvelopeIsTheContract pins one rendered fixture document in
// full. Every other test here looks at cases; nothing looked at the file, so
// SchemaVersion, the envelope key names, the input kind/value pair and the fact
// that `expected` is a JSON *string* rather than the object it contains were all
// unpinned -- and the Rust reader parses each of them by name.
func TestWireFixtureEnvelopeIsTheContract(t *testing.T) {
	f := emit(provenance{
		Generator: "cmd/oracle",
		Repo:      "github.com/sipeed/picoclaw",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		Seed:      0x5EED,
	}, "x.json", "pkg/providers/protocoltypes + encoding/json.Marshal", "a note",
		[]fixtureCase{wireCase("message_zero", "message", protocoltypes.Message{}, describeMessage)},
		func(c fixtureCase) any { return marshalWire(c.wire) })

	if got := string(f.raw); got != wantWireEnvelope {
		t.Errorf("rendered fixture changed.\ngot:\n%s\nwant:\n%s", got, wantWireEnvelope)
	}
}

// wantWireEnvelope is the format, written out rather than derived: a fixture
// that regenerates its own expectation cannot fail.
// Every key below is parsed by name on the Rust side: the envelope
// (schema/generator/source_repo/source_commit/seed/seam/note/case_count/cases),
// the case (name/input/expected), the description envelope (kind/value), and
// `expected` as a JSON string rather than the object it holds. The zero Message
// is the smallest case that still shows both halves of the contract: the
// description carries all 13 Message fields, while the wire document carries
// only the two bare fields -- role and content -- because every other field is
// omitempty or a nil pointer/slice.
const wantWireEnvelope = `{
  "schema": "rusty-crab-conformance/1",
  "generator": "cmd/oracle",
  "source_repo": "github.com/sipeed/picoclaw",
  "source_commit": "0123456789abcdef0123456789abcdef01234567",
  "seed": 24301,
  "seam": "pkg/providers/protocoltypes + encoding/json.Marshal",
  "note": "a note",
  "case_count": 1,
  "cases": [
    {
      "name": "message_zero",
      "input": {
        "kind": "message",
        "value": {
          "attachments": null,
          "content": "",
          "created_at": null,
          "media": null,
          "model_name": "",
          "prompt_layer": "",
          "prompt_slot": "",
          "prompt_source": "",
          "reasoning_content": "",
          "role": "",
          "system_parts": null,
          "tool_call_id": "",
          "tool_calls": null
        }
      },
      "expected": "{\"role\":\"\",\"content\":\"\"}"
    }
  ]
}
`

// TestReasoningDetailDescriptionsAgree guards a duplication: a nested
// ReasoningDetails slice is described by portable() over the struct's own tags,
// while a top-level case uses describeReasoningDetail's hand-written map. They
// carry the same fields today; this is what keeps them doing so. Compared after
// decode, because a map marshals its keys sorted and a struct in declaration
// order, and only the key set and the values are the contract.
func TestReasoningDetailDescriptionsAgree(t *testing.T) {
	for _, d := range []protocoltypes.ReasoningDetail{
		{},
		{Format: "plain", Index: 0, Type: "text", Text: "thinking"},
		{Format: "html", Index: 7, Type: "summary", Text: ""},
		{Format: "", Index: -1, Type: "", Text: "<b> & é ☃ \u2028"},
	} {
		viaMap, err := json.Marshal(describeReasoningDetail(d))
		if err != nil {
			t.Fatalf("describeReasoningDetail(%+v): %v", d, err)
		}
		viaTags, err := json.Marshal(portable(d))
		if err != nil {
			t.Fatalf("portable(%+v): %v", d, err)
		}
		var a, b map[string]any
		if err := json.Unmarshal(viaMap, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(viaTags, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%+v: hand-written description %s diverges from the struct tags %s", d, viaMap, viaTags)
		}
	}
}

// TestEveryGoFieldIsDescribed is the check the corpus format invites and never
// had: a dropped field decodes to its zero value on both sides and stays
// invisible, so the Rust port silently stops emitting it. The expected key set is
// derived from the Go type by reflection -- not from the description -- so this
// is not the corpus agreeing with itself. It also fails the day someone adds a
// field to protocoltypes and forgets the generator.
func TestEveryGoFieldIsDescribed(t *testing.T) {
	for _, tc := range []struct {
		kind string
		typ  reflect.Type
	}{
		{"message", reflect.TypeOf(protocoltypes.Message{})},
		{"llm_response", reflect.TypeOf(protocoltypes.LLMResponse{})},
		{"tool_definition", reflect.TypeOf(protocoltypes.ToolDefinition{})},
		{"stream_chunk", reflect.TypeOf(protocoltypes.StreamChunk{})},
		{"content_block", reflect.TypeOf(protocoltypes.ContentBlock{})},
		{"attachment", reflect.TypeOf(protocoltypes.Attachment{})},
		{"reasoning_detail", reflect.TypeOf(protocoltypes.ReasoningDetail{})},
	} {
		seen := map[string]bool{}
		cases := 0
		for _, c := range wireCases(0x5EED, 1500) {
			if kind, _ := c.Input["kind"].(string); kind != tc.kind {
				continue
			}
			cases++
			value, ok := c.Input["value"].(map[string]any)
			if !ok {
				t.Errorf("%s/%s: value is not an object", tc.kind, c.Name)
				continue
			}
			for k := range value {
				seen[k] = true
			}
		}
		if cases == 0 {
			t.Errorf("kind %q has no cases at all", tc.kind)
			continue
		}
		want := descriptionKeys(tc.typ)
		missing := make([]string, 0)
		for _, k := range want {
			if !seen[k] {
				missing = append(missing, k)
			}
		}
		extra := make([]string, 0)
		for k := range seen {
			if !contains(want, k) {
				extra = append(extra, k)
			}
		}
		if len(missing) > 0 || len(extra) > 0 {
			sort.Strings(missing)
			sort.Strings(extra)
			t.Errorf("%s (%d cases): described fields diverge from %s -- never described %v, not a field %v",
				tc.kind, cases, tc.typ, missing, extra)
		}
	}
}

// descriptionKeys names the key the description uses for each field of a Go
// struct: its json tag, the field name when it has no tag (StreamChunk has
// none), and the snake_case name for json:"-". The last one is a convention, not
// a Go rule -- those fields never reach the wire, so the generator is free to
// name them, and it names them this way so the Rust reader can still set them.
func descriptionKeys(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			switch base := strings.Split(tag, ",")[0]; base {
			case "-":
				name = snakeCase(f.Name)
			case "":
			default:
				name = base
			}
		}
		out = append(out, name)
	}
	return out
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestWireCorpusIsByteStable pins the whole default corpus with one digest.
// Everything else here checks a property; this checks the bytes, which is what
// "deterministic at a fixed seed" actually promised and nothing pinned: the
// reviewer's M9 (fuzz seed 0x17E -> 0x17F) survived the suite precisely because
// the committed fixture lives in another repository, so no Go test could notice
// the corpus had moved.
func TestWireCorpusIsByteStable(t *testing.T) {
	render := func(seed int64, fuzz int) string {
		f := emit(provenance{
			Generator: "cmd/oracle",
			Repo:      "github.com/sipeed/picoclaw",
			Commit:    "0123456789abcdef0123456789abcdef01234567",
			Seed:      seed,
		}, "x.json", "pkg/providers/protocoltypes + encoding/json.Marshal", "wire",
			wireCases(seed, fuzz), func(c fixtureCase) any { return marshalWire(c.wire) })
		return string(f.raw)
	}

	first := render(0x5EED, 1500)
	if second := render(0x5EED, 1500); first != second {
		t.Fatal("the corpus is not reproducible at a fixed seed")
	}
	if render(0x5EED, 1500) == render(0x5EE, 1500) {
		t.Error("-seed does not change the wire corpus: the fuzz seed is hardcoded again")
	}
	if n := len(wireCases(0x5EED, 0)); n != 48 {
		t.Errorf("-fuzz 0 yields %d cases, want the 48 curated ones", n)
	}

	sum := sha256.Sum256([]byte(first))
	if got := hex.EncodeToString(sum[:]); got != wantWireCorpusSHA {
		t.Errorf("the default wire corpus moved.\n got %s\nwant %s\n"+
			"If the change is intended, update wantWireCorpusSHA and regenerate the fixture in "+
			"rusty-crab/conformance/fixtures from the same commit.", got, wantWireCorpusSHA)
	}
}

// wantWireCorpusSHA is sha256 of the rendered default corpus (seed 0x5EED, fuzz
// 1500) with the fixed provenance above: 248 cases, 48 curated and 200 fuzz.
const wantWireCorpusSHA = "bfb5d1c1dfcc057ef827180da0759c0d733a7c61d9408a14b8c3678503e16929"

// TestMemoryCorpusIsByteStable is the same pin the wire seam got, for the seam
// whose expectations move the most: 66 of 68 cases changed when the clock rule
// did (measured at the head that introduced the rule, when the corpus was 68
// cases -- it is 79 now), and the file the port reads lives in another
// repository, so nothing here could notice the corpus drifting. Two renders of
// the whole script set must be identical, and the digest must not move without
// someone deciding it should.
func TestMemoryCorpusIsByteStable(t *testing.T) {
	render := func() string {
		f := emit(provenance{
			Generator: "cmd/oracle",
			Repo:      "github.com/sipeed/picoclaw",
			Commit:    "0123456789abcdef0123456789abcdef01234567",
			Seed:      0x5EED,
		}, "x.json", "pkg/memory.JSONLStore", "memory",
			memoryCases(), func(c fixtureCase) any { return runStoreScript(c.Name, c.script, c.expectErrOp) })
		return string(f.raw)
	}
	first := render()
	if second := render(); first != second {
		t.Fatal("the memory corpus is not reproducible: a timestamp is leaking through the mask")
	}
	sum := sha256.Sum256([]byte(first))
	if got := hex.EncodeToString(sum[:]); got != wantMemoryCorpusSHA {
		t.Errorf("the memory corpus moved.\n got %s\nwant %s\n"+
			"If the change is intended, update wantMemoryCorpusSHA and regenerate the fixture in "+
			"rusty-crab/conformance/fixtures from the same commit.", got, wantMemoryCorpusSHA)
	}
}

const wantMemoryCorpusSHA = "0e78fce312124ff8ff734d11425ec42146050e392dca963dec2775b3d1ac855f"

// exitPanic is what the stubbed exit throws, so a test can tell "the generator
// refused the case" from "the generator crashed".
type exitPanic struct{}

// captureFailf runs fn with failf's exit stubbed, and returns what failf printed
// and whether it was reached at all.
//
// The message is the assertion, not the unwinding. A guard that is deleted does
// not usually make the case succeed: write_file with the name "a/b" still fails,
// just later and inside WriteFile with "is a directory". Only the text says which
// of the two refused, so a test that only checks "it exited" passes with the
// guard removed -- which is the exact failure mode these guards have.
func captureFailf(t *testing.T, fn func()) (string, bool) {
	t.Helper()

	origExit, origErr := exitFunc, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	refused := false
	exitFunc = func(int) {
		refused = true
		panic(exitPanic{})
	}
	os.Stderr = w

	func() {
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(exitPanic); !ok {
					panic(p)
				}
			}
		}()
		fn()
	}()

	os.Stderr, exitFunc = origErr, origExit
	_ = w.Close()
	msg, _ := io.ReadAll(r)
	_ = r.Close()
	return string(msg), refused
}

// TestWriteFileGuardRefusesNonBareNames pins the harness guard that no fixture
// can reach: a case whose write_file name is "." or ".." is refused by name, not
// left to fail inside WriteFile. Restoring the guard to `Base(name) != name ||
// name == ""` leaves all 71 cases green, which is precisely why this test exists.
func TestWriteFileGuardRefusesNonBareNames(t *testing.T) {
	for _, name := range []string{".", "..", "sub/dir.jsonl", "s1/../s2.jsonl", ""} {
		msg, refused := captureFailf(t, func() {
			runStoreScript("probe", []storeStep{stepWriteFile(name, "x")}, "")
		})
		if !refused || !strings.Contains(msg, "bare file name") {
			t.Errorf("write_file %q: refused=%v, message %q; want the bare-file-name guard",
				name, refused, strings.TrimSpace(msg))
		}
	}
	// The other side: a bare name is accepted, or every fixture would have
	// refused and the guard would look like it works.
	msg, refused := captureFailf(t, func() {
		runStoreScript("probe", []storeStep{stepWriteFile("s1.jsonl", "x"), stepList()}, "")
	})
	if refused {
		t.Errorf("write_file with a bare name was refused: %s", strings.TrimSpace(msg))
	}
}

// TestAtRefusesAnInstantTheMaskWouldErase pins the second harness guard. A
// scripted instant at or after process start is erased by the clock mask, so a
// case that scripts one silently pins <time> instead of the instant it names.
func TestAtRefusesAnInstantTheMaskWouldErase(t *testing.T) {
	msg, refused := captureFailf(t, func() {
		at("user", "hi", time.Now().Unix(), 0)
	})
	if !refused || !strings.Contains(msg, "at or after process start") {
		t.Errorf("at(now): refused=%v, message %q; want the clock guard", refused, strings.TrimSpace(msg))
	}

	m := at("user", "hi", 1700000000, 123456789)
	if m.CreatedAt == nil || m.CreatedAt.Unix() != 1700000000 || m.CreatedAt.Nanosecond() != 123456789 {
		t.Errorf("a scripted past instant was not carried through: %+v", m.CreatedAt)
	}
}

// TestFailedQueryRecordsNoResult pins the shape of an expected-failure case: the
// op that failed contributes no entry to results. Its value would be whatever Go
// returned beside the error -- a nil slice here -- and that is a convention about
// Go, not a property of the store, so grading it fails a correct port.
func TestFailedQueryRecordsNoResult(t *testing.T) {
	got, ok := runStoreScript("probe", []storeStep{
		stepAddSimple("s1", "user", "hi"),
		stepWriteRawMeta("s1", "{\"key\": \"s1\", \"cou"),
		stepGetHistory("s1"),
	}, "get_history").(map[string]any)
	if !ok {
		t.Fatal("expected-failure script did not return an expectation")
	}
	if got["error"] != "get_history" {
		t.Fatalf("error = %v, want get_history", got["error"])
	}
	results, ok := got["results"].([]any)
	if !ok {
		t.Fatalf("results is %T, want a list", got["results"])
	}
	if len(results) != 0 {
		t.Errorf("results has %d entries; a failed query must contribute none: %v", len(results), results)
	}
	if _, ok := got["files"]; !ok {
		t.Error("a refusal must still record the directory: files is the assertion")
	}
}

// TestSucceededQueriesStillRecord is the other half: appending after the error
// check must not quietly drop the entries that succeed, including two queries in
// one script, which is the case shape that motivated results being a list.
func TestSucceededQueriesStillRecord(t *testing.T) {
	got, ok := runStoreScript("probe", []storeStep{
		stepAddSimple("s1", "user", "hi"),
		stepGetHistory("s1"),
		stepGetMeta("s1"),
		stepList(),
	}, "").(map[string]any)
	if !ok {
		t.Fatal("script did not return an expectation")
	}
	results := got["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results has %d entries, want one per query: %v", len(results), results)
	}
	for i, want := range []string{"get_history", "get_meta", "list_all"} {
		entry := results[i].(map[string]any)
		if entry["op"] != want || entry["op_index"] != i+1 {
			t.Errorf("results[%d] = {op: %v, op_index: %v}, want {op: %s, op_index: %d}",
				i, entry["op"], entry["op_index"], want, i+1)
		}
	}
}
