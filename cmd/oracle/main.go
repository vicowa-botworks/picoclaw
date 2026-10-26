// Command oracle emits conformance fixtures for the Rust port in
// vicowa-botworks/rusty-crab.
//
// The Go implementation is the oracle: this program contains no expectations, it
// only records what the current code does. A fixture is an (input, output) pair
// produced by calling the real function, so the Rust side is checked against
// measured behavior rather than against a re-reading of this source. Regenerate
// after any intentional behavior change and review the diff.
//
// Usage:
//
//	go run -tags goolm,stdjson ./cmd/oracle -out /path/to/rusty-crab/conformance/fixtures
//
// Output is deterministic for a given -seed, so committed fixtures diff cleanly
// and a behavior change shows up as a value change rather than a reshuffle.
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
		failf("create %s: %v", *out, err)
	}

	provenance := readProvenance(*seed)

	// Every file is rendered before any is written: see writeAll.
	writeAll([]fixtureFile{
		emit(
			provenance,
			filepath.Join(*out, "tokenizer_estimate_message_tokens.json"),
			"tokenizer/EstimateMessageTokens",
			"Content and ReasoningContent are counted in runes; ToolCall and ToolCallID fields are counted in bytes; the scale is a floor division; media is added after scaling.",
			messageCases(*seed, *fuzzCases),
			func(c fixtureCase) any { return tokenizer.EstimateMessageTokens(c.message) },
		),

		emit(
			provenance,
			filepath.Join(*out, "tokenizer_estimate_tool_defs_tokens.json"),
			"tokenizer/EstimateToolDefsTokens",
			"Name and Description are counted in bytes; Parameters contributes len(json.Marshal(x)), i.e. Go's exact encoding including sorted map keys and HTML escaping; a Parameters value that cannot marshal is skipped entirely.",
			toolDefCases(*seed, *fuzzCases),
			func(c fixtureCase) any { return tokenizer.EstimateToolDefsTokens(c.defs) },
		),

		emit(
			provenance,
			filepath.Join(*out, "protocoltypes_marshal.json"),
			"pkg/providers/protocoltypes + encoding/json.Marshal",
			"expected is the exact output of json.Marshal over the described Go value, or null when it returns an error -- which it can, because Arguments and Parameters carry arbitrary map[string]any and a NaN, channel or func inside one is a marshaling failure. Such a value is not dropped from input: portable() replaces it with {\"$go_marshal_error\": \"unsupported_type\"} (or NaN, +Inf, -Inf, complex for those), so the refusal is visible in the case rather than silent. Field order is declaration order. Absent, null, [] and \"\" are four different encodings and each comes from a different Go value: an omitempty field is dropped for its zero value -- including an empty non-nil slice or map, which is why media: [] and media: null both omit the key -- a bare field is always emitted, a nil slice or map emits null and an empty one emits [] or {}. time.Time is RFC3339Nano, which trims trailing zeros from the fraction (an exact second has none). StreamChunk has no json tags, so its keys are the Go field names with their original capitalisation. Fields tagged json:\"-\" (Name, Arguments, ThoughtSignature on ToolCall; PromptLayer, PromptSlot, PromptSource everywhere) never appear.",
			wireCases(*seed, *fuzzCases),
			func(c fixtureCase) any { return marshalWire(c.wire) },
		),

		// The tool-defs seam above only exposes json.Marshal through a token count,
		// so a one-byte encoding difference shows up as an off-by-one estimate and
		// says nothing about which byte moved. This fixture pins the encoding itself:
		// expected is the exact output string, or null where json.Marshal errors.
		emit(
			provenance,
			filepath.Join(*out, "go_json_encode_parameters.json"),
			"encoding/json.Marshal",
			"expected is the exact output of json.Marshal, or null when it returns an error. Map keys are sorted by byte; <, > and & are escaped as \\u003c \\u003e \\u0026; U+2028 and U+2029 are escaped; \\b and \\f have short forms and the rest of C0 is \\u00XX with lower-case hex; non-ASCII is not escaped. Floats are shortest round-trip, positional in [1e-6, 1e21) and exponent form outside it, with a positive exponent padded to two digits (1e+21) and a negative exponent stripped to one (1e-7, but 5e-324).",
			paramValueCases(*seed, *fuzzCases),
			func(c fixtureCase) any {
				b, err := json.Marshal(c.params)
				if err != nil {
					return nil
				}
				return string(b)
			},
		),

		// The session store's contract is a directory, not a return value. This
		// fixture runs a script of public store calls in a temporary directory and
		// records the file names and file bytes it left behind, so the port is
		// checked against the artifact users actually keep rather than against the
		// Go structs that produced it. Wall-clock timestamps are masked to "<time>";
		// everything else is byte-exact.
		emit(provenance, filepath.Join(*out, "memory_jsonl_store.json"),
			"pkg/memory.JSONLStore",
			memoryStoreNote,
			memoryCases(),
			func(c fixtureCase) any { return runStoreScript(c.Name, c.script, c.expectErrOp) }),
	})
}

// fixtureCase is one row. Exactly one of message / defs is populated, chosen by
// the seam being recorded; Input is the portable description of it that the Rust
// side parses into its own types.
type fixtureCase struct {
	Name     string         `json:"name"`
	Input    map[string]any `json:"input"`
	Expected any            `json:"expected"`

	message     protocoltypes.Message
	defs        []protocoltypes.ToolDefinition
	params      any
	wire        any
	script      []storeStep
	expectErrOp string
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

func emit(p provenance, file, seam, note string, cases []fixtureCase, score func(fixtureCase) any) fixtureFile {
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
		failf("marshal %s: %v", file, err)
	}
	return fixtureFile{path: file, raw: append(raw, '\n'), count: fx.CaseCount}
}

// fixtureFile is a rendered fixture that has not been written yet.
type fixtureFile struct {
	path  string
	raw   []byte
	count int
}

// writeAll is the reason render and write are separate. The five files share one
// source_commit, so a failure on the third must not leave the first two already
// rewritten at the new commit: a half-regenerated set is exactly what the
// provenance field exists to prevent. Measured before this existed, a value
// portable() did not recognize killed the run mid-write and left two fresh files
// and two stale ones.
func writeAll(files []fixtureFile) {
	for _, f := range files {
		if err := os.WriteFile(f.path, f.raw, 0o644); err != nil {
			failf("write %s: %v", f.path, err)
		}
	}
	for _, f := range files {
		fmt.Printf("%-46s %5d cases\n", filepath.Base(f.path), f.count)
	}
}

// exitFunc is what failf terminates with. It is a variable so the harness's own
// guards are testable: failf is the only way the generator refuses a malformed
// case, and the refusals that protect the corpus (a write_file name that is "."
// or "..", an at() instant the clock mask would erase) are exactly the code no
// fixture can grade -- a case that reaches them never renders, so no expectation
// moves when they are deleted. A test swaps in a stub that unwinds and asserts
// the refusal happened; production never reassigns it.
var exitFunc = os.Exit

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "oracle: "+format+"\n", args...)
	exitFunc(1)
	// Unreachable while exitFunc is os.Exit. A test stub must unwind (panic or
	// runtime.Goexit); this is here so a stub that merely returns cannot let the
	// generator carry on and render a corpus from half-executed cases.
	panic("oracle: exitFunc returned without unwinding")
}
