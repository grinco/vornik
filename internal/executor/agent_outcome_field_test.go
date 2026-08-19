package executor

import "testing"

// The agent's step-quality label and the lead's workflow decision used one
// top-level `outcome` field with disjoint vocabularies until 2026-08-19, and the
// agent's label was injected after the structured merge — so a recovery hop that
// also hit its iteration cap had its decision checkpoint overwritten with
// `iteration_exhausted`, and every recovery attempt failed its contract.
//
// The agent now writes `agentOutcome`. The daemon must read that, and must still
// honour a legacy `outcome` so an older agent image keeps working against a
// newer daemon — the two are deployed separately.
func TestAgentQualityOutcome_PrefersNewFieldAndAcceptsLegacy(t *testing.T) {
	cases := map[string]struct {
		result      string
		wantOutcome string
		wantDetail  string
	}{
		"new field": {
			`{"status":"COMPLETED","agentOutcome":"iteration_exhausted","agentOutcomeDetail":"cap"}`,
			"iteration_exhausted", "cap",
		},
		"legacy field from an older agent image": {
			`{"status":"COMPLETED","outcome":"iteration_exhausted","outcomeDetail":"cap"}`,
			"iteration_exhausted", "cap",
		},
		// The case the collision broke: a lead decision in `outcome` must NOT be
		// read as a quality label, or a checkpoint gets recorded as a bail.
		"lead decision is not a quality label": {
			`{"status":"COMPLETED","outcome":"checkpoint","checkpoint_kind":"decision"}`,
			"", "",
		},
		"new field wins when both are present": {
			`{"status":"COMPLETED","outcome":"checkpoint","agentOutcome":"budget_tripwire","agentOutcomeDetail":"spent"}`,
			"budget_tripwire", "spent",
		},
		"neither": {`{"status":"COMPLETED"}`, "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotOutcome, gotDetail := agentQualityOutcome([]byte(tc.result))
			if gotOutcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", gotOutcome, tc.wantOutcome)
			}
			if gotDetail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", gotDetail, tc.wantDetail)
			}
		})
	}
}
