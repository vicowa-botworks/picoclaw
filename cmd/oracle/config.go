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
	}
}
