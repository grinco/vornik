package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// fakeIncidentDiagnoser scripts the Diagnoser the self-healer calls, and files
// a real self-heal DRAFT proposal (as the true Diagnoser would) so dedup +
// coverage are exercised against the real ledger.
type fakeIncidentDiagnoser struct {
	repo  persistence.ProposalRepository
	err   error
	calls int
}

func (d *fakeIncidentDiagnoser) Diagnose(ctx context.Context, focus string, _ bool) (*DiagnoseVerdict, string, error) {
	d.calls++
	if d.err != nil {
		return nil, "", d.err
	}
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: focus,
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "Diagnose: " + focus, Status: persistence.ProposalStatusDraft,
		ProposedBy: "self-heal",
	}
	_ = d.repo.Create(ctx, p)
	return &DiagnoseVerdict{RootCause: "web_fetch timeout", Confidence: "high"}, p.ID, nil
}

func newSelfHeal(_ *testing.T, diag IncidentDiagnoser, repo persistence.ProposalRepository) *SelfHealWorker {
	return &SelfHealWorker{
		Proposals: repo, Metrics: &fakeMetrics{}, Diagnose: diag, Interval: 1,
		Threshold: 0.5, MinSamples: 5, BreachesToOpen: 3, MaxIncidentsPerHour: 3,
		Logger:   zerolog.Nop(),
		breaches: map[string]int{},
	}
}

func breach(project string) map[string]RateSample {
	return map[string]RateSample{project: {Failed: 8, Total: 10, Rate: 0.8}}
}

func TestSelfHeal_OpensIncidentOnSustainedBreach(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeIncidentDiagnoser{repo: repo}
	w := newSelfHeal(t, diag, repo)
	m := w.Metrics.(*fakeMetrics)
	m.ret = breach("janka")
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx)
	if diag.calls != 0 {
		t.Fatalf("must not diagnose before 3 breaches, got %d calls", diag.calls)
	}
	w.tick(ctx)
	if diag.calls != 1 {
		t.Fatalf("expected exactly 1 diagnosis on the 3rd breach, got %d", diag.calls)
	}
	// Continued breaching must dedup on the open self-heal DRAFT.
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	if diag.calls != 1 {
		t.Fatalf("open incident must dedup further diagnoses, got %d calls", diag.calls)
	}
}

func TestSelfHeal_NeverDiagnosesSystemProject(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeIncidentDiagnoser{repo: repo}
	w := newSelfHeal(t, diag, repo)
	w.SystemProjectID = "vornik-operator"
	m := w.Metrics.(*fakeMetrics)
	m.ret = breach("vornik-operator")
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if diag.calls != 0 {
		t.Fatalf("system project must never be self-diagnosed, got %d calls", diag.calls)
	}
}

func TestSelfHeal_GlobalRateCap(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeIncidentDiagnoser{repo: repo}
	w := newSelfHeal(t, diag, repo)
	w.MaxIncidentsPerHour = 2
	alerts := 0
	w.Alert = func(_, _ string) { alerts++ }
	ctx := context.Background()
	// Drive 3 distinct projects each to a sustained breach in one hour.
	for _, p := range []string{"a", "b", "c"} {
		w.Metrics.(*fakeMetrics).ret = breach(p)
		w.breaches = map[string]int{} // isolate each project's streak
		w.tick(ctx)
		w.tick(ctx)
		w.tick(ctx)
	}
	if diag.calls != 2 {
		t.Fatalf("global cap of 2 must limit diagnoses to 2, got %d", diag.calls)
	}
	if alerts == 0 {
		t.Fatal("hitting the rate cap must emit at least one throttle alert")
	}
}

func TestSelfHeal_DiagnoserErrorFilesGenericProposal(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeIncidentDiagnoser{repo: repo, err: errors.New("LLM down")}
	w := newSelfHeal(t, diag, repo)
	w.Metrics.(*fakeMetrics).ret = breach("janka")
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	// No diagnosis proposal, but the generic failed-rate proposal must exist —
	// tagged "self-heal" (not "tune-detector") so the open-incidents counter
	// reflects it and the worker's own dedup matches on re-arm.
	ps, _ := repo.List(ctx, persistence.ProposalListFilter{ProjectID: "janka"})
	if len(ps) != 1 || ps[0].ProposedBy != "self-heal" || ps[0].Title != tuneFailedRateTitle("janka") {
		t.Fatalf("diagnoser error must fall back to the generic self-heal failed-rate proposal, got %+v", ps)
	}
}

// noProposalDiagnoser scripts a diagnosis that SUCCEEDS (no error) but files
// nothing — proposalID "". In production this happens when the Diagnoser's
// output-validation gate refuses the verdict (a URL/secret smuggled into the
// suggested change, or the proposal store rejects the create); parseVerdict
// requires a non-empty root_cause, so a truly-empty verdict errors instead.
type noProposalDiagnoser struct{ calls int }

func (d *noProposalDiagnoser) Diagnose(_ context.Context, _ string, _ bool) (*DiagnoseVerdict, string, error) {
	d.calls++
	return &DiagnoseVerdict{RootCause: "rejected verdict"}, "", nil
}

// Regression 2026-07-22: self-heal logged "opened incident (auto-diagnosis)"
// with an EMPTY proposal_id when the diagnosis filed nothing. No
// control_plane_proposals row existed → the "open self-heal incidents" counter
// showed 0 and hasOpenIncident() dedup never matched, so the worker re-opened +
// re-alerted on every re-armed scan. A successful-but-non-filing diagnosis must
// fall back to the generic self-heal-tagged coverage proposal (which also
// alerts once + counts against the rate cap), and dedup must then hold.
func TestSelfHeal_SuccessButNoProposalStillCovers(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &noProposalDiagnoser{}
	w := newSelfHeal(t, diag, repo)
	alerts := 0
	w.Alert = func(_, _ string) { alerts++ }
	w.Metrics.(*fakeMetrics).ret = breach("janka")
	ctx := context.Background()
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	ps, _ := repo.List(ctx, persistence.ProposalListFilter{ProjectID: "janka"})
	if len(ps) != 1 || ps[0].ProposedBy != "self-heal" {
		t.Fatalf("a successful-but-non-filing diagnosis must file exactly one self-heal coverage proposal, got %+v", ps)
	}
	if diag.calls != 1 {
		t.Fatalf("expected one diagnosis, got %d", diag.calls)
	}
	if alerts != 1 {
		t.Fatalf("a coverage incident must push exactly one operator alert, got %d", alerts)
	}
	// The coverage proposal is a self-heal DRAFT, so dedup must suppress
	// further diagnoses even as the breach continues.
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	if diag.calls != 1 {
		t.Fatalf("the coverage incident must dedup further diagnoses, got %d calls", diag.calls)
	}
}

func TestSelfHeal_AlertNilSafe(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeIncidentDiagnoser{repo: repo}
	w := newSelfHeal(t, diag, repo)
	w.Alert = nil // no notifier
	w.Metrics.(*fakeMetrics).ret = breach("janka")
	ctx := context.Background()
	// Must not panic and must still file the incident.
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	if diag.calls != 1 {
		t.Fatalf("incident must still open with a nil Alert, got %d calls", diag.calls)
	}
}

func TestSelfHeal_StreakResetsOnRecovery(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeIncidentDiagnoser{repo: repo}
	w := newSelfHeal(t, diag, repo)
	m := w.Metrics.(*fakeMetrics)
	ctx := context.Background()
	m.ret = breach("janka")
	w.tick(ctx)
	w.tick(ctx)                                                               // 2 breaches
	m.ret = map[string]RateSample{"janka": {Failed: 1, Total: 10, Rate: 0.1}} // recovery
	w.tick(ctx)
	m.ret = breach("janka")
	w.tick(ctx)
	w.tick(ctx)
	if diag.calls != 0 {
		t.Fatalf("streak must reset on recovery; expected 0 diagnoses, got %d", diag.calls)
	}
	w.tick(ctx)
	if diag.calls != 1 {
		t.Fatalf("expected 1 diagnosis after a fresh streak, got %d", diag.calls)
	}
}
