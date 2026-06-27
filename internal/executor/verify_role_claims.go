package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// verifyRoleClaims is the cross-cutting deception check called after every
// agent step that produces structured claims. Inspects the result.json for
// role-class assertions (testing.passed, review.checked_commit,
// implementation.files_changed) and verifies each against ground truth —
// the agent's own toolAudit, the worktree's git state, file presence on
// disk.
//
// Each individual check is deterministic — re-run a counted diff, look up
// a sha, scan the audit array — not a heuristic. They collectively close
// the "agent fabricated success" failure class that today only surfaces
// when a downstream step trips over the lie.
//
// Returns a non-nil error when ANY claim fails verification. The error
// message lists every failure so the caller's outcome attribution
// doesn't ping-pong between near-equivalent symptoms.
//
// Stability item 1 of the post-2026.5.3 roadmap.
func (e *Executor) verifyRoleClaims(
	ctx context.Context,
	resultBytes []byte,
	preRoleHEAD, postRoleHEAD, projectDir string,
) error {
	if len(resultBytes) == 0 {
		return nil
	}
	claims := parseRoleClaims(resultBytes)
	var problems []string

	// Tester claimed testing.passed:true → at least one execution-class
	// tool call must appear in this step's toolAudit. An LLM that emits
	// {testing: {passed: true}} without ever running anything has
	// fabricated the result wholesale; the gate downstream that branches
	// on testing.passed would then promote untested code.
	if claims.claimedTestingPassed != nil && *claims.claimedTestingPassed {
		if !resultHasExecutionToolCall(resultBytes) {
			problems = append(problems,
				"testing.passed:true claimed but no test_run / lint_run / typecheck_run / "+
					"run_shell tool call appears in this step's toolAudit — the agent did "+
					"not actually run anything")
		}
	}

	// Coder claimed files_changed:N AND HEAD advanced → diff and count
	// must match within rename tolerance. The HEAD-didn't-advance case is
	// caught upstream in plan_step.go's existing block; here we guard
	// against the "claimed 5 files but only 1 actually changed" shape.
	//
	// The `+1` tolerance covers git's rename detection: a file rename
	// shows as one diff entry but operators sometimes count it as two
	// (deleted+added). We only flag when the agent's claim materially
	// exceeds reality.
	if claims.claimedFilesChanged > 0 && projectDir != "" &&
		preRoleHEAD != "" && postRoleHEAD != "" && preRoleHEAD != postRoleHEAD {
		actual, ok := gitDiffFileCount(ctx, projectDir, preRoleHEAD, postRoleHEAD)
		if ok && claims.claimedFilesChanged > actual+1 {
			problems = append(problems, fmt.Sprintf(
				"files_changed:%d claimed but git diff %s..%s shows only %d files",
				claims.claimedFilesChanged, short(preRoleHEAD), short(postRoleHEAD), actual))
		}
	}

	// Reviewer claimed checked_commit:<sha> → the sha must exist in the
	// repo. Catches an LLM hallucinating a plausible-looking hash. We
	// don't enforce that the sha is in the project's actual commit
	// history (it could be on a branch or in a worktree we haven't
	// looked at); presence in the object DB is the deterministic check.
	if claims.claimedCheckedCommit != "" && projectDir != "" {
		if !gitObjectExists(ctx, projectDir, claims.claimedCheckedCommit) {
			problems = append(problems, fmt.Sprintf(
				"review.checked_commit:%s claimed but that object does not exist in the project repo",
				short(claims.claimedCheckedCommit)))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("agent fabrication detected: %s", strings.Join(problems, "; "))
}

// resultHasExecutionToolCall reports whether the agent's toolAudit
// contains at least one tool call from the "actually ran something"
// set. Used by the testing.passed:true verifier — a model that claims
// tests passed without invoking any execution tool has fabricated.
//
// The set is deliberately broad (test_run AND lint_run AND
// typecheck_run AND run_shell) because operators run their own test
// commands via run_shell as often as they use the dedicated tools, and
// the verifier shouldn't false-positive on legitimate `go test ./...`
// invocations dispatched through the shell.
func resultHasExecutionToolCall(resultBytes []byte) bool {
	var parsed struct {
		ToolAudit []struct {
			Tool string `json:"tool"`
		} `json:"toolAudit"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		return false
	}
	for _, entry := range parsed.ToolAudit {
		switch entry.Tool {
		case "test_run", "lint_run", "typecheck_run", "run_shell":
			return true
		}
	}
	return false
}

// gitDiffFileCount returns the number of files changed between two
// commits in projectDir, exclusive of the from-commit. Returns
// (count, true) on success; (0, false) when git fails (binary missing,
// not a repo, sha invalid). The bool lets callers distinguish "no
// files" from "couldn't check" and skip the comparison rather than
// false-positive on environment failures.
func gitDiffFileCount(ctx context.Context, projectDir, from, to string) (int, bool) {
	out, err := gitExec.output(ctx, "-C", projectDir,
		"diff", "--name-only", from+".."+to)
	if err != nil {
		return 0, false
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, true
}

// gitObjectExists reports whether the given sha references an object
// in projectDir's git repo. Uses `cat-file -e` which exits 0 only when
// the object is present; any other exit (including non-fast-forward
// invalid-sha errors) means "not present here."
func gitObjectExists(ctx context.Context, projectDir, sha string) bool {
	if sha == "" {
		return false
	}
	_, err := gitExec.combined(ctx, "-C", projectDir,
		"cat-file", "-e", sha)
	return err == nil
}
