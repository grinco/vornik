package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseRoleClaims_PinsExpandedFields covers the new fields added
// for stability item 1: claimedTestingPassed (pointer because absent ≠
// false) and claimedCheckedCommit. The legacy claimedCommit /
// claimedFilesChanged checks live in plan_step_test.go.
func TestParseRoleClaims_PinsExpandedFields(t *testing.T) {
	t.Run("testing.passed:true sets pointer non-nil", func(t *testing.T) {
		got := parseRoleClaims([]byte(`{"testing": {"passed": true}}`))
		if got.claimedTestingPassed == nil || !*got.claimedTestingPassed {
			t.Errorf("claimedTestingPassed = %v, want pointer to true", got.claimedTestingPassed)
		}
	})
	t.Run("testing.passed:false sets pointer to false", func(t *testing.T) {
		got := parseRoleClaims([]byte(`{"testing": {"passed": false}}`))
		if got.claimedTestingPassed == nil || *got.claimedTestingPassed {
			t.Errorf("claimedTestingPassed = %v, want pointer to false", got.claimedTestingPassed)
		}
	})
	t.Run("absent passed → nil pointer (no claim)", func(t *testing.T) {
		got := parseRoleClaims([]byte(`{"writing": {"written": true}}`))
		if got.claimedTestingPassed != nil {
			t.Errorf("claimedTestingPassed = %v, want nil (no claim made)", got.claimedTestingPassed)
		}
	})
	t.Run("review.checked_commit captured", func(t *testing.T) {
		got := parseRoleClaims([]byte(`{"review": {"approved": true, "checked_commit": "abc1234"}}`))
		if got.claimedCheckedCommit != "abc1234" {
			t.Errorf("claimedCheckedCommit = %q, want abc1234", got.claimedCheckedCommit)
		}
	})
}

// TestResultHasExecutionToolCall covers the toolAudit scan that
// testing.passed:true verification depends on.
func TestResultHasExecutionToolCall(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty body", `{}`, false},
		{"toolAudit absent", `{"testing": {"passed": true}}`, false},
		{"empty toolAudit array", `{"toolAudit": []}`, false},
		{"only file_read", `{"toolAudit": [{"tool": "file_read"}]}`, false},
		{"test_run present", `{"toolAudit": [{"tool": "test_run"}]}`, true},
		{"lint_run present", `{"toolAudit": [{"tool": "lint_run"}]}`, true},
		{"typecheck_run present", `{"toolAudit": [{"tool": "typecheck_run"}]}`, true},
		{"run_shell present (covers go test ./...)", `{"toolAudit": [{"tool": "run_shell"}]}`, true},
		{"mix of read + run_shell", `{"toolAudit": [{"tool": "file_read"}, {"tool": "run_shell"}]}`, true},
		{"malformed JSON", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resultHasExecutionToolCall([]byte(tc.body)); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVerifyRoleClaims_TestingPassedRequiresExecution pins the headline
// stability check: an LLM that claims tests passed without ever running
// anything gets caught here, not at the gate downstream that branches
// on testing.passed.
func TestVerifyRoleClaims_TestingPassedRequiresExecution(t *testing.T) {
	e := &Executor{}
	t.Run("passed:true with run_shell in audit → ok", func(t *testing.T) {
		body := []byte(`{
			"testing": {"passed": true},
			"toolAudit": [{"tool": "run_shell"}]
		}`)
		if err := e.verifyRoleClaims(context.Background(), body, "", "", ""); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("passed:true with no execution tool → fabrication detected", func(t *testing.T) {
		body := []byte(`{
			"testing": {"passed": true},
			"toolAudit": [{"tool": "file_read"}, {"tool": "grep"}]
		}`)
		err := e.verifyRoleClaims(context.Background(), body, "", "", "")
		if err == nil {
			t.Fatal("expected fabrication error, got nil")
		}
		if !strings.Contains(err.Error(), "testing.passed:true claimed but no") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
	t.Run("passed:false → no check applied", func(t *testing.T) {
		body := []byte(`{
			"testing": {"passed": false, "failures": "x failed"},
			"toolAudit": []
		}`)
		if err := e.verifyRoleClaims(context.Background(), body, "", "", ""); err != nil {
			t.Errorf("unexpected error on passed:false: %v", err)
		}
	})
}

// TestVerifyRoleClaims_CheckedCommitMustExist covers the reviewer
// hallucination case: model emits a plausible-looking sha that doesn't
// exist in the repo. Uses a real git repo set up in a tempdir so the
// gitObjectExists path runs end-to-end.
func TestVerifyRoleClaims_CheckedCommitMustExist(t *testing.T) {
	dir := setupGitRepo(t)
	realSha := gitFirstCommit(t, dir)
	e := &Executor{}

	t.Run("real sha → ok", func(t *testing.T) {
		body := []byte(`{"review": {"approved": true, "checked_commit": "` + realSha + `"}}`)
		if err := e.verifyRoleClaims(context.Background(), body, "", "", dir); err != nil {
			t.Errorf("unexpected error for real sha: %v", err)
		}
	})
	t.Run("hallucinated sha → fabrication detected", func(t *testing.T) {
		// 40 hex chars; matches git sha shape but no such object exists.
		body := []byte(`{"review": {"approved": true, "checked_commit": "0123456789abcdef0123456789abcdef01234567"}}`)
		err := e.verifyRoleClaims(context.Background(), body, "", "", dir)
		if err == nil {
			t.Fatal("expected fabrication error, got nil")
		}
		if !strings.Contains(err.Error(), "checked_commit") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

// TestVerifyRoleClaims_FilesChangedAccuracy covers the coder
// hallucination case: model claims more files changed than the actual
// diff produces. The +1 tolerance covers git rename detection so we
// don't false-positive on legitimate renames.
func TestVerifyRoleClaims_FilesChangedAccuracy(t *testing.T) {
	dir := setupGitRepo(t)
	preHEAD := gitFirstCommit(t, dir)
	gitWriteAndCommit(t, dir, "second.txt", "second")
	postHEAD := gitFirstCommit(t, dir) // ID of the new HEAD
	e := &Executor{}

	t.Run("claim matches actual count → ok", func(t *testing.T) {
		body := []byte(`{"implementation": {"committed": true, "files_changed": 1}}`)
		if err := e.verifyRoleClaims(context.Background(), body, preHEAD, postHEAD, dir); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("claim within +1 tolerance → ok (rename detection slack)", func(t *testing.T) {
		body := []byte(`{"implementation": {"committed": true, "files_changed": 2}}`)
		if err := e.verifyRoleClaims(context.Background(), body, preHEAD, postHEAD, dir); err != nil {
			t.Errorf("unexpected error within tolerance: %v", err)
		}
	})
	t.Run("claim materially exceeds reality → fabrication detected", func(t *testing.T) {
		body := []byte(`{"implementation": {"committed": true, "files_changed": 5}}`)
		err := e.verifyRoleClaims(context.Background(), body, preHEAD, postHEAD, dir)
		if err == nil {
			t.Fatal("expected fabrication error, got nil")
		}
		if !strings.Contains(err.Error(), "files_changed:5") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
	t.Run("preHEAD == postHEAD → check skipped", func(t *testing.T) {
		// The HEAD-didn't-advance case is caught upstream in
		// plan_step.go's existing block. verifyRoleClaims should not
		// double-fire on the same condition.
		body := []byte(`{"implementation": {"committed": true, "files_changed": 5}}`)
		if err := e.verifyRoleClaims(context.Background(), body, postHEAD, postHEAD, dir); err != nil {
			t.Errorf("verifier should skip when HEADs match (already caught upstream): %v", err)
		}
	})
}

// setupGitRepo creates a git repo in a tempdir with one initial commit
// and returns the repo path. Real git operations because the verifier
// shells out to git — testing it without a real repo would just stub
// the helpers and prove nothing about the verifier's actual behaviour.
func setupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.email=t@x", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init"},
	} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// gitFirstCommit returns the current HEAD sha (despite the name —
// reused for "current HEAD" after each commit in tests below).
func gitFirstCommit(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func gitWriteAndCommit(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	for _, args := range [][]string{
		{"add", name},
		{"-c", "user.email=t@x", "-c", "user.name=t", "commit", "-m", "add " + name},
	} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
