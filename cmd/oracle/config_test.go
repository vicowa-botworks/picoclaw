package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// TestConfigCorpusIsByteStable pins the config seam the way the other two are
// pinned. This one has a second reason to be pinned beyond drift: the loader
// reads $HOME and a dozen PICOCLAW_* variables, so a corpus that is stable on
// the machine that made it is not evidence that it is stable anywhere. The
// digest only holds if pinConfigEnv is doing its job.
func TestConfigCorpusIsByteStable(t *testing.T) {
	render := func() string {
		f := emit(provenance{
			Generator: "cmd/oracle",
			Repo:      "github.com/sipeed/picoclaw",
			Commit:    "0123456789abcdef0123456789abcdef01234567",
			Seed:      0x5EED,
		}, "x.json", "pkg/config.LoadConfig + SaveConfig + DefaultConfig", "config",
			configCases(), func(c fixtureCase) any {
				return runConfigScript(c.Name, c.configScript, c.expectErrIdx)
			})
		return string(f.raw)
	}
	first := render()
	if second := render(); first != second {
		t.Fatal("the config corpus is not reproducible")
	}
	if n := len(configCases()); n != wantConfigCaseCount {
		t.Errorf("config seam has %d cases, want %d", n, wantConfigCaseCount)
	}
	sum := sha256.Sum256([]byte(first))
	if got := hex.EncodeToString(sum[:]); got != wantConfigCorpusSHA {
		t.Errorf("the config corpus moved.\n got %s\nwant %s\n"+
			"If the change is intended, update wantConfigCorpusSHA and regenerate the fixture in "+
			"rusty-crab/conformance/fixtures from the same commit.", got, wantConfigCorpusSHA)
	}
}

// wantConfigCaseCount is spelled once, next to the digest, because the two are the same
// assertion wearing different clothes: a case added without a re-pin moves both, and a
// message that hardcodes the number goes stale the moment someone updates the check above
// and only looks at that line.
const wantConfigCaseCount = 96

const wantConfigCorpusSHA = "9e24b57869906e2492626d1a921363c7675800aeeedb2410345c896c847d09fe"

// TestConfigCorpusIgnoresTheMachine is the part the digest above cannot prove on
// its own: it runs the generator with the environment a real user would have and
// asserts the corpus still says the sentinel home.
//
// Without pinConfigEnv this is not a hypothetical failure. DefaultConfig expands
// $HOME into agents.defaults.workspace, so the corpus would carry whichever home
// belonged to whoever ran the generator, and the copy in the other repository
// would then pin a path that only exists on that one machine.
func TestConfigCorpusIgnoresTheMachine(t *testing.T) {
	t.Setenv("HOME", "/home/someone-else")
	t.Setenv("PICOCLAW_LOG_LEVEL", "debug")
	t.Setenv("PICOCLAW_CHANNELS_TELEGRAM_STREAMING_ENABLED", "true")

	f := emit(provenance{
		Generator: "cmd/oracle",
		Repo:      "github.com/sipeed/picoclaw",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		Seed:      0x5EED,
	}, "x.json", "pkg/config.LoadConfig + SaveConfig + DefaultConfig", "config",
		configCases(), func(c fixtureCase) any {
			return runConfigScript(c.Name, c.configScript, c.expectErrIdx)
		})
	body := string(f.raw)

	if strings.Contains(body, "/home/someone-else") {
		t.Error("the corpus recorded the generator's HOME; pinConfigEnv is not pinning it")
	}
	if !strings.Contains(body, configHomeSentinel) {
		t.Error("the corpus does not contain the sentinel home, so nothing pins HOME expansion")
	}
	if strings.Contains(body, "PICOCLAW_CHANNELS_TELEGRAM_STREAMING_ENABLED") {
		t.Error("an inherited PICOCLAW_* variable leaked into the corpus")
	}
	// The environment has to be handed back: the rest of the suite runs in the
	// same process, and a leaked HOME changes what every later case loads.
	if got := os.Getenv("HOME"); got != "/home/someone-else" {
		t.Errorf("HOME is %q after the run, want it restored", got)
	}
}

// TestConfigBackupNameIsMasked pins the one thing standing between this fixture
// and a corpus that changes at midnight.
func TestConfigBackupNameIsMasked(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"config.json.20261026.bak", "config.json.<date>.bak"},
		{".security.yml.19990101.bak", ".security.yml.<date>.bak"},
		{"config.json", "config.json"},
		// Not a MakeBackup name: eight digits that are not its suffix stay put, so
		// the mask cannot quietly swallow a file the code named deliberately.
		{"notes.20260101.txt", "notes.20260101.txt"},
		{"config.json.202610261.bak", "config.json.202610261.bak"},
	} {
		if got := bakDate.ReplaceAllString(tc.in, ".<date>.bak"); got != tc.want {
			t.Errorf("mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestConfigSaveWithoutLoadIsRefused guards the rule that stops a case from
// pinning a state the harness invented. The same guard exists on the memory
// seam for the same reason, and the same mutation deletes both.
func TestConfigSaveWithoutLoadIsRefused(t *testing.T) {
	out, refused := captureFailf(t, func() {
		runConfigScript("no_load", []configStep{
			cfgWrite("config.json", `{"version":3}`),
			cfgSave("config.json"),
		}, -1)
	})
	if !refused {
		t.Fatal("save with nothing to write was allowed to run")
	}
	if !strings.Contains(out, "has no config to write") {
		t.Errorf("refused for the wrong reason: %s", out)
	}
}

// TestConfigErrorIndexMustMatch pins that an error case names an op that really
// refuses. An index that points at a succeeding op, or past the end of the
// script, has to be a generation failure: otherwise the case would record a
// refusal that never happened and the port would be graded against a fiction.
func TestConfigErrorIndexMustMatch(t *testing.T) {
	steps := []configStep{cfgWrite("config.json", `{"version":3,`), cfgLoad("config.json")}

	// An index that points at an op which cannot be the failing one: the write at
	// 0 is skipped by the runner, so the refusal the case promised never arrives
	// and the *next* op's error becomes the unexpected one. Either way generation
	// stops; what the test wants is that it stops with the offending index in the
	// sentence, because "op 1" is what tells you the case was mis-numbered rather
	// than the code mis-behaving.
	out, refused := captureFailf(t, func() {
		runConfigScript("wrong_index", steps, 0)
	})
	if !refused {
		t.Fatal("an error index pointing at a write_file was accepted")
	}
	if !strings.Contains(out, "op 1") {
		t.Errorf("refusal does not name the op that actually broke: %s", out)
	}

	// An index past the end of the script is the same class of mistake.
	out, refused = captureFailf(t, func() {
		runConfigScript("past_end", []configStep{cfgWrite("config.json", `{"version":3}`)}, 7)
	})
	if !refused {
		t.Fatal("an error index past the end of the script was accepted")
	}
	if !strings.Contains(out, "expected op 7 to fail, and it never did") {
		t.Errorf("wrong refusal: %s", out)
	}

	// And an index on an op that succeeds for real, not by being skipped.
	_, refused = captureFailf(t, func() {
		runConfigScript("load_succeeds", []configStep{
			cfgWrite("config.json", `{"version":3}`),
			cfgLoad("config.json"),
		}, 1)
	})
	if !refused {
		t.Fatal("an error index on a load that succeeded was accepted")
	}
}

// TestConfigTempDirGuardBites is the test for the guard that caught a real leak
// while this seam was being written: a security-file parse error quotes the
// absolute path it failed on, so recording error text would have put a /tmp path
// in every refusal case.
func TestConfigTempDirGuardBites(t *testing.T) {
	dir, err := os.MkdirTemp("", "picoclaw-oracle-config-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	out, refused := captureFailf(t, func() {
		assertNoTempDir("leaky", map[string]any{"files": map[string]string{
			"config.json": "loaded from " + dir + " today",
		}}, dir)
	})
	if !refused {
		t.Fatal("an expectation containing the temp dir was accepted")
	}
	if !strings.Contains(out, "not portable") {
		t.Errorf("wrong refusal: %s", out)
	}

	// The clean case must stay silent, or every real case would be refused and
	// the guard would be deleted rather than fixed.
	if _, refused := captureFailf(t, func() {
		assertNoTempDir("clean", map[string]any{"files": map[string]string{
			"config.json": `{"version":3}`,
		}}, dir)
	}); refused {
		t.Error("a clean expectation was refused")
	}
}

// TestConfigSaveStabilityVerdict pins the three answers saveIsStable can give.
// The verdict is in the fixture, so a port that rewrites the user's file on every
// start is graded against the real one -- which means the generator has to be
// able to say "unstable" and mean it.
func TestConfigSaveStabilityVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		saves      map[string][]string
		wantStable bool
		wantAsked  bool
	}{
		// Vacuously stable: nothing was saved twice, so nothing contradicted it.
		// The caller must not record a verdict here, which is what wantAsked is for.
		{"no saves", map[string][]string{}, true, false},
		{"one save", map[string][]string{"c": {"x"}}, true, false},
		{"two identical", map[string][]string{"c": {"x", "x"}}, true, true},
		{"two different", map[string][]string{"c": {"x", "y"}}, false, true},
		{"second file drifts", map[string][]string{
			"c": {"x", "x"}, "s": {"a", "b"},
		}, false, true},
		{"three saves, last one moves", map[string][]string{
			"c": {"x", "x", "z"},
		}, false, true},
	} {
		stable, asked := saveIsStable(tc.saves)
		if stable != tc.wantStable || asked != tc.wantAsked {
			t.Errorf("%s: saveIsStable = (%v, %v), want (%v, %v)",
				tc.name, stable, asked, tc.wantStable, tc.wantAsked)
		}
	}
}

// TestRecordSaveStability is the test for the recording step itself.
//
// No case in the corpus drifts -- measured: six two-save scripts, every one of
// them byte-identical on the second save -- so the fixture can only ever carry
// save_stable: true, and a generator that recorded the verdict as
// `stable || ok` would produce a corpus nobody could tell from the right one.
// This test is what makes that mutation expensive instead of free.
func TestRecordSaveStability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		saves    map[string][]string
		wantHave bool
		wantVal  bool
	}{
		{"no saves records nothing", map[string][]string{}, false, false},
		{"one save records nothing", map[string][]string{"c": {"x"}}, false, false},
		{"stable pair records true", map[string][]string{"c": {"x", "x"}}, true, true},
		{"drifting pair records false", map[string][]string{"c": {"x", "y"}}, true, false},
	} {
		expected := map[string]any{}
		recordSaveStability(expected, tc.saves)

		got, have := expected["save_stable"]
		if have != tc.wantHave {
			t.Errorf("%s: save_stable present = %v, want %v", tc.name, have, tc.wantHave)
			continue
		}
		if have && got != tc.wantVal {
			t.Errorf("%s: save_stable = %v, want %v", tc.name, got, tc.wantVal)
		}
	}
}

// TestConfigRoundTripIsStable states the property the fixture's save_stable field
// records, in the language of the code under test rather than of the corpus: load
// a config, save it, load what you wrote, save again, and the document must not
// have moved.
//
// It is worth having here as well as there because the corpus only records the
// verdict for the one script that asks twice. This one asks about a config with
// two api_keys, which is the case where an instability compounds: the virtual
// key entries are invented at load and filtered at save, so a save that persisted
// what it loaded would add a model to the user's file on every start.
func TestConfigRoundTripIsStable(t *testing.T) {
	restore := pinConfigEnv()
	defer restore()

	dir, err := os.MkdirTemp("", "picoclaw-oracle-stable-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := dir + "/config.json"
	body := `{"version":3,"model_list":[{"model_name":"m1","provider":"openrouter",` +
		`"model":"llama-3","api_keys":["sk-1","sk-2"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var first string
	for round := 1; round <= 3; round++ {
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("round %d load: %v", round, err)
		}
		if err := config.SaveConfig(path, cfg); err != nil {
			t.Fatalf("round %d save: %v", round, err)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("round %d read: %v", round, rerr)
		}
		if round == 1 {
			first = string(data)
			continue
		}
		if string(data) != first {
			t.Fatalf("round %d wrote a different document than round 1: the config drifts "+
				"on every start", round)
		}
	}

	// And the virtual entries stayed out of the file: two keys, one model.
	var doc map[string]any
	if err := json.Unmarshal([]byte(first), &doc); err != nil {
		t.Fatalf("the saved document is not JSON: %v", err)
	}
	models, ok := doc["model_list"].([]any)
	if !ok {
		t.Fatalf("no model_list in the saved document: %v", doc)
	}
	if len(models) != 1 {
		t.Errorf("the saved document holds %d models, want the one real entry: %s",
			len(models), first)
	}
}
