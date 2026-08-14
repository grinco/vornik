package agentbench

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Schema following is a gate for most roles: a step whose output does not
// conform does not merely score badly, it fails. These tests pin that the probe
// reports it as the gate it is.

func TestSchemaProbe_NeedsNoGold(t *testing.T) {
	// The ground truth for schema following is the role's DECLARED schema —
	// config, not a recording. So this probe requires no unrestricted-ceiling
	// pass, no operator review of a gold set, and can gate from day one. It is
	// the cheapest probe in the design.
	trace := Trace{
		ExecutionID: "e1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "worker", Outcome: OutcomeOK, Attempt: 1},
		},
	}
	probe := SchemaProbe{}
	if _, err := probe.Score(context.Background(), TaskRef{ID: "t1"}, Gold{}, trace); err != nil {
		t.Fatalf("schema probe demanded gold it does not need: %v", err)
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})
	if !v.SchemaConformanceDefined || v.SchemaConformance != 1.0 {
		t.Errorf("conformance = %v (defined %v), want 1.0", v.SchemaConformance, v.SchemaConformanceDefined)
	}
}

// parse_error and schema_violation are DIFFERENT failures with different fixes.
// retry.go already makes this distinction for its corrective hints — telling a
// model that produced valid JSON to "respond only with valid JSON" misleads it
// about the actual problem — so the probe must not blend them.
func TestSchemaProbe_SeparatesUnparseableFromWrongShape(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "worker", Outcome: OutcomeParseError, Attempt: 1},
			{StepID: "s2", Role: "worker", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "s3", Role: "worker", Outcome: OutcomeOK, Attempt: 1},
			{StepID: "s4", Role: "worker", Outcome: OutcomeOK, Attempt: 1},
		},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})
	if v.ParseErrors != 1 {
		t.Errorf("parse errors = %d, want 1", v.ParseErrors)
	}
	if v.SchemaViolations != 1 {
		t.Errorf("schema violations = %d, want 1", v.SchemaViolations)
	}
	if want := 0.5; v.SchemaConformance != want {
		t.Errorf("conformance = %v, want %v", v.SchemaConformance, want)
	}
}

// The cost of poor schema following is retries, and retries are tokens. A role
// that gets there on attempt 3 every time is not "conformant".
func TestSchemaProbe_CountsRetriesToValid(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "worker", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "s1", Role: "worker", Outcome: OutcomeSchemaViolation, Attempt: 2},
			{StepID: "s1", Role: "worker", Outcome: OutcomeOK, Attempt: 3},
		},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})
	if v.RetriesToValid != 2 {
		t.Errorf("retries to valid = %d, want 2", v.RetriesToValid)
	}
	// First-emission conformance is the honest headline: the step DID
	// eventually succeed, and reporting only that would hide two wasted rounds.
	if v.FirstEmissionConformance != 0 {
		t.Errorf("first-emission conformance = %v, want 0 — the first attempt violated",
			v.FirstEmissionConformance)
	}
	if v.SchemaConformance != 1.0 {
		t.Errorf("eventual conformance = %v, want 1.0", v.SchemaConformance)
	}
}

// Schema-valid but rejected downstream is a distinct and interesting failure:
// the role obeyed the contract and still produced something unusable.
func TestSchemaProbe_TracksDownstreamRejectionSeparately(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "worker", Outcome: OutcomeDownstreamRejected, Attempt: 1},
			{StepID: "s2", Role: "worker", Outcome: OutcomeGateFailed, Attempt: 1},
		},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})
	if v.DownstreamRejected != 1 || v.GateFailed != 1 {
		t.Errorf("downstream=%d gate=%d, want 1 and 1", v.DownstreamRejected, v.GateFailed)
	}
	// Neither is a schema failure, so neither may drag conformance down.
	if v.ParseErrors != 0 || v.SchemaViolations != 0 {
		t.Error("a downstream rejection was counted as a schema failure")
	}
}

func TestSchemaProbe_PerRoleBreakdown(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "lead", Outcome: OutcomeOK, Attempt: 1},
			{StepID: "s2", Role: "worker", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "s3", Role: "worker", Outcome: OutcomeOK, Attempt: 1},
		},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})
	// A blended figure hides which role needs the fix, and schema following is
	// gated per role.
	if got := v.ByRole["lead"].Conformance; got != 1.0 {
		t.Errorf("lead conformance = %v, want 1.0", got)
	}
	if got := v.ByRole["worker"].Conformance; got != 0.5 {
		t.Errorf("worker conformance = %v, want 0.5", got)
	}
}

func TestSchemaProbe_NoTerminalOutcomesLeavesConformanceUndefined(t *testing.T) {
	v := SchemaProbe{}.ScoreSchema(Trace{ExecutionID: "e1"}, TaskRef{ID: "t1"})
	if v.SchemaConformanceDefined {
		t.Error("conformance reported with nothing to conform — an execution that " +
			"emitted nothing is not 0% conformant, it is unmeasured")
	}
}

// pending_validation is not yet an answer. Counting it either way invents a
// result the ledger has not recorded.
func TestSchemaProbe_PendingValidationIsNotTerminal(t *testing.T) {
	trace := Trace{
		ExecutionID: "e1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "worker", Outcome: OutcomePendingValidation, Attempt: 1},
			{StepID: "s2", Role: "worker", Outcome: OutcomeOK, Attempt: 1},
		},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})
	if v.Terminal != 1 {
		t.Errorf("terminal = %d, want 1 — pending_validation has not resolved", v.Terminal)
	}
	if v.SchemaConformance != 1.0 {
		t.Errorf("conformance = %v, want 1.0 over the one terminal outcome", v.SchemaConformance)
	}
}

// The outcome vocabulary is owned by the migration that created the table, and
// this package restates it. A restatement that drifts silently starts scoring
// outcomes that no longer exist — or missing ones that do.
func TestOutcomeConstants_MatchTheMigrationTaxonomy(t *testing.T) {
	src, err := os.ReadFile("../persistence/migrations.go")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	body := string(src)
	const marker = "Per-step outcome taxonomy:"
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatal("could not find the outcome taxonomy comment — the scan is broken, " +
			"which would make this law vacuous")
	}
	taxonomy := body[idx : idx+400]

	for _, outcome := range AllStepOutcomes() {
		if !strings.Contains(taxonomy, outcome) {
			t.Errorf("outcome %q is scored here but absent from the migration's taxonomy — "+
				"one of the two has drifted", outcome)
		}
	}
}

// Schema following gates per role, so the actionable output is which roles fail
// — an average is a number no single role experiences.
func TestSchemaVerdict_WorstRolesRanksBelowThreshold(t *testing.T) {
	v := SchemaVerdict{ByRole: map[string]RoleConformance{
		"lead":     {Terminal: 4, Conforming: 4, Conformance: 1.0},
		"worker":   {Terminal: 4, Conforming: 3, Conformance: 0.75},
		"reviewer": {Terminal: 4, Conforming: 1, Conformance: 0.25},
		"idle":     {Terminal: 0},
	}}

	got := v.WorstRoles(0.9)
	if len(got) != 2 || got[0] != "reviewer" || got[1] != "worker" {
		t.Fatalf("WorstRoles = %v, want [reviewer worker] — worst first", got)
	}
	// A role that emitted nothing is unmeasured, not failing.
	for _, r := range got {
		if r == "idle" {
			t.Error("a role with no terminal outcomes was reported as failing")
		}
	}
}

// A step whose container exited non-zero produced NOTHING. Counting it as
// conformant — which the first implementation did, since it is merely "not a
// shape failure" — inflates the metric with steps that never emitted anything.
// The first live run turned a true 2/7 into a reported 3/8 exactly this way.
func TestSchemaProbe_StepsThatProducedNoOutputLeaveTheDenominator(t *testing.T) {
	// The shape observed on the 2026-08-14 smoke run, verbatim.
	trace := Trace{
		ExecutionID: "exec_20260814002914",
		Outcomes: []StepOutcome{
			{StepID: "audit", Role: "reviewer", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "audit_shape_retry", Role: "reviewer", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "audit_model_fallback", Role: "reviewer", Outcome: OutcomeFailed, Attempt: 1},
		},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "ca-01"})

	if v.Terminal != 3 {
		t.Errorf("terminal = %d, want 3 — all three resolved", v.Terminal)
	}
	if v.Judged != 2 {
		t.Errorf("judged = %d, want 2 — the crashed step emitted nothing to judge", v.Judged)
	}
	if v.NoOutput != 1 {
		t.Errorf("noOutput = %d, want 1", v.NoOutput)
	}
	if v.SchemaConformance != 0 {
		t.Errorf("conformance = %v, want 0 — both judged steps violated; the old rule "+
			"reported 1/3 by counting a container crash as conformant", v.SchemaConformance)
	}
	// And it is not counted as a violation either: a crash is a reliability
	// fact, not a schema one.
	if v.SchemaViolations != 2 {
		t.Errorf("violations = %d, want 2 — the crash must not be folded in", v.SchemaViolations)
	}
}

// The whole smoke run, hand-computed: 7 judged steps, 2 conforming.
func TestSchemaProbe_MatchesTheHandComputedSmokeRun(t *testing.T) {
	execs := [][]StepOutcome{
		{ // exec ...002914
			{StepID: "audit", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "audit_shape_retry", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "audit_model_fallback", Outcome: OutcomeFailed, Attempt: 1},
		},
		{ // exec ...003036
			{StepID: "audit", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "audit_shape_retry", Outcome: OutcomeSchemaViolation, Attempt: 1},
			{StepID: "audit_model_fallback", Outcome: OutcomeSchemaViolation, Attempt: 1},
		},
		{{StepID: "audit", Outcome: OutcomeOK, Attempt: 1}},    // exec ...003148
		{{StepID: "validate", Outcome: OutcomeOK, Attempt: 1}}, // exec ...003223
	}

	judged, conforming := 0, 0
	for _, outcomes := range execs {
		v := SchemaProbe{}.ScoreSchema(Trace{Outcomes: outcomes}, TaskRef{ID: "t"})
		judged += v.Judged
		conforming += int(v.SchemaConformance*float64(v.Judged) + 0.5)
	}
	if judged != 7 || conforming != 2 {
		t.Fatalf("judged/conforming = %d/%d, want 7/2 — hand-counted from the ledger",
			judged, conforming)
	}
	if got := float64(conforming) / float64(judged); got < 0.2857 || got > 0.2858 {
		t.Errorf("aggregate conformance = %.4f, want 0.2857", got)
	}
}
