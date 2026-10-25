package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Characterization tests for the on-disk session format.
//
// A session is exactly two files in the store directory:
//
//	<sanitized_key>.jsonl       one compact JSON object per line, append-only
//	<sanitized_key>.meta.json   MarshalIndent(meta, "", "  ")
//
// These files are the user's conversation history. Nothing in picoclaw can
// regenerate them and no migration can repair a misread, so the byte-level
// shape is the contract a reimplementation has to hit — not the Go structs.
// The tests below therefore assert on file names and raw file bytes rather than
// on round-tripped Go values.

func TestCompat_SanitizeKeyProducesExactFilenames(t *testing.T) {
	tests := []struct {
		key      string
		wantBase string
	}{
		{"telegram:12345", "telegram_12345"},
		{"agent:main:main", "agent_main_main"},
		{"telegram:12345/678", "telegram_12345_678"},
		{`slack:C0123\1700000000`, `slack_C0123_1700000000`},
		{"sk_v1_deadbeef", "sk_v1_deadbeef"},
		{"already_safe", "already_safe"},
		// An empty key is not rejected: it sanitizes to "" and produces a hidden
		// ".jsonl" file. Callers must never pass "", but nothing stops them.
		{"", ""},
		{"a::b", "a__b"},
		{"trailing:", "trailing_"},
		{"unicode:名前", "unicode_名前"},
	}

	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	for _, tt := range tests {
		if err := store.AddMessage(ctx, tt.key, "user", "x"); err != nil {
			t.Fatalf("AddMessage(%q): %v", tt.key, err)
		}

		jsonl := filepath.Join(dir, tt.wantBase+".jsonl")
		meta := filepath.Join(dir, tt.wantBase+".meta.json")
		if _, err := os.Stat(jsonl); err != nil {
			t.Errorf("key %q: expected %s: %v", tt.key, filepath.Base(jsonl), err)
		}
		if _, err := os.Stat(meta); err != nil {
			t.Errorf("key %q: expected %s: %v", tt.key, filepath.Base(meta), err)
		}
	}
}

// TestCompat_SanitizeKeyCollisionsAreReal documents a hazard rather than a
// feature. Because ':' , '/' and '\' all collapse to '_', distinct session keys
// share one pair of files. A reimplementation that escapes differently (for
// example percent-encoding) will not find history written by picoclaw, and one
// that escapes *more* aggressively will silently split a session in two.
func TestCompat_SanitizeKeyCollisionsAreReal(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	stamp := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	for _, key := range []string{"svc:room", "svc/room", `svc\room`, "svc_room"} {
		if err := store.AddFullMessage(ctx, key, providers.Message{
			Role: "user", Content: key, CreatedAt: &stamp,
		}); err != nil {
			t.Fatalf("AddFullMessage(%q): %v", key, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("four colliding keys must share one file pair, found %d entries: %v", len(entries), names)
	}

	// All four writes landed in the same file, in call order.
	got, err := store.GetHistory(ctx, "svc:room")
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 messages in the shared file, got %d", len(got))
	}
	for i, want := range []string{"svc:room", "svc/room", `svc\room`, "svc_room"} {
		if got[i].Content != want {
			t.Errorf("message %d: content = %q, want %q", i, got[i].Content, want)
		}
	}
}

// TestCompat_JSONLFileBytes pins the exact bytes of a .jsonl file: compact
// JSON, one object per line, LF terminated, no trailing blank line, and the
// field order/omission rules of protocoltypes.Message.
func TestCompat_JSONLFileBytes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	stamp := time.Date(2026, 10, 25, 10, 30, 0, 0, time.UTC)
	key := "compat:bytes"

	if err := store.AddFullMessage(ctx, key, providers.Message{
		Role: "user", Content: "hello", CreatedAt: &stamp,
	}); err != nil {
		t.Fatalf("AddFullMessage: %v", err)
	}
	if err := store.AddFullMessage(ctx, key, providers.Message{
		Role:      "assistant",
		Content:   "",
		ModelName: "model-x",
		CreatedAt: &stamp,
		ToolCalls: []providers.ToolCall{{
			ID:       "tc-1",
			Type:     "function",
			Function: &providers.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
		}},
	}); err != nil {
		t.Fatalf("AddFullMessage: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "compat_bytes.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	want := `{"role":"user","content":"hello","created_at":"2026-10-25T10:30:00Z"}` + "\n" +
		`{"role":"assistant","content":"","model_name":"model-x","created_at":"2026-10-25T10:30:00Z",` +
		`"tool_calls":[{"id":"tc-1","type":"function","function":{"name":"exec","arguments":"{\"cmd\":\"ls\"}"}}]}` + "\n"

	if string(raw) != want {
		t.Errorf("jsonl bytes differ\n got: %q\nwant: %q", raw, want)
	}
}

// TestCompat_MetaFileExactShape pins the metadata document: two-space indent,
// the full key set, and which keys disappear when empty.
func TestCompat_MetaFileExactShape(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	key := "compat:meta"
	if err := store.AddMessage(ctx, key, "user", "one"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.SetSummary(ctx, key, "a summary"); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	scope := json.RawMessage(`{"channel":"telegram","chat_id":"42"}`)
	if err := store.UpsertSessionMeta(ctx, key, scope, []string{"old:key", "old:key", "", key}); err != nil {
		t.Fatalf("UpsertSessionMeta: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "compat_meta.meta.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var pretty map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pretty); err != nil {
		t.Fatalf("meta is not valid JSON: %v", err)
	}

	for _, wantKey := range []string{"key", "summary", "skip", "count", "created_at", "updated_at", "scope", "aliases"} {
		if _, ok := pretty[wantKey]; !ok {
			t.Errorf("meta is missing key %q: %s", wantKey, raw)
		}
	}
	if got := string(pretty["key"]); got != `"compat:meta"` {
		t.Errorf("meta.key must be the UNSANITIZED key, got %s", got)
	}
	if got := string(pretty["count"]); got != "1" {
		t.Errorf("meta.count = %s, want 1", got)
	}
	if got := string(pretty["skip"]); got != "0" {
		t.Errorf("meta.skip = %s, want 0", got)
	}

	// Values are compared after decoding because MarshalIndent re-indents
	// nested documents too; the exact byte layout is asserted separately below.
	var decoded SessionMeta
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("meta must decode into SessionMeta: %v", err)
	}
	// normalizeAliases drops empties, the canonical key itself, and duplicates,
	// preserving first-seen order.
	if len(decoded.Aliases) != 1 || decoded.Aliases[0] != "old:key" {
		t.Errorf("meta.aliases = %v, want [old:key]", decoded.Aliases)
	}
	// MarshalIndent re-indents an embedded json.RawMessage rather than copying
	// it verbatim: scope is stored pretty-printed at the document's depth. It
	// does NOT reorder keys — "channel" stays before "chat_id" — so a port that
	// decodes scope into a sorted map will rewrite the file's key order on the
	// next save.
	if got := string(decoded.Scope); !strings.Contains(got, `"channel":"telegram"`) &&
		!strings.Contains(got, `"channel": "telegram"`) {
		t.Errorf("meta.scope must round-trip its contents, got %s", got)
	}
	if !strings.Contains(string(raw), "\"scope\": {\n    \"channel\": \"telegram\",\n    \"chat_id\": \"42\"\n  }") {
		t.Errorf("meta.scope must be re-indented at the document depth, key order preserved:\n%s", raw)
	}
	if decoded.Summary != "a summary" {
		t.Errorf("meta.summary = %q, want \"a summary\"", decoded.Summary)
	}

	// MarshalIndent with two spaces, and no trailing newline (WriteFileAtomic
	// writes exactly what MarshalIndent returns).
	if !strings.HasPrefix(string(raw), "{\n  \"key\": ") {
		t.Errorf("meta must be MarshalIndent(meta, \"\", \"  \"), got prefix %q", string(raw)[:min(24, len(raw))])
	}
	if !strings.Contains(string(raw), "\"aliases\": [\n    \"old:key\"\n  ]") {
		t.Errorf("nested values must be indented four spaces under the two-space document indent:\n%s", raw)
	}
	if strings.HasSuffix(string(raw), "\n") {
		t.Error("meta must not end with a newline")
	}
}

// TestCompat_TruncationIsLogicalNotPhysical is the core storage invariant:
// history is never deleted, only skipped. A port that physically rewrites the
// file on /clear loses data that picoclaw still considers recoverable, and
// diverges on the skip offset that later reads depend on.
func TestCompat_TruncationIsLogicalNotPhysical(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	key := "compat:trunc"
	stamp := time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
	for _, c := range []string{"one", "two", "three", "four"} {
		if err := store.AddFullMessage(ctx, key, providers.Message{
			Role: "user", Content: c, CreatedAt: &stamp,
		}); err != nil {
			t.Fatalf("AddFullMessage: %v", err)
		}
	}

	path := filepath.Join(dir, "compat_trunc.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if err := store.TruncateHistory(ctx, key, 2); err != nil {
		t.Fatalf("TruncateHistory: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("TruncateHistory must not touch the .jsonl file\nbefore: %q\nafter:  %q", before, after)
	}

	got, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(got) != 2 || got[0].Content != "three" || got[1].Content != "four" {
		t.Errorf("keep-last 2 must yield [three four], got %d messages", len(got))
	}

	meta, err := store.GetSessionMeta(ctx, key)
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if meta.Count != 4 {
		t.Errorf("meta.count must stay the PHYSICAL line count: got %d, want 4", meta.Count)
	}
	if meta.Skip != 2 {
		t.Errorf("meta.skip must be the number of skipped raw lines: got %d, want 2", meta.Skip)
	}

	// keep-last 0 hides everything but still deletes nothing.
	if err := store.TruncateHistory(ctx, key, 0); err != nil {
		t.Fatalf("TruncateHistory(0): %v", err)
	}
	if got, err := store.GetHistory(ctx, key); err != nil || len(got) != 0 {
		t.Errorf("keep-last 0 must hide all history, got %d messages (%v)", len(got), err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the .jsonl file must survive full truncation: %v", err)
	}
}

// TestCompat_CrashPartialLineIsSkippedNotFatal pins the recovery rule: a torn
// final line is dropped, the rest of the file still loads, and the bad line
// still consumes a slot in the skip counter because skip counts RAW lines.
func TestCompat_CrashPartialLineIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	key := "compat:crash"
	stamp := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []string{"a", "b"} {
		if err := store.AddFullMessage(ctx, key, providers.Message{
			Role: "user", Content: c, CreatedAt: &stamp,
		}); err != nil {
			t.Fatalf("AddFullMessage: %v", err)
		}
	}

	path := filepath.Join(dir, "compat_crash.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(`{"role":"user","content":"cut o`); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()

	got, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory must tolerate a torn line: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected the 2 intact messages, got %d", len(got))
	}

	// Blank lines are skipped without consuming skip budget; corrupt lines are
	// skipped AFTER consuming it. That asymmetry is observable through
	// TruncateHistory, so pin it.
	if err := os.WriteFile(path,
		[]byte("\n\n{\"role\":\"user\",\"content\":\"1\"}\n{\"broken\n{\"role\":\"user\",\"content\":\"2\"}\n"),
		0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err = store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(got) != 2 || got[0].Content != "1" || got[1].Content != "2" {
		t.Fatalf("expected [1 2], got %d messages", len(got))
	}
	if err := store.TruncateHistory(ctx, key, 1); err != nil {
		t.Fatalf("TruncateHistory: %v", err)
	}
	meta, err := store.GetSessionMeta(ctx, key)
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	// meta.skip is NOT a byte offset or a physical line number: it is the
	// ordinal of the last hidden line among NON-BLANK lines, and a corrupt line
	// consumes an ordinal even though it never yields a message. Here the two
	// leading blank lines are invisible to the counter, so the retained lines
	// are ordinals 1 ("1"), 2 (corrupt) and 3 ("2"); keeping the last message
	// hides everything up to ordinal 1.
	if meta.Count != 3 {
		t.Errorf("meta.count must count non-blank raw lines: got %d, want 3", meta.Count)
	}
	if meta.Skip != 1 {
		t.Errorf("meta.skip must be a non-blank-line ordinal including corrupt lines: got %d, want 1", meta.Skip)
	}
	got, err = store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(got) != 1 || got[0].Content != "2" {
		t.Errorf("expected [2], got %d messages", len(got))
	}
}

// TestCompat_ListSessionsReturnsCanonicalKeys pins that the directory listing
// is driven by meta files, not by file names — so the keys callers see keep
// their ':' separators even though the files use '_'.
func TestCompat_ListSessionsReturnsCanonicalKeys(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	for _, key := range []string{"telegram:1", "discord:2"} {
		if err := store.AddMessage(ctx, key, "user", "x"); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	keys := store.ListSessions()
	if len(keys) != 2 {
		t.Fatalf("ListSessions() = %v, want 2 entries", keys)
	}
	for _, k := range keys {
		if !strings.Contains(k, ":") {
			t.Errorf("ListSessions must return canonical keys with ':', got %q", k)
		}
	}
}

// TestCompat_AddMessageStampsCreatedAt documents that the store, not the
// caller, fills created_at when it is absent — so a port must stamp on write
// and must not re-stamp on read.
func TestCompat_AddMessageStampsCreatedAt(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	key := "compat:stamp"
	before := time.Now()
	if err := store.AddMessage(ctx, key, "user", "x"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	got, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(got) != 1 || got[0].CreatedAt == nil {
		t.Fatalf("expected one message with created_at set, got %+v", got)
	}
	if got[0].CreatedAt.Before(before.Add(-time.Second)) {
		t.Errorf("created_at %v is older than the write", got[0].CreatedAt)
	}

	// An explicit created_at must survive untouched.
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.AddFullMessage(ctx, key, providers.Message{
		Role: "user", Content: "y", CreatedAt: &stamp,
	}); err != nil {
		t.Fatalf("AddFullMessage: %v", err)
	}
	got, err = store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if got[1].CreatedAt == nil || !got[1].CreatedAt.Equal(stamp) {
		t.Errorf("explicit created_at must be preserved, got %v", got[1].CreatedAt)
	}
}
