package main

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
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
		t.Fatalf("marshalling portable output: %v", err)
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
