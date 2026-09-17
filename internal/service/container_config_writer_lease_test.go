package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// approvedWorkspaceProposal files an APPROVED workspace_context proposal for
// project p whose read set matches the (absent) target, so the only thing
// that can refuse the apply is the machinery under test.
// The project is fixed: these cases are about lifecycle and serialisation,
// not project routing.
const wsLeaseTestProject = "proj"

func approvedWorkspaceProposal(t *testing.T, c *Container) *persistence.ControlPlaneProposal {
	project := wsLeaseTestProject
	t.Helper()
	path := "workspace/" + project + "/.autonomy/PROJECT_CONTEXT.md"
	ops, err := json.Marshal([]map[string]string{{"op": "create", "path": path, "content": "# context\n"}})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := json.Marshal(map[string]any{"read_set": map[string]string{path: "ABSENT"}})
	if err != nil {
		t.Fatal(err)
	}
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: project,
		Kind: persistence.ProposalKindWorkspaceContext, BlastRadius: persistence.ProposalScopeProject,
		Title: "context", ApplyOps: string(ops), Evidence: string(ev),
		Status: persistence.ProposalStatusDraft, ProposedBy: "test",
	}
	ctx := context.Background()
	if err := c.repos.Proposals.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := c.repos.Proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "approver"); err != nil {
		t.Fatal(err)
	}
	return p
}

// Regression: re-audit 2026-09-15 CA-19 (reopened) — "config-writer elector is
// attached but never started".
//
// The first CA-19 fix assigned ApplyEngine.LeaderGate and WriterEpoch from a
// newly constructed Elector and stopped there. Nothing ever called
// BootstrapAcquire or Run on it, so no `config_writer` lock row existed;
// Elector.VerifyEpoch reads ErrNotFound and reports ok=false, and
// leaderelection.DangerousWriteAllowed turns that into "superseded by a newer
// leader epoch".
//
// The result was strictly worse than the defect it fixed: on an ordinary
// single-node deployment EVERY config apply was refused — including
// non-assistant proposals, which share this applier. A fence with no lifecycle
// is not a fence, it is an outage.
//
// This test goes through the PRODUCTION constructor and the PRODUCTION
// lifecycle entry point, because asserting non-nil fields is exactly what
// missed it the first time.
func TestConfigWriterLease_ProductionApplySucceedsAfterStartup(t *testing.T) {
	c := newProposalApplierContainer(t)
	ws := t.TempDir()
	c.Config = &config.Config{}
	c.Config.Runtime.ProjectWorkspacePath = ws

	ctx := context.Background()
	// The production startup step that must leave a usable writer lease.
	if err := c.StartConfigWriterLease(ctx); err != nil {
		t.Fatalf("StartConfigWriterLease: %v", err)
	}
	t.Cleanup(func() { _ = c.StopConfigWriterLease(context.Background()) })

	if err := c.ReconcileConfigApplyJournal(ctx); err != nil {
		t.Fatalf("startup reconcile: %v", err)
	}

	p := approvedWorkspaceProposal(t, c)
	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier returned nil")
	}
	if err := engine.Apply(ctx, p.ID, "operator", false); err != nil {
		t.Fatalf("apply refused on a single-node deployment: %v\n\nA writer fence that is never acquired refuses every apply.", err)
	}
	target := filepath.Join(ws, "proj", ".autonomy", "PROJECT_CONTEXT.md")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("apply reported success but wrote nothing: %v", err)
	}
}

// Without the lease the write MUST be refused — the fence still has to fence.
// This pins that the fix is "acquire the lease", not "remove the gate".
func TestConfigWriterLease_ApplyRefusedWhenTheLeaseIsNotHeld(t *testing.T) {
	c := newProposalApplierContainer(t)
	ws := t.TempDir()
	c.Config = &config.Config{}
	c.Config.Runtime.ProjectWorkspacePath = ws

	ctx := context.Background()
	p := approvedWorkspaceProposal(t, c)
	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier returned nil")
	}
	if engine.LeaderGate == nil {
		t.Fatal("the writer fence must still be wired")
	}
	err := engine.Apply(ctx, p.ID, "operator", false)
	if err == nil {
		t.Fatal("an apply without the config-writer lease must be refused")
	}
	if !strings.Contains(err.Error(), "leader") && !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("refusal should name the fence, got %v", err)
	}
}

// Regression: re-audit 2026-09-15 CA-20 — "unresolved journal recovery does
// not block affected execution".
//
// This is the WIRING half, and it is the half that was missing.
// ApplyEngine.BlockedProjects was implemented and unit-tested; nothing in
// production ever called it. The lesson CA-19 taught one commit earlier —
// reviewing the mechanism is not reviewing the wiring — applies to the guard
// as much as to the journal it guards.
func TestContainer_SuppliesTheConfigBlockedGateToTheScheduler(t *testing.T) {
	c := newProposalApplierContainer(t)
	gate := c.configApplyBlockedGate()
	if gate == nil {
		t.Fatal("no config-blocked gate: the scheduler would dispatch against unresolved config")
	}
	// With no open journal rows nothing is blocked.
	if blocked, reason := gate("digest"); blocked {
		t.Fatalf("a clean journal must block nothing, got blocked (%s)", reason)
	}
}

// Regression: re-audit 2026-09-15 CA-06 (REOPENED, second probe) — "two
// production-created workspace appliers sharing the same acquired lease both
// saw an absent target ... then the first overwrote it. Both proposals became
// APPLIED."
//
// The engine's mutex is PER ENGINE, and the API and the UI each construct
// their own (container_http.go builds one per surface). The config-writer
// lease is per DAEMON, so it cannot serialise two engines inside one process —
// it was never meant to; it fences other daemons. Between them, nothing held
// the line the design calls "one writer", and two concurrent applies to the
// same path interleaved read-check-write.
//
// THE SEAM: "the lease is held" and "this write is serialised" are different
// guarantees, and the first was mistaken for the second.
func TestWorkspaceApply_TwoEnginesInOneDaemonDoNotInterleave(t *testing.T) {
	c := newProposalApplierContainer(t)
	ws := t.TempDir()
	c.Config = &config.Config{}
	c.Config.Runtime.ProjectWorkspacePath = ws

	ctx := context.Background()
	if err := c.StartConfigWriterLease(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.StopConfigWriterLease(context.Background()) })

	first := approvedWorkspaceProposal(t, c)
	second := approvedWorkspaceProposal(t, c)

	// Two engines, exactly as the API and the UI surfaces build them.
	engineA := c.newProposalApplier()
	engineB := c.newProposalApplier()
	if engineA == nil || engineB == nil {
		t.Fatal("newProposalApplier returned nil")
	}

	errs := make(chan error, 2)
	go func() { errs <- engineA.Apply(ctx, first.ID, "operator-a", false) }()
	go func() { errs <- engineB.Apply(ctx, second.ID, "operator-b", false) }()
	errA, errB := <-errs, <-errs

	// Both creating the SAME absent path cannot both succeed: the second
	// must see the first's file and refuse (scaffold conflict or stale base),
	// or be refused as an apply already in progress. What must never happen
	// is two APPLIED proposals for one create.
	applied := 0
	for _, id := range []string{first.ID, second.ID} {
		p, err := c.repos.Proposals.GetByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status == persistence.ProposalStatusApplied {
			applied++
		}
	}
	if applied > 1 {
		t.Fatalf("two concurrent creates of one path both reached APPLIED (errA=%v errB=%v): the writes interleaved", errA, errB)
	}
}

// The fence must be usable, not merely present. Suggested by companion
// review-20260915-10cd F2: the previous test asserted LeaderGate != nil, and
// a never-started elector satisfies that — so the assertion that catches the
// regression is on the epoch and the verdict, not the pointer.
func TestConfigWriterLease_FenceActuallyPermitsWritesAfterStartup(t *testing.T) {
	c := newProposalApplierContainer(t)
	ctx := context.Background()
	if err := c.StartConfigWriterLease(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.StopConfigWriterLease(context.Background()) })

	engine := c.newProposalApplier()
	if engine == nil || engine.LeaderGate == nil {
		t.Fatal("no engine or no fence")
	}
	if got := engine.WriterEpoch(); got <= 0 {
		t.Fatalf("writer epoch = %d after startup, want > 0: the lease was never acquired", got)
	}
	proceed, reason := leaderelection.DangerousWriteAllowed(ctx, engine.LeaderGate)
	if !proceed {
		t.Fatalf("the fence refuses writes after startup: %s", reason)
	}
}
