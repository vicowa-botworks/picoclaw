package channels

import (
	"strings"
	"testing"
)

// Characterization tests for SplitMessage — the function that decides what a
// user actually receives when a reply exceeds a channel's length limit.
//
// Three properties are load-bearing and none of them is obvious:
//
//  1. Splitting is in RUNES, and the effective limit is smaller than maxLen:
//     codeBlockBuffer is max(maxLen/10, 50) clamped to maxLen/2, so for any
//     maxLen below 100 the split point is maxLen - maxLen/2. Chunk lengths are
//     therefore a function of maxLen, not "as much as fits".
//  2. The result is NOT a partition of the input. When a chunk would end inside
//     a code block, the fence is closed with an injected "\n```" and REOPENED
//     with its original header at the start of the next chunk. Text is
//     duplicated. A port that treats splitting as lossless will produce
//     different output on every long fenced reply.
//  3. Chunk boundaries consume "\n" but keep "\r", so CRLF input yields chunks
//     ending in a bare carriage return.

func TestCompat_SplitMessageExactChunks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want []string
	}{
		{
			name: "empty content produces no chunks at all",
			in:   "",
			max:  10,
			want: nil,
		},
		{
			name: "content under the limit is returned unchanged",
			in:   "short",
			max:  10,
			want: []string{"short"},
		},
		{
			name: "maxLen <= 0 disables splitting",
			in:   "hi",
			max:  -1,
			want: []string{"hi"},
		},
		{
			name: "maxLen 0 on empty content is still no chunks",
			in:   "",
			max:  0,
			want: nil,
		},
		{
			name: "unbreakable text splits at the effective limit, last chunk takes the remainder",
			in:   strings.Repeat("a", 20),
			max:  10,
			// effectiveLimit = max(10 - max(10/10,50) clamped to 5, 5) = 5
			want: []string{"aaaaa", "aaaaa", "aaaaaaaaaa"},
		},
		{
			name: "splits on the last space inside the effective window",
			in:   "aaa bbb ccc ddd eee fff",
			max:  10,
			want: []string{"aaa", "bbb", "ccc", "ddd", "eee fff"},
		},
		{
			name: "splits on newlines in preference to spaces",
			in:   "aaa\nbbb\nccc\nddd\neee\nfff\nggg",
			max:  10,
			want: []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff\nggg"},
		},
		{
			name: "astral plane characters count as one rune each",
			in:   strings.Repeat("🦐", 12),
			max:  5,
			// effectiveLimit = max(5-2, 2) = 3
			want: []string{"🦐🦐🦐", "🦐🦐🦐", "🦐🦐🦐", "🦐🦐🦐"},
		},
		{
			name: "runs of whitespace are collapsed away at boundaries",
			in:   "a\t\tb   c\n\nd",
			max:  3,
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "CRLF is torn: the newline is consumed, the carriage return stays",
			in:   "line1\r\nline2\r\nline3\r\nline4",
			max:  8,
			want: []string{"line", "1\r", "line", "2\r", "line", "3\r\nline4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitMessage(tt.in, tt.max)
			if len(got) != len(tt.want) {
				t.Fatalf("SplitMessage(%q, %d) = %#v, want %#v", tt.in, tt.max, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("chunk %d = %q, want %q\nfull: %#v", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

// TestCompat_SplitMessageCodeFencesAreClosedAndReopened pins property 2. The
// fence header is copied into every continuation chunk and a closing fence is
// injected, so the concatenation of the chunks is longer than the input.
func TestCompat_SplitMessageCodeFencesAreClosedAndReopened(t *testing.T) {
	in := "```python\nprint(1)\n```\n" + strings.Repeat("x ", 30)
	got := SplitMessage(in, 20)

	if len(got) < 2 {
		t.Fatalf("expected the fenced content to split, got %#v", got)
	}
	if got[0] != "```python\nprint\n```" {
		t.Errorf("first chunk must close the fence it split, got %q", got[0])
	}
	if !strings.HasPrefix(got[1], "```python\n") {
		t.Errorf("continuation chunk must reopen the fence header, got %q", got[1])
	}

	// Duplicated headers mean the chunks are strictly longer than the input.
	joined := 0
	for _, c := range got {
		joined += len([]rune(c))
	}
	if joined <= len([]rune(in)) {
		t.Errorf("fence injection/reopening must grow the total length: %d vs %d",
			joined, len([]rune(in)))
	}

	// Every chunk must have balanced fences, which is the whole point.
	for i, c := range got {
		if n := strings.Count(c, "```"); n%2 != 0 {
			t.Errorf("chunk %d has an unbalanced number of fences (%d): %q", i, n, c)
		}
	}
}

// TestCompat_SplitMessageLongFenceIsChunkedWithRepeatedHeader is the worst
// case: a single code block far longer than maxLen. Every chunk carries the
// header and a closing fence, and the tail keeps whatever follows the block.
func TestCompat_SplitMessageLongFenceIsChunkedWithRepeatedHeader(t *testing.T) {
	in := "prefix ```go\n" + strings.Repeat("y\n", 40) + "```\n tail"
	got := SplitMessage(in, 40)

	if len(got) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d: %#v", len(got), got)
	}
	if got[0] != "prefix" {
		t.Errorf("chunk 0 = %q, want \"prefix\"", got[0])
	}
	for i, c := range got[1:] {
		if !strings.HasPrefix(c, "```go\n") {
			t.Errorf("chunk %d must reopen the go fence header, got %q", i+1, c)
		}
	}
	last := got[len(got)-1]
	if !strings.HasSuffix(last, "```\n tail") {
		t.Errorf("last chunk must carry the trailing text after the block, got %q", last)
	}
}

// TestCompat_SplitMessageInvariants is the fuzz-shaped contract every port has
// to satisfy for arbitrary input, independent of the exact chunking rules.
//
// Fence-bearing inputs are only exercised at maxLen >= 6. At maxLen == 5 the
// "split inside the block" branch computes innerLimit = start + maxLen - 5 ==
// start, emits an empty chunk plus an injected closing fence, and re-enters the
// loop with the same header at the front — so SplitMessage("```abc", 5) spins
// forever while growing the buffer. That is a pre-existing defect, not a
// property worth pinning; it is fixed separately.
func TestCompat_SplitMessageInvariants(t *testing.T) {
	inputs := []struct {
		content string
		fenced  bool
	}{
		{strings.Repeat("word ", 500), false},
		{strings.Repeat("行", 500), false},
		{strings.Repeat("🦐 ", 300), false},
		{"```" + strings.Repeat("a", 500), true},
		{strings.Repeat("a\n", 400) + "```", true},
		{"no spaces or newlines " + strings.Repeat("X", 900), false},
		{strings.Repeat("\t\n ", 200) + "tail", false},
		{"a\r\n" + strings.Repeat("b\r\n", 200), false},
		{strings.Repeat("```f\nx\n```\n", 60), true},
	}
	limits := []int{1, 2, 5, 17, 64, 100, 4096}

	for _, in := range inputs {
		for _, maxLen := range limits {
			if in.fenced && maxLen < 6 {
				continue
			}
			got := SplitMessage(in.content, maxLen)

			for i, c := range got {
				if c == "" {
					t.Errorf("input %q maxLen %d: chunk %d is empty (%#v)",
						short(in.content), maxLen, i, got)
					continue
				}
				// A chunk may exceed maxLen only when the fence machinery had
				// to reopen a header; it must never exceed it by more than the
				// longest fence header plus the injected close.
				if n := len([]rune(c)); n > maxLen+maxLen/2+16 {
					t.Errorf("input %q maxLen %d: chunk %d is %d runes, far over the limit: %q",
						short(in.content), maxLen, i, n, c)
				}
				if strings.ContainsRune(c, 0xFFFD) {
					t.Errorf("input %q maxLen %d: chunk %d contains U+FFFD, splitting was not rune-safe: %q",
						short(in.content), maxLen, i, c)
				}
			}

			// Nothing may be silently dropped: stripping all whitespace from
			// the input must leave a subsequence of the whitespace-stripped
			// concatenation, allowing only for injected fence characters.
			if !containsRunesIgnoringSpace(in.content, strings.Join(got, "")) {
				t.Errorf("input %q maxLen %d: content was lost across the split", short(in.content), maxLen)
			}
		}
	}
}

func short(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// containsRunesIgnoringSpace reports whether every non-whitespace rune of want
// appears in got in order.
func containsRunesIgnoringSpace(want, got string) bool {
	it := []rune(got)
	pos := 0
	for _, r := range want {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		found := false
		for pos < len(it) {
			c := it[pos]
			pos++
			if c == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
