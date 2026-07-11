package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Actionable-proposal behaviour of the Tune worker (LLD 2026-07-11 §4.4):
// latency breaches attribute the slowest step and, when its explicit timeout
// is binding, file an APPLYABLE proposal; instinct tool-timeout breaches on
// MCP tools render the server's timeout_seconds raise.

func actionableFiles() map[string]string {
	return map[string]string{
		// Step "slow" explicit timeout 300s; other step is faster.
		"configs/workflows/wf-a.md":   "---\nworkflowId: wf-a\nsteps:\n  slow:\n    type: agent\n    role: coder\n    timeout: \"300s\"\n  quick:\n    type: gate\n    role: gate\n    timeout: \"60s\"\n---\nbody\n",
		"config.yaml":                 "mcp:\n  servers:\n    - name: scraper\n      timeout_seconds: 60\n",
		"configs/projects/janka.yaml": "projectId: \"janka\"\n",
	}
}

func drafts(t *testing.T, repo persistence.ProposalRepository) []*persistence.ControlPlaneProposal {
	t.Helper()
	ps, err := repo.List(context.Background(), persistence.ProposalListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return ps
}

func tickN(w *TuneWorker, n int) {
	for i := 0; i < n; i++ {
		w.tick(context.Background())
	}
}

// Latency breach + binding step timeout (p95 936s ≥ 80% of 300s… step p95
// 400s ≥ 0.8×300s=240s) → applyable workflow_step_timeout proposal.
func TestTune_LatencyBreachFilesApplyableStepTimeout(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{
		lats: map[string]LatencySample{"janka": {P95Seconds: 936, Count: 12}},
		steps: []StepLatencySample{
			{Project: "janka", Workflow: "wf-a", Step: "slow", Role: "coder", Model: "m1", P95Seconds: 400, Count: 9},
			{Project: "janka", Workflow: "wf-a", Step: "quick", Role: "gate", Model: "m2", P95Seconds: 50, Count: 9},
		},
	}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)

	ps := drafts(t, repo)
	if len(ps) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(ps))
	}
	p := ps[0]
	if p.ApplyTarget != "configs/workflows/wf-a.md" || p.ApplyContent == "" {
		t.Fatalf("proposal must be applyable: target=%q", p.ApplyTarget)
	}
	// suggested ceil(400*1.5)=600s=10m; cap max(5m, 2×300s=600s)=600s → 10m.
	if !strings.Contains(p.ApplyContent, "10m") {
		t.Fatalf("want 10m timeout in content:\n%s", p.ApplyContent)
	}
	if !strings.Contains(p.Rationale, `"slow"`) || !strings.Contains(p.Rationale, "coder") {
		t.Fatalf("rationale must name the slow step + role: %s", p.Rationale)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil {
		t.Fatalf("evidence not JSON: %v", err)
	}
	if ev["base_hash"] == "" || ev["change"] == nil || ev["slowest_step"] != "slow" {
		t.Fatalf("evidence must carry base_hash + change + attribution: %s", p.Evidence)
	}
	if p.Diff == "" {
		t.Fatal("diff must be filled for review")
	}
}

// Timeout not binding (step p95 well under 80% of its timeout) → the proposal
// stays informational but names the slowest step and the model dimension.
func TestTune_LatencyBreachNotBindingStaysInformational(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{
		lats: map[string]LatencySample{"janka": {P95Seconds: 400, Count: 12}},
		steps: []StepLatencySample{
			{Project: "janka", Workflow: "wf-a", Step: "slow", Role: "coder", Model: "m1", P95Seconds: 100, Count: 9},
		},
	}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)

	ps := drafts(t, repo)
	if len(ps) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(ps))
	}
	p := ps[0]
	if p.ApplyTarget != "" {
		t.Fatalf("not-binding branch must be informational, got target %q", p.ApplyTarget)
	}
	if !strings.Contains(p.Rationale, `"slow"`) || !strings.Contains(p.Rationale, "m1") {
		t.Fatalf("rationale must name step + model: %s", p.Rationale)
	}
}

// Inclusive boundary (round-2 review): step p95 EXACTLY 80% of the timeout is
// binding and proposes a raise.
func TestTimeoutBindingThreshold_InclusiveBoundary(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{
		lats: map[string]LatencySample{"janka": {P95Seconds: 400, Count: 12}},
		steps: []StepLatencySample{
			// 240 = 0.8 × 300s exactly.
			{Project: "janka", Workflow: "wf-a", Step: "slow", Role: "coder", Model: "m1", P95Seconds: 240, Count: 9},
		},
	}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)

	ps := drafts(t, repo)
	if len(ps) != 1 || ps[0].ApplyTarget == "" {
		t.Fatalf("p95 exactly at the threshold must be binding (applyable); got %+v", ps)
	}
}

// Attribution unavailable (no step rows) → today's generic informational text.
func TestTune_LatencyBreachNoAttributionStaysGeneric(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{lats: map[string]LatencySample{"janka": {P95Seconds: 400, Count: 12}}}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)

	ps := drafts(t, repo)
	if len(ps) != 1 || ps[0].ApplyTarget != "" {
		t.Fatalf("want 1 informational proposal, got %+v", ps)
	}
	if !strings.Contains(ps[0].Rationale, "Investigate slow steps/tools") {
		t.Fatalf("generic rationale expected: %s", ps[0].Rationale)
	}
}

// Deterministic slowest-step selection: p95 DESC, count DESC, step ASC.
func TestTune_SlowestStepDeterministicOrder(t *testing.T) {
	w := &TuneWorker{Logger: zerolog.Nop(), MinSamples: 5, Metrics: &fakeMetrics{steps: []StepLatencySample{
		{Project: "p", Step: "b", P95Seconds: 100, Count: 9},
		{Project: "p", Step: "a", P95Seconds: 100, Count: 9},
		{Project: "p", Step: "c", P95Seconds: 100, Count: 12},
		{Project: "p", Step: "d", P95Seconds: 90, Count: 50},
		{Project: "p", Step: "tiny", P95Seconds: 500, Count: 2}, // below MinSamples
	}}}
	best, ok := w.slowestStep(context.Background(), "p")
	if !ok || best.Step != "c" {
		t.Fatalf("want step c (highest count among p95 ties, MinSamples respected), got %+v ok=%v", best, ok)
	}
}

// Instinct MCP tool breach renders the server timeout raise (daemon scope,
// LiveApply), with the qualified-name mapping.
func TestInstinct_MCPToolTimeoutFilesApplyable(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{tools: []ToolLatencySample{
		{Key: ProjectToolKey{Project: "janka", Tool: "mcp__scraper__web_fetch"}, P95Seconds: 90, Count: 9},
	}}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)

	ps := drafts(t, repo)
	if len(ps) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(ps))
	}
	p := ps[0]
	if p.ApplyTarget != "config.yaml" || !p.LiveApply || p.BlastRadius != persistence.ProposalScopeDaemon {
		t.Fatalf("daemon-scope live-apply expected: %+v", p)
	}
	// suggested ceil(90*1.5)=135s > current 60 → 135.
	if !strings.Contains(p.ApplyContent, "timeout_seconds: 135") {
		t.Fatalf("content:\n%s", p.ApplyContent)
	}
}

// A builtin (non-MCP) tool breach stays informational — no timeout key exists.
func TestInstinct_BuiltinToolStaysInformational(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{tools: []ToolLatencySample{
		{Key: ProjectToolKey{Project: "janka", Tool: "file_read"}, P95Seconds: 90, Count: 9},
	}}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)

	ps := drafts(t, repo)
	if len(ps) != 1 || ps[0].ApplyTarget != "" {
		t.Fatalf("builtin tool must stay informational: %+v", ps)
	}
}

// SkipFailedRate is a per-tick closure now: flipping it mid-run hands the
// failed-rate signal back without reconstruction (emergency-brake seam).
func TestTune_SkipFailedRateIsLiveClosure(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{ret: map[string]RateSample{"janka": {Failed: 8, Total: 10, Rate: 0.8}}}
	w := newTuneWorker(repo, m)
	skip := true
	w.SkipFailedRate = func() bool { return skip }
	tickN(w, 5)
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("skipped signal must not propose, got %d", n)
	}
	skip = false // self-heal turned off via config reload
	tickN(w, 3)
	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("after un-skip the generic failed-rate path must propose, got %d", n)
	}
}

// SelfHealWorker.Enabled gates ticks live and resets streaks while disabled.
func TestSelfHeal_EnabledClosureGatesAndResets(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{ret: map[string]RateSample{"janka": {Failed: 8, Total: 10, Rate: 0.8}}}
	enabled := false
	diag := &fakeIncidentDiagnoser{repo: repo}
	w := &SelfHealWorker{
		Proposals: repo, Metrics: m, Diagnose: diag,
		Enabled:  func() bool { return enabled },
		Logger:   zerolog.Nop(),
		breaches: map[string]int{},
	}
	tickN2 := func(n int) {
		for i := 0; i < n; i++ {
			w.tick(context.Background())
		}
	}
	tickN2(5)
	if diag.calls != 0 {
		t.Fatalf("disabled worker must not diagnose, got %d calls", diag.calls)
	}
	enabled = true
	// Streaks were reset while disabled: needs 3 fresh breaches.
	tickN2(2)
	if diag.calls != 0 {
		t.Fatalf("streak must restart after re-enable, got %d calls", diag.calls)
	}
	tickN2(1)
	if diag.calls != 1 {
		t.Fatalf("want exactly 1 diagnosis after 3 fresh breaches, got %d", diag.calls)
	}
}

// Review finding #7 (implementation review round 1): a latency proposal that
// starts informational (timeout not yet binding) and later becomes applyable
// carries the SAME title — the open-DRAFT title dedup must not strand the
// operator on the stale informational row. The applyable render supersedes:
// old informational DRAFT → REJECTED, new applyable DRAFT filed.
func TestTune_ApplyableUpgradeSupersedesInformationalDraft(t *testing.T) {
	repo := newTuneTestRepo(t)
	m := &fakeMetrics{
		lats: map[string]LatencySample{"janka": {P95Seconds: 936, Count: 12}},
		steps: []StepLatencySample{
			// Below the binding threshold first: 100 < 0.8×300s.
			{Project: "janka", Workflow: "wf-a", Step: "slow", Role: "coder", Model: "m1", P95Seconds: 100, Count: 9},
		},
	}
	w := newTuneWorker(repo, m)
	w.Actionize = testActionizer(actionableFiles())
	tickN(w, 3)
	ps := drafts(t, repo)
	if len(ps) != 1 || ps[0].ApplyTarget != "" {
		t.Fatalf("precondition: one informational draft, got %+v", ps)
	}
	informationalID := ps[0].ID

	// The step degrades further: now binding → the next proposing cycle
	// files the applyable proposal despite the same title.
	m.steps[0].P95Seconds = 400
	tickN(w, 3)

	ps = drafts(t, repo)
	var open, rejected *persistence.ControlPlaneProposal
	for _, p := range ps {
		switch p.Status {
		case persistence.ProposalStatusDraft:
			open = p
		case persistence.ProposalStatusRejected:
			rejected = p
		}
	}
	if open == nil || open.ApplyTarget == "" {
		t.Fatalf("want an applyable open DRAFT after the upgrade, got %+v", ps)
	}
	if rejected == nil || rejected.ID != informationalID {
		t.Fatalf("the stale informational draft must be superseded (REJECTED), got %+v", ps)
	}
	// And a plain re-tick must still dedup (no third proposal).
	tickN(w, 3)
	count := 0
	for _, p := range drafts(t, repo) {
		if p.Status == persistence.ProposalStatusDraft {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("continued breaching must not duplicate the applyable draft, got %d open", count)
	}
}
