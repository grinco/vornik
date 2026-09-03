package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// A sustained latency breach whose slowest step's TIMEOUT is not the binding
// constraint used to file prose: "Consider a faster model for role reviewer —
// run Diagnose for a specific recommendation." That is kind=observation with an
// empty diff, which the ledger defines as not decidable, so the detector did the
// analysis and handed the operator homework (cpp_20260902120842 on headmatch).
//
// Design https://docs.vornik.io

// fakeDiagnoser records invocation so a test can assert the LLM was NOT called,
// which "the right proposal was filed" cannot.
type fakeDiagnoser struct {
	calls    int
	propose  []bool
	verdict  *DiagnoseVerdict
	err      error
	lastFocs string
}

func (f *fakeDiagnoser) Diagnose(_ context.Context, focus string, propose bool) (*DiagnoseVerdict, string, error) {
	f.calls++
	f.propose = append(f.propose, propose)
	f.lastFocs = focus
	if f.err != nil {
		return nil, "", f.err
	}
	return f.verdict, "", nil
}

// The swarm is always the dev-swarm fixture; only role and model vary across
// the cases, so they are the parameters.
func modelSwapVerdict(role, model string) *DiagnoseVerdict {
	return &DiagnoseVerdict{
		RootCause: "the reviewer role runs a slow model",
		ConfigChange: &DiagnoseConfigChange{
			Kind: "swarm_role_model", Swarm: "dev-swarm", Role: role, Model: model,
		},
	}
}

// latencyWorker builds a Tune worker over a REAL sqlite proposal repo — the
// applyable-upgrade path this design depends on calls List(), which the package
// fake stubs to nil, so a fake cannot exercise it.
//
// The slowest step's timeout is deliberately not binding (the fixture step has
// no explicit timeout), which is what routes through the escalation.
func latencyWorker(t *testing.T, repo persistence.ProposalRepository, diag IncidentDiagnoser) *TuneWorker {
	t.Helper()
	w := newTuneWorker(repo, &fakeMetrics{})
	w.Actionize = testActionizer(stdFiles())
	w.Diagnose = diag
	w.MinSamples = 1
	w.BreachesToPropose = 1
	return w
}

// openWithLatencyTitle returns the open proposals carrying the stable latency
// title, which is the key the applyable-upgrade path matches on.
// Scoped to the "p1" fixture project, which is the only one these cases use.
func openWithLatencyTitle(t *testing.T, repo persistence.ProposalRepository) []*persistence.ControlPlaneProposal {
	t.Helper()
	const project = "p1"
	all, err := repo.List(context.Background(), persistence.ProposalListFilter{ProjectID: project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []*persistence.ControlPlaneProposal
	for _, p := range all {
		if p.Title == tuneLatencyTitle(project) &&
			(p.Status == persistence.ProposalStatusDraft || p.Status == persistence.ProposalStatusApproved) {
			out = append(out, p)
		}
	}
	return out
}

func TestLatencyEscalation_FilesAnApplyableProposal(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeDiagnoser{verdict: modelSwapVerdict("coder", "new-model")}
	w := latencyWorker(t, repo, diag)

	slow := StepLatencySample{
		Project: "p1", Workflow: "dev-pipeline", Step: "review",
		Role: "coder", Model: "old-model", P95Seconds: 230, Count: 7,
	}
	ok := w.tryDiagnosedLatency(context.Background(), "p1",
		LatencySample{P95Seconds: 337, Count: 7}, slow, `{"signal":"latency_p95_seconds"}`)

	if !ok {
		t.Fatal("a renderable model swap must file an applyable proposal")
	}
	if diag.calls != 1 {
		t.Fatalf("diagnoser calls = %d, want 1", diag.calls)
	}
	// D1: propose=false. With propose=true the Diagnoser files the proposal
	// itself under a title it generates, which breaks the applyable-upgrade
	// match that D4 depends on.
	if len(diag.propose) != 1 || diag.propose[0] {
		t.Errorf("Diagnose must be called with propose=false, got %v", diag.propose)
	}

	open := openWithLatencyTitle(t, repo)
	if len(open) != 1 {
		t.Fatalf("want exactly one open proposal, got %d", len(open))
	}
	p := open[0]
	if p.Kind != persistence.ProposalKindConfig {
		t.Errorf("kind = %q, want config — an observation is not decidable", p.Kind)
	}
	if p.ApplyTarget == "" || p.ApplyContent == "" || p.Diff == "" {
		t.Errorf("proposal must be applyable: target=%q diff=%q", p.ApplyTarget, p.Diff)
	}
	// D4: the title must be the stable one, NOT anything the model produced.
	if p.Title != tuneLatencyTitle("p1") {
		t.Errorf("title = %q, want %q — the LLM names the model, not the proposal",
			p.Title, tuneLatencyTitle("p1"))
	}
	// D1's security property: no model prose reaches the ledger.
	if strings.Contains(p.Rationale, "the reviewer role runs a slow model") {
		t.Error("the verdict's free text must not reach the proposal — the rationale is " +
			"written from metrics, and only the structured model id is taken from the verdict")
	}
	if !strings.Contains(p.Rationale, "new-model") {
		t.Errorf("rationale must name the proposed model:\n%s", p.Rationale)
	}
}

// TestLatencyEscalation_DegradationsFileNothingHere — C4. Each degradation
// returns false so proposeLatency's informational floor runs. Asserted per case
// that NO proposal was filed by the escalation itself, so the caller's fallback
// is the single filer.
func TestLatencyEscalation_DegradationsFileNothingHere(t *testing.T) {
	slow := StepLatencySample{
		Project: "p1", Workflow: "dev-pipeline", Step: "review",
		Role: "coder", Model: "old-model", P95Seconds: 230, Count: 7,
	}
	cases := []struct {
		name      string
		diag      IncidentDiagnoser
		wantCalls int
	}{
		{"LLM unwired", &fakeDiagnoser{err: ErrDiagnoseNoLLM}, 1},
		{"transport error", &fakeDiagnoser{err: errors.New("dial tcp: refused")}, 1},
		{"no config_change", &fakeDiagnoser{verdict: &DiagnoseVerdict{RootCause: "prose only"}}, 1},
		{"wrong change kind", &fakeDiagnoser{verdict: &DiagnoseVerdict{
			ConfigChange: &DiagnoseConfigChange{Kind: "workflow_step_timeout"}}}, 1},
		{"model outside the universe", &fakeDiagnoser{
			verdict: modelSwapVerdict("coder", "hallucinated-model")}, 1},
		{"no-op change", &fakeDiagnoser{
			verdict: modelSwapVerdict("coder", "old-model")}, 1},
		{"role does not exist", &fakeDiagnoser{
			verdict: modelSwapVerdict("ghost", "new-model")}, 1},
		{"nil verdict", &fakeDiagnoser{}, 1},
		// Escalation disabled entirely: must not even attempt a call.
		{"diagnoser unwired", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTuneTestRepo(t)
			w := latencyWorker(t, repo, tc.diag)
			if w.tryDiagnosedLatency(context.Background(), "p1",
				LatencySample{P95Seconds: 337, Count: 7}, slow, "{}") {
				t.Fatal("degradation must report false so the informational floor runs")
			}
			if n := draftCount(t, repo); n != 0 {
				t.Errorf("escalation filed %d proposal(s) on a degradation; the caller's "+
					"fallback is the single filer", n)
			}
			if f, isFake := tc.diag.(*fakeDiagnoser); isFake && f.calls != tc.wantCalls {
				t.Errorf("diagnoser calls = %d, want %d", f.calls, tc.wantCalls)
			}
		})
	}
}

// TestLatencyEscalation_SystemProjectIsNeverDiagnosed — loop safety. The daemon
// must not diagnose its own project, and must not spend an LLM call finding
// that out.
func TestLatencyEscalation_SystemProjectIsNeverDiagnosed(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeDiagnoser{verdict: modelSwapVerdict("coder", "new-model")}
	w := latencyWorker(t, repo, diag)
	w.SystemProjectID = "vornik-system"

	if w.tryDiagnosedLatency(context.Background(), "vornik-system",
		LatencySample{P95Seconds: 900, Count: 9},
		StepLatencySample{Workflow: "w", Step: "s", Role: "r", Model: "m", P95Seconds: 800, Count: 9}, "{}") {
		t.Fatal("the system project must never be escalated")
	}
	if diag.calls != 0 {
		t.Errorf("diagnoser was called %d time(s) for the system project — the exclusion "+
			"must short-circuit BEFORE the LLM call", diag.calls)
	}
}

// TestLatencyEscalation_RateCapBoundsLLMSpend — C5. The cap counts ATTEMPTS,
// not successes: a failed call still cost money.
func TestLatencyEscalation_RateCapBoundsLLMSpend(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeDiagnoser{err: errors.New("boom")} // every attempt fails
	w := latencyWorker(t, repo, diag)
	w.MaxLatencyDiagnosesPerHour = 2

	slow := StepLatencySample{Workflow: "w", Step: "s", Role: "r", Model: "m", P95Seconds: 800, Count: 9}
	for i := 0; i < 5; i++ {
		w.tryDiagnosedLatency(context.Background(), "p1", LatencySample{P95Seconds: 900, Count: 9}, slow, "{}")
	}
	if diag.calls != 2 {
		t.Errorf("diagnoser calls = %d, want 2 — the cap must count failed attempts, "+
			"or a persistently broken LLM bills without bound", diag.calls)
	}
}

// TestProposeLatency_EscalationSupersedesTheInformationalDraft — D4's
// cross-tick scenario, the one round-1 review predicted would double-file.
//
// Tick 1 degrades → informational. Tick 2 succeeds → applyable. The operator
// must end with ONE actionable row, and it works only because both ticks use
// tuneLatencyTitle(project).
func TestProposeLatency_EscalationSupersedesTheInformationalDraft(t *testing.T) {
	repo := newTuneTestRepo(t)
	diag := &fakeDiagnoser{err: ErrDiagnoseNoLLM}
	w := latencyWorker(t, repo, diag)
	w.Metrics = &fakeMetrics{steps: []StepLatencySample{{
		Project: "p1", Workflow: "dev-pipeline", Step: "review",
		Role: "coder", Model: "old-model", P95Seconds: 230, Count: 7,
	}}}

	ctx := context.Background()
	sample := LatencySample{P95Seconds: 337, Count: 7}

	w.proposeLatency(ctx, "p1", sample) // tick 1: LLM unwired → informational
	first := openWithLatencyTitle(t, repo)
	if len(first) != 1 || first[0].ApplyTarget != "" {
		t.Fatalf("tick 1 must file exactly one informational proposal, got %d", len(first))
	}

	diag.err = nil
	diag.verdict = modelSwapVerdict("coder", "new-model")
	w.proposeLatency(ctx, "p1", sample) // tick 2: renders

	open := openWithLatencyTitle(t, repo)
	if len(open) != 1 {
		t.Fatalf("%d open proposals with the latency title; the applyable upgrade must "+
			"supersede the informational DRAFT, not sit beside it", len(open))
	}
	if open[0].ApplyTarget == "" {
		t.Error("tick 2 must leave the operator on the APPLYABLE row, not the informational one")
	}
}

// TestProposeLatency_MismatchedTitleWouldNotUpgrade documents the failure mode
// D4 names. It asserts the *mechanism's* precondition rather than our code:
// if a future change let the model name the proposal, the upgrade would stop
// firing and the operator would collect two rows per breach.
func TestProposeLatency_MismatchedTitleWouldNotUpgrade(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := latencyWorker(t, repo, nil)
	ctx := context.Background()

	w.propose(ctx, "p1", tuneLatencyTitle("p1"), "informational", "{}", "tune-detector")
	// A DIFFERENT title — what an LLM-generated title would produce.
	w.propose(ctx, "p1", "Diagnose: swap the reviewer model", "other", "{}", "tune-detector")

	if n := draftCount(t, repo); n < 2 {
		t.Fatalf("expected two rows for two distinct titles, got %d", n)
	}
	if n := len(openWithLatencyTitle(t, repo)); n != 1 {
		t.Errorf("the stable title must hold exactly one row, got %d", n)
	}
}
