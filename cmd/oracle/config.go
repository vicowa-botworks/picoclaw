package main

// The config seam.
//
// This is the second stored format, and it is memory-shaped in the same way: the
// artifact is the files on disk, not a return value. One SaveConfig call writes
// TWO files -- the JSON document at the requested path and `.security.yml` beside
// it, which holds the subset of the struct that carries yaml tags -- and a port
// that writes one plausible file instead of the pair leaves a user's machine in a
// state the Go build cannot read back.
//
// So a case is a directory: files are written into a fresh temp dir, a script of
// load/save/defaults ops runs against them, and the expectation is the directory
// afterwards (every file's exact bytes) plus one entry per query that succeeded.
// The query value is the loaded *Config re-marshaled by encoding/json, which is
// the complete record of what the loader decided -- defaults filled, migrations
// applied, references resolved -- rather than a projection this file invents.
//
// Nothing here is masked on a timer: the config format carries no wall-clock
// field, so every byte in these files is a decision the code made. Two things
// are not portable and both are handled rather than pinned:
//
//   - the machine. DefaultConfig() expands $HOME into agents.defaults.workspace,
//     and LoadConfig reads a dozen PICOCLAW_* variables, so the generator runs
//     every case under a fixed HOME with PICOCLAW_* cleared. The sentinel is
//     pinned byte for byte, not masked: a port that expands HOME differently
//     fails, which is the point.
//   - the day. Migrating a config calls MakeBackup, which copies the file to
//     <path>.YYYYMMDD.bak. Only the eight date digits vary, so the file *name*
//     is masked to <date> and its existence, content and pairing with the
//     security backup stay exact.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
)

// configHomeSentinel is the $HOME every case runs under.
const configHomeSentinel = "/home/oracle"

// bakDate matches the suffix MakeBackup appends: time.Now().Format(".20060102.bak").
var bakDate = regexp.MustCompile(`\.\d{8}\.bak$`)

// pinConfigEnv fixes $HOME and clears every PICOCLAW_* variable for the duration
// of a case, and returns the restore function.
//
// Clearing rather than setting: the corpus has to mean the same thing on the
// maintainer's laptop and on the machine replaying it, and an inherited
// PICOCLAW_SKILLS_GITHUB_AUTH_TOKEN would otherwise show up in the recorded
// config of whoever happened to run the generator.
func pinConfigEnv() func() {
	var undo []func()

	if prev, ok := os.LookupEnv("HOME"); ok {
		undo = append(undo, func() { _ = os.Setenv("HOME", prev) })
	} else {
		undo = append(undo, func() { _ = os.Unsetenv("HOME") })
	}
	_ = os.Setenv("HOME", configHomeSentinel)

	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "PICOCLAW_") {
			continue
		}
		prev := kv[len(key)+1:]
		undo = append(undo, func() { _ = os.Setenv(key, prev) })
		_ = os.Unsetenv(key)
	}

	return func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
}

const configNote = "input.ops is a script run in a fresh directory; expected is {files, results}; " +
	"a case that must fail adds error_op_index and still records files and results, because for a " +
	"refusal the directory is the assertion. results lists only the queries that SUCCEEDED, and is " +
	"[] rather than null when there are none, so the port never has to tell an empty list from an " +
	"absent one. A query value is the loaded *Config re-marshaled by encoding/json, so it is the " +
	"loader's whole decision, not a projection. write_file puts a literal file in the directory " +
	"with no code involved; load reads the named file through LoadConfig; defaults calls " +
	"DefaultConfig without touching the disk; save writes the config from the last load or " +
	"defaults call back out through SaveConfig, and save refuses to run without one rather than " +
	"inventing a config to write. files maps every file left in the directory to its exact bytes, " +
	"with the eight date digits of a MakeBackup suffix replaced by <date> because they are " +
	"wall-clock. No error text is recorded anywhere: the Go message quotes the absolute path of " +
	"the file it failed on. A case that saves the same file twice adds save_stable, whether the " +
	"second save produced the identical document, because a config that drifts on every start is a " +
	"property of the code and not of the machine. Every case runs with HOME fixed at " +
	"/home/oracle and PICOCLAW_* cleared, so a workspace path in the expectation is the loader's " +
	"expansion and not the generator's environment."

// configStep is one op in a case's script.
type configStep struct {
	op      string
	name    string
	content string
}

func cfgWrite(name, content string) configStep {
	return configStep{op: "write_file", name: name, content: content}
}

func cfgLoad(name string) configStep { return configStep{op: "load", name: name} }

func cfgSave(name string) configStep { return configStep{op: "save", name: name} }

func cfgDefaults() configStep { return configStep{op: "defaults"} }

// describe renders the op for the corpus. The Rust side replays this, so it is
// the portable half; configStep itself is the Go half, with the fields the
// runner needs.
func (s configStep) describe() map[string]any {
	out := map[string]any{"op": s.op}
	switch s.op {
	case "write_file":
		out["name"] = s.name
		out["content"] = s.content
	case "load", "save":
		out["name"] = s.name
	case "defaults":
	default:
		panic("oracle: unknown config step " + s.op)
	}
	return out
}

func configCase(name string, steps ...configStep) fixtureCase {
	described := make([]any, 0, len(steps))
	for _, s := range steps {
		described = append(described, s.describe())
	}
	return fixtureCase{
		Name:         name,
		Input:        map[string]any{"ops": described},
		configScript: steps,
		expectErrIdx: -1,
	}
}

// configCaseErr records a script that is supposed to fail at the op with this
// index. The index, not the op name: a case that loads twice -- once to get a
// security file written, once to test it -- has two loads, and naming the
// operation cannot say which one must refuse. The Go error text is not portable,
// so the expectation names the index that refused and records the directory as
// it stands; for a refused load that is the whole assertion, nothing was
// rewritten.
func configCaseErr(name string, wantErrIdx int, steps ...configStep) fixtureCase {
	c := configCase(name, steps...)
	c.expectErrIdx = wantErrIdx
	return c
}

// ------------------------------------------------------------------- runner --

// runConfigScript executes the script and returns the directory it left behind.
// Any error outside the one op the case names is fatal: a step the loader rejects
// for a reason the case did not intend is a bug in the corpus.
func runConfigScript(name string, steps []configStep, wantErrIdx int) any {
	restoreEnv := pinConfigEnv()
	defer restoreEnv()

	dir, err := os.MkdirTemp("", "picoclaw-oracle-config-")
	if err != nil {
		failf("config: temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	results := []any{}
	// Bytes written by each save op, per file name, so a case that saves twice can
	// say whether the second save produced the same document as the first. See
	// save_stable below.
	saves := map[string][]string{}
	var loaded *config.Config

	for i, s := range steps {
		switch s.op {
		case "write_file":
			if err = os.WriteFile(filepath.Join(dir, s.name), []byte(s.content), 0o600); err != nil {
				failf("config %s: write_file: %v", name, err)
			}
			continue

		case "defaults":
			loaded = config.DefaultConfig()

		case "load":
			loaded, err = config.LoadConfig(filepath.Join(dir, s.name))

		case "save":
			if loaded == nil {
				failf("config %s: save at op %d has no config to write: the case never loaded "+
					"one, and a harness that manufactures the thing a case is named for cannot "+
					"pin its absence", name, i)
			}
			err = config.SaveConfig(filepath.Join(dir, s.name), loaded)
			if err == nil {
				if data, rerr := os.ReadFile(filepath.Join(dir, s.name)); rerr == nil {
					saves[s.name] = append(saves[s.name], string(data))
				}
			}

		default:
			failf("config %s: unknown op %q", name, s.op)
		}

		if wantErrIdx >= 0 && i == wantErrIdx {
			if err == nil {
				failf("config %s: op %d (%s) was expected to fail and did not", name, i, s.op)
			}
			// The Go error text is not in the expectation on purpose: it quotes
			// the absolute path of the file it failed on, which is the one thing
			// in this fixture that cannot be the same on two machines. What the
			// port has to reproduce is that the call is refused, and where.
			expected := map[string]any{"error_op_index": i}
			files, ferr := readConfigDir(dir)
			if ferr != nil {
				failf("config %s: read dir: %v", name, ferr)
			}
			expected["files"] = files
			expected["results"] = results
			assertNoTempDir(name, expected, dir)
			return expected
		}
		if err != nil {
			failf("config %s: op %d (%s): %v", name, i, s.op, err)
		}

		if s.op == "load" || s.op == "defaults" {
			encoded, merr := json.Marshal(loaded)
			if merr != nil {
				failf("config %s: marshal loaded config: %v", name, merr)
			}
			results = append(results, queryResult(i, s.op, string(encoded)))
		}
	}

	if wantErrIdx >= 0 {
		failf("config %s: expected op %d to fail, and it never did: the script ran %d ops "+
			"and every one of them that ran succeeded", name, wantErrIdx, len(steps))
	}

	files, err := readConfigDir(dir)
	if err != nil {
		failf("config %s: read dir: %v", name, err)
	}
	expected := map[string]any{"files": files, "results": results}
	recordSaveStability(expected, saves)
	assertNoTempDir(name, expected, dir)
	return expected
}

// saveIsStable reports whether every file that two saves wrote produced the same
// bytes, and whether the question was asked at all.
//
// A config that changes a little on every save is the failure mode this whole
// seam exists to catch: the user's file is rewritten at every start, so a
// migration bug compounds, and two runs of the same binary disagree about the
// same directory. The verdict is in the fixture rather than asserted by the
// generator because it is a property of the code under test, and a port is
// graded on reproducing it either way.
//
// stable is vacuously true when asked is false: nothing was saved twice, so
// nothing contradicted stability. Callers must read asked first.
func saveIsStable(saves map[string][]string) (bool, bool) {
	asked := false
	for _, writes := range saves {
		if len(writes) < 2 {
			continue
		}
		asked = true
		for _, w := range writes[1:] {
			if w != writes[0] {
				return false, true
			}
		}
	}
	return true, asked
}

// readConfigDir records every file the code left behind, by name, exact bytes.
func readConfigDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		files[bakDate.ReplaceAllString(entry.Name(), ".<date>.bak")] = string(data)
	}
	return files, nil
}

// assertNoTempDir refuses a case whose expectation contains the temporary
// directory.
//
// The memory seam masks what varies; this one refuses, because a path is not
// noise. A config value that came out absolute is either the loader expanding
// something (which the port has to reproduce, and could not from a masked
// string) or the generator leaking its own scratch space into the corpus (which
// must never be copied to the Rust side at all). Both are worth a loud failure
// at generation time and neither is worth a <path> placeholder that makes the
// case pass in two languages for the wrong reason.
//
// It earns its keep: the first version recorded err.Error() on the refusal
// cases, and a security-file parse failure quotes the absolute path of the file
// it failed on. Every case in the fixture would have carried a /tmp path.
func assertNoTempDir(name string, expected any, dir string) {
	encoded, err := json.Marshal(expected)
	if err != nil {
		failf("config %s: encode expectation: %v", name, err)
	}
	if strings.Contains(string(encoded), filepath.Base(dir)) {
		failf("config %s: the expectation contains the temporary directory %q; a value that "+
			"depends on where the file lives is not portable, so pin it differently", name, dir)
	}
}

// recordSaveStability writes the save_stable verdict into a case's expectation,
// if the script saved anything twice.
//
// It is a function rather than two lines at the call site because the two lines
// are the whole contract and a mutation of either has to be visible to a test:
// recording `stable || ok` instead of `stable` -- the kind of edit that looks
// like a no-op -- survives every case in the corpus, because no script in it
// drifts. What catches it is a test that hands this function a script that does.
func recordSaveStability(expected map[string]any, saves map[string][]string) {
	if stable, asked := saveIsStable(saves); asked {
		expected["save_stable"] = stable
	}
}

func configCases() []fixtureCase {
	return []fixtureCase{
		// The baseline every other case is a delta from.
		configCase("defaults_canonical", cfgDefaults()),

		// A missing file is not an error: the loader logs and returns defaults.
		configCase("load_missing_file_returns_defaults", cfgLoad("config.json")),

		// len(data) <= 10 short-circuits to defaults before any migration runs.
		configCase("tiny_file_returns_defaults",
			cfgWrite("config.json", "{}"),
			cfgLoad("config.json")),

		// The current version, and nothing else: every field in the result below
		// came from a default, not from the file.
		configCase("load_current_version_minimal",
			cfgWrite("config.json", `{"version":3}`),
			cfgLoad("config.json")),

		// What SaveConfig writes for that same config is the pair of files, and
		// the JSON one is MarshalIndent with two spaces.
		configCase("save_writes_the_document",
			cfgWrite("config.json", `{"version":3}`),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A version below CurrentVersion is raised on the way out but not on the
		// way in, so the loaded value and the saved file disagree on purpose.
		configCase("save_bumps_an_old_version",
			cfgWrite("config.json", `{"version":1}`),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// Malformed JSON is refused with the diagnostic wrapper, and the file on
		// disk is untouched: a refused load must not have rewritten anything.
		configCaseErr("malformed_json_is_refused", 1,
			cfgWrite("config.json", `{"version":3,`),
			cfgLoad("config.json")),

		// Zero bytes is "broken", not "defaults". The length check that returns
		// defaults for a tiny file runs *after* the version probe, and the probe
		// parses: an empty file dies on "unexpected end of JSON input" while `{}`
		// two bytes away is defaults. The order is the whole case.
		configCaseErr("empty_file_is_refused", 1,
			cfgWrite("config.json", ""),
			cfgLoad("config.json")),

		// A version above CurrentVersion is refused outright. The loader's switch
		// has no default branch that tolerates the unknown, so a config written by
		// a newer build stops the older one with an error rather than quietly
		// re-writing it at the older schema -- which is the opposite of what
		// SaveConfig does on the way out, where only a lower version is raised.
		configCaseErr("version_ahead_is_refused", 1,
			cfgWrite("config.json", `{"version":99}`),
			cfgLoad("config.json")),

		// ---------------------------------------------------------------- migrations --
		//
		// Loading a config below CurrentVersion is not a read. The loader
		// migrates, calls MakeBackup, and defers SaveConfig -- so it rewrites the
		// user's file in place and leaves a dated copy behind. A port that treats
		// load as pure fails three ways at once: no backup, no upgrade on disk,
		// and a directory listing that does not match.

		// v0 spelled the key as api_key (a string); v1 wants api_keys (a list).
		// This one does migrate, and the rewrite of the user's file is in files.
		configCase("migrate_v0_api_key_becomes_api_keys",
			cfgWrite("config.json", `{"model_list":[{"model_name":"m1","provider":"openrouter",`+
				`"model":"llama-3","api_key":"sk-1"}]}`),
			cfgLoad("config.json")),

		// A v0 config that carries a providers map cannot be loaded at all.
		// migrateV0ToV1 does convert providers into model_list -- but it never
		// deletes the providers key, and loadConfig then decodes the migrated map
		// against the current *Config through the unknown-field validator, which
		// has no providers field to match it to. So the conversion happens and the
		// file is refused anyway, on a key the migration itself left behind. This
		// is a bug in the Go code, and the port reproduces it: a Rust loader that
		// accepts this file would take configs the shipped binary rejects, and the
		// two would then disagree about the same user's directory forever.
		configCaseErr("v0_providers_is_refused_after_migration", 1,
			cfgWrite("config.json", `{"providers":{"openrouter":{"api_key":"sk-1","api_base":"https://x",`+
				`"models":["llama-3"]}},"agents":{"defaults":{"provider":"openrouter","model":"llama-3"}}}`),
			cfgLoad("config.json")),

		// v1 keeps channels under "channels"; v2 renames the key to channel_list.
		configCase("migrate_v1_channels_renamed",
			cfgWrite("config.json", `{"version":1,"channels":{"telegram":{"enabled":true,`+
				`"bot_token":"123:abc","allowed_user_ids":[1]}}}`),
			cfgLoad("config.json")),

		// v2 to v3 is the same rename one version later, and the version probe
		// picks the chain: only the steps above the file's own version run.
		configCase("migrate_v2_channels_to_channel_list",
			cfgWrite("config.json", `{"version":2,"channels":{"discord":{"enabled":true,`+
				`"bot_token":"tok","allowed_user_ids":[7]}}}`),
			cfgLoad("config.json")),

		// The backup is a pair when the security file exists: MakeBackup copies
		// both under the same date suffix, so a migrated config never leaves one
		// half of the pair upgraded. The security file here is one the code wrote
		// itself (load, then save) rather than a hand-typed YAML, because the
		// loader decodes it into a strict struct and a guessed document fails for
		// reasons that have nothing to do with what this case is about.
		configCase("migrate_backs_up_both_files",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json"),
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m2",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-2"]}]}`),
			cfgLoad("config.json")),

		// A security file that is not parseable YAML stops the whole load: the
		// config half is never applied and nothing is rewritten. Content the
		// loader does not recognize is *not* refused here -- the security file is
		// read into a yaml.Node tree and walked by key, so only a syntax error
		// fails. That asymmetry with the JSON side, which refuses every unknown
		// key, is the case.
		configCaseErr("unparseable_security_file_is_refused", 4,
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json"),
			cfgWrite(".security.yml", "model_list: [\n"),
			cfgLoad("config.json")),

		// An unknown key in the security file is not refused, and it is not merged
		// either: the entry replaces the JSON one wholesale, so the api_keys the
		// JSON document carried are gone. Measured -- a security file of
		// model_list: {m1:0: {zzz: 1}} loads with zero keys for a model that had
		// one. The two files are two layers applied per model, not two halves
		// merged field by field, and a port that merges keeps a key the Go build
		// drops.
		configCase("security_entry_replaces_the_json_model",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json"),
			cfgWrite(".security.yml", "model_list:\n    m1:0:\n      zzz: 1\n"),
			cfgLoad("config.json")),

		// The security file's model_list is a map keyed name:index -- which is what
		// toNameIndex produces -- not the sequence the JSON document uses. Writing
		// the JSON shape in YAML is a type error the loader refuses.
		configCaseErr("security_seq_form_is_refused", 4,
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json"),
			cfgWrite(".security.yml", "model_list:\n    - model_name: m1\n"),
			cfgLoad("config.json")),

		// ---------------------------------------------------------------- security --
		//
		// What the security file can say is narrower than its struct suggests, and
		// narrower per section: inside `web` only `api_keys` is readable (Brave,
		// Tavily, Kagi, ... all tag `enabled`, `base_url` and `max_results`
		// `yaml:"-"`), inside a skills registry only `auth_token` plus whatever keys
		// land in Param, and inside `model_list` nothing at all. The file exists to
		// hold secrets, so every non-secret field of these types is JSON-only -- and a
		// port that reads `enabled` out of the security file is inventing a key the
		// shipped binary ignores.
		//
		// `skills` additionally gets a second, legacy pass (applyLegacySkillsSecurity-
		// Config, security.go:96) that the plain unmarshal does not cover: a bare
		// `skills: {github: {...}}` also *creates* a registry entry.
		//
		// The web section is NOT pinned here, and cannot be through a load. Its only
		// readable key is `api_keys`, a SecureStrings, and SecureStrings.IsZero
		// (config_struct.go:114) returns true whenever the caller is not the YAML
		// encoder -- deliberately, on the comment's own words, "to prevent marshal
		// this field". So under `encoding/json` every `omitzero` secure field is
		// omitted whatever it holds, and a security file's `web.brave.api_keys` leaves
		// the loaded value byte-identical to one with no security file at all. That
		// was measured, not read: the candidate case for it recorded the same value
		// with and without the section, so it is not in the corpus. The merge is
		// observable only in what `save` writes back, so it belongs with the save
		// cases.

		// A legacy `skills.github.token` does two things, and only the second is what
		// the plain unmarshal does. It reaches tools.skills.github.token -- which then
		// marshals as *nothing*, for the IsZero reason above -- and the legacy pass
		// copies the same value into the `github` registry's Param, where
		// SkillRegistryConfig.MarshalJSON (config_struct.go:503) re-emits it
		// **in plaintext** beside a masked `auth_token: "[NOT_HERE]"`. So one secret
		// appears twice in the loaded value, once hidden and once not, and the
		// difference is that MarshalJSON builds the registry object by hand and cannot
		// see the omit options. A port that reads the section once leaves the registry
		// list untouched and every skill install then reports no credentials; a port
		// that masks Param leaks nothing but disagrees with Go byte for byte.
		configCase("security_legacy_github_token_becomes_a_registry",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: ghp_legacy\n"),
			cfgLoad("config.json")),

		// The modern path, `skills.registries.<name>.auth_token`, is handled by the
		// plain unmarshal alone and reaches the registry the defaults already carry.
		// Renaming `registries` to a registry name moves the token to a different
		// entry, which is what makes this case more than a restatement of the one
		// above: the two spellings are two different code paths, and the file decides
		// which by one key.
		//
		// What this case does *not* pin is the legacy pass's explicit `continue` on the
		// name `registries` (security.go:104). Measured, dropping that skip changes
		// nothing here: a `registries` entry decoded as a registry has no auth_token of
		// its own, so the token rule below skips it anyway. The skip is pinned by
		// `security_a_scalar_under_registries_loads_unchanged` instead, where removing
		// it turns a file that loads into one that is refused.
		configCase("security_registries_path_sets_the_registry_token",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n        clawhub:\n"+
				"            auth_token: tok_claw\n"),
			cfgLoad("config.json")),

		// A legacy registry entry with no token is skipped outright (the `continue` at
		// security.go:159), so it never reaches the registry list. The entry carries a
		// `base_url` rather than being empty so the skip is actually visible: an
		// unknown key lands in Param and Param is re-emitted by MarshalJSON, so a port
		// without the skip produces a `other` registry showing that base_url, and a
		// port with it shows nothing. A name that is not `github` also cannot borrow
		// the github token to qualify -- the github entry in the same file is what
		// makes that distinction real rather than vacuous.
		configCase("security_legacy_registry_without_a_token_is_skipped",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    other:\n        base_url: https://x\n"+
				"    github:\n        token: ghp_legacy\n"),
			cfgLoad("config.json")),

		// The eight cases below exist because eight injected faults in the Rust port
		// walked through the corpus above. Each one is the fault's own shape: a filter
		// removed, a guard widened, a skip dropped.
		//
		// A scalar under `registries` loads, and changes nothing. The yaml pass walks
		// `value.Content` for its mapping pairs and a scalar node has none, so it finds
		// zero registries and no error -- and the legacy pass never reaches it either,
		// because that is the one key its loop names and skips.
		//
		// The skip is the whole reason this file loads. Remove it and the same bytes
		// are refused: the legacy pass decodes every other value into a
		// SkillRegistryConfig, whose `UnmarshalYAML` decodes into a `map[string]any`,
		// and a scalar does not decode into a map. So this case pins the skip at
		// security.go:104 from the only direction that can show it -- not that a
		// `registries` registry is not created, but that the file is not rejected --
		// and it pins that a port which refuses this file, or reads the scalar as a
		// registry name, disagrees with the shipped binary.
		configCase("security_a_scalar_under_registries_loads_unchanged",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries: tok_direct\n"),
			cfgLoad("config.json")),

		// `base_url` is filled only when the stored registry has none, so a legacy
		// entry cannot repoint a registry the config already placed elsewhere. The
		// stored URL has to be something other than the default for this to say
		// anything: with `https://github.com` on both sides the two rules agree.
		configCase("security_legacy_base_url_does_not_replace_a_stored_one",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"github":`+
				`{"enabled":true,"base_url":"https://stored"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: ghp_legacy\n"+
				"        base_url: https://legacy\n"),
			cfgLoad("config.json")),

		// `name` is one of the four keys the registry decoder names, so it sets the
		// entry's name -- which the caller then overwrites with the key it came under.
		// It never reaches Param, and Param is what marshals, so a port that treats it
		// as an unknown key emits a `name` key the shipped binary never writes.
		configCase("security_legacy_name_key_is_not_a_param",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        name: renamed\n"+
				"        token: ghp_legacy\n"),
			cfgLoad("config.json")),

		// An underscore key survives the decode -- only `_auth_token` is named -- and
		// is dropped on the way out by MarshalJSON's `_` prefix filter. Two filters,
		// one key: a port that drops it during the decode cannot tell the reader that
		// `_auth_token` is special for a different reason, and a port that drops
		// neither writes a key Go hides.
		configCase("security_legacy_underscore_param_is_dropped_on_the_way_out",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: ghp_legacy\n"+
				"        _internal: hidden\n"),
			cfgLoad("config.json")),

		// The nested `param` map is merged into the same Param, so `base_url` can arrive
		// there -- and still cannot displace the `base_url` MarshalJSON writes itself,
		// because that one goes into the map first and the loop skips keys already
		// present. A port that appends Param blindly writes the key twice.
		//
		// `region` is here for the other half of the same rule. Without a key that is
		// not one of the three the marshaler writes, a port that never reads `param` at
		// all produces this document too: `base_url` is dropped by both, so the case
		// would pin the collision and say nothing about the merge.
		configCase("security_nested_param_cannot_displace_base_url",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: ghp_legacy\n"+
				"        param:\n            base_url: https://via-param\n"+
				"            region: eu\n"),
			cfgLoad("config.json")),

		// The deprecated field is read from the *loaded* value, not from the security
		// file, so a token already in config.json qualifies the github registry with no
		// token in the file at all. It also pins the other half of SecureString's JSON
		// decoder: only the literal `[NOT_HERE]` is refused, so a plaintext token in
		// config.json is read as a value -- and it stays invisible in the loaded value,
		// because the field itself marshals to nothing.
		configCase("security_github_token_already_in_the_config_fills_the_registry",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"github":`+
				`{"token":"ghp_doc"}}}}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        base_url: https://legacy\n"),
			cfgLoad("config.json")),

		// `auth_token` reaches the SecureString through yaml.Marshal of whatever the
		// generic decode produced, so any scalar arrives as its printed text: `true`
		// is the four-character string, which is non-empty, which is a token. A port
		// that types the field as a string and drops what is not one loses the
		// registry entirely -- the emptiness test is what decides creation.
		configCase("security_a_bool_auth_token_reads_as_a_string",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n        clawhub:\n"+
				"            auth_token: true\n"),
			cfgLoad("config.json")),

		// A `registries` entry in config.json merges into the one the defaults carry,
		// because the type's `UnmarshalJSON` clones the stored entry and decodes over the
		// clone. Naming only `enabled` therefore leaves `base_url` at the default rather
		// than at the empty string -- the opposite of `channel_list`, where a map entry
		// is allocated fresh and the same document does wipe the defaults. The two
		// spellings look alike in a file and are decided by different code.
		configCase("config_registries_entry_merges_into_the_default",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":`+
				`{"github":{"enabled":true}}}}}`),
			cfgLoad("config.json")),

		// Registry names are trimmed, so a quoted key with padding lands on the entry
		// the defaults already carry instead of creating one whose name has spaces in
		// it. `clawhub` rather than `github` because the github path has the token
		// fallback, which would let an untrimmed port reach the same entry anyway.
		configCase("security_a_padded_registry_name_is_trimmed",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    \" clawhub \":\n        auth_token: tok_pad\n"),
			cfgLoad("config.json")),

		// ------------------------------------ security -- shapes the port could not see
		//
		// Twenty cases over six shapes the earlier corpus never used. The set was
		// established by injecting faults into the Rust port and recording which ones
		// walked through a green 53-case replay, not by listing the branches of
		// `apply_security` and ticking them off: the first six faults became six cases,
		// and a second round over the same surface still found holes. Every expectation
		// is what the shipped binary recorded rather than anything argued from the
		// source.
		//
		// Two things about the shape. First, none of these puts a `save` between the
		// two files, and that is the whole reason they were missing: a save moves
		// every secret out of config.json and into .security.yml, so by a second load
		// the JSON document no longer holds the value under test.
		// `security_entry_replaces_the_json_model` reads like proof that a security
		// entry without `api_keys` clears the model's keys, but it cannot tell
		// "cleared" from "left alone" -- the key it compares against had already been
		// written out of config.json three steps earlier. The case below has no save,
		// so `sk-1` is still in the document when the security file is applied.
		//
		// Second, the channel cases probe with `media_group_delay_ms` and
		// `mention_only`, not with a token. Two earlier drafts of these two cases used
		// `bot_token` and a `typing` node and both recorded a value *byte-identical to
		// the same config with no security file at all*: `bot_token` is a SecureString,
		// which `encoding/json` never writes, and `typing` is a Channel field rather
		// than a settings field, so it is `yaml:"-"` and unreadable from this file. A
		// case that cannot differ from its own control witnesses nothing, so the
		// probes are non-secret settings keys.
		//
		// What they then showed is an asymmetry between the two files that neither
		// tag suggests: a channel the *config* names replaces its default outright,
		// while a channel only the *security file* names merges into it.

		// The last shape the security file's `model_list` can be written in, and the one
		// that settles where a generated `name:index` key goes. The config lists `m2`
		// first and the file names `m2:0`, while `m1`'s key is generated by the save:
		// the loaded list is `[m2, m1__key_1, m1]` and the written section comes back
		// `m1:0: [k1]` then `m2:0: [sk]`. So "insert at the parsed index", "append" and
		// "follow the model list" are all refused -- the section is a **map**, and Go
		// marshals maps with sorted keys, so a key's position is a property of its name
		// and not of how it got there. The model order is the witness: with the list in
		// name order all three rules write the same bytes, which is why the input here
		// puts `m2` first. A port that models the section as a list has an insertion
		// order to choose and passes a corpus that never separates the three.
		// `virtual_key_models_are_written_back_once` already pins that a multi-key model
		// contributes only `name:0` with its primary key; this is the case where that
		// generated key has to share the section with a parsed one.
		configCase("security_model_list_keys_are_written_sorted_not_in_order",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m2","provider":"p",`+
				`"model":"r"},{"model_name":"m1","provider":"p","model":"q",`+
				`"api_keys":["k1","k2"]}]}`),
			cfgWrite(".security.yml", "model_list:\n    m2:0:\n        api_keys: [sk]\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A legacy `skills` value that is not a mapping is a refusal, not a no-op. Go's
		// yaml decode targets the typed `SkillsToolsConfig`, so a scalar, a sequence or a
		// number is an unmarshal error out of `loadSecurityConfig` and the whole load
		// fails. One case per shape, because a port that only handles the scalar still
		// accepts the sequence.
		configCaseErr("security_skills_scalar_is_refused", 2,
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills: hello\n"),
			cfgLoad("config.json")),

		configCaseErr("security_skills_sequence_is_refused", 2,
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    - github\n"),
			cfgLoad("config.json")),

		configCaseErr("security_skills_number_is_refused", 2,
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills: 7\n"),
			cfgLoad("config.json")),

		// `null` is the exception, and the reason the three refusals above need their own
		// cases rather than one "not a mapping" case: yaml.v3 unmarshals a null into the
		// zero value of the typed target without complaint, so the load succeeds and the
		// config is unchanged. A port that refuses "not an object" would reject a file Go
		// accepts -- which is the same class of bug as the refusals above, pointed the other
		// way.
		configCase("security_skills_null_loads_unchanged",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills: null\n"),
			cfgLoad("config.json")),

		// What a legacy registry token may be. `auth_token` is a SecureString, and yaml.v3
		// short-circuits `!!null` for a type with a custom unmarshaler: it zeroes the target
		// and never calls `UnmarshalYAML`. So an empty or explicit-null token leaves the
		// SecureString empty, the legacy pass `continue`s before `Set`, and the registry is
		// still created -- but with no `auth_token` in the rendering at all. A port that
		// stringifies the node manufactures a credential the shipped binary does not have,
		// and a blank legacy key then qualifies a registry.
		configCase("security_legacy_empty_token_creates_no_credential",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n        legacy_n:\n            auth_token:\n"),
			cfgLoad("config.json")),

		configCase("security_legacy_null_token_creates_no_credential",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n        legacy_n2:\n            auth_token: null\n"),
			cfgLoad("config.json")),

		// A number is a printed scalar too, so it *does* become a token. Both faces are
		// recorded because the pair is what makes the float attributable to the scalar's
		// kind rather than to the field: `42` creates a registry, and `1.5` must too.
		configCase("security_legacy_int_token_creates_a_credential",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n        legacy_i:\n            auth_token: 42\n"),
			cfgLoad("config.json")),

		configCase("security_legacy_float_token_creates_a_credential",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n        legacy_f:\n            auth_token: 1.5\n"),
			cfgLoad("config.json")),

		// `skills.github.token` in the security file *overwrites* the config's token; it does
		// not fill it. Pass 1 is `yaml.Unmarshal(data, cfg)` and yaml decodes a struct field
		// into the existing value, so `SecureString.UnmarshalYAML` assigns whatever the file
		// says -- including nothing. These two cases are the only way to see the direction:
		// with both sides non-empty the mask renders `[NOT_HERE]` either way, so an
		// empty-in-the-file value is the third state that separates fill from overwrite.
		configCase("security_github_document_token_null_with_a_file_entry",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"github":{"token":null}}}}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        base_url: https://github.com\n"),
			cfgLoad("config.json")),

		configCase("security_github_file_token_empty_overwrites_the_documents",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"github":{"token":"ghp_doc"}}}}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: \"\"\n"),
			cfgLoad("config.json")),

		// The other half: with the key absent the document's token survives, so the rule is
		// "take the file's text when the key is present", not "take the file's text when it
		// is non-empty".
		configCase("security_github_file_token_absent_keeps_the_documents",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"github":{"token":"ghp_doc"}}}}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        base_url: https://github.com\n"),
			cfgLoad("config.json")),

		// A security node whose kind disagrees with its target is a load failure, not an
		// overwrite. `loadSecurityConfig` decodes the whole file into the typed `Config` with
		// plain `yaml.Unmarshal`, so a scalar where a struct belongs and a map where a slice
		// belongs are both unmarshal errors out of the decode -- the same stance the
		// non-mapping `skills` refusals above take, and the reason a merge that "keeps the
		// base" or "takes the overlay" on a kind mismatch is wrong in both directions.
		configCaseErr("security_scalar_where_a_struct_belongs", 2,
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "web:\n    brave: oops\n"),
			cfgLoad("config.json")),

		configCaseErr("security_map_where_a_sequence_belongs", 2,
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "web:\n    brave:\n        api_keys: {a: b}\n"),
			cfgLoad("config.json")),

		// A `.security.yml` is a secrets file, and Go enforces that with tags rather than
		// with a filter: every field that is not a credential is `yaml:"-"` on the types
		// the file can reach -- `Channel`'s whole base (`config_channel.go:224-231`), a
		// search provider's `max_results` (`config.go:896`), and everything else -- so the
		// file can name a key the target type has and still set nothing. Measured:
		// `max_results: 9` under `web.brave` leaves the loaded value at the default 5,
		// identical to no security file. A port that merges whatever the file says sets 9
		// and produces a config the shipped binary cannot produce from that file.
		configCase("security_cannot_set_a_field_tagged_yaml_dash",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "web:\n    brave:\n        max_results: 9\n"),
			cfgLoad("config.json")),

		// A key the target type does not have at all is dropped, not stored. The security
		// file is decoded with plain `yaml.Unmarshal` -- not `UnmarshalStrict`, which is what
		// lets Go read the `web` and `skills` sections at all -- so an unknown key is silent
		// rather than a refusal, and it must not reach the document either. Pinned at two
		// depths because the merge walks each one separately: a provider under `web`, and a
		// top-level struct.
		configCase("security_unknown_key_under_a_provider_is_dropped",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "web:\n    brave:\n        api_keys: [bk]\n"+
				"        bogus: 1\n"),
			cfgLoad("config.json")),

		// The same unknown key, one op further: neither file it could land in comes back
		// with it. `config.json` is written from the typed struct and `.security.yml` from
		// the schema, so a key the security file invented is gone from both. This is the
		// byte-level half of the rule above -- the load case cannot see it either way, and a
		// port that renders either file straight from its merged document leaks `bogus` into
		// the user's config.
		configCase("security_unknown_key_reaches_neither_written_file",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "agents:\n    bogus: 1\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		configCase("security_unknown_key_under_a_top_level_struct_is_dropped",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "agents:\n    bogus: 1\n"),
			cfgLoad("config.json")),

		// A settings scalar the config already set is *replaced*, not filled. This is the
		// one shape that reaches the replace arm of the security merge: the key has to be
		// present on both sides, because a key only the file has is appended and never
		// touches that code. Measured 1234, from the file, over the config's 500.
		configCase("security_settings_replace_a_scalar_the_config_set",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"media_group_delay_ms":500}}}}`),
			cfgWrite(".security.yml", "channel_list:\n    telegram:\n        settings:\n"+
				"            media_group_delay_ms: 1234\n"),
			cfgLoad("config.json")),

		// A model entry written with an empty optional field does not keep it. `proxy` and
		// `api_base` are `omitempty`, so "" is dropped on the way out -- which is a different
		// document from the one the file had, and the port has to agree about which of the
		// model's fields are always written (`model_name`, `model`, `provider`) and which are
		// conditional. Measured: `[{"model_name":"m1","provider":"p","model":"q"}]`.
		// Not a security case -- there is no `.security.yml` in it. It is in this block
		// because the mutation round that opened the block is what found it, and the
		// fault that found it was committed against the model entry's own tags.
		configCase("model_entry_drops_its_empty_optional_fields",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"p","model":"q","proxy":"","api_base":""}]}`),
			cfgLoad("config.json")),

		// The `web` section is applied, and only a save can show it. Measured: the loaded
		// value for `web.brave.api_keys: [bk]` is byte-identical to the same config with no
		// security file at all -- every field under `web` that could carry a key is a
		// `SecureStrings`, and `SecureStrings.IsZero` reports true for any caller but the
		// YAML encoder, so `encoding/json` never writes one. What does differ is what gets
		// written back: with the section the security file comes out `brave:` /
		// `api_keys:` / `- bk`, without it `brave: {}`. So the save is the only rendering
		// that can witness this merge, and a load case for it would structurally be unable
		// to see its own subject.
		configCase("save_writes_back_a_web_key_from_the_security_file",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "web:\n    brave:\n        api_keys: [bk]\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A security entry that names a model but carries no `api_keys` clears the keys the
		// JSON document had: `SecureModelList.UnmarshalYAML` *assigns* the entry's keys to
		// the matching model rather than merging, so an entry with nothing in them assigns
		// nil rather than doing nothing.
		//
		// This is a save case because the load cannot see it, and it was a load case first.
		// Measured against a control with no security file at all, the recorded loaded value
		// is **identical** either way -- `api_keys` is a `SecureStrings`, invisible to
		// `encoding/json` like every other field in that family -- so the case as originally
		// written passed with the keys cleared and with the keys intact, and its name
		// claimed the opposite of what its bytes witnessed. What does differ is the file
		// written back: `m1:0: {}` with the entry, `m1:0:` / `api_keys:` / `- sk-1` without
		// it. A port that skips an entry with no keys keeps `sk-1` and writes credentials the
		// shipped binary would have dropped.
		configCase("save_clears_keys_for_a_security_entry_without_keys",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1"]}]}`),
			cfgWrite(".security.yml", "model_list:\n    m1:0: {}\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A real model field in a security entry is not read either -- the pass copies
		// `api_keys` and nothing else -- which the load cannot show for the same reason:
		// `provider` is written back from the JSON document either way. So this compares the
		// two files a save produces. A port that merges the whole entry writes
		// `provider: anthropic` into config.json and fails here; a port that only ever
		// merges keys passes, and passes the case above for the right reason.
		configCase("save_does_not_copy_a_model_field_from_a_security_entry",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"model":"llama-3","api_keys":["sk-1"]}]}`),
			cfgWrite(".security.yml", "model_list:\n    m1:0:\n      provider: anthropic\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A nested settings node *fills*, it does not replace. The config says
		// `streaming.enabled: true`, the security file says `streaming.throttle_seconds: 7`,
		// and the loaded value carries both. A port that assigns the security node over the
		// stored one loses `enabled` -- and loses it in the one place a user would notice,
		// since `streaming` is the section whose explicit `false` Go goes out of its way to
		// re-inject.
		configCase("security_channel_settings_fill_a_nested_node",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"streaming":{"enabled":true}}}}}`),
			cfgWrite(".security.yml", "channel_list:\n    telegram:\n        settings:\n"+
				"            streaming:\n                throttle_seconds: 7\n"),
			cfgLoad("config.json")),

		// A channel only the security file names, under a name that is not a channel type.
		// Refused -- and refused for the reason the case is named for, which is not what I
		// predicted: the entry's own `type: telegram` is **ignored**, because
		// `InitChannelList` infers the type from the map key whenever the channel's is empty
		// (config_channel.go:738) and the security merge leaves it empty even though `type`
		// is one of Channel's base fields. So the type is `mychan`, `isValidChannelType`
		// rejects it, and the load fails. A port that reads `type` out of the security entry
		// accepts the file; a port that only ever walks the channels it already has never
		// sees the entry at all and also accepts it. Both are wrong in opposite directions,
		// and only one refusal message names the key rather than a type.
		configCaseErr("security_channel_list_entry_with_an_unknown_name_is_refused", 2,
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "channel_list:\n    mychan:\n        enabled: true\n"+
				"        type: telegram\n"),
			cfgLoad("config.json")),

		// A security entry is applied to a channel the *config* already names, and it
		// merges into that channel instead of replacing it. Telegram is in the config
		// with one settings key and in the security file with another; the loaded value
		// carries `media_group_delay_ms: 1234` from the file on a channel that still has
		// its `enabled: true` and its type. Skipping channels already in the defaults
		// leaves the 0 here, and replacing the channel instead of merging loses the rest
		// of it. What this case cannot witness is its earlier name's claim, that the
		// merge happens into the file's zeros rather than into the defaults-filled
		// document: every telegram settings default except `media_group_delay_ms` is
		// zero, and the file overwrites that one, so both stories record the same bytes.
		// The replaced default is pinned where a field the file does *not* set is
		// visible -- `security_channel_settings_fill_a_nested_node` records
		// `media_group_delay_ms: 0` against a default of 500, and
		// `security_channel_list_entry_merges_into_the_default` shows the other half.
		configCase("security_settings_apply_to_a_channel_the_config_names",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"bot_token":"123:abc"}}}}`),
			cfgWrite(".security.yml", "channel_list:\n    telegram:\n        settings:\n"+
				"            media_group_delay_ms: 1234\n"),
			cfgLoad("config.json")),

		// The mirror image, and the reason the two cannot share one rule: `maixcam` is in
		// neither the config nor a user's file, only in the defaults and in the security
		// file. It comes back `settings: {host: "0.0.0.0", port: 9999}` -- the file's
		// `port` *and* the default `host` it never mentioned. A port that replaces
		// instead of merging drops `host`. The same case carries the other half of the
		// asymmetry in its telegram entry: the config names that one, and its
		// `media_group_delay_ms` comes back 0 where the default is 500, because a channel
		// the *config* names replaces its default outright. Discord would have witnessed
		// neither half -- `defaultChannels()` gives it `map[string]any{}` and every field
		// an earlier draft of this comment offered as evidence is also the zero value, so
		// merge and replace record identical bytes. One merge rule for both files is
		// wrong on one of them, and the direction it goes wrong in depends on which file
		// the port reads first.
		configCase("security_channel_list_entry_merges_into_the_default",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram"}}}`),
			cfgWrite(".security.yml", "channel_list:\n    maixcam:\n        settings:\n"+
				"            port: 9999\n"),
			cfgLoad("config.json")),

		// A legacy `skills.github.token` reaches the registry on the *migration* path
		// too, not only on the current version: a v1 config comes back with the secret
		// twice -- masked as `auth_token` and in plaintext as `token` -- as the v3 load
		// does. The mechanism is migration's **own in-line lift** (`migration.go:436-457`),
		// not `applyLegacySkillsSecurityConfig`: deleting that call leaves this case
		// untouched, while deleting the lift's `githubRegistry["auth_token"] = token`
		// moves this case and nothing else. Naming the pass here instead would have a
		// porter implement a call the shipped binary never makes on this path.
		configCase("security_legacy_token_on_the_migration_path",
			cfgWrite("config.json", `{"version":1}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: ghp_mig\n"),
			cfgLoad("config.json")),

		// Only three sections of a config are readable from `.security.yml`:
		// `channel_list`, `model_list` and `tools` (inlined, so it contributes `web` and
		// `skills`). Everything else on `Config` is `yaml:"-"`, so a section the file
		// spells in the JSON spelling -- or any section at all outside those three --
		// is not merely unknown to the decoder, it is unreadable: `agents.defaults`
		// keeps the empty string it had, and the key does not come back in either
		// written file. A port that walks the JSON field list instead of the yaml one
		// applies this section and diverges on every field inside it.
		configCase("security_agents_section_is_not_readable_from_the_file",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "agents:\n    defaults:\n"+
				"        modelname: from-security\n        bogus_key: keep_me\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A literal `tools:` section in the security file is discarded. `Tools` is
		// inlined, so the readable sections are `web` and `skills` at the top level and
		// `tools` is not one of them; the migration pre-merge then assigns
		// `m2["tools"] = m3` wholesale, so anything the file put under `tools` is
		// overwritten rather than merged. `ghp_from_tools` does not reach the registry,
		// while `ghp_mig` in the case above does -- which is what makes this a test of
		// the assignment and not of the unmarshal. A port that extends instead of
		// replacing writes a second credential the shipped binary never reads.
		configCase("migration_security_tools_section_is_dropped",
			cfgWrite("config.json", `{"version":1}`),
			cfgWrite(".security.yml", "tools:\n    skills:\n        github:\n"+
				"            token: ghp_from_tools\nweb:\n    brave:\n        api_keys: [bk]\n"),
			cfgLoad("config.json")),

		// ---------------------------------------------------------------- the pair --
		//
		// .security.yml is the yaml-tagged half and the JSON document is the rest;
		// neither file alone is the config. The half is NOT just channel_list and
		// model_list, which is what reading those two tags suggests: Tools carries
		// `yaml:",inline"`, so the yaml-tagged fields inside it -- web and skills --
		// are emitted at the TOP LEVEL of the security file. Measured on a default
		// config: four sections, channel_list, model_list, web, skills. A port that
		// inlines nothing loses two sections, and the next load then re-adds them
		// from defaults, so the file changes on every start.
		//
		// Inside channel_list only `settings` survives (every other Channel field is
		// yaml:"-"), and inside model_list nothing does -- each entry is `{}` keyed
		// name:index, because the secrets are what the file exists to hold and the
		// non-secret fields belong to the JSON document. web keeps insertion order
		// (brave, tavily, kagi, ...), which is a struct's field order, not a map's
		// sorted keys.

		// Saving a config that has channels and models writes both halves. The
		// channel vocabulary in v3 is enabled/type/allow_from/reasoning_channel_id/
		// group_trigger/typing/placeholder plus a free-form settings map, so a
		// bot token belongs to settings (and to .security.yml), not to the channel
		// itself -- which is why the same key at the top of a v1 channel is fine
		// and inside a v3 channel is refused.
		configCase("save_writes_both_halves",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":["1"],"settings":{"bot_token":"123:abc"}}},`+
				`"model_list":[{"model_name":"m1","provider":"openrouter","model":"llama-3",`+
				`"api_keys":["sk-1"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A model with two api_keys is expanded on load into one real entry plus
		// one virtual entry per extra key, and SaveConfig filters the virtual ones
		// back out. So the loaded value and the file disagree on purpose, and a
		// port that persists what it loaded grows the user's config on every save.
		configCase("virtual_key_models_are_written_back_once",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1","sk-2"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// The multi-key expansion copies the model field by field, and Go names the fields in
		// two different literals: `additionalEntry` (config.go:1802) never names `Fallbacks`,
		// and only `primaryEntry` gets it assigned afterwards (config.go:1849,
		// `append(fallbackNames, m.Fallbacks...)`). So the virtual copy carries no fallbacks
		// at all while the primary carries the generated names *before* the user's own. A
		// blocklist that excludes only `enabled` leaks the user's fallback list into every
		// copy, and `fallbacks` is `omitempty` over a non-empty slice, so it is written: a
		// byte difference in the loaded value of every failover-shaped model -- several keys
		// *and* a model fallback, which is what a failover setup actually looks like.
		configCase("virtual_key_model_copy_and_the_users_own_fallbacks",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"provider":"openrouter","model":"llama-3","api_keys":["sk-1","sk-2"],`+
				`"fallbacks":["z"]}]}`),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// The save writes both files from one document, and the registry token is the one
		// value whose rendering differs between them: `.security.yml` gets the real secret,
		// `config.json` keeps the `[NOT_HERE]` placeholder the marshaler writes. The plaintext
		// `token` key beside it is Go's own leak -- `SkillRegistryConfig.MarshalJSON`
		// hand-builds a map and re-emits every `Param` raw -- so it is pinned, not masked.
		// This is the pair the unmask/remask dance exists for, pinned directly on a `save`
		// op rather than through a migration's internal write.
		configCase("security_registry_token_save_splits_masked_and_real",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"github":{"token":"ghp_doc"}}}}`),
			cfgWrite(".security.yml", "skills:\n    github:\n        token: ghp_file\n"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// session is *SessionConfig with omitempty behind a custom MarshalJSON: it
		// is dropped when dimensions, identity_links and dm_scope are all empty,
		// which a plain struct tag cannot express because the zero struct is not
		// what decides.
		configCase("session_omitted_when_empty",
			cfgWrite("config.json", `{"version":3,"session":{"dimensions":[],"identity_links":{}}}`),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// A YAML file is refused before anything is read: the loader probes the
		// version with encoding/json, so the example config shipped as .yml is not
		// a config this code can load.
		configCaseErr("yaml_file_is_refused", 1,
			cfgWrite("config.json", "version: 3\nagents:\n  defaults:\n    workspace: ~/ws\n"),
			cfgLoad("config.json")),

		// Round-trip stability: save what you just loaded, load that, save again.
		// The two documents must be byte-identical, or every start rewrites the
		// user's file a little more.
		configCase("save_then_load_then_save_is_stable",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":["1"],"settings":{"bot_token":"123:abc"}},`+
				`"discord":{"enabled":false,"type":"discord","settings":{"bot_token":"tok"}}},`+
				`"model_list":[{"model_name":"m1","provider":"openrouter","model":"llama-3",`+
				`"api_keys":["sk-1","sk-2"]}],"agents":{"defaults":{"workspace":"~/ws",`+
				`"max_tokens":4096}}}`),
			cfgLoad("config.json"),
			cfgSave("config.json"),
			cfgLoad("config.json"),
			cfgSave("config.json")),

		// ---------------------------------------------------------------- strictness --
		//
		// Every decode goes through decodeJSONWithDiagnostics, which reflects the
		// target type and refuses a key it cannot place -- at any depth, with the
		// path to the offender. A port that lets one unknown key through loads a
		// file the Go build stops on, and then the two disagree about whether the
		// user's config is valid.

		// A typo inside one channel's own options is refused, named by path.
		configCaseErr("unknown_channel_option_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"nonsense":1}}}`),
			cfgLoad("config.json")),

		// And inside a model_list entry, where the index is part of the name.
		configCaseErr("unknown_model_field_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1","zzz":1}]}`),
			cfgLoad("config.json")),

		// A top-level key from a newer schema is refused the same way, so the
		// refusal a user sees for a misspelling is the same code path as the one
		// for a version they cannot load.
		configCaseErr("unknown_top_level_key_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"bogus":1}`),
			cfgLoad("config.json")),

		// ----------------------------------------------------------------- channels
		// Everything above pins what a channel entry looks like when the file names
		// its type. These pin the stage that turns a stored `settings` map into the
		// typed struct -- InitChannelList -- whose whole effect is that the loaded
		// value carries the struct, not the map. Each case here exists because
		// injecting the opposite behaviour into the Rust port left every case above
		// passing; the default channel list alone cannot see any of them.

		// "Infer Type from map key if not explicitly set": a channel keyed by a known
		// type name needs no `type` field, and it gets that type's settings struct --
		// every field of it, in declaration order.
		configCase("channel_type_is_inferred_from_the_key",
			cfgWrite("config.json", `{"version":3,"channel_list":{"maixcam":`+
				`{"enabled":true,"settings":{"host":"10.0.0.4"}}}}`),
			cfgLoad("config.json")),

		// The same inference is what refuses a channel named after nothing: the key
		// *becomes* the type, so a friendly name like "mybot" is an unknown type and
		// the load fails. A user gets this for a config that looks perfectly ordinary,
		// and a port that defaulted an unrecognised type to telegram would load it.
		configCaseErr("a_channel_key_that_is_no_type_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"mychan":`+
				`{"enabled":true,"settings":{"token":"x"}}}}`),
			cfgLoad("config.json")),

		// And the explicit type wins over the key, so a slack channel under a
		// telegram-shaped name gets slack's struct (which is empty, so `settings`
		// comes back as `{}` rather than as the keys the file mentioned).
		configCase("explicit_channel_type_wins_over_the_key",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":`+
				`{"type":"slack","settings":{"bot_token":"x","app_token":"y"}}}}`),
			cfgLoad("config.json")),

		// A type the registry does not know is refused at load, not ignored: the
		// alternative is a channel that silently has no settings at all.
		configCaseErr("unknown_channel_type_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"x":{"type":"nosuchtype"}}}`),
			cfgLoad("config.json")),

		// A `null` entry is deleted rather than refused. What it must not do is
		// delete the default channel list with it.
		configCase("a_null_channel_entry_is_dropped",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":null}}`),
			cfgLoad("config.json")),

		// `"streaming": {"enabled": false}` means off, and the struct cannot say
		// that -- false is the zero value, so `streaming,omitzero` drops the whole
		// section and the reader would fall back to its own default. Go puts the
		// explicit false back through a map round trip, which also re-sorts every
		// key in the settings object.
		configCase("explicitly_disabled_streaming_survives",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"streaming":{"enabled":false}}}}}`),
			cfgLoad("config.json")),

		// A number with a fraction does not load into an int field, even when its value
		// is an integer: Go refuses `1500.0` for `media_group_delay_ms` outright. The
		// obvious port -- round-trip the number through the field's type -- accepts a
		// document the shipped binary rejects, and then writes it back.
		configCaseErr("a_fractional_number_in_an_int_channel_field", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"media_group_delay_ms":1500.0}}}}`),
			cfgLoad("config.json")),

		// The sibling of the case above, and the reason that case says anything: the
		// same field with the same value written without a fraction loads fine. Without
		// it the refusal is only attributable to the document as a whole -- the field,
		// the channel type and the fraction are all equally suspected, and a port that
		// refuses all three numbers for one shared wrong reason still passes.
		configCase("an_integer_in_an_int_channel_field_loads",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"media_group_delay_ms":1500}}}}`),
			cfgLoad("config.json")),

		// What a slice in a channel actually does, measured rather than assumed. The
		// tempting claim -- nil marshals as null and [] as [], so the two are different
		// documents -- is false here, and false in a way a port would get wrong in the
		// safe-looking direction: `Channel.AllowFrom` is `json:"allow_from,omitempty"`
		// (config_channel.go:227), and `omitempty` drops a length-0 slice exactly as it
		// drops a nil one. So in the loaded value absent, `null` and `[]` are one thing,
		// and only a non-empty list survives.
		//
		// That is a real port trap, not a curiosity: `Option<Vec<String>>` with
		// `skip_serializing_if = "Option::is_none"` writes `[]` for the empty case, which
		// is a document Go never writes and which a later load then reads back as one
		// permitted sender with an empty id.
		//
		// Second thing this case pins, and the only reason to keep three channels rather
		// than one: `channel_list` is a Go map, so it marshals with **sorted** keys and
		// the file's `a`, `b`, `c` lead the built-in defaults instead of trailing them.
		// No other case has a file-provided channel sorting ahead of a default.
		configCase("an_empty_channel_list_is_indistinguishable_from_an_absent_one",
			cfgWrite("config.json", `{"version":3,"channel_list":{`+
				`"a":{"enabled":true,"type":"telegram"},`+
				`"b":{"enabled":true,"type":"telegram","allow_from":[]},`+
				`"c":{"enabled":true,"type":"telegram","allow_from":["1"]}}}`),
			cfgLoad("config.json")),

		// The rest of the type matrix, one field at a time. `a_fractional_number_in_an_
		// int_channel_field` above pins int-vs-float; these pin that a field's declared
		// type is checked at all, which is a different rule and one a port can satisfy
		// for numbers while breaking for everything else. A projection that asks only
		// "is the target numeric" accepts `"enabled":"true"` and then re-emits a string
		// in the slot every downstream reader treats as a flag -- and with `save` behind
		// it, a config Go refused gets written back on the next start.
		//
		// A string where the field is a bool. Go: cannot unmarshal string into Go value
		// of type bool.
		configCaseErr("a_string_in_a_bool_channel_field", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":`+
				`{"enabled":"true","type":"telegram"}}}`),
			cfgLoad("config.json")),

		// A number where the field is a bool. Go has no number-to-bool coercion either,
		// so `1` is refused, not true. This is the case that separates "checks the
		// target's type" from "accepts whatever JSON numbers map to", which is the exact
		// mistake the fractional-number fix is made of, one type over. Measured wording,
		// from picoclaw's own decoder rather than encoding/json's: `type error at line 1,
		// column 24 for field "telegram.enabled": expected bool but got number`.
		configCaseErr("a_number_in_a_bool_channel_field", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":`+
				`{"enabled":1,"type":"telegram"}}}`),
			cfgLoad("config.json")),

		// The sibling that makes the two refusals above say something: a string in a
		// string field, in the same settings object of the same channel type, loads.
		// Without it the refusals are only attributable to `channel_list.telegram` as a
		// whole, and a port that refuses every channel entry carrying a string value
		// anywhere still passes.
		configCase("a_string_in_a_string_channel_setting_loads",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"base_url":"https://example.test"}}}}`),
			cfgLoad("config.json")),

		// The same rule one level down, because the nested struct is a separate decode:
		// `settings.streaming.enabled` is a bool inside StreamingConfig inside
		// TelegramSettings. A projection that type-checks the channel's own fields and
		// then hands `settings` to a generic walker lets `"yes"` through.
		configCaseErr("a_string_in_a_bool_inside_settings", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"streaming":{"enabled":"yes"}}}}}`),
			cfgLoad("config.json")),

		// A scalar where the whole settings object belongs. `settings` is a RawNode, so
		// nothing rejects these bytes at decode time -- the string is stored verbatim and
		// the refusal only arrives when InitChannelList decodes it into the typed struct
		// (config_channel.go:746, wrapped as `channel %q failed to decode settings`). A
		// port that reads the stored value with `get(field)` on a non-object simply gets
		// None for every field and emits the fully-shaped *default* settings: a silently
		// different config, not an error.
		configCaseErr("a_string_where_a_channel_settings_object_belongs", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":"nope"}}}`),
			cfgLoad("config.json")),

		// And an array in the same position, because the two shapes fail differently in
		// a port that tests `is_object()` only after trying to read fields from it.
		configCaseErr("an_array_where_a_channel_settings_object_belongs", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":[{"a":1}]}}}`),
			cfgLoad("config.json")),

		// The exception that keeps the five refusals above from being over-generalised.
		// `token` is a SecureString: a **struct**, so "a non-object where a struct
		// belongs is refused" read off the kind alone would reject this file too -- but
		// SecureString declares its own UnmarshalJSON (config_struct.go:241) which
		// decodes a bare string, so Go loads the file and keeps the secret. It is the
		// only struct reachable through a channel's settings that does not decode from an
		// object, which is why the rule needs the exception rather than a shape test.
		//
		// Measured, and the second half is not what reading the tags suggests: the
		// emitted settings carry base_url, proxy, use_markdown_v2 and
		// media_group_delay_ms and **no token at all**. Not `"[NOT_HERE]"`, and not the
		// secret. Channel.MarshalJSON drops SecureString fields outright (the rule
		// promoted for #1558), and it does so independently of `omitzero` -- a port that
		// only applies omitzero sees a non-zero SecureString, keeps it, and writes either
		// the placeholder or, if it echoes the stored value, every secret in the file.
		configCase("a_string_in_a_secure_string_channel_setting_loads",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"token":"hunter2"}}}}`),
			cfgLoad("config.json")),

		// ------------------------------------------------ channel-list validators
		// The tail of InitChannelList is two validators (config_channel.go:753 and :761),
		// and no case above reaches either: they fire only on a shape none of those files
		// uses. Both are worth pinning because each refuses a config a plausible port
		// would happily load -- and because a corpus of refusals alone proves nothing: a
		// failed case records error_op_index and no message, so a port that refuses the
		// file for an entirely wrong reason replays green. Every refusal below therefore
		// has a neighbouring acceptance that a wrong rule breaks.

		// validateChannelStreamingConfig: a negative streaming bound is refused. The
		// check runs on the **decoded typed struct**, not on the stored map, so it is
		// reached only after InitChannelList's Decode -- a port that validates the stored
		// keys before decoding is looking at a different document.
		configCaseErr("negative_streaming_throttle_seconds_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"streaming":{"enabled":true,"throttle_seconds":-1}}}}}`),
			cfgLoad("config.json")),

		// The sibling field, refused by its own comparison rather than a shared "any
		// negative number" rule: with only one of the two present, a port that checks
		// throttle alone replays green.
		configCaseErr("negative_streaming_min_growth_chars_is_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"streaming":{"enabled":true,"min_growth_chars":-1}}}}}`),
			cfgLoad("config.json")),

		// Zero is legal on both, which is what makes the two above about the sign and not
		// about the field being present at all.
		configCase("zero_streaming_bounds_load",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","settings":{"streaming":{"enabled":true,"throttle_seconds":0,`+
				`"min_growth_chars":0}}}}}`),
			cfgLoad("config.json")),

		// The switch that decides *which* channels are checked at all
		// (config_channel.go:793-802) names PicoSettings, TelegramSettings and
		// WeComSettings. These two refusals and the acceptance below are what pin that
		// membership: without them a port that validates any settings node carrying a
		// "streaming" key refuses a file Go accepts, and a port that hardcodes telegram
		// accepts a file Go refuses.
		configCaseErr("negative_streaming_bounds_are_refused_on_pico", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"assistant":{"enabled":true,`+
				`"type":"pico","settings":{"streaming":{"throttle_seconds":-1}}}}}`),
			cfgLoad("config.json")),

		configCaseErr("negative_streaming_bounds_are_refused_on_wecom", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"wecom1":{"enabled":true,`+
				`"type":"wecom","settings":{"streaming":{"min_growth_chars":-1}}}}}`),
			cfgLoad("config.json")),

		// And the other side of the switch's default arm: maixcam has no streaming field
		// at all, so the key is dropped when the struct is marshalled and the validator
		// never looks. A port that treats "the file has a streaming key" as the trigger
		// rejects this file.
		configCase("streaming_on_a_type_without_the_field_is_dropped_not_refused",
			cfgWrite("config.json", `{"version":3,"channel_list":{"maixcam":{"enabled":true,`+
				`"settings":{"streaming":{"throttle_seconds":-1}}}}}`),
			cfgLoad("config.json")),

		// validateSingletonChannels: a singleton type allows one **enabled** instance.
		// The count is keyed on the resolved type, so two differently-named channels of
		// the same singleton type is what a user hits by adding a second pico channel
		// and expecting it to work.
		configCaseErr("two_enabled_channels_of_a_singleton_type_are_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"pico1":{"enabled":true,`+
				`"type":"pico"},"pico2":{"enabled":true,"type":"pico"}}}`),
			cfgLoad("config.json")),

		// Disabled channels are not counted (`if !bc.Enabled { continue }`), so a second
		// instance is fine switched off. Without this case a port that counts entries
		// rather than enabled entries refuses a working config.
		configCase("a_disabled_second_singleton_channel_loads",
			cfgWrite("config.json", `{"version":3,"channel_list":{"pico1":{"enabled":true,`+
				`"type":"pico"},"pico2":{"enabled":false,"type":"pico"}}}`),
			cfgLoad("config.json")),

		// The registry is what makes this a rule about types rather than about channels:
		// discord is not registered as a singleton, so two enabled discord channels load
		// and the refusal above cannot be generalised to "one channel per type".
		configCase("two_enabled_channels_of_a_shared_non_singleton_type_load",
			cfgWrite("config.json", `{"version":3,"channel_list":{"d1":{"enabled":true,`+
				`"type":"discord"},"d2":{"enabled":true,"type":"discord"}}}`),
			cfgLoad("config.json")),

		// The second registered singleton type, so the registry is pinned as a set and
		// not as one name. It needs its own acceptance first: unlike pico, telegram and
		// wecom, `pico_client` is not in the default channel list, so without this case
		// the refusal below would also be satisfied by a port that simply does not know
		// the type -- and the corpus cannot tell those two refusals apart.
		configCase("a_single_pico_client_channel_loads",
			cfgWrite("config.json", `{"version":3,"channel_list":{"pc1":{"enabled":true,`+
				`"type":"pico_client"}}}`),
			cfgLoad("config.json")),

		configCaseErr("two_enabled_pico_client_channels_are_refused", 1,
			cfgWrite("config.json", `{"version":3,"channel_list":{"pc1":{"enabled":true,`+
				`"type":"pico_client"},"pc2":{"enabled":true,"type":"pico_client"}}}`),
			cfgLoad("config.json")),

		// FlexibleStringSlice (config_struct.go:22) is a []string whose UnmarshalJSON also
		// accepts a bare string, a bare number, and a mixed array -- its own doc comment says
		// it exists "so allow_from can contain both "123" and 123". The coercion is not a
		// no-op on the bytes: `"alice"` comes back as `["alice"]` and `123456789` as
		// `["123456789"]`, so a port that merely *accepts* a scalar and keeps it disagrees
		// with the recorded value. Inside an array each element is formatted on its own, and
		// the two arms differ: a number goes through `%.0f`, which rounds half to even (1.5
		// and 2.5 both print "2") and keeps the sign of a value that rounds to zero (-0.4
		// prints "-0"), while anything else goes through `%v` -- so a bool is "true" and an
		// object is Go's map syntax, "map[a:1]".
		//
		// The reachable load-side sites are Channel.allow_from (every channel type,
		// `omitempty` only) and the FlexibleStringSlice fields inside channel settings, which
		// the last case shows: `channels: "picoclaw"` and `request_caps: 9` both arrive as
		// one-element lists through the JSON decoder that settings go through.
		configCase("a_flexible_string_slice_from_a_bare_string",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":"alice"}}}`),
			cfgLoad("config.json")),

		configCase("a_flexible_string_slice_from_a_bare_number",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":123456789}}}`),
			cfgLoad("config.json")),

		// A non-unique witness, stated so the next reader does not hunt for the fault it is
		// the only one for: its fault set is a subset of the formats case below. The one thing
		// it carries that that case lacks is a *string* element beside a number, and deleting
		// the loop's `case string:` moves nothing -- the failover renders a string
		// identically. Keep it because it is the payload the type's own doc comment names and
		// the fixture is what the port replays; do not read it as pinning a rule.
		configCase("a_flexible_string_slice_from_a_mixed_array",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":["alice",123]}}}`),
			cfgLoad("config.json")),

		// The two formatting arms side by side, because the distinction is invisible in either
		// half alone: `%.0f` for a number, `%v` for everything else. Recorded as
		// ["2", "2", "-0", "true", "map[a:1]"].
		configCase("a_flexible_string_slice_formats_its_elements_individually",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":[1.5,2.5,-0.4,true,{"a":1}]}}}`),
			cfgLoad("config.json")),

		configCase("a_flexible_string_slice_inside_a_channels_settings",
			cfgWrite("config.json", `{"version":3,"channel_list":{"irc":{"enabled":true,`+
				`"type":"irc","settings":{"channels":"picoclaw","request_caps":9}}}}`),
			cfgLoad("config.json")),

		// A `registries:` section left empty in a security file is not the same file as one
		// that omits it. Recorded, with the document carrying a `custom` registry:
		//
		// | the section | the registries that load |
		// |---|---|
		// | `registries:` (null) | `clawhub` and `github`, both `enabled: false` and `base_url: ""`; `custom` gone |
		// | `registries: {}` | all three, enabled, with their base URLs |
		// | `registries: []` | byte-identical to `{}` |
		//
		// So a null replaces the slice -- yaml decodes `!!null` into a nil slice, and the
		// builtin pass then materialises the two builtins as zero values rather than as
		// defaults -- while an empty mapping or sequence leaves it alone. A port that reads an
		// empty section as "nothing to do" gets rows two and three right and row one wrong,
		// and wrong in the direction that silently disables a user's registries.
		//
		// What these three cases cannot do: no fault *in this tree* touches the
		// replace-on-null rule. yaml.v3 zeroes a slice target for a `!!null` node before
		// `SkillsRegistriesConfig.UnmarshalYAML` is reached -- the same short-circuit that makes
		// a null `SecureString` keep what it held, arriving at the opposite result because the
		// kind differs. So the cases pin the zero-value materialisation, correctly, and that is
		// the in-tree half. The port's rule for a null section is replace, not merge, and only
		// the port's own tests can hold that.
		configCase("a_null_registries_section_in_the_security_file",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"custom":`+
				`{"enabled":true,"base_url":"https://custom.example"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n"),
			cfgLoad("config.json")),

		configCase("an_empty_mapping_registries_section_in_the_security_file",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"custom":`+
				`{"enabled":true,"base_url":"https://custom.example"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    registries: {}\n"),
			cfgLoad("config.json")),

		// In a fixture diff this is byte-identical to the `{}` case above and reads as a
		// duplicate. It is not one: it is the corpus's only witness that the legacy pass skips
		// `registries` *before* decoding it. Deleting the `|| name == "registries"` guard
		// (`pkg/config/security.go:139`) moves this case and a pre-existing scalar case and
		// nothing else, aborting with `cannot unmarshal !!seq into map[string]interface {}`;
		// the `{}` case survives it, because an empty mapping decodes happily.
		configCase("an_empty_sequence_registries_section_in_the_security_file",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"custom":`+
				`{"enabled":true,"base_url":"https://custom.example"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    registries: []\n"),
			cfgLoad("config.json")),

		configCase("a_null_registries_section_beside_a_legacy_registry_key",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"custom":`+
				`{"enabled":true,"base_url":"https://custom.example"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n    legacy_k:\n"+
				"      token: ghp_legacy\n"),
			cfgLoad("config.json")),

		// Neither this nor the null case above gets its legacy key lifted, and the reason is
		// not the wipe and not the name `registries`: an entry under `skills:` is lifted iff it
		// carries an `auth_token` (`pkg/config/security.go:158-162`). `SkillRegistryConfig`'s
		// decoder reads `auth_token` by name (`config_struct.go:551`) and everything else --
		// `token:` included -- falls through the `default:` arm into `Param`, which
		// `MarshalJSON` writes back as a top-level plaintext key. So `token: ghp_legacy` leaves
		// `AuthToken` empty and the lift `continue`s the entry before writing anything:
		// `legacy_k` is absent rather than present-but-tokenless. Naming `registries`, whatever
		// its value, does nothing to the lift -- the claim an earlier version of this comment
		// made, which the case below falsifies.
		configCase("an_empty_mapping_registries_section_beside_a_legacy_registry_key",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"custom":`+
				`{"enabled":true,"base_url":"https://custom.example"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    registries: {}\n    legacy_k:\n"+
				"      token: ghp_legacy\n"),
			cfgLoad("config.json")),

		// The case that makes the null pair discriminate. Both `beside_a_legacy_registry_key`
		// cases above create nothing in either half, so neither can rule out "the wipe ran
		// after the lift and took the new registry with it". Here a wipe and a lift happen in
		// one file and the credential is spelled the way the lift reads it: `clawhub` and
		// `github` come back zeroed, the document's `custom` is gone, and `legacy_k` is
		// **created** at `enabled: false, base_url: ""`. So the order is wipe first, lift
		// after -- which is exactly what a merge-order bug in a port gets wrong, in both
		// directions: merging instead of replacing leaves `custom`, and lifting before the wipe
		// deletes the registry the file just asked for.
		configCase("a_null_registries_section_beside_a_legacy_key_with_a_credential",
			cfgWrite("config.json", `{"version":3,"tools":{"skills":{"registries":{"custom":`+
				`{"enabled":true,"base_url":"https://custom.example"}}}}}`),
			cfgWrite(".security.yml", "skills:\n    registries:\n    legacy_k:\n"+
				"      auth_token: ghp_legacy\n"),
			cfgLoad("config.json")),

		// The lift copies `auth_token`, `base_url` and the `Param` keys -- never `Enabled`
		// (`pkg/config/security.go:166` onward). So `enabled: true` on a legacy entry is
		// dropped and the created registry is disabled, beside a `base_url` that does arrive.
		// Unpinned until now, and a porter writing "legacy entry -> registry" field by field
		// copies the flag and diverges on the one field that decides whether the registry is
		// used at all.
		configCase("a_legacy_registry_key_lifts_its_credential_but_not_enabled",
			cfgWrite("config.json", `{"version":3}`),
			cfgWrite(".security.yml", "skills:\n    legacy_k:\n"+
				"      auth_token: ghp_legacy\n"+
				"      base_url: https://legacy.example\n      enabled: true\n"),
			cfgLoad("config.json")),

		// The coercion routes a number through `float64`, so an integer past 2^53 is silently
		// rewritten on the way to `%.0f`: `12345678901234567890` comes back as
		// "12345678901234567168", both bare and inside an array. `123456789` is small enough to
		// hide this entirely, and a serde-based port that keeps the integer exact is the most
		// plausible way to disagree with Go here -- so this, not a second small number, is the
		// case that holds the rule.
		configCase("a_flexible_string_slice_past_2_pow_53_is_rewritten_through_float64",
			cfgWrite("config.json", `{"version":3,"channel_list":{"telegram":{"enabled":true,`+
				`"type":"telegram","allow_from":[12345678901234567890,`+
				`"12345678901234567890"]}}}`),
			cfgLoad("config.json")),

		// ------------------------------------------------------- legacy versions --
		//
		// Everything above this line is a version-3 document, which meant the whole
		// migration arm of LoadConfig -- `case 0..=2` -- was graded by zero cases. The
		// replay therefore could not see the pre-merge that makes a migration read a
		// different document than the files on disk, and could not see that the
		// migration *writes*: `SaveConfig` and `MakeBackup` both run inside the load, so
		// the directory a case compares against is one the load itself rewrote.
		// Recorded as rusty-crab#20.
		//
		// Two things every case here had to get right to be a witness at all. A valid
		// legacy document needs `model_name` AND `model`: `ModelConfig.Validate`
		// (pkg/config/config.go:839-844) requires both and runs before and after the
		// ladder, so a document carrying only `model_name` dies with
		// `model_list[0]: model is required` *after* the merge -- a probe bug, not a
		// finding. And the loaded value is blind to every credential on this path,
		// because `SecureStrings.IsZero` is true for any caller but the YAML encoder, so
		// `encoding/json` never writes one: for the credential cases the discriminating
		// bytes are in the `.security.yml` the migration rewrote, and each comment says
		// which half carries the fact.
		//
		// Deliberately absent: a `version` 1 document, measured byte-identical to the
		// `version` 2 case below in all three files, so it would witness the ladder
		// twice and nothing else; and a security `model_list` naming one slot two ways
		// (`m1:0:` and `m1:`), which Go decides by map order (migration.go:520) and
		// would be a flaky case by construction.
		configCase("a_version_two_document_is_migrated_and_rewritten",
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgLoad("config.json")),

		// Byte-identical to the case above in all three files, so it is not a second
		// witness of the ladder -- it is the only witness that an *absent* `version` means
		// "migrate" rather than "current". A port defaulting a missing version to
		// `CurrentVersion` produces the same loaded value and the same config.json and
		// simply never writes `config.json.<date>.bak`, so the file list is the whole
		// assertion. That is also why the case above keeps its backup: the two together
		// say the backup comes from the migration, not from the version number.
		configCase("a_document_with_no_version_key_is_migrated",
			cfgWrite("config.json", `{"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgLoad("config.json")),

		// `MakeBackup` covers both files: `.security.yml.<date>.bak` appears here and not
		// above, and it appears only when a security file existed to back up (Go's
		// `MakeBackup` returns without writing when the source is missing). The rewritten
		// `.security.yml` is byte-identical to the no-security-file case, so `tools: {}`
		// contributes nothing beyond triggering the backup -- which is the point: this case
		// is about the file list, not the contents, and a porter who diffs contents
		// against the case above will read it as a duplicate.
		configCase("a_legacy_document_backs_up_both_files_the_migration_rewrites",
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "tools: {}\n"),
			cfgLoad("config.json")),

		// The four refusals below share one fact the fixture cannot say: a refused
		// migration writes nothing. Each records `error_op_index` with `config.json` still
		// holding `"version": 2` and the security file exactly as written, so a port that
		// saves before it validates leaves a migrated file behind a load that failed. The
		// Go text is not portable and a failing case records none, so the sentences are
		// quoted here from measurement (one process per shape at origin/main) rather than
		// from the corpus.
		//
		// Go's lift asks `m2[key] != nil` twice: once to decide whether it runs at all,
		// and again **per key** inside it (migration.go:429, :433). A null-valued section
		// therefore stays in the merged document, where the strict validator names it --
		// `unknown field(s): web`. A lift that tests only presence loads this file.
		configCaseErr("a_null_web_section_is_refused_for_a_legacy_document", 2,
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "web:\n"),
			cfgLoad("config.json")),

		// The mixed shape, and the one that a one-level-too-high guard survives: the outer
		// test is satisfied by the `skills` mapping, so the pass runs, and only the per-key
		// test keeps the null `skills` out of the lift. Go refuses
		// `unknown field(s): skills`; a port that lifts by presence loads it. This shape
		// reached no case at all until now, which is why a wrong guard here replayed green
		// through three review rounds.
		configCaseErr("a_null_skills_section_beside_a_web_mapping_is_refused_for_a_legacy_document", 2,
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "web:\n    brave:\n        api_keys: [bk]\nskills:\n"),
			cfgLoad("config.json")),

		// Both null: the pass does not run at all and each key is named on its own
		// (`unknown field(s): skills, web` in Go's map order, which the corpus records no
		// text for and so does not pin). Included because it is the shape a hand-edited
		// file with two bare section lines actually has, and because it is the only one of
		// the three where an outer guard alone and an inner guard alone disagree about
		// *what* gets refused -- here, nothing: both nulls are refused either way.
		configCaseErr("two_null_security_sections_are_refused_for_a_legacy_document", 2,
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "web:\nskills:\n"),
			cfgLoad("config.json")),

		// The same `!= nil` fact one level down. Inside the rewrite, a valueless
		// `clawhub:` is nil, so Go's guard skips the rewrite *and* the `delete` inside it,
		// leaving `clawhub` under `skills` for the validator to name
		// (`unknown field(s): tools.skills.clawhub`). Give the same key a value --
		// `clawhub: {token: ch}` -- and the rewrite moves it to `registries` and the file
		// loads at both versions. So the refusal is about the null, not about the name, and
		// a port that rewrites unconditionally refuses a file Go accepts while this one
		// refuses a file Go refuses.
		configCaseErr("a_valueless_clawhub_under_a_legacy_security_file_is_refused", 2,
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "skills:\n    clawhub:\n"),
			cfgLoad("config.json")),

		// The pair, and the only one of these cases where the version number alone decides
		// the answer. Same two files: at `version` 2 the file's `channel_list` reaches the
		// loaded config -- `telegram.enabled` comes back **true**, overriding both the
		// built-in default and a `false` the document carries -- and it merges per key at
		// the channel level, so the document's other channels and the named channel's
		// other fields survive. At `version` 3 the same section is **ignored**: `enabled`
		// stays false, because the current-version security loader walks a narrow section
		// allowlist that does not name `channel_list`. Measured one process per shape, and
		// the reason the two cases sit adjacent: neither is a witness without the other.
		// Note the second half, visible only in the files: at `version` 2 the rewritten
		// `.security.yml` is byte-identical to a migration with no security file, so the
		// channel setting is applied and then not written back.
		configCase("a_legacy_security_channel_list_reaches_the_loaded_config",
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "channel_list:\n    telegram:\n        enabled: true\n"),
			cfgLoad("config.json")),

		configCase("a_current_security_channel_list_is_ignored",
			cfgWrite("config.json", `{"version":3,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "channel_list:\n    telegram:\n        enabled: true\n"),
			cfgLoad("config.json")),

		// The file's credential wins and the document's is gone -- the rewritten
		// `.security.yml` carries `sk-file` and no `sk-doc`, where the same migration with
		// no security file writes `api_keys` nowhere at all. The loaded value shows
		// neither: `model_list[0]` comes back as `model_name`, `provider` and `model` with
		// no `api_keys` key, because `SecureStrings` is never marshalled by
		// `encoding/json`. So this is a case whose assertion lives entirely in the file the
		// migration wrote, and a port that grades only the loaded value cannot see which
		// key won.
		configCase("a_legacy_security_model_list_key_wins_over_the_documents",
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o","api_keys":["sk-doc"]}]}`),
			cfgWrite(".security.yml", "model_list:\n    m1:0:\n        api_keys: [sk-file]\n"),
			cfgLoad("config.json")),

		// Same blindness, other section: the file's `brave.api_keys` is carried through the
		// migration and re-emitted into the rewritten `.security.yml` (`brave:` with an
		// `api_keys:` entry, where the no-security-file baseline writes `brave: {}`), while
		// the loaded value reports `tools.web.brave.api_keys` as **null** and
		// `tools.web.enabled` at its default. So a `web` credential can only be graded on
		// the written file: a case read off the loaded value is structurally unable to see
		// its own subject, and would report a key that survives as a key that is lost.
		// That is also why the port's `web` merge gap (rusty-crab#16) could live in its
		// ledger with no corpus case against it -- the one case that does grade it,
		// `save_writes_back_a_web_key_from_the_security_file`, has to save before it can
		// see anything.
		configCase("a_legacy_security_web_key_survives_into_the_rewritten_file",
			cfgWrite("config.json", `{"version":2,"model_list":[{"model_name":"m1",`+
				`"model":"openai/gpt-4o"}]}`),
			cfgWrite(".security.yml", "web:\n    brave:\n        api_keys: [bk]\n"),
			cfgLoad("config.json")),
	}
}
