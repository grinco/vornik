package stepid

import "testing"

// The executor writes every retry rung as its own step-outcome row under a
// suffixed id. Anything that reads those rows and wants "the step" or "the first
// attempt" must strip or recognise the same suffixes the executor appends —
// this package is that one vocabulary (self-evolving-workflows design, addendum
// 2026-09-03).
func TestStripRetrySuffix(t *testing.T) {
	cases := map[string]string{
		"plan":                       "plan",
		"plan_shape_retry":           "plan",
		"plan_model_fallback":        "plan",
		"plan_infra_retry1":          "plan",
		"plan_infra_retry12":         "plan",
		"plan_refusal_retry":         "plan",
		"plan_route_retry":           "plan",
		"discover_model_fallback":    "discover",
		"plan_lead_lead_shape_retry": "plan_lead_lead",
		"write_infra_retry":          "write", // no digit: still the infra prefix
		"retry_shape":                "retry_shape",
		"_shape_retry":               "_shape_retry", // a bare suffix is not a retry of anything
	}
	for in, want := range cases {
		if got := StripRetrySuffix(in); got != want {
			t.Errorf("StripRetrySuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsRetryAttempt(t *testing.T) {
	for _, id := range []string{"plan_shape_retry", "plan_model_fallback", "plan_infra_retry3", "plan_refusal_retry", "plan_route_retry"} {
		if !IsRetryAttempt(id) {
			t.Errorf("IsRetryAttempt(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"plan", "review", "plan_lead_lead", "_shape_retry", ""} {
		if IsRetryAttempt(id) {
			t.Errorf("IsRetryAttempt(%q) = true, want false", id)
		}
	}
}
