package projectwizard

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

type captureRecorder struct {
	rows []*persistence.TaskLLMUsage
}

func (c *captureRecorder) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	c.rows = append(c.rows, u)
	return nil
}

func resp() *chat.ChatResponse {
	r := &chat.ChatResponse{Model: "m"}
	r.Usage.PromptTokens = 10
	r.Usage.CompletionTokens = 5
	return r
}

func TestRecordUsageRoleParameterized(t *testing.T) {
	rec := &captureRecorder{}
	w := &Wizard{LLMUsage: rec}

	w.recordUsage(context.Background(), resp(), "sess-1", RoleAutomationComposer, nil)
	if len(rec.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rec.rows))
	}
	if rec.rows[0].Role != RoleAutomationComposer {
		t.Errorf("Role = %q, want %q", rec.rows[0].Role, RoleAutomationComposer)
	}
	if rec.rows[0].StepID != "sess-1" {
		t.Errorf("StepID = %q", rec.rows[0].StepID)
	}

	// Historical default: project_wizard.
	w.recordUsage(context.Background(), resp(), "sess-2", RoleProjectWizard, nil)
	if rec.rows[1].Role != RoleProjectWizard {
		t.Errorf("Role = %q, want %q", rec.rows[1].Role, RoleProjectWizard)
	}
}

func TestRecordUsageEmptyRoleDefaults(t *testing.T) {
	rec := &captureRecorder{}
	w := &Wizard{LLMUsage: rec}
	w.recordUsage(context.Background(), resp(), "s", "", nil)
	if len(rec.rows) != 1 || rec.rows[0].Role != RoleProjectWizard {
		t.Fatalf("empty role should default to project_wizard, got %+v", rec.rows)
	}
}

func TestRecordUsageGuards(t *testing.T) {
	rec := &captureRecorder{}
	w := &Wizard{LLMUsage: rec}

	// nil response → no row.
	w.recordUsage(context.Background(), nil, "s", RoleProjectWizard, nil)
	// zero tokens → no row.
	empty := &chat.ChatResponse{Model: "m"}
	w.recordUsage(context.Background(), empty, "s", RoleProjectWizard, nil)
	// nil recorder → no panic.
	(&Wizard{}).recordUsage(context.Background(), resp(), "s", RoleProjectWizard, nil)

	if len(rec.rows) != 0 {
		t.Errorf("expected no rows, got %d", len(rec.rows))
	}
}
