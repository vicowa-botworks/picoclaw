package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceCommitSaysWhenTheTreeIsDirty grades the whole claim the stamp makes: the
// `-dirty` suffix appears exactly when the tracked tree differs from the commit being
// stamped, and not otherwise.
//
// The header's `source_commit` is the only thing telling a reader which tree produced a
// fixture, and a stamp taken over a dirty tree names a commit that cannot have produced
// the file beside it -- rusty-crab #14, where the header declared a tree with 44 config
// cases while the fixture held 53, so re-running the generator at the stamped commit
// "proved" a current fixture was stale. Neither half of that is visible in the corpus:
// the digest and case count stay correct while the provenance quietly lies, so no
// recorded case can witness it and this test is the only pin it has.
func TestSourceCommitSaysWhenTheTreeIsDirty(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=oracle", "GIT_AUTHOR_EMAIL=oracle@example.invalid",
			"GIT_COMMITTER_NAME=oracle", "GIT_COMMITTER_EMAIL=oracle@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "--initial-branch=main")
	write("cmd/oracle/config.go", "package oracle\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	head := git("rev-parse", "HEAD")

	// readProvenance shells out to git, so the process cwd is what it inspects.
	t.Chdir(repo)

	if got := readProvenance(1).Commit; got != head {
		t.Errorf("a clean tree must stamp the bare commit:\n got %q\nwant %q", got, head)
	}

	// A tracked change is the state the stamp has to admit to: the bytes that produced
	// the file are not in the commit it names.
	write("cmd/oracle/config.go", "package oracle\n\n// a new case, not yet committed\n")
	got := readProvenance(1).Commit
	if got != head+"-dirty" {
		t.Errorf("a tracked change must stamp the commit with -dirty:\n got %q\nwant %q", got, head+"-dirty")
	}

	// The other direction, and the reason the check ignores untracked files: a corpus
	// generation in a tree holding a build artefact is still the stamped code, and
	// counting scratch would stamp every generation `-dirty` until someone cleaned up --
	// a suffix that stops meaning anything is the same failure as no suffix.
	write("cmd/oracle/config.go", "package oracle\n")
	write("oracle", "#!/bin/sh\n")
	if got := readProvenance(1).Commit; got != head {
		t.Errorf("an untracked file must not stamp -dirty:\n got %q\nwant %q", got, head)
	}

	// A committed change is clean again, which is what lets a case-adding commit carry a
	// clean stamp without a second generation run.
	git("add", "-A")
	git("commit", "-qm", "cases")
	if got, want := readProvenance(1).Commit, git("rev-parse", "HEAD"); got != want {
		t.Errorf("after committing the cases the stamp must be the new bare commit:\n got %q\nwant %q", got, want)
	}
}
