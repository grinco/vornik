package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

func validScoredWorkflow() *Workflow {
	w := validAgentWorkflow()
	w.Steps = map[string]WorkflowStep{
		"start":  {Type: "agent", Role: "analyst", OnSuccess: "verify"},
		"verify": {Type: "agent", Role: "tester", OnSuccess: "done"},
	}
	w.QualityScoring = &quality.ScoringPolicy{
		Kind:         quality.ScoreKindPinnedCaseValidation,
		ProducerStep: "start",
		VerifierStep: "verify",
	}
	return w
}

func TestWorkflowValidate_QualityScoringContract(t *testing.T) {
	if err := validScoredWorkflow().Validate("wf.md"); err != nil {
		t.Fatalf("valid quality scoring policy rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*quality.ScoringPolicy)
		want string
	}{
		{"unsupported kind", func(p *quality.ScoringPolicy) { p.Kind = "review_prose" }, "kind"},
		{"missing producer", func(p *quality.ScoringPolicy) { p.ProducerStep = "" }, "producerStep"},
		{"unknown producer", func(p *quality.ScoringPolicy) { p.ProducerStep = "typo" }, "producerStep"},
		{"missing verifier", func(p *quality.ScoringPolicy) { p.VerifierStep = "" }, "verifierStep"},
		{"unknown verifier", func(p *quality.ScoringPolicy) { p.VerifierStep = "typo" }, "verifierStep"},
		{"same producer and verifier", func(p *quality.ScoringPolicy) { p.VerifierStep = p.ProducerStep }, "distinct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := validScoredWorkflow()
			tc.edit(w.QualityScoring)
			err := w.Validate("wf.md")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestDevPipelineDeclaresPinnedCaseQualityScoring(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "workflows", "dev-pipeline.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped dev-pipeline: %v", err)
	}
	wf, err := ParseWorkflowMarkdown(content, path)
	if err != nil {
		t.Fatalf("parse shipped dev-pipeline: %v", err)
	}
	if wf.QualityScoring == nil {
		t.Fatal("dev-pipeline must declare qualityScoring")
	}
	if got := *wf.QualityScoring; got.Kind != quality.ScoreKindPinnedCaseValidation ||
		got.ProducerStep != "analyze" || got.VerifierStep != "test" {
		t.Fatalf("dev-pipeline qualityScoring = %+v", got)
	}
}
