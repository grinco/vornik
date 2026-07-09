package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestPerStepVisitCapExceeded pins the per-step visit-cap decision used to
// bound issue-fix's review→remediate loopback at 2 rounds (design
// 2026-07-09-issue-fix-remediation-loopback). A step opts in with maxVisits>0;
// the cap trips on the (maxVisits+1)-th entry. maxVisits<=0 leaves the step on
// the workflow-global cap only.
func TestPerStepVisitCapExceeded(t *testing.T) {
	cases := []struct {
		name      string
		maxVisits int
		visits    int
		want      bool
	}{
		{"disabled (0) never trips", 0, 99, false},
		{"negative disabled", -1, 5, false},
		{"under cap", 2, 1, false},
		{"at cap does not trip", 2, 2, false},
		{"one over cap trips", 2, 3, true},
		{"review cap 3 at third visit ok", 3, 3, false},
		{"review cap 3 fourth visit trips", 3, 4, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := registry.WorkflowStep{MaxVisits: tc.maxVisits}
			if got := perStepVisitCapExceeded(step, tc.visits); got != tc.want {
				t.Fatalf("perStepVisitCapExceeded(max=%d, visits=%d)=%v, want %v", tc.maxVisits, tc.visits, got, tc.want)
			}
		})
	}
}
