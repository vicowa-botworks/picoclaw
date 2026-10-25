package main

// The memory-store seam.
//
// Unlike the other four fixtures this one does not call a function and record
// what it returned. It drives the real JSONLStore through a script of public
// calls in a temporary directory and records **what is left on disk** — file
// names and exact file bytes — plus whatever the script's final query returned.
// That is the only honest shape for this seam: the artifact is the directory,
// and a port can pass every round-trip test in the Go package while writing
// bytes the original could not read back.
//
// Two things are masked, because they are wall-clock and nothing else:
// a timestamp value anywhere in a file becomes "<time>", and a metadata
// timestamp in a query result becomes "<time>" or null. Presence, position and
// the surrounding bytes stay pinned; the timestamp *format* is pinned by the
// protocoltypes fixture, which compares it byte for byte.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// memoryStoreNote is the specification the Rust side reads alongside the cases.
// It states measured behavior, not intended behavior: the scope entry in
// particular is the opposite of what "it is a RawMessage, so it is stored
// verbatim" predicts, and the fixture, not the comment, is what a port is graded
// against.
const memoryStoreNote = "input.ops is a script run against a fresh store in an empty directory; " +
	"expected is {files, result}, or {error, <op>} for the cases that must fail. files maps " +
	"each file the store created to its exact contents, with any RFC3339 timestamp replaced " +
	"by <time> because the value is wall-clock; the timestamp format itself is pinned byte for " +
	"byte by the protocoltypes fixture. " +
	"A session key becomes sanitizeKey(key)+\".jsonl\" and the same stem +\".meta.json\", where " +
	"sanitizeKey replaces only ':', '/' and '\\\\' with '_' and nothing else: a key with a newline " +
	"in it produces a filename with a newline in it, and ':' and '/' name the same file. " +
	"The .jsonl is one compact json.Marshal per message plus LF with no trailing blank line, " +
	"HTML-escaped like every Go encoding; the .meta.json is json.MarshalIndent with two spaces " +
	"and no trailing newline, keys in declaration order (key, summary, skip, count, created_at, " +
	"updated_at), then scope and aliases only when non-empty. " +
	"scope is a json.RawMessage but is not copied through untouched: MarshalIndent re-indents " +
	"it to the enclosing document and re-escapes it, so a scope of {\"q\":\"<b> &\"} comes out of " +
	"the file with \\u003c and \\u0026; a stored literal null is emitted as null and is not " +
	"omitted, because it is non-empty bytes; a scope that is not valid JSON is refused when the " +
	"metadata is encoded. " +
	"An assistant message with blank content and non-blank reasoning_content and no tool calls, " +
	"media, attachments or tool_call_id is transient: it is never appended, so a session holding " +
	"only one creates no files at all, and one already on disk is filtered on read. " +
	"skip counts raw non-blank lines including ones that fail to decode, so a torn final line is " +
	"dropped but still consumes a slot, and meta.count is that same raw count and so can exceed " +
	"the number of messages GetHistory returns. A corrupt line is skipped rather than fatal, but " +
	"a corrupt .meta.json is fatal to GetHistory, because the skip offset comes from it and " +
	"defaulting it to zero would replay truncated history. Unknown JSON members are ignored in " +
	"both files; CRLF endings and a missing final newline both read. " +
	"TruncateHistory only moves the skip offset and never touches the .jsonl; Compact is the " +
	"only operation that rewrites it and is a no-op when skip is 0. SetHistory filters transient " +
	"thoughts, stamps any missing created_at and zeroes skip. ListSessions is driven by the " +
	".meta.json files in directory order and reports the canonical key from each, so a key with " +
	"':' is listed with its ':' even though its file uses '_'; a meta file that does not decode, " +
	"or whose key is empty, is not listed, and an orphan .jsonl is invisible."

// timeMask matches anything Go's time.Time encoder produces. It is deliberately
// permissive: the point is to hide the value, and a port that writes a
// non-timestamp where a timestamp belongs must still fail.
var timeMask = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// maskedTime is the query-result form of the same rule.
const maskedTime = "<time>"

// storeStep is one operation in a session-store script. The unexported fields
// are what the Go side executes; describeStep renders the same step into the
// portable form the Rust side replays.
type storeStep struct {
	op      string
	key     string
	msg     *protocoltypes.Message
	msgs    []protocoltypes.Message
	role    string
	text    string
	keep    int
	summary string
	scope   string
	aliases []string
}

func (s storeStep) describe() map[string]any {
	out := map[string]any{"op": s.op}
	switch s.op {
	case "add":
		out["key"] = s.key
		out["message"] = describeMessage(*s.msg)
	case "add_simple":
		out["key"] = s.key
		out["role"] = s.role
		out["content"] = s.text
	case "truncate":
		out["key"] = s.key
		out["keep_last"] = s.keep
	case "set_history":
		out["key"] = s.key
		out["messages"] = describeMessages(s.msgs)
	case "compact", "list_all":
		if s.key != "" {
			out["key"] = s.key
		}
	case "set_summary":
		out["key"] = s.key
		out["summary"] = s.summary
	case "upsert_meta":
		out["key"] = s.key
		if s.scope == "" {
			out["scope"] = nil
		} else {
			out["scope"] = s.scope
		}
		out["aliases"] = describeStrings(s.aliases)
	case "write_raw", "write_raw_meta":
		out["key"] = s.key
		out["content"] = s.text
	case "get_history", "get_meta":
		out["key"] = s.key
	default:
		panic("oracle: unknown store step " + s.op)
	}
	return out
}

func describeMessages(msgs []protocoltypes.Message) any {
	if msgs == nil {
		return nil
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, describeMessage(m))
	}
	return out
}

// ---------------------------------------------------------------- step sugar --

func stepAdd(key string, m protocoltypes.Message) storeStep {
	return storeStep{op: "add", key: key, msg: &m}
}

func stepAddSimple(key, role, content string) storeStep {
	return storeStep{op: "add_simple", key: key, role: role, text: content}
}

func stepTruncate(key string, keepLast int) storeStep {
	return storeStep{op: "truncate", key: key, keep: keepLast}
}

func stepSetHistory(key string, msgs ...protocoltypes.Message) storeStep {
	return storeStep{op: "set_history", key: key, msgs: msgs}
}

func stepCompact(key string) storeStep { return storeStep{op: "compact", key: key} }

func stepSetSummary(key, summary string) storeStep {
	return storeStep{op: "set_summary", key: key, summary: summary}
}

func stepUpsertMeta(key, scope string, aliases []string) storeStep {
	return storeStep{op: "upsert_meta", key: key, scope: scope, aliases: aliases}
}

// stepWriteRaw replaces the session's .jsonl with exact bytes, bypassing the
// store. This is how a crash is simulated: a torn final line, a stray blank
// line, a line the store would never have written.
func stepWriteRaw(key, content string) storeStep {
	return storeStep{op: "write_raw", key: key, text: content}
}

// stepWriteRawMeta overwrites the .meta.json with exact bytes. This is how a
// half-written or hand-edited metadata file is simulated.
func stepWriteRawMeta(key, content string) storeStep {
	return storeStep{op: "write_raw_meta", key: key, text: content}
}

func stepGetHistory(key string) storeStep { return storeStep{op: "get_history", key: key} }
func stepGetMeta(key string) storeStep    { return storeStep{op: "get_meta", key: key} }
func stepList() storeStep                 { return storeStep{op: "list_all"} }

// ------------------------------------------------------------------ the case --

func memoryCase(name string, steps ...storeStep) fixtureCase {
	described := make([]any, 0, len(steps))
	for _, s := range steps {
		described = append(described, s.describe())
	}
	return fixtureCase{
		Name:   name,
		Input:  map[string]any{"ops": described},
		script: steps,
	}
}

// memoryCaseErr records a script that is *supposed* to fail at wantErrOp. The
// expected value is then just the name of the operation that failed: the Go
// error text is not portable and pinning it would force the Rust port to
// reproduce a sentence. What the port has to reproduce is that the call is
// refused at all, and where.
func memoryCaseErr(name, wantErrOp string, steps ...storeStep) fixtureCase {
	c := memoryCase(name, steps...)
	c.expectErrOp = wantErrOp
	return c
}

// ------------------------------------------------------------------- runner --

// runStoreScript executes the script and returns the directory it left behind.
// Any error is fatal: a script step that the Go store rejects is a bug in the
// corpus, not a case to record.
func runStoreScript(name string, steps []storeStep, wantErrOp string) any {
	dir, err := os.MkdirTemp("", "picoclaw-oracle-memory-")
	if err != nil {
		fail("memory: temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		fail("memory: new store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	var result any

	for _, s := range steps {
		switch s.op {
		case "add":
			err = store.AddFullMessage(ctx, s.key, *s.msg)
		case "add_simple":
			err = store.AddMessage(ctx, s.key, s.role, s.text)
		case "truncate":
			err = store.TruncateHistory(ctx, s.key, s.keep)
		case "set_history":
			err = store.SetHistory(ctx, s.key, s.msgs)
		case "compact":
			err = store.Compact(ctx, s.key)
		case "set_summary":
			err = store.SetSummary(ctx, s.key, s.summary)
		case "upsert_meta":
			var scope json.RawMessage
			if s.scope != "" {
				scope = json.RawMessage(s.scope)
			}
			err = store.UpsertSessionMeta(ctx, s.key, scope, s.aliases)
		case "write_raw", "write_raw_meta":
			var stem string
			stem, err = stemFor(ctx, store, dir, s.key)
			if err == nil {
				suffix := ".jsonl"
				if s.op == "write_raw_meta" {
					suffix = ".meta.json"
				}
				err = os.WriteFile(filepath.Join(dir, stem+suffix), []byte(s.text), 0o644)
			}
		case "get_history":
			var msgs []protocoltypes.Message
			msgs, err = store.GetHistory(ctx, s.key)
			result = describeMessages(msgs)
		case "get_meta":
			var meta memory.SessionMeta
			meta, err = store.GetSessionMeta(ctx, s.key)
			result = describeMeta(meta)
		case "list_all":
			result = describeStrings(store.ListSessions())
		default:
			fail("memory: unknown step %q", s.op)
		}
		if err != nil {
			if s.op == wantErrOp {
				return map[string]any{"error": s.op}
			}
			fail("memory: step %s on %q: %v", s.op, s.key, err)
		}
	}

	if wantErrOp != "" {
		fail("memory: case %q expected %q to fail, and it did not", name, wantErrOp)
	}

	files, err := readStoreDir(dir)
	if err != nil {
		fail("memory: read back: %v", err)
	}
	return map[string]any{"files": files, "result": maskTimes(result)}
}

// stemFor asks the store which file a key maps to, rather than computing it
// here. The whole point of the filename cases is that the port must discover
// the mapping from the store's behavior, so the oracle refuses to duplicate
// sanitizeKey and risk agreeing with itself.
//
// It works by letting the store create the .meta.json for the key and then
// reading the stem back off the directory: the two files share one stem. A meta
// file that no longer decodes is matched by its stem instead, so a case can
// corrupt a meta file and still address it.
func stemFor(
	ctx context.Context,
	store *memory.JSONLStore,
	dir, key string,
) (string, error) {
	stem, found := findStemByKey(dir, key)
	if found {
		return stem, nil
	}
	if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
		return "", err
	}
	stem, found = findStemByKey(dir, key)
	if !found {
		return "", fmt.Errorf("no meta file found for key %q", key)
	}
	return stem, nil
}

func findStemByKey(dir, key string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var meta memory.SessionMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if meta.Key == key {
			return strings.TrimSuffix(entry.Name(), ".meta.json"), true
		}
	}
	return "", false
}

// readStoreDir returns every file in the store directory, name to contents.
// Contents are masked for wall-clock only; every other byte is the contract.
func readStoreDir(dir string) (map[string]string, error) {
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
		files[entry.Name()] = timeMask.ReplaceAllString(string(data), maskedTime)
	}
	return files, nil
}

// generatorStart is the moment this process began. It is the line between a
// timestamp the corpus supplied and a timestamp the code under test produced:
// every scripted time in this file is a constant in the past, so anything at or
// after process start can only have come from the store's own time.Now().
//
// Masking those is not only what makes the fixture reproducible — it also keeps
// the generator's timezone out of it. A stamped time is marshaled with the
// local offset, so without the mask this file would record -14400 on one machine
// and +0800 on another, and the port would fail for a reason that is not a bug.
var generatorStart = time.Now()

// maskTimes applies the same rule to a query result: any timestamp string, and
// any described time the store itself produced, becomes <time>.
func maskTimes(v any) any {
	switch t := v.(type) {
	case string:
		return timeMask.ReplaceAllString(t, maskedTime)
	case []any:
		for i, item := range t {
			t[i] = maskTimes(item)
		}
		return t
	case map[string]any:
		if stampedByStore(t) {
			return maskedTime
		}
		for k, item := range t {
			t[k] = maskTimes(item)
		}
		return t
	default:
		return v
	}
}

// stampedByStore reports whether v is the portable description of a time that
// could only have come from time.Now().
func stampedByStore(v map[string]any) bool {
	if len(v) != 3 {
		return false
	}
	unix, ok := v["unix_seconds"].(int64)
	if !ok {
		return false
	}
	_, hasNanos := v["nanos"]
	_, hasOffset := v["offset_seconds"]
	return hasNanos && hasOffset && unix >= generatorStart.Unix()-1
}

func describeMeta(m memory.SessionMeta) map[string]any {
	scope := any(nil)
	if len(m.Scope) > 0 {
		scope = string(m.Scope)
	}
	created := any(nil)
	if !m.CreatedAt.IsZero() {
		created = maskedTime
	}
	updated := any(nil)
	if !m.UpdatedAt.IsZero() {
		updated = maskedTime
	}
	return map[string]any{
		"key":        m.Key,
		"summary":    m.Summary,
		"skip":       m.Skip,
		"count":      m.Count,
		"created_at": created,
		"updated_at": updated,
		"scope":      scope,
		"aliases":    describeStrings(m.Aliases),
	}
}

// ------------------------------------------------------------------- corpus --

// at builds a message with an explicit created_at, so the .jsonl bytes are
// reproducible. Cases that want to observe the store's own stamping pass nil
// and let the mask absorb the value.
func at(role, content string, unix int64, nanos int32) protocoltypes.Message {
	t := time.Unix(unix, int64(nanos)).UTC()
	return protocoltypes.Message{Role: role, Content: content, CreatedAt: &t}
}

func memoryCases() []fixtureCase {
	cases := []fixtureCase{
		// -- filenames -------------------------------------------------------
		// sanitizeKey collapses ':', '/' and '\' to '_'. Nothing else. A port
		// that percent-encodes cannot find existing history; one that also
		// strips other characters silently splits a session in two.
		memoryCase("name_plain",
			stepAddSimple("agent:main:signal:+447700900000", "user", "hi"),
			stepGetMeta("agent:main:signal:+447700900000")),
		memoryCase("name_slashes",
			stepAddSimple("channel/123/thread_456", "user", "hi"),
			stepGetMeta("channel/123/thread_456")),
		memoryCase("name_backslash",
			stepAddSimple(`slack\C0123\1700000000.000100`, "user", "hi")),
		memoryCase("name_mixed_separators",
			stepAddSimple("a:b/c\\d:e", "user", "hi")),
		memoryCase("name_relative_path",
			stepAddSimple("../../etc/passwd", "user", "hi")),
		memoryCase("name_dot_only", stepAddSimple(".", "user", "hi")),
		memoryCase("name_empty", stepAddSimple("", "user", "hi")),
		memoryCase("name_spaces_and_tabs", stepAddSimple("a b\tc d", "user", "hi")),
		memoryCase("name_unicode", stepAddSimple("agent:日本語:🦀", "user", "hi")),
		memoryCase("name_newline", stepAddSimple("a\nb", "user", "hi")),
		// ':' and '/' collapse to the same character, so these two keys are one
		// session. Documented as a hazard: a port that escapes them differently
		// would keep them apart and lose half the history on upgrade.
		memoryCase("name_collision_colon_and_slash",
			stepAddSimple("a:b", "user", "first"),
			stepAddSimple("a/b", "user", "second"),
			stepGetHistory("a:b"),
			stepList()),

		// -- file bytes ------------------------------------------------------
		memoryCase("jsonl_two_messages_exact_bytes",
			stepAdd("s1", at("user", "hello", 1700000000, 0)),
			stepAdd("s1", at("assistant", "world", 1700000001, 123456789))),
		memoryCase("jsonl_html_escaped_on_disk",
			stepAdd("s1", at("user", "<b> & \"quoted\" \u2028", 1700000000, 0))),
		memoryCase("jsonl_control_chars_and_unicode",
			stepAdd("s1", at("user", "a\tb\nc\u0001d\u00e9\u65e5\U0001f980", 1700000000, 0))),
		memoryCase("jsonl_media_and_attachments",
			stepAdd("s1", withAttachments(at("user", "look", 1700000000, 0)))),
		memoryCase("jsonl_tool_call",
			stepAdd("s1", withToolCall(at("assistant", "", 1700000000, 0)))),
		memoryCase("jsonl_empty_content_and_reasoning",
			stepAdd("s1", at("assistant", "", 1700000000, 0))),
		// The store stamps created_at when the caller leaves it out, and does
		// not re-stamp on read.
		memoryCase("jsonl_created_at_stamped_on_write",
			stepAddSimple("s1", "user", "no timestamp"),
			stepGetHistory("s1")),
		// The mirror image: a message that already carries a timestamp keeps it.
		// The value here is a constant in the past, so the mask cannot be hiding a
		// re-stamp — if the store overwrote it on read, the fixture would say so.
		memoryCase("jsonl_explicit_created_at_preserved_on_read",
			stepAdd("s1", at("user", "kept", 1700000000, 100000000)),
			stepGetHistory("s1")),

		// -- what is never written ------------------------------------------
		// An assistant message whose only content is reasoning is transient: it
		// is not appended at all, so the session does not exist afterwards.
		memoryCase("transient_thought_creates_no_files",
			stepAdd("s1", transientThought()),
			stepGetHistory("s1"),
			stepList()),
		memoryCase("transient_thought_among_real_messages",
			stepAdd("s1", at("user", "q", 1700000000, 0)),
			stepAdd("s1", transientThought()),
			stepAdd("s1", at("assistant", "a", 1700000001, 0)),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		// The same rule has teeth: with a tool call, or a media item, or a
		// tool_call_id, the message is real even though it is blank.
		memoryCase("blank_assistant_with_tool_call_is_kept",
			stepAdd("s1", withToolCall(transientThought())),
			stepGetHistory("s1")),
		memoryCase("blank_assistant_with_whitespace_content_is_transient",
			stepAdd("s1", withReasoning("   ", "  thought  ")),
			stepGetHistory("s1"),
			stepList()),
		memoryCase("blank_assistant_with_untrimmed_reasoning_is_transient",
			stepAdd("s1", withReasoning("", "\n\tthought\n")),
			stepList()),

		// -- skip semantics --------------------------------------------------
		memoryCase("truncate_keeps_last_two",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepAdd("s1", at("user", "2", 1700000001, 0)),
			stepAdd("s1", at("user", "3", 1700000002, 0)),
			stepAdd("s1", at("user", "4", 1700000003, 0)),
			stepTruncate("s1", 2),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		// The file is never rewritten by a truncate: history is skipped, not
		// deleted, and a port that physically rewrites loses data the Go store
		// still considers recoverable.
		memoryCase("truncate_leaves_the_file_alone",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepAdd("s1", at("user", "2", 1700000001, 0)),
			stepTruncate("s1", 0),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("truncate_zero_clears_everything",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepTruncate("s1", 0),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("truncate_negative_clears_everything",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepTruncate("s1", -5),
			stepGetMeta("s1")),
		memoryCase("truncate_more_than_exists_keeps_all",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepTruncate("s1", 10),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("truncate_on_missing_session",
			stepTruncate("nope", 3),
			stepGetMeta("nope"),
			stepList()),
		// Truncating, appending more, then reading: the new lines land after the
		// skipped region, so skip must not be recomputed from the file length.
		memoryCase("truncate_then_append",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepAdd("s1", at("user", "2", 1700000001, 0)),
			stepAdd("s1", at("user", "3", 1700000002, 0)),
			stepTruncate("s1", 1),
			stepAdd("s1", at("user", "4", 1700000003, 0)),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("compact_after_truncate_rewrites_file",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepAdd("s1", at("user", "2", 1700000001, 0)),
			stepAdd("s1", at("user", "3", 1700000002, 0)),
			stepTruncate("s1", 1),
			stepCompact("s1"),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("compact_without_truncate_is_a_noop",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepCompact("s1"),
			stepGetMeta("s1")),
		// skip counts raw lines, so a blank line between two messages does not
		// consume a slot but a corrupt one does.
		memoryCase("blank_lines_do_not_count_toward_skip",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\"}\n\n"+
				"{\"role\":\"user\",\"content\":\"2\"}\n\n"+
				"{\"role\":\"user\",\"content\":\"3\"}\n"),
			stepTruncate("s1", 1),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("corrupt_line_is_skipped_not_fatal",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\"}\n"+
				"{\"role\":\"user\",\"content\":\n"+
				"{\"role\":\"user\",\"content\":\"3\"}\n"),
			stepGetHistory("s1"),
			stepList()),
		// The torn line is the crash signature. It is the last line, so the
		// recovery is "drop the tail", and it still occupies a raw slot.
		memoryCase("torn_final_line_is_dropped",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\"}\n"+
				"{\"role\":\"user\",\"content\":\"trunc"),
			stepGetHistory("s1")),
		memoryCase("torn_final_line_counts_in_skip",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\"}\n"+
				"{\"role\":\"user\",\"content\":\"trunc"),
			stepTruncate("s1", 1),
			stepGetMeta("s1")),
		memoryCase("empty_file_reads_as_no_history",
			stepWriteRaw("s1", ""),
			stepGetHistory("s1"),
			stepList()),
		memoryCase("only_blank_lines",
			stepWriteRaw("s1", "\n\n\n"),
			stepGetHistory("s1")),
		memoryCase("crlf_line_endings",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\"}\r\n"),
			stepGetHistory("s1")),
		memoryCase("no_trailing_newline",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\"}"),
			stepGetHistory("s1")),
		// A line the store would never write: valid JSON, wrong shape. Go's
		// decoder ignores unknown members, so this decodes to a zero Message.
		memoryCase("unknown_fields_are_ignored",
			stepWriteRaw("s1", "{\"role\":\"user\",\"content\":\"1\",\"bogus\":42}\n"),
			stepGetHistory("s1")),
		memoryCase("json_array_line_is_corrupt",
			stepWriteRaw("s1", "[1,2,3]\n"),
			stepGetHistory("s1")),
		// A transient thought already on disk is filtered on read, so a session
		// written by an older build hides it the same way.
		memoryCase("transient_thought_on_disk_is_filtered_on_read",
			stepWriteRaw("s1", `{"role":"assistant","content":"",`+
				`"reasoning_content":"hmm","created_at":"2023-11-14T22:13:20Z"}`+"\n"),
			stepGetHistory("s1")),

		// -- metadata ------------------------------------------------------
		memoryCase("meta_shape_after_one_message",
			stepAdd("s1", at("user", "hi", 1700000000, 0))),
		memoryCase("meta_shape_after_summary_only",
			stepSetSummary("s1", "a summary")),
		memoryCase("meta_summary_empty_string_is_emitted",
			stepSetSummary("s1", "")),
		memoryCase("meta_missing_session_returns_key_only",
			stepGetMeta("absent")),
		// Scope is a json.RawMessage: its bytes go into the document verbatim,
		// indentation and all. A port that re-marshals it changes the file.
		memoryCase("meta_scope_is_reindented_by_marshal_indent",
			stepUpsertMeta("s1", "{\n  \"channel\": \"telegram\",\n  \"chat\":  1\n}", nil),
			stepGetMeta("s1")),
		// A RawMessage is *not* copied through untouched: MarshalIndent re-indents
		// it to the enclosing document and runs it through the same escaping, so
		// '<' and '&' come back out as \u003c and \u0026. A port that pastes the
		// stored bytes in verbatim produces a different file than Go did.
		memoryCase("meta_scope_is_reindented_and_reescaped",
			stepUpsertMeta("s1", `{"q":"<b> & \u2028"}`, nil),
			stepGetMeta("s1")),
		// Scope is stored as given and only validated when the meta file is
		// encoded, so a malformed scope is refused at write time rather than
		// silently repaired. The port has to refuse it too.
		memoryCaseErr("meta_scope_invalid_json_is_refused", "upsert_meta",
			stepUpsertMeta("s1", `{"a": <1>}`, nil),
			stepGetMeta("s1")),
		memoryCase("meta_scope_empty_is_omitted",
			stepUpsertMeta("s1", "", nil)),
		memoryCase("meta_scope_null_literal",
			stepUpsertMeta("s1", "null", nil),
			stepGetMeta("s1")),
		memoryCase("meta_aliases_drop_canonical_and_duplicates",
			stepUpsertMeta("s1", "", []string{"s1", " a ", "a", "", "b", "b"}),
			stepGetMeta("s1")),
		memoryCase("meta_aliases_all_blank_is_omitted",
			stepUpsertMeta("s1", "", []string{"", "  ", "s1"}),
			stepGetMeta("s1")),
		memoryCase("meta_aliases_nil_is_omitted",
			stepUpsertMeta("s1", "", nil),
			stepGetMeta("s1")),
		memoryCase("meta_upsert_preserves_count_and_skip",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepAdd("s1", at("user", "2", 1700000001, 0)),
			stepTruncate("s1", 1),
			stepUpsertMeta("s1", `{"x":1}`, []string{"alias"}),
			stepGetMeta("s1")),

		// -- listing -------------------------------------------------------
		memoryCase("list_empty_store", stepList()),
		memoryCase("list_returns_canonical_keys_not_filenames",
			stepAddSimple("agent:main:telegram:1", "user", "hi"),
			stepAddSimple("agent:main:telegram:2", "user", "hi"),
			stepList()),
		// The listing is driven by meta files: a stray .jsonl with no meta is
		// invisible, and a meta file the decoder rejects is skipped entirely.
		memoryCase("list_ignores_orphan_jsonl",
			stepWriteRaw("orphan", "{}\n"),
			stepList()),
		memoryCase("list_skips_unreadable_meta",
			stepAddSimple("good", "user", "hi"),
			stepAddSimple("bad", "user", "hi"),
			stepWriteRawMeta("bad", "not json at all\n"),
			stepList()),
		// The same unreadable meta is not survivable on the read path: the skip
		// offset comes from it, so GetHistory refuses rather than defaulting to
		// 0 and silently replaying truncated history.
		memoryCaseErr("get_history_with_corrupt_meta_errors", "get_history",
			stepAddSimple("s1", "user", "hi"),
			stepWriteRawMeta("s1", "{\"key\": \"s1\", \"cou"),
			stepGetHistory("s1")),
		memoryCase("meta_with_unknown_fields_still_loads",
			stepAddSimple("s1", "user", "hi"),
			stepWriteRawMeta("s1",
				"{\n  \"key\": \"s1\",\n  \"summary\": \"\",\n  \"skip\": 1,\n  \"count\": 1,\n  \"bogus\": true\n}"),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		// A meta file whose key is empty is filled in from the requested key on
		// read, but ListSessions skips it: the listing has no key to report.
		memoryCase("meta_with_empty_key_is_not_listed",
			stepAddSimple("s1", "user", "hi"),
			stepWriteRawMeta("s1",
				"{\n  \"key\": \"\",\n  \"summary\": \"\",\n  \"skip\": 0,\n  \"count\": 1\n}"),
			stepGetMeta("s1"),
			stepList()),

		// -- set_history ---------------------------------------------------
		memoryCase("set_history_rewrites_file",
			stepSetHistory("s1",
				at("user", "1", 1700000000, 0),
				at("assistant", "2", 1700000001, 0)),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("set_history_empty_clears_and_zeroes_skip",
			stepAdd("s1", at("user", "1", 1700000000, 0)),
			stepTruncate("s1", 0),
			stepSetHistory("s1"),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("set_history_filters_transient_thoughts",
			stepSetHistory("s1", transientThought(), at("user", "kept", 1700000000, 0)),
			stepGetHistory("s1"),
			stepGetMeta("s1")),
		memoryCase("set_history_stamps_missing_timestamps",
			stepSetHistory("s1", protocoltypes.Message{Role: "user", Content: "x"}),
			stepGetHistory("s1")),
	}

	return cases
}

// ------------------------------------------------------------ message sugar --

func withReasoning(content, reasoning string) protocoltypes.Message {
	t := time.Unix(1700000000, 0).UTC()
	return protocoltypes.Message{
		Role: "assistant", Content: content, ReasoningContent: reasoning, CreatedAt: &t,
	}
}

func transientThought() protocoltypes.Message {
	return withReasoning("", "internal deliberation")
}

func withAttachments(m protocoltypes.Message) protocoltypes.Message {
	m.Media = []string{"m1", "m2"}
	m.Attachments = []protocoltypes.Attachment{{
		Type: "image", Ref: "r", URL: "u", Filename: "f.png", ContentType: "image/png",
	}}
	return m
}

func withToolCall(m protocoltypes.Message) protocoltypes.Message {
	m.ToolCalls = []protocoltypes.ToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: &protocoltypes.FunctionCall{
			Name: "lookup", Arguments: `{"q":"x"}`,
		},
	}}
	return m
}
