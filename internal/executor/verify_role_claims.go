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
	// tool call that actually ran something must appear in this step's
	// toolAudit. An LLM that emits {testing: {passed: true}} without ever
	// running anything has fabricated the result wholesale; the gate
	// downstream that branches on testing.passed would then promote
	// untested code.
	//
	// The message names no field, tool class or heuristic the detector
	// reads. A failed step's error can reach the next agent through the
	// recovery path, and a precise account of what the gate accepts is a
	// recipe for satisfying it cheaply — the same reasoning that keeps the
	// injected prompt block on the norm rather than the mechanism
	// (claim_verification_prompt.go).
	if claims.claimedTestingPassed != nil && *claims.claimedTestingPassed {
		if !resultHasExecutionToolCall(resultBytes) {
			problems = append(problems,
				"testing.passed:true claimed but no tool call in this step's audit shows a "+
					"check having actually run — if a check could not be run, report that "+
					"instead of a pass")
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

// toolAuditEntry mirrors one record of the agent's toolAudit array, as the
// agent image writes it (`images/vornik-agent/entrypoint.sh`, the jq that
// emits {audit_id, tool, input, output, duration_ms}). Input carries the
// tool call's ARGUMENTS as a JSON string, not a bare command line.
//
// NOT A TRUST BOUNDARY. The agent authors this array inside the container,
// outside the executor's trust domain, so every field here is a model
// self-report. The checks below raise the cost of a sham; they cannot make
// one impossible. Real per-call provenance (an HMAC seeded into the
// container) is the v2 item if trust is ever actually needed.
type toolAuditEntry struct {
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

// resultHasExecutionToolCall reports whether the agent's toolAudit contains at
// least one SUBSTANTIVE call from the "actually ran something" set. Used by the
// testing.passed:true verifier — a model that claims tests passed without
// invoking any execution tool has fabricated.
//
// The tool set is deliberately broad (test_run AND lint_run AND typecheck_run
// AND run_shell) because operators run their own test commands via run_shell as
// often as they use the dedicated tools, and the verifier shouldn't
// false-positive on legitimate `go test ./...` invocations through the shell.
//
// WHY MERE PRESENCE IS NOT ENOUGH (§13 design review, 2026-08-13). Presence of
// an execution-class entry was the whole test until now, so `run_shell: echo ok`
// satisfied a testing.passed claim — one trivial token defeating a gate whose
// entire job is catching fabricated success. Substantive means:
//
//   - the call produced output. A run that printed nothing is no evidence a
//     check ran, and the dedicated runners cannot legitimately be silent: each
//     returns a JSON summary object even on a clean tree.
//   - for run_shell, the command is not purely trivial (see
//     shellCommandIsTrivial). The dedicated runners need no such test: their
//     argv is fixed by the image, so invoking one IS running a check.
//
// The direction of error is deliberate. A false positive here HARD-FAILS a step
// that genuinely ran its tests, so every rule above rejects only what cannot be
// evidence, and anything unreadable is resolved in the agent's favour.
func resultHasExecutionToolCall(resultBytes []byte) bool {
	var parsed struct {
		ToolAudit []toolAuditEntry `json:"toolAudit"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		return false
	}
	for _, entry := range parsed.ToolAudit {
		if isSubstantiveExecution(entry) {
			return true
		}
	}
	return false
}

// isSubstantiveExecution reports whether one audit entry is evidence that a
// check actually ran.
func isSubstantiveExecution(entry toolAuditEntry) bool {
	if strings.TrimSpace(entry.Output) == "" {
		return false
	}
	switch entry.Tool {
	case "test_run", "lint_run", "typecheck_run":
		return true
	case "run_shell":
		cmd := shellCommandFromAuditInput(entry.Input)
		// An unreadable or absent command is judged on its output alone: an
		// agent image whose argument encoding differs from ours must not
		// hard-fail every honest step that reports a test run.
		if cmd == "" {
			return true
		}
		return !shellCommandIsTrivial(cmd, 0)
	}
	return false
}

// shellCommandFromAuditInput pulls the `command` argument out of a run_shell
// audit entry's input, which the image records as the arguments JSON encoded
// into a string. Returns "" when the input is absent or not the expected shape.
func shellCommandFromAuditInput(input string) string {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Command)
}

// trivialShellCommands are commands that cannot themselves run a check: no-ops
// and workspace inspection. A DENYLIST rather than an allowlist of known test
// runners, because the failure modes are not symmetric — an unlisted runner
// (`./scripts/ci.sh`, `make verify`, a house tool) would hard-fail a step that
// really did run its tests, while an unlisted no-op merely leaves the gate where
// it already was.
var trivialShellCommands = map[string]bool{
	"echo": true, "printf": true, "true": true, ":": true, "exit": true,
	"ls": true, "pwd": true, "cat": true, "cd": true, "sleep": true,
	"date": true, "whoami": true, "which": true, "id": true, "env": true,
	"head": true, "tail": true, "hostname": true, "uname": true, "test": true,
}

// shellCommandIsTrivial reports whether EVERY segment of a shell command is a
// trivial one. Every, not the first: `cd project && go test ./...` and
// `echo running tests; pytest -q` both open with a trivial command and are
// perfectly ordinary ways to run a suite, so judging on the first token alone
// would reject real runs.
//
// One level of `sh -c` / `bash -c` is unwrapped per recursion (bounded by
// depth) so the obvious dodge — wrapping the no-op in a shell — is caught too.
func shellCommandIsTrivial(cmd string, depth int) bool {
	const maxUnwrapDepth = 3
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return true
	}
	segments := splitShellSegments(cmd)
	for _, seg := range segments {
		fields := strings.Fields(seg)
		// Skip leading VAR=value environment prefixes: `CGO_ENABLED=0 go test`
		// is a real run whose first token is neither trivial nor a program.
		for len(fields) > 0 && strings.Contains(fields[0], "=") &&
			!strings.HasPrefix(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		head := strings.Trim(fields[0], `"'`)
		head = head[strings.LastIndex(head, "/")+1:]
		// `sh -c "<inner>"` is judged on <inner>, not on the wrapper.
		if depth < maxUnwrapDepth && isShellBinary(head) && len(fields) > 1 && fields[1] == "-c" {
			inner := unquote(strings.TrimSpace(strings.Join(fields[2:], " ")))
			if !shellCommandIsTrivial(inner, depth+1) {
				return false
			}
			continue
		}
		if !trivialShellCommands[head] {
			return false
		}
	}
	return true
}

// splitShellSegments breaks a command on the operators that separate one
// invocation from the next (&&, ||, ;, |). Quote-unaware on purpose: an
// operator inside a quoted string splits a segment that then reads as two
// commands, which can only make a command look LESS trivial and so cannot
// produce a false rejection.
func splitShellSegments(cmd string) []string {
	replacer := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "&", "\n")
	var out []string
	for _, seg := range strings.Split(replacer.Replace(cmd), "\n") {
		if s := strings.TrimSpace(seg); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isShellBinary(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

// unquote strips one layer of matching surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
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
