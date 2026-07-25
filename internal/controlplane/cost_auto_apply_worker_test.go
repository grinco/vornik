package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// --- test harness: real sqlite repos (higher fidelity than interface stubs) ---

type aaHarness struct {
	t        *testing.T
	props    persistence.ProposalRepository
	canaries persistence.CostTuningCanaryRepository
	applied  []string // proposal ids Apply() was called with
	ackSeen  []bool   // ackDaemon per Apply call
	applyErr error    // if set, Apply returns it (and does not "write")
	files    map[string]string
}

type failingAACanaries struct {
	persistence.CostTuningCanaryRepository
	openErr     error
	cooldownErr error
	getErr      error
}

type failingAAPendingList struct {
	persistence.ProposalRepository
}

func (f failingAAPendingList) List(ctx context.Context, filter persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	for _, status := range filter.Statuses {
		if status == persistence.ProposalStatusApplied {
			return nil, errors.New("database unavailable")
		}
	}
	return f.ProposalRepository.List(ctx, filter)
}

func (f failingAACanaries) HasOpenForSwarmRole(ctx context.Context, swarm, role string) (bool, error) {
	if f.openErr != nil {
		return false, f.openErr
	}
	return f.CostTuningCanaryRepository.HasOpenForSwarmRole(ctx, swarm, role)
}

func (f failingAACanaries) HasActiveCooldown(ctx context.Context, swarm, role, knob string, notBefore time.Time) (bool, error) {
	if f.cooldownErr != nil {
		return false, f.cooldownErr
	}
	return f.CostTuningCanaryRepository.HasActiveCooldown(ctx, swarm, role, knob, notBefore)
}

func (f failingAACanaries) GetByProposalID(ctx context.Context, id string) (*persistence.CostTuningCanary, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.CostTuningCanaryRepository.GetByProposalID(ctx, id)
}

func newAAHarness(t *testing.T) *aaHarness {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &aaHarness{
		t: t, props: sqlite.NewProposalRepository(db.DB),
		canaries: sqlite.NewCostTuningCanaryRepository(db.DB),
		files:    map[string]string{},
	}
}

func (h *aaHarness) worker() *CostAutoApplyWorker {
	return &CostAutoApplyWorker{
		Proposals: h.props, Canaries: h.canaries,
		Apply: func(_ context.Context, id, _ string, ack bool) error {
			h.applied = append(h.applied, id)
			h.ackSeen = append(h.ackSeen, ack)
			if h.applyErr != nil {
				return h.applyErr
			}
			// Simulate a successful apply: write ApplyContent to the target + mark APPLIED.
			p, _ := h.props.GetByID(context.Background(), id)
			h.files[p.ApplyTarget] = p.ApplyContent
			return h.props.MarkApplied(context.Background(), id, persistence.CostAutoApplyActor, "snap")
		},
		ReadFile:          func(rel string) ([]byte, error) { return []byte(h.files[rel]), nil },
		Enabled:           func() bool { return true },
		CanaryEnabled:     func() bool { return true },
		SwarmAllowed:      func(string) bool { return true },
		IsTradingSwarm:    isTradingSwarmTest,
		MinPassedCanaries: 2,
		Metrics:           nil,
		Now:               func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) },
		Logger:            zerolog.Nop(),
	}
}

func isTradingSwarmTest(s string) bool { return s == "ibkr-trader-swarm" }

// aaKnob is the single knob these tests exercise (the trust query keys on
// (swarm,role,knob), so draft + seeded canaries must share it).
const aaKnob = "BUDGET"

// draftProposal creates a DRAFT cost-quality-detector proposal on (swarm,role,aaKnob).
func (h *aaHarness) draftProposal(id, swarm, role string) {
	h.t.Helper()
	ev := `{"change":{"kind":"swarm_role_env","swarm":"` + swarm + `","role":"` + role + `","key":"` + aaKnob + `","value":"260245"}}`
	p := &persistence.ControlPlaneProposal{
		ID: id, Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeSwarm,
		Title: "cost/quality: " + swarm + "/" + role, ApplyTarget: "configs/swarms/" + swarm + ".md",
		ApplyContent: "content-" + id, Status: persistence.ProposalStatusDraft,
		ProposedBy: costQualityDetectorProposedBy, Evidence: ev,
	}
	if err := h.props.Create(context.Background(), p); err != nil {
		h.t.Fatalf("create draft %s: %v", id, err)
	}
}

// seedPassedApply creates an APPLIED proposal (applied_by=actor) + a passed canary
// on (swarm,role,knob) at appliedAt — one unit of track record with a given actor.
func (h *aaHarness) seedPassedApply(id, swarm, role, actor string, appliedAt time.Time) {
	h.t.Helper()
	knob := aaKnob
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{
		ID: id, Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeSwarm,
		Title: "seed " + id, ApplyTarget: "configs/swarms/" + swarm + ".md", ApplyContent: "seed",
		Status: persistence.ProposalStatusDraft, ProposedBy: costQualityDetectorProposedBy,
	}
	if err := h.props.Create(ctx, p); err != nil {
		h.t.Fatalf("seed create: %v", err)
	}
	if err := h.props.SetStatus(ctx, id, persistence.ProposalStatusApproved, "operator"); err != nil {
		h.t.Fatalf("seed approve: %v", err)
	}
	if err := h.props.MarkApplied(ctx, id, actor, "snap"); err != nil {
		h.t.Fatalf("seed apply: %v", err)
	}
	c := &persistence.CostTuningCanary{
		ProposalID: id, SwarmID: swarm, Role: role, Knob: knob,
		AppliedAt: appliedAt, BaselineStart: appliedAt.Add(-168 * time.Hour), WindowUntil: appliedAt.Add(168 * time.Hour),
		Status: persistence.CanaryStatusOpen, OpenedAt: appliedAt,
	}
	if err := h.canaries.Open(ctx, c); err != nil {
		h.t.Fatalf("seed canary open: %v", err)
	}
	if err := h.canaries.Finalize(ctx, id, persistence.CanaryStatusPassed, "", appliedAt.Add(169*time.Hour)); err != nil {
		h.t.Fatalf("seed canary finalize: %v", err)
	}
}

func (h *aaHarness) status(id string) string {
	p, err := h.props.GetByID(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get %s: %v", id, err)
	}
	return p.Status
}

// --- tests ---

// Trusted knob (>=K passed + last apply human) → auto-applied with ack=true.
func TestAutoApply_TrustedKnobApplies(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")

	h.worker().tick(context.Background())

	if len(h.applied) != 1 || h.applied[0] != "d1" {
		t.Fatalf("expected d1 auto-applied, got %v", h.applied)
	}
	if !h.ackSeen[0] {
		t.Error("auto-apply must call Apply with ackDaemon=true (D5)")
	}
	if got := h.status("d1"); got != persistence.ProposalStatusApplied {
		t.Errorf("d1 status = %s, want APPLIED", got)
	}
}

// < K passed canaries → untrusted, left DRAFT.
func TestAutoApply_UntrustedSkipped(t *testing.T) {
	h := newAAHarness(t)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	h.draftProposal("d1", "assistant-swarm", "researcher") // only 1 passed, K=2

	h.worker().tick(context.Background())
	if len(h.applied) != 0 {
		t.Errorf("untrusted knob must not apply, got %v", h.applied)
	}
	if got := h.status("d1"); got != persistence.ProposalStatusDraft {
		t.Errorf("d1 status = %s, want DRAFT", got)
	}
}

// M=1: K passed but the most-recent apply was itself auto → blocked until a human re-seeds.
func TestAutoApply_M1BlocksAfterAutoApply(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", persistence.CostAutoApplyActor, base.Add(200*time.Hour)) // last = auto
	h.draftProposal("d1", "assistant-swarm", "researcher")

	h.worker().tick(context.Background())
	if len(h.applied) != 0 {
		t.Errorf("M=1 must block when most-recent apply was auto, got %v", h.applied)
	}
	if got := h.status("d1"); got != persistence.ProposalStatusDraft {
		t.Errorf("d1 status = %s, want DRAFT (M=1 blocked)", got)
	}
}

// M=1 unblock direction (review-20260724-3bce suggestion 1): after a human
// re-seeds (most-recent apply human again), the knob becomes eligible.
func TestAutoApply_M1UnblocksAfterHumanReseed(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", persistence.CostAutoApplyActor, base.Add(200*time.Hour))
	h.seedPassedApply("s3", "assistant-swarm", "researcher", "operator", base.Add(400*time.Hour)) // human re-seed (newest)
	h.draftProposal("d1", "assistant-swarm", "researcher")

	h.worker().tick(context.Background())
	if len(h.applied) != 1 || h.applied[0] != "d1" {
		t.Fatalf("after human re-seed the knob must be eligible again, got %v", h.applied)
	}
}

// Trading swarm is excluded at scan (and would be refused at apply anyway).
func TestAutoApply_TradingExcluded(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "ibkr-trader-swarm", "strategist", "operator", base)
	h.seedPassedApply("s2", "ibkr-trader-swarm", "strategist", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "ibkr-trader-swarm", "strategist")

	h.worker().tick(context.Background())
	if len(h.applied) != 0 {
		t.Errorf("trading swarm must never auto-apply, got %v", h.applied)
	}
}

// Apply failure (content error) → proposal REJECTED, not retried on the next tick.
func TestAutoApply_ApplyFailureRejectsNoRetry(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	h.applyErr = errors.New("reload rejected")

	w := h.worker()
	w.tick(context.Background())
	if got := h.status("d1"); got != persistence.ProposalStatusRejected {
		t.Errorf("failed apply must REJECT, got status %s", got)
	}
	// Second tick: must NOT re-apply (it's REJECTED, worker only picks DRAFTs).
	before := len(h.applied)
	w.tick(context.Background())
	if len(h.applied) != before {
		t.Errorf("rejected proposal must not be retried; apply calls %d→%d", before, len(h.applied))
	}
}

// Kill-switch off OR canary guard disabled → no-op.
func TestAutoApply_GatesOff(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name              string
		enabled, canaryOn bool
	}{
		{"auto-apply disabled", false, true},
		{"canary guard disabled (D4)", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAAHarness(t)
			h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
			h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
			h.draftProposal("d1", "assistant-swarm", "researcher")
			w := h.worker()
			w.Enabled = func() bool { return tc.enabled }
			w.CanaryEnabled = func() bool { return tc.canaryOn }
			w.tick(context.Background())
			if len(h.applied) != 0 {
				t.Errorf("%s: must no-op, got %v", tc.name, h.applied)
			}
		})
	}
}

// Empty allow-list = NONE: a nil/deny SwarmAllowed blocks even a trusted knob.
func TestAutoApply_EmptyAllowListDenies(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	w := h.worker()
	w.SwarmAllowed = func(string) bool { return false } // empty allow-list encoded as deny-all
	w.tick(context.Background())
	if len(h.applied) != 0 {
		t.Errorf("empty allow-list (deny) must block, got %v", h.applied)
	}
}

// raceProps embeds a real ProposalRepository and forces SetStatus(APPROVED) to
// fail — simulating an operator decision racing the worker's approve
// (review-20260724-d8d3 #8).
type raceProps struct {
	persistence.ProposalRepository
}

func (r raceProps) SetStatus(ctx context.Context, id, status, actor string) error {
	if status == persistence.ProposalStatusApproved {
		return persistence.ErrProposalNotDraft // operator already decided it
	}
	return r.ProposalRepository.SetStatus(ctx, id, status, actor)
}

// Approve race (#8): if the worker's SetStatus(APPROVED) fails (operator already
// decided), the worker skips WITHOUT rejecting — it must not touch a proposal
// another actor now owns.
func TestAutoApply_ApproveRacedSkipsNoReject(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	w := h.worker()
	w.Proposals = raceProps{h.props} // approve will fail

	w.tick(context.Background())
	if len(h.applied) != 0 {
		t.Errorf("approve-race must not apply, got %v", h.applied)
	}
	if got := h.status("d1"); got != persistence.ProposalStatusDraft {
		t.Errorf("approve-race must leave the proposal DRAFT (not rejected), got %s", got)
	}
}

// Mid-tick brake (#7): the apply-time re-check suppresses a pending apply when
// the kill-switch flips between the draft scan and autoApply.
func TestAutoApply_MidTickBrakeSuppresses(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	w := h.worker()
	// Enabled() returns true for tick's top gate, false by the apply-time re-check.
	calls := 0
	w.Enabled = func() bool { calls++; return calls <= 1 }

	w.tick(context.Background())
	if len(h.applied) != 0 {
		t.Errorf("mid-tick brake must suppress the apply, got %v", h.applied)
	}
}

func TestAutoApply_OpenCanaryQueryErrorFailsClosed(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	w := h.worker()
	w.Canaries = failingAACanaries{CostTuningCanaryRepository: h.canaries, openErr: errors.New("database unavailable")}
	w.tick(context.Background())
	if len(h.applied) != 0 {
		t.Fatalf("open-canary query error must suppress auto-apply, got %v", h.applied)
	}
}

func TestAutoApply_CooldownQueryErrorFailsClosed(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	w := h.worker()
	w.CooldownDuration = 24 * time.Hour
	w.Canaries = failingAACanaries{CostTuningCanaryRepository: h.canaries, cooldownErr: errors.New("database unavailable")}
	w.tick(context.Background())
	if len(h.applied) != 0 {
		t.Fatalf("cooldown query error must suppress auto-apply, got %v", h.applied)
	}
}

func TestAutoApply_PendingDiscoveryQueryErrorFailsClosed(t *testing.T) {
	h := newAAHarness(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	h.seedPassedApply("s1", "assistant-swarm", "researcher", "operator", base)
	h.seedPassedApply("s2", "assistant-swarm", "researcher", "operator", base.Add(200*time.Hour))
	h.draftProposal("d1", "assistant-swarm", "researcher")
	w := h.worker()
	w.Proposals = failingAAPendingList{ProposalRepository: h.props}
	w.tick(context.Background())
	if len(h.applied) != 0 {
		t.Fatalf("pending-discovery query error must suppress scan, got %v", h.applied)
	}
}

func TestAutoApply_CanaryMidTickBrakeSuppresses(t *testing.T) {
	h := newAAHarness(t)
	h.draftProposal("d1", "assistant-swarm", "researcher")
	p, err := h.props.GetByID(context.Background(), "d1")
	if err != nil {
		t.Fatal(err)
	}
	w := h.worker()
	w.CanaryEnabled = func() bool { return false }
	w.autoApply(context.Background(), p, "assistant-swarm", "researcher", aaKnob)
	if len(h.applied) != 0 || h.status("d1") != persistence.ProposalStatusDraft {
		t.Fatalf("disabled canary guard must brake before approval, applied=%v status=%s", h.applied, h.status("d1"))
	}
}

// Reconcile idempotency (#7 suggestion): running reconcile twice on the same
// crash-stranded proposal completes it once and is safe the second time.
func TestAutoApply_ReconcileIdempotent(t *testing.T) {
	h := newAAHarness(t)
	ctx := context.Background()
	h.draftProposal("crash", "assistant-swarm", "researcher")
	if err := h.props.SetStatus(ctx, "crash", persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := h.props.StagePreApplySnapshot(ctx, "crash", "pre-image"); err != nil {
		t.Fatal(err)
	}
	p, _ := h.props.GetByID(ctx, "crash")
	h.files[p.ApplyTarget] = p.ApplyContent

	w := h.worker()
	w.reconcile(ctx)
	w.reconcile(ctx) // second pass (simulates a leader re-election)
	if got := h.status("crash"); got != persistence.ProposalStatusApplied {
		t.Errorf("crash proposal must be APPLIED and stay APPLIED across reconciles, got %s", got)
	}
}

// reconcile (D8): APPROVED + staged snapshot + file==ApplyContent → complete forward
// to APPLIED; file != ApplyContent → left APPROVED.
func TestAutoApply_ReconcileCompletesCrashStranded(t *testing.T) {
	h := newAAHarness(t)
	ctx := context.Background()
	// A crash-stranded proposal: APPROVED, snapshot staged, file already == ApplyContent.
	h.draftProposal("crash", "assistant-swarm", "researcher")
	if err := h.props.SetStatus(ctx, "crash", persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := h.props.StagePreApplySnapshot(ctx, "crash", "pre-image"); err != nil {
		t.Fatal(err)
	}
	p, _ := h.props.GetByID(ctx, "crash")
	h.files[p.ApplyTarget] = p.ApplyContent // file was written before the crash

	// A cleanly-approved proposal: snapshot staged but file NOT yet written.
	h.draftProposal("clean", "assistant-swarm", "writer")
	if err := h.props.SetStatus(ctx, "clean", persistence.ProposalStatusApproved, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := h.props.StagePreApplySnapshot(ctx, "clean", "pre-image"); err != nil {
		t.Fatal(err)
	}
	// (no file write for "clean")

	h.worker().reconcile(ctx)

	if got := h.status("crash"); got != persistence.ProposalStatusApplied {
		t.Errorf("crash-stranded proposal must complete forward to APPLIED, got %s", got)
	}
	if got := h.status("clean"); got != persistence.ProposalStatusApproved {
		t.Errorf("clean APPROVED proposal (file not written) must stay APPROVED, got %s", got)
	}
}
