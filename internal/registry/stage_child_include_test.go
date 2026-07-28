package registry

// Load-time validation for stage_child_artifacts_include (T-1089 follow-up).
//
// Both failure modes are silent at runtime, which is why they are config errors:
// an include glob without the flag simply does nothing, and a MALFORMED glob
// matches nothing — staging an empty artifacts/in/ that is indistinguishable
// from "the children had nothing to say". Catch both at load.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// includeWorkflow is a minimal resume_after_children graph: a fan-out
// decompose entrypoint plus the post-delegation consumer that may declare the
// flag and the glob.
func includeWorkflow(consumerFlag bool, glob string) *Workflow {
	return &Workflow{
		ID:                  "incl-test",
		Entrypoint:          "decompose",
		ResumeAfterChildren: true,
		Steps: map[string]WorkflowStep{
			"decompose": {
				Type:              "agent",
				Role:              "lead",
				DelegatedWorkflow: "leaf",
				OnSuccess:         "consume",
				OnFail:            "failed",
			},
			"consume": {
				Type:                       "agent",
				Role:                       "writer",
				OnSuccess:                  "done",
				OnFail:                     "failed",
				StageChildArtifacts:        consumerFlag,
				StageChildArtifactsInclude: glob,
			},
		},
		Terminals: map[string]WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
}

func TestStageChildArtifactsInclude_ValidGlobAccepted(t *testing.T) {
	wf := includeWorkflow(true, "findings-*.md")
	if err := wf.validateStageChildArtifacts("incl.md"); err != nil {
		t.Fatalf("valid glob rejected: %v", err)
	}
}

func TestStageChildArtifactsInclude_EmptyGlobAccepted(t *testing.T) {
	wf := includeWorkflow(true, "")
	if err := wf.validateStageChildArtifacts("incl.md"); err != nil {
		t.Fatalf("unset glob must stay valid (it is the default): %v", err)
	}
}

// The glob is inert without the flag — that is a config mistake the author
// almost certainly did not intend, and it fails silently at runtime.
func TestStageChildArtifactsInclude_RejectedWithoutFlag(t *testing.T) {
	wf := includeWorkflow(false, "findings-*.md")
	err := wf.validateStageChildArtifacts("incl.md")
	if err == nil {
		t.Fatal("expected include-without-flag to be rejected")
	}
	if !strings.Contains(err.Error(), "no effect without stage_child_artifacts") {
		t.Fatalf("unexpected message: %v", err)
	}
}

// A malformed pattern must fail at LOAD, not silently match nothing at 3am.
// `[` is an unterminated character class — filepath.ErrBadPattern.
func TestStageChildArtifactsInclude_RejectsMalformedGlob(t *testing.T) {
	// Sanity-check the fixture really is malformed, so this test can't pass for
	// the wrong reason if Go's matcher ever becomes lenient.
	if _, mErr := filepath.Match("findings-[.md", "probe"); mErr == nil {
		t.Fatal("fixture is no longer a malformed glob; pick another")
	}

	wf := includeWorkflow(true, "findings-[.md")
	err := wf.validateStageChildArtifacts("incl.md")
	if err == nil {
		t.Fatal("expected malformed glob to be rejected at load")
	}
	if !strings.Contains(err.Error(), "not a valid glob") {
		t.Fatalf("unexpected message: %v", err)
	}
}

// The include-glob checks must run even when NO step sets the flag — otherwise
// an include-without-flag typo hides behind the cheap `declared` early-return.
func TestStageChildArtifactsInclude_CheckedEvenWhenNoStepDeclaresFlag(t *testing.T) {
	wf := includeWorkflow(false, "findings-[.md")
	err := wf.validateStageChildArtifacts("incl.md")
	if err == nil {
		t.Fatal("include glob on a non-declaring step must still be validated")
	}
}

// The shipped deep-research workflow must carry the glob on its synthesize
// step — this is the config half of the T-1089 input-bloat fix, and a silent
// revert would restore the 26-entry stage.
func TestShippedDeepResearch_DeclaresFindingsIncludeGlob(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	content, err := os.ReadFile(filepath.Join(root, "configs", "workflows", "deep-research.md"))
	if err != nil {
		t.Fatalf("read deep-research.md: %v", err)
	}
	wf, err := ParseWorkflowMarkdown(content, "deep-research.md")
	if err != nil {
		t.Fatalf("parse deep-research.md: %v", err)
	}
	step, ok := wf.Steps["synthesize"]
	if !ok {
		t.Fatal("deep-research has no synthesize step")
	}
	if !step.StageChildArtifacts {
		t.Fatal("synthesize must declare stage_child_artifacts")
	}
	if step.StageChildArtifactsInclude != "findings-*.md" {
		t.Fatalf("synthesize include glob = %q, want %q (T-1089 input-bloat fix)",
			step.StageChildArtifactsInclude, "findings-*.md")
	}
	// The glob must actually match what research-subtask's harvested findings
	// file is named (`findings.md` → `findings-<date>-<short>.md`), else the
	// filter would stage nothing.
	if ok, _ := filepath.Match(step.StageChildArtifactsInclude, "findings-20260728-0c32.md"); !ok {
		t.Fatalf("glob %q does not match a harvested findings filename",
			step.StageChildArtifactsInclude)
	}
}
