package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunExecutionToolGrantSuite is the backend-agnostic contract for
// persistence.ExecutionToolGrantRepository (registry design §10.1–§10.4).
//
// The store carries a privilege decision, so the contract is about more than
// round-tripping. Three properties are load-bearing and both backends must agree:
//
//   - Current returns the newest ACCEPTED grant. A refused request must not narrow
//     anything, or a hostile grant naming one invalid tool starves a step.
//   - The table is append-only: a later grant supersedes an earlier one without
//     erasing it, so the audit trail survives.
//   - Escalations are counted including refused ones, because the limit exists to
//     bound audited cycles and a refused cycle costs the same write.
func RunExecutionToolGrantSuite(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	t.Helper()
	t.Run("Record_then_Current_round_trips", func(t *testing.T) { grantRoundTrip(t, repo) })
	t.Run("Current_is_nil_when_no_grant", func(t *testing.T) { grantNoneYet(t, repo) })
	t.Run("Current_skips_refused_rows", func(t *testing.T) { grantRefusedNotCurrent(t, repo) })
	t.Run("newest_accepted_supersedes_without_erasing", func(t *testing.T) { grantSupersede(t, repo) })
	t.Run("EscalationCount_includes_refused", func(t *testing.T) { grantEscalationCount(t, repo) })
	t.Run("Record_requires_execution_and_step", func(t *testing.T) { grantRequiresScope(t, repo) })
}

func grantRoundTrip(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	ctx := context.Background()
	when := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	in := &persistence.ExecutionToolGrant{
		ExecutionID: "exec-rt", ProjectID: "p1", StepID: "research", Role: "researcher",
		RequestedTools: []string{"mcp__scraper__web_fetch", "mcp__vornik__recall"},
		Accepted:       true,
		CeilingHash:    "abc123", CeilingModifiedAt: &when, Actor: "lead",
	}
	if err := repo.Record(ctx, in); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if in.ID == "" {
		t.Error("Record did not stamp an ID")
	}
	got, err := repo.Current(ctx, "exec-rt", "research")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got == nil {
		t.Fatal("Current returned nil for a recorded accepted grant")
	}
	if len(got.RequestedTools) != 2 || got.RequestedTools[0] != "mcp__scraper__web_fetch" {
		t.Errorf("requested_tools = %v, want the request verbatim and in order", got.RequestedTools)
	}
	if got.CeilingHash != "abc123" {
		t.Errorf("ceiling_hash = %q; without it an audit reader cannot tell 'never in the "+
			"ceiling' from 'the ceiling was tightened after the grant'", got.CeilingHash)
	}
	if got.CeilingModifiedAt == nil {
		t.Error("ceiling_modified_at lost; drift becomes unreconstructable")
	}
	if got.Role != "researcher" || got.Actor != "lead" {
		t.Errorf("role/actor = %q/%q, want researcher/lead", got.Role, got.Actor)
	}
}

func grantNoneYet(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	got, err := repo.Current(context.Background(), "exec-none", "step")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got != nil {
		t.Errorf("Current = %+v for a step with no grant; nil is what keeps the feature "+
			"inert (ceiling-only) until a lead grants", got)
	}
}

// grantRefusedNotCurrent is the security-relevant one: a refused request must not
// become the effective grant, or naming one invalid tool would starve the step.
func grantRefusedNotCurrent(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	ctx := context.Background()
	err := repo.Record(ctx, &persistence.ExecutionToolGrant{
		ExecutionID: "exec-ref", ProjectID: "p1", StepID: "s",
		RequestedTools: []string{"mcp__broker__place_order"},
		Accepted:       false,
		RefusedTools:   []string{"mcp__broker__place_order"},
		CeilingHash:    "ceil",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := repo.Current(ctx, "exec-ref", "s")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got != nil {
		t.Errorf("a REFUSED grant became the current grant (%v); a request naming one "+
			"invalid tool would then narrow the step to nothing", got.RequestedTools)
	}
}

func grantSupersede(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	ctx := context.Background()
	first := &persistence.ExecutionToolGrant{
		ExecutionID: "exec-sup", ProjectID: "p1", StepID: "s",
		RequestedTools: []string{"a"}, Accepted: true,
		CreatedAt: time.Now().UTC().Add(-2 * time.Minute),
	}
	second := &persistence.ExecutionToolGrant{
		ExecutionID: "exec-sup", ProjectID: "p1", StepID: "s",
		RequestedTools: []string{"a", "b"}, Accepted: true, IsEscalation: true,
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Record(ctx, first); err != nil {
		t.Fatalf("Record first: %v", err)
	}
	if err := repo.Record(ctx, second); err != nil {
		t.Fatalf("Record second: %v", err)
	}
	got, err := repo.Current(ctx, "exec-sup", "s")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got == nil || len(got.RequestedTools) != 2 {
		t.Fatalf("Current = %+v, want the newest grant (2 tools)", got)
	}
	// Append-only: the earlier decision must still exist. EscalationCount seeing
	// exactly one escalation proves the first row was not overwritten.
	n, err := repo.EscalationCount(ctx, "exec-sup", "s")
	if err != nil {
		t.Fatalf("EscalationCount: %v", err)
	}
	if n != 1 {
		t.Errorf("EscalationCount = %d, want 1 — the superseding write must APPEND, not "+
			"replace, or the audit trail is lost", n)
	}
}

func grantEscalationCount(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	ctx := context.Background()
	for i, accepted := range []bool{true, false, true} {
		err := repo.Record(ctx, &persistence.ExecutionToolGrant{
			ExecutionID: "exec-esc", ProjectID: "p1", StepID: "s",
			RequestedTools: []string{"a"}, Accepted: accepted, IsEscalation: true,
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	n, err := repo.EscalationCount(ctx, "exec-esc", "s")
	if err != nil {
		t.Fatalf("EscalationCount: %v", err)
	}
	if n != 3 {
		t.Errorf("EscalationCount = %d, want 3 including the refused attempt — the limit "+
			"bounds audited cycles, and a refused cycle costs the same write", n)
	}
	// A non-escalation grant must not inflate the count.
	if err := repo.Record(ctx, &persistence.ExecutionToolGrant{
		ExecutionID: "exec-esc", ProjectID: "p1", StepID: "s",
		RequestedTools: []string{"a"}, Accepted: true,
	}); err != nil {
		t.Fatalf("Record plain grant: %v", err)
	}
	if n2, _ := repo.EscalationCount(ctx, "exec-esc", "s"); n2 != 3 {
		t.Errorf("EscalationCount = %d after a plain grant, want 3", n2)
	}
}

func grantRequiresScope(t *testing.T, repo persistence.ExecutionToolGrantRepository) {
	ctx := context.Background()
	if err := repo.Record(ctx, &persistence.ExecutionToolGrant{StepID: "s"}); err == nil {
		t.Error("recorded a grant with no execution_id; the row could never be found again")
	}
	if err := repo.Record(ctx, &persistence.ExecutionToolGrant{ExecutionID: "e"}); err == nil {
		t.Error("recorded a grant with no step_id; a grant for one step must not apply to another")
	}
	if err := repo.Record(ctx, nil); err == nil {
		t.Error("recorded a nil grant")
	}
}
