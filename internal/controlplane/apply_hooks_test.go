package controlplane

import (
	"context"
	"errors"
	"os"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Apply-engine hooks from the actionable-proposals design: swarm-scope ack
// (§4.5), apply-time semantic re-validation (ValidateChange, review #6), and
// the once-per-proposal source-tree Mirror (§4.7, review #4).

func TestApply_SwarmScopeRequiresAck(t *testing.T) {
	e, _, id, _ := approvedProposal(t, persistence.ProposalScopeSwarm)
	ctx := context.Background()
	if err := e.Apply(ctx, id, "vadim", false); !errors.Is(err, ErrDaemonAckRequired) {
		t.Fatalf("swarm scope without ack: want ErrDaemonAckRequired, got %v", err)
	}
	if err := e.Apply(ctx, id, "vadim", true); err != nil {
		t.Fatalf("swarm scope with ack must apply: %v", err)
	}
}

func TestApply_ValidateChangeGate(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	boom := errors.New("model no longer in universe")
	var gotID string
	e.ValidateChange = func(_ context.Context, p *persistence.ControlPlaneProposal) error {
		gotID = p.ID
		return boom
	}
	err := e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, boom) {
		t.Fatalf("ValidateChange rejection must abort apply, got %v", err)
	}
	if gotID != id {
		t.Fatalf("hook must receive the proposal under apply, got %q", gotID)
	}
	if b, _ := os.ReadFile(file); string(b) != oldContent {
		t.Fatal("no write may happen before ValidateChange passes")
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusApproved {
		t.Fatalf("proposal must stay APPROVED, got %s", p.Status)
	}
}

func TestApply_MirrorCalledOncePerProposalAfterSuccess(t *testing.T) {
	e, _, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	calls := 0
	var got map[string][]byte
	e.Mirror = func(_ string, files map[string][]byte) error {
		calls++
		got = files
		return nil
	}
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if calls != 1 {
		t.Fatalf("mirror must be called exactly once, got %d", calls)
	}
	if string(got["config.yaml"]) != newContent {
		t.Fatalf("mirror must receive the final content, got %q", got["config.yaml"])
	}
}

func TestApply_MirrorErrorDoesNotFailApply(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	e.Mirror = func(string, map[string][]byte) error { return errors.New("source tree offline") }
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("mirror failure must not fail the apply: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != newContent {
		t.Fatal("deployed write must stand")
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusApplied {
		t.Fatalf("proposal must be APPLIED, got %s", p.Status)
	}
}

func TestApply_MirrorNotCalledOnFailure(t *testing.T) {
	e, _, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	e.Reload = func() error { return errors.New("reload rejected") }
	calls := 0
	e.Mirror = func(string, map[string][]byte) error { calls++; return nil }
	if err := e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("apply must fail on reload rejection")
	}
	if calls != 0 {
		t.Fatalf("mirror must not run on a reversed apply, got %d calls", calls)
	}
}

func TestRollback_MirrorsRestoredState(t *testing.T) {
	e, _, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	var rolled map[string][]byte
	calls := 0
	e.Mirror = func(_ string, files map[string][]byte) error {
		calls++
		rolled = files
		return nil
	}
	ctx := context.Background()
	if err := e.Apply(ctx, id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := e.Rollback(ctx, id); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if calls != 2 {
		t.Fatalf("mirror must run once per apply + once per rollback, got %d", calls)
	}
	if string(rolled["config.yaml"]) != oldContent {
		t.Fatalf("rollback mirror must carry the restored pre-image, got %q", rolled["config.yaml"])
	}
}

// RevalidateChange (Actionizer): the apply-time semantic re-check the
// container wires into ValidateChange.
func TestRevalidateChange(t *testing.T) {
	a := testActionizer(stdFiles())
	ok := func(ev string) error { return a.RevalidateChange("janka", ev) }
	// Not an actionized proposal → nil.
	if err := ok(`{"signal":"x"}`); err != nil {
		t.Fatalf("no change key must pass: %v", err)
	}
	if err := ok(""); err != nil {
		t.Fatalf("empty evidence must pass: %v", err)
	}
	// Valid step timeout.
	if err := ok(`{"change":{"kind":"workflow_step_timeout","workflow":"dev-pipeline","step":"implement","timeout":"20m"}}`); err != nil {
		t.Fatalf("valid step change: %v", err)
	}
	// Missing step.
	if err := ok(`{"change":{"kind":"workflow_step_timeout","workflow":"dev-pipeline","step":"ghost","timeout":"20m"}}`); err == nil {
		t.Fatal("missing step must fail")
	}
	// Model dropped from the universe.
	if err := ok(`{"change":{"kind":"swarm_role_model","swarm":"dev-swarm","role":"coder","model":"hallucinated-model"}}`); err == nil {
		t.Fatal("out-of-universe model must fail")
	}
	// Valid model.
	if err := ok(`{"change":{"kind":"swarm_role_model","swarm":"dev-swarm","role":"coder","model":"new-model"}}`); err != nil {
		t.Fatalf("valid model change: %v", err)
	}
	// MCP server vanished.
	if err := ok(`{"change":{"kind":"mcp_server_timeout","server":"ghost","timeout_seconds":90}}`); err == nil {
		t.Fatal("missing server must fail")
	}
	// Unknown kind.
	if err := ok(`{"change":{"kind":"nonsense"}}`); err == nil {
		t.Fatal("unknown kind must fail")
	}
}
