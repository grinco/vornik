package cli

// Tests for `vornikctl workflow validate`. The CLI handler is
// just a thin shell over registry.ValidateWorkflowMarkdown; the
// per-rule tests live in internal/registry. These tests cover
// the file-vs-directory dispatch, the exit-code contract, the
// --fix output shape, and the no-files-found-in-dir case.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

const cliMinimalValid = `---
name: cli-demo
description: Tiny valid workflow used by the CLI's own tests.
version: "1.0.0"
author: vornik
license: Apache-2.0
entrypoint: only
steps:
  only:
    type: agent
    role: lead
    on_success: done
terminals:
  done:
    status: COMPLETED
---

# CLI demo

## Prompts

### only

Do the thing.
`

// runValidateForTest invokes the CLI handler directly, isolating
// it from the rest of vornikctl. Returns the captured stdout and
// the RunE error.
func runValidateForTest(t *testing.T, target string, asFix, asJSON bool) (string, error) {
	t.Helper()
	// Reset the package-level flags so consecutive test runs
	// don't leak state. The cobra command itself is a global,
	// so resetting via the variables is the safer path than
	// re-creating the command.
	workflowValidateFix = asFix
	workflowValidateJSON = asJSON
	defer func() {
		workflowValidateFix = false
		workflowValidateJSON = false
	}()

	var buf bytes.Buffer
	workflowValidateCmd.SetOut(&buf)
	workflowValidateCmd.SetErr(&buf)
	err := runWorkflowValidate(workflowValidateCmd, []string{target})
	return buf.String(), err
}

// writeTempWorkflow drops a fixture into a fresh temp file.
func writeTempWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestWorkflowValidateCmd_AcceptsCleanFile(t *testing.T) {
	path := writeTempWorkflow(t, cliMinimalValid)
	out, err := runValidateForTest(t, path, false, false)
	if err != nil {
		t.Fatalf("expected nil error on clean file; got %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Fatalf("clean file should print OK; got %q", out)
	}
}

func TestWorkflowValidateCmd_RejectsBadFile(t *testing.T) {
	// Strip the description so the file fails the required
	// rule; verify a non-nil error (exit 1).
	bad := strings.Replace(cliMinimalValid, "description: Tiny valid workflow used by the CLI's own tests.\n", "", 1)
	path := writeTempWorkflow(t, bad)
	out, err := runValidateForTest(t, path, false, false)
	if err == nil {
		t.Fatalf("expected error on bad file; got nil (out=%q)", out)
	}
	if !strings.Contains(out, "description_missing") {
		t.Fatalf("output should mention description_missing; got %q", out)
	}
}

func TestWorkflowValidateCmd_FixFlagPrintsHint(t *testing.T) {
	// Mangle the name so the validator produces a finding with
	// a Hint; `--fix` must surface it.
	bad := strings.Replace(cliMinimalValid, "name: cli-demo\n", "name: Bad_Name\n", 1)
	path := writeTempWorkflow(t, bad)
	out, err := runValidateForTest(t, path, true, false)
	if err == nil {
		t.Fatalf("expected error on bad name; got nil (out=%q)", out)
	}
	if !strings.Contains(out, "name_shape") {
		t.Fatalf("expected name_shape finding; got %q", out)
	}
	if !strings.Contains(out, "bad-name") {
		t.Fatalf("fix hint should suggest the corrected name; got %q", out)
	}
}

func TestWorkflowValidateCmd_DirectoryRunsEveryFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.md")
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(good, []byte(cliMinimalValid), 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}
	badBody := strings.Replace(cliMinimalValid, "name: cli-demo\n", "name: cli-demo-bad\nbroken_field: [unclosed\n", 1)
	if err := os.WriteFile(bad, []byte(badBody), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	out, err := runValidateForTest(t, dir, false, false)
	if err == nil {
		t.Fatalf("expected error on dir with at least one bad file; got nil (out=%q)", out)
	}
	if !strings.Contains(out, "good.md") || !strings.Contains(out, "bad.md") {
		t.Fatalf("output should mention both files; got %q", out)
	}
}

func TestWorkflowValidateCmd_DirectoryNoFiles(t *testing.T) {
	dir := t.TempDir()
	// An empty directory is almost always operator error
	// (wrong path) so the handler returns a real error rather
	// than silently exiting 0.
	out, err := runValidateForTest(t, dir, false, false)
	if err == nil {
		t.Fatalf("empty dir should produce an error; got nil (out=%q)", out)
	}
	if !strings.Contains(err.Error(), "no *.md files") {
		t.Fatalf("error should mention 'no *.md files'; got %v", err)
	}
}

func TestWorkflowValidateCmd_NonexistentPath(t *testing.T) {
	out, err := runValidateForTest(t, "/nonexistent-path-here/missing.md", false, false)
	if err == nil {
		t.Fatalf("expected error for missing path; got nil (out=%q)", out)
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "stat") {
		t.Fatalf("expected stat / not-exist error; got %v", err)
	}
}

func TestWorkflowValidateCmd_JSONShape(t *testing.T) {
	// JSON mode skips the human-readable rendering; we don't
	// assert against the full schema (encoding/json is stable)
	// but we do verify the envelope is `{"files": [...]}` so a
	// future format change is intentional.
	path := writeTempWorkflow(t, cliMinimalValid)
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	_, runErr := runValidateForTest(t, path, false, true)
	_ = w.Close()
	os.Stdout = saved
	if runErr != nil {
		t.Fatalf("runErr: %v", runErr)
	}
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, `"files"`) {
		t.Fatalf("JSON envelope missing 'files' key; got %q", out)
	}
}

// TestCollectWorkflowFiles is a small unit covering the dispatch
// helper. Pulled out because it's pure logic — no cobra harness
// dependency — and the directory-mode integration test would mask
// a bug in the .md filter.
func TestCollectWorkflowFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("a.md")
	mustWrite("b.md")
	mustWrite("c.txt")
	mustWrite("README.md")
	files, err := collectWorkflowFiles(dir, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 .md files; got %d (%v)", len(files), files)
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".md") {
			t.Fatalf("non-md slipped through: %s", f)
		}
	}
	// Single-file mode is a passthrough.
	single, err := collectWorkflowFiles("/tmp/x.md", false)
	if err != nil {
		t.Fatalf("collect single: %v", err)
	}
	if len(single) != 1 || single[0] != "/tmp/x.md" {
		t.Fatalf("single-file passthrough broke: %v", single)
	}
}

// TestPrintFileReport_FormatPin keeps the human output stable so
// downstream tooling (and the matching doctor check, which lifts
// the finding format verbatim) doesn't silently churn.
func TestPrintFileReport_FormatPin(t *testing.T) {
	report := &registry.WorkflowMDValidationReport{
		Filename: "demo.md",
		Findings: []registry.WorkflowMDFinding{
			{Severity: registry.SeverityError, Code: "name_shape", Field: "name", Message: "bad", Hint: "demo"},
		},
	}
	var buf bytes.Buffer
	printFileReport(&buf, "demo.md", report, true)
	got := buf.String()
	if !strings.Contains(got, "demo.md: 1 error(s), 0 warning(s)") {
		t.Fatalf("header line missing; got %q", got)
	}
	if !strings.Contains(got, "[ERROR] name_shape: name — bad") {
		t.Fatalf("finding line missing; got %q", got)
	}
	if !strings.Contains(got, "      | demo") {
		t.Fatalf("hint indentation missing; got %q", got)
	}
}
