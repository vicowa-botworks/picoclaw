// Command oracle emits conformance fixtures for the Rust port in
// vicowa-botworks/rusty-crab.
//
// The Go implementation is the oracle: this program contains no expectations, it
// only records what the current code does. A fixture is an (input, output) pair
// produced by calling the real function, so the Rust side is checked against
// measured behaviour rather than against a re-reading of this source. Regenerate
// after any intentional behaviour change and review the diff.
//
// Usage:
//
//	go run -tags goolm,stdjson ./cmd/oracle -out /path/to/rusty-crab/conformance/fixtures
//
// Output is deterministic for a given -seed, so committed fixtures diff cleanly
// and a behaviour change shows up as a value change rather than a reshuffle.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// SchemaVersion identifies the fixture envelope. A reader that does not
// understand it must refuse the file rather than guess.
const SchemaVersion = "rusty-crab-conformance/1"

func main() {
	out := flag.String("out", "conformance/fixtures", "directory to write fixtures into")
	seed := flag.Int64("seed", 0x5EED, "seed for the generated fuzz corpus")
	fuzzCases := flag.Int("fuzz", 1500, "pseudo-random cases per seam, on top of the curated ones")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail("create %s: %v", *out, err)
	}

	provenance := readProvenance(*seed)

	emit(provenance, filepath.Join(*out, "tokenizer_estimate_message_tokens.json"),
		"tokenizer/EstimateMessageTokens",
		"Content and ReasoningContent are counted in runes; ToolCall and ToolCallID fields are counted in bytes; the scale is a floor division; media is added after scaling.",
		messageCases(*seed, *fuzzCases),
		func(c fixtureCase) any { return tokenizer.EstimateMessageTokens(c.message) })

	emit(provenance, filepath.Join(*out, "tokenizer_estimate_tool_defs_tokens.json"),
		"tokenizer/EstimateToolDefsTokens",
		"Name and Description are counted in bytes; Parameters contributes len(json.Marshal(x)), i.e. Go's exact encoding including sorted map keys and HTML escaping; a Parameters value that cannot marshal is skipped entirely.",
		toolDefCases(*seed, *fuzzCases),
		func(c fixtureCase) any { return tokenizer.EstimateToolDefsTokens(c.defs) })

	emit(provenance, filepath.Join(*out, "protocoltypes_marshal.json"),
		"pkg/providers/protocoltypes + encoding/json.Marshal",
		"expected is the exact output of json.Marshal over the described Go value. Field order is declaration order. Absent, null, [] and \"\" are four different encodings and each comes from a different Go value: an omitempty field is dropped for its zero value, a bare field is always emitted, a nil slice or map emits null and an empty one emits [] or {}. time.Time is RFC3339Nano, which trims trailing zeros from the fraction (an exact second has none). StreamChunk has no json tags, so its keys are the Go field names with their original capitalisation. Fields tagged json:\"-\" (Name, Arguments, ThoughtSignature on ToolCall; PromptLayer, PromptSlot, PromptSource everywhere) never appear.",
		wireCases(),
		func(c fixtureCase) any { return marshalWire(c.wire) })

	// The tool-defs seam above only exposes json.Marshal through a token count,
	// so a one-byte encoding difference shows up as an off-by-one estimate and
	// says nothing about which byte moved. This fixture pins the encoding itself:
	// expected is the exact output string, or null where json.Marshal errors.
	emit(provenance, filepath.Join(*out, "go_json_encode_parameters.json"),
		"encoding/json.Marshal",
		"expected is the exact output of json.Marshal, or null when it returns an error. Map keys are sorted by byte; <, > and & are escaped as \\u003c \\u003e \\u0026; U+2028 and U+2029 are escaped; \\b and \\f have short forms and the rest of C0 is \\u00XX with lower-case hex; non-ASCII is not escaped. Floats are shortest round-trip, positional in [1e-6, 1e21) and exponent form outside it, with a positive exponent padded to two digits (1e+21) and a negative exponent stripped to one (1e-7, but 5e-324).",
		paramValueCases(*seed, *fuzzCases),
		func(c fixtureCase) any {
			b, err := json.Marshal(c.params)
			if err != nil {
				return nil
			}
			return string(b)
		})
}

// fixtureCase is one row. Exactly one of message / defs is populated, chosen by
// the seam being recorded; Input is the portable description of it that the Rust
// side parses into its own types.
type fixtureCase struct {
	Name     string         `json:"name"`
	Input    map[string]any `json:"input"`
	Expected any            `json:"expected"`

	message protocoltypes.Message
	defs    []protocoltypes.ToolDefinition
	params  any
	wire    any
}

type fixture struct {
	Schema       string        `json:"schema"`
	Generator    string        `json:"generator"`
	SourceRepo   string        `json:"source_repo"`
	SourceCommit string        `json:"source_commit"`
	Seed         int64         `json:"seed"`
	Seam         string        `json:"seam"`
	Note         string        `json:"note,omitempty"`
	CaseCount    int           `json:"case_count"`
	Cases        []fixtureCase `json:"cases"`
}

type provenance struct {
	Generator string `json:"-"`
	Repo      string `json:"-"`
	Commit    string `json:"-"`
	Seed      int64  `json:"-"`
}

func readProvenance(seed int64) provenance {
	p := provenance{Generator: "cmd/oracle", Repo: "github.com/sipeed/picoclaw", Seed: seed}
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		p.Commit = strings.TrimSpace(string(out))
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if p.Commit == "" {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					p.Commit = s.Value
				}
			}
		}
	}
	if p.Commit == "" {
		p.Commit = "unknown"
	}
	return p
}

func emit(p provenance, file, seam, note string, cases []fixtureCase, score func(fixtureCase) any) {
	fx := fixture{
		Schema:       SchemaVersion,
		Generator:    p.Generator,
		SourceRepo:   p.Repo,
		SourceCommit: p.Commit,
		Seed:         p.Seed,
		Seam:         seam,
		Note:         note,
		CaseCount:    len(cases),
		Cases:        make([]fixtureCase, 0, len(cases)),
	}

	seen := make(map[string]bool, len(cases))
	for _, c := range cases {
		name := c.Name
		for i := 2; seen[name]; i++ {
			name = fmt.Sprintf("%s_%d", c.Name, i)
		}
		c.Name = name
		seen[name] = true
		c.Expected = score(c)
		fx.Cases = append(fx.Cases, c)
	}

	raw, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		fail("marshal %s: %v", file, err)
	}
	if err := os.WriteFile(file, append(raw, '\n'), 0o644); err != nil {
		fail("write %s: %v", file, err)
	}
	fmt.Printf("%-46s %5d cases\n", filepath.Base(file), fx.CaseCount)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "oracle: "+format+"\n", args...)
	os.Exit(1)
}
