package api

import (
	"strings"
	"testing"
	"time"
)

// THE check that would have caught the gemma4 rung without an audit.
//
// Measured 2026-08-26: `discover_model_fallback` on `gemma4:26b` failed 4 of 4,
// all sub-second. Re-measured 2026-09-03 and the reason is plain — the agent
// circuit for that model has been OPEN since 2026-08-23, so the sub-second exits
// were the breaker refusing before any inference. Eleven days of a configured
// fallback rung that could not work, and nothing reported it.
func TestFallbackRungs_ReportsARungThatNeverReachedInference(t *testing.T) {
	last := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	items, status, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "discover_model_fallback", role: "scout", model: "gemma4:26b",
		attempts: 4, lastClass: "model_unhealthy", lastFailed: last,
	}})

	if status != "ERROR" {
		t.Fatalf("a fallback rung that has never worked must be an ERROR, got %q", status)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(items), items)
	}
	for _, want := range []string{"discover_model_fallback", "scout", "gemma4:26b", "4 attempts", "model_unhealthy"} {
		if !strings.Contains(items[0], want) {
			t.Errorf("finding %q does not name %q — an operator cannot act on it", items[0], want)
		}
	}
	if !strings.Contains(msg, "retry budget") {
		t.Errorf("the message does not say what the dead rung COSTS: %q", msg)
	}
}

// When every dead rung is on one model, say so. Several rungs failing is usually
// ONE model being unavailable — naming it is what turns a list into an action.
// This is the live shape on 2026-09-03: seven rungs, all zai.glm-5.
func TestFallbackRungs_NamesTheModelWhenItIsTheCommonCause(t *testing.T) {
	last := time.Now().UTC()
	rungs := []deadFallbackRung{
		{stepID: "ingest_model_fallback_infra_retry1", role: "ingestor", model: "zai.glm-5", attempts: 11, lastClass: "llm_call_failed", lastFailed: last},
		{stepID: "recover_lead_lead_model_fallback_infra_retry1", role: "lead", model: "zai.glm-5", attempts: 11, lastClass: "model_unhealthy", lastFailed: last},
		{stepID: "ingest_model_fallback_infra_retry2", role: "ingestor", model: "zai.glm-5", attempts: 8, lastClass: "llm_call_failed", lastFailed: last},
	}
	_, _, msg := evaluateFallbackRungs(rungs)
	if !strings.Contains(msg, `"zai.glm-5"`) {
		t.Errorf("three dead rungs on one model did not name it: %q", msg)
	}
	if !strings.Contains(msg, "the model is the fault rather than the rungs") {
		t.Errorf("the message does not point at the actual fault: %q", msg)
	}
}

// Rungs spread across DIFFERENT models are not one fault, and must not be
// reported as though pointing at one model would fix them.
func TestFallbackRungs_DoesNotInventACommonCause(t *testing.T) {
	last := time.Now().UTC()
	_, _, msg := evaluateFallbackRungs([]deadFallbackRung{
		{stepID: "a_model_fallback", model: "zai.glm-5", attempts: 3, lastFailed: last},
		{stepID: "b_model_fallback", model: "gemma4:26b", attempts: 3, lastFailed: last},
	})
	if strings.Contains(msg, "the model is the fault") {
		t.Errorf("two different models were reported as one common cause: %q", msg)
	}
}

// A healthy deployment must be quiet. A check that reports something every run
// is one that gets muted — which is how the original 4-of-4 survived eleven days.
func TestFallbackRungs_QuietWhenNothingIsDead(t *testing.T) {
	items, status, msg := evaluateFallbackRungs(nil)
	if status != "OK" || len(items) != 0 {
		t.Fatalf("status = %q with %d items, want a quiet OK", status, len(items))
	}
	if !strings.Contains(msg, "before inference") {
		t.Errorf("the OK message does not say what was actually checked: %q", msg)
	}
}

// The class list is the whole check, so it is asserted rather than left to the
// query. A rung that RAN and was judged badly is not dead — reporting it would
// have turned seven real findings into thirteen on the live deployment, and a
// check that cries wolf gets muted.
func TestFallbackRungs_JudgedFailureClassesAreNotDeadRungs(t *testing.T) {
	unreached := map[string]bool{}
	for _, c := range unreachedInferenceClasses {
		unreached[c] = true
	}
	for _, ranAndWasJudged := range []string{
		"hallucinated_claim",
		"verifier_warn",
		"prompt_token_budget",
		"context_timeout",
		"plausibility_violation",
		"missing_declared_output",
		"degenerate_loop",
		"iteration_cap",
		"parse_invalid_json",
		"verify_claims_failed",
	} {
		if unreached[ranAndWasJudged] {
			t.Errorf("%q counts as never-reached-inference, but a step classified that way "+
				"demonstrably reached the model — it is a quality problem for another surface",
				ranAndWasJudged)
		}
	}
	// And the ones that genuinely mean "never got to the call" must be present,
	// or the check goes quiet on exactly the failure it exists to report.
	for _, neverRan := range []string{"model_unhealthy", "container_non_zero_exit", "container_start_failed"} {
		if !unreached[neverRan] {
			t.Errorf("%q is missing from unreachedInferenceClasses; the gemma4 case would go unreported", neverRan)
		}
	}
}

// A rung REFUSED by an open circuit and a rung that CALLED AND FAILED are not
// the same finding, and the remedies are opposite.
//
// Backlog 2026-09-04: "The fallback_rungs check cannot tell a refused call from
// a failed one". Seven dead rungs on zai.glm-5 were reported as "never reached
// inference", which read as credentials/endpoint — for a model that had ok=15
// on another rung. The ledger showed two populations: 0.0-1.1s model_unhealthy
// (the breaker doing its job before any request) and 25-53s llm_call_failed
// (a request made and failed upstream). Lumping them cost a day of
// misdiagnosis. The class is typed; the check must read it.
func TestFallbackRungs_ARefusedRungIsNamedAsRefused(t *testing.T) {
	last := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	items, _, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "recover_lead_lead_model_fallback_infra_retry1", role: "lead", model: "zai.glm-5",
		attempts: 11, refused: 11, lastClass: "model_unhealthy", lastFailed: last,
		avgDuration: 400 * time.Millisecond,
	}})
	if len(items) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(items), items)
	}
	for _, want := range []string{"11 refused by an open circuit", "0.4s"} {
		if !strings.Contains(items[0], want) {
			t.Errorf("finding %q does not say %q — the operator cannot tell a refusal from a failed call", items[0], want)
		}
	}
	if !strings.Contains(msg, "breaker") || !strings.Contains(msg, "circuit is open") {
		t.Errorf("every attempt was a refusal, but the message does not point at the OPEN CIRCUIT: %q", msg)
	}
	if strings.Contains(msg, "credentials") || strings.Contains(msg, "endpoint") {
		t.Errorf("a refused rung must not send the operator to credentials/endpoints: %q", msg)
	}
}

func TestFallbackRungs_ACalledAndFailedRungIsNamedAsFailedUpstream(t *testing.T) {
	last := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	items, _, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "ingest_model_fallback_infra_retry1", role: "ingestor", model: "zai.glm-5",
		attempts: 7, calledAndFailed: 7, lastClass: "llm_call_failed", lastFailed: last,
		avgDuration: 41 * time.Second,
	}})
	if len(items) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(items), items)
	}
	for _, want := range []string{"7 called and failed upstream", "41s"} {
		if !strings.Contains(items[0], want) {
			t.Errorf("finding %q does not say %q", items[0], want)
		}
	}
	if !strings.Contains(msg, "provider") {
		t.Errorf("calls were made and failed; the message must point at the model or its provider: %q", msg)
	}
	if strings.Contains(msg, "circuit is open") {
		t.Errorf("no attempt was refused, yet the message blames an open circuit: %q", msg)
	}
}

// Mixed populations are reported as both, per rung — never collapsed into
// whichever class happened to be last.
func TestFallbackRungs_MixedRungReportsBothCounts(t *testing.T) {
	last := time.Now().UTC()
	items, _, msg := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "a_model_fallback", model: "zai.glm-5",
		attempts: 5, refused: 3, calledAndFailed: 2, lastClass: "model_unhealthy", lastFailed: last,
	}})
	for _, want := range []string{"3 refused by an open circuit", "2 called and failed upstream"} {
		if !strings.Contains(items[0], want) {
			t.Errorf("finding %q does not say %q", items[0], want)
		}
	}
	if strings.Contains(msg, "every attempt") {
		t.Errorf("only some attempts were refusals, yet the message generalises to every attempt: %q", msg)
	}
}

// Attempts in neither population — container_*, missing_prerequisite — are the
// environment, not the model, and the item says so instead of hiding them.
func TestFallbackRungs_EnvironmentFailuresAreNamedAsSuch(t *testing.T) {
	last := time.Now().UTC()
	items, _, _ := evaluateFallbackRungs([]deadFallbackRung{{
		stepID: "a_model_fallback", model: "gemma4:26b",
		attempts: 4, lastClass: "container_start_failed", lastFailed: last,
	}})
	if !strings.Contains(items[0], "4 failed before any call (container or environment)") {
		t.Errorf("finding %q does not name the environment population", items[0])
	}
}
