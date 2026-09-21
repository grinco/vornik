package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflowWithClass writes a minimal valid workflow whose publish step retries
// on the given class.
func workflowWithClass(t *testing.T, dir, name, class string) {
	t.Helper()
	body := `---
workflowId: "` + strings.TrimSuffix(strings.TrimSuffix(name, ".md"), ".md.tmpl") + `"
displayName: "probe"
version: "1.0"
entrypoint: "publish"
steps:
  publish:
    type: "agent"
    role: "writer"
    retry:
      on: ["` + class + `"]
      attempts: 2
---

## Prompts

### publish

Do the thing.
`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func classCheck(t *testing.T, configDir string) DoctorCheck {
	t.Helper()
	h := &DoctorHandlers{configDir: configDir}
	return h.checkConfigClassCompat()
}

// TestConfigClassCompat_ReportsADeadClass is the core case: a deployed tree
// naming a step error class the installed binary rejects will take the whole
// daemon down on its next start — the 2026-09-17 membench incident, which
// flapped on Restart=on-failure until stopped.
func TestConfigClassCompat_ReportsADeadClass(t *testing.T) {
	dir := t.TempDir()
	workflowWithClass(t, filepath.Join(dir, "workflows"), "deep-research.md", "container_non_zero_exit")

	got := classCheck(t, dir)
	if got.Status != "ERROR" {
		t.Fatalf("Status = %q, want ERROR — the finding predicts a daemon that will not start", got.Status)
	}
	joined := got.Message + " " + strings.Join(got.Items, " ")
	for _, want := range []string{"deep-research.md", "publish", "container_non_zero_exit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("finding %q does not name %q", joined, want)
		}
	}
	// The valid set is what the operator edits toward.
	if !strings.Contains(joined, "unclassified") {
		t.Fatalf("finding %q does not offer the valid classes", joined)
	}
}

// A clean tree must report OK, not SKIPPED: "examined and clean" has to be
// distinguishable from "never examined".
func TestConfigClassCompat_CleanTreeIsOKNotSkipped(t *testing.T) {
	dir := t.TempDir()
	workflowWithClass(t, filepath.Join(dir, "workflows"), "research.md", "unclassified")

	got := classCheck(t, dir)
	if got.Status != "OK" {
		t.Fatalf("Status = %q, want OK", got.Status)
	}
}

// The cry-wolf guard, built from the three live occurrences in production
// research.md: all inside `#` comments, which a grep-based check would have
// reported as findings on a healthy tree on its first run.
func TestConfigClassCompat_IgnoresPoseAndComments(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "workflows")
	workflowWithClass(t, wf, "research.md", "unclassified")

	// Append the dead class in prose and in a comment — the shapes the
	// reference deployment actually carries.
	path := filepath.Join(wf, "research.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body = append(body, []byte("\n# Encode the observed recovery pattern where container_non_zero_exit\n"+
		"# failures self-resolve on retry (confidence 0.61 in production).\n")...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := classCheck(t, dir); got.Status != "OK" {
		t.Fatalf("Status = %q, want OK — a dead class in prose is not an armed retry.on", got.Status)
	}
}

// The examined-direction test. The loader's rule is HasSuffix(name, ".md"), so
// a multi-dot basename IS loaded; an allowlist anchored on a single-dot
// basename would skip it and make the check BLIND to an armed file, which is
// strictly worse than the denylist it replaced.
func TestConfigClassCompat_ScopeMatchesTheLoader(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "workflows")

	t.Run("a multi-dot .md basename is examined", func(t *testing.T) {
		d := t.TempDir()
		workflowWithClass(t, filepath.Join(d, "workflows"), "deep-research.v2.md", "container_non_zero_exit")
		if got := classCheck(t, d); got.Status != "ERROR" {
			t.Fatalf("Status = %q, want ERROR — the loader loads this file", got.Status)
		}
	})

	// All three sideline shapes the reference deployment carries. None ends in
	// ".md", so none is loaded, so none may be reported.
	for _, name := range []string{
		"research-and-publish.md.pre-T-9d21",
		"outreach-discover.md.bak-20260823T095842Z-pre-jira-format",
		"workflow.md.tmpl.bak-20260917",
	} {
		t.Run("sidelined "+name, func(t *testing.T) {
			d := t.TempDir()
			workflowWithClass(t, filepath.Join(d, "workflows"), name, "container_non_zero_exit")
			if got := classCheck(t, d); got.Status == "ERROR" {
				t.Fatalf("Status = ERROR for %s, which the loader does not load", name)
			}
		})
	}
	_ = wf
}

// A malformed file must not blind the check to its siblings: SKIP is per file,
// never per check.
func TestConfigClassCompat_SkipIsPerFile(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "workflows")
	workflowWithClass(t, wf, "good.md", "container_non_zero_exit")
	if err := os.WriteFile(filepath.Join(wf, "broken.md"), []byte("not a workflow at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := classCheck(t, dir)
	if got.Status != "ERROR" {
		t.Fatalf("Status = %q, want ERROR — one malformed file must not hide a sibling's dead class", got.Status)
	}
	if !strings.Contains(got.Message+strings.Join(got.Items, " "), "good.md") {
		t.Fatalf("the dead class in good.md was lost: %+v", got)
	}
}

// project-templates/ is the mechanical re-arm vector: creating a project from a
// stale template writes a live tree, with no human in the loop.
func TestConfigClassCompat_ScansProjectTemplates(t *testing.T) {
	dir := t.TempDir()
	workflowWithClass(t, filepath.Join(dir, "project-templates", "docs-rag-sync"),
		"workflow.md.tmpl", "container_non_zero_exit")

	got := classCheck(t, dir)
	if got.Status != "ERROR" {
		t.Fatalf("Status = %q, want ERROR — a stale template arms the next project", got.Status)
	}
	if !strings.Contains(strings.Join(got.Items, " "), "project-templates") {
		t.Fatalf("the finding does not say which tree: %+v", got)
	}
}

// A shipped template carries `workflowId: "{{.projectId}}-x"`, which the
// per-file validator reports as an ERROR with code name_shape. Reporting every
// ERROR finding would cry wolf on every template on the first run — the
// filter on the retry-class finding is what excludes it.
func TestConfigClassCompat_CleanTemplateIsNotAFinding(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "project-templates", "report-pipeline")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `---
workflowId: "{{.projectId}}-report"
displayName: "{{.displayName}} report run"
version: "1.0"
entrypoint: "publish"
steps:
  publish:
    type: "agent"
    role: "writer"
    retry:
      on: ["unclassified"]
      attempts: 2
---

## Prompts

### publish

Write it.
`
	if err := os.WriteFile(filepath.Join(tdir, "workflow.md.tmpl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := classCheck(t, dir); got.Status != "OK" {
		t.Fatalf("Status = %q, want OK — a name_shape ERROR is not a dead class: %+v", got.Status, got)
	}
}

// The valid set comes from the loader's own source, not a local copy: a
// snapshot test would fail on every legitimate class addition and train
// someone to delete it, restoring the drift the assertion prevents.
func TestConfigClassCompat_UsesTheLoadersClassSet(t *testing.T) {
	dir := t.TempDir()
	// Every class the binary declares must be accepted, whatever they are.
	for i, class := range stepOutcomeClassesForTest() {
		d := filepath.Join(dir, "t")
		_ = os.RemoveAll(d)
		workflowWithClass(t, filepath.Join(d, "workflows"), "w.md", class)
		if got := classCheck(t, d); got.Status != "OK" {
			t.Fatalf("class %d (%q) declared by the binary was reported: %+v", i, class, got)
		}
	}
}

// No config dir is "could not evaluate", never OK.
func TestConfigClassCompat_NoConfigDirSkips(t *testing.T) {
	if got := classCheck(t, ""); got.Status != "SKIPPED" {
		t.Fatalf("Status = %q, want SKIPPED", got.Status)
	}
}
