package executor

import (
	"encoding/json"
	"testing"
)

// The pre-work checkout selects on HeadRef ALONE (design
// 2026-09-08-forge-ci-outcomes-design.md §17.3).
//
// INCIDENT: a CI-triggered review is not a pull-request EVENT, so its job
// carries IsChangeRequest=false — correctly. The executor required
// `IsChangeRequest && HeadRef != ""`, so every CI-triggered review took the
// default-branch rebase and reviewed with a working tree on `main`. Reviewers
// then described the change on main rather than the one in the diff they had
// been handed, and that was misdiagnosed as model unreliability.

// ciTriggeredPayload is the real shape, copied from
// task_20260909144328_ea0c06a8386d704b on headmatch PR #54.
//
// forge_job sits at the TOP LEVEL, which is where the daemon actually writes
// it. The first version of this fixture nested it under `context` — a shape
// forgeCheckoutSpec also accepts, so the test passed while pinning a payload
// the daemon does not produce. Caught by reading a live task's payload and
// finding the key somewhere else.
const ciTriggeredPayload = `{"context":{"prompt":"review"},"forge_job":{
  "repo":"grinco/headmatch","number":54,"action":"completed",
  "head_sha":"bd19f8a7a24fdf059d28de6c87c2b7b32dde451b",
  "head_ref":"refs/pull/54/head",
  "default_branch":"main","is_change_request":false,
  "ci":{"run_id":34350913564,"conclusion":"success"}}}`

// ciTriggeredPayloadNested is the same job under `context`, the OTHER shape
// forgeCheckoutSpec accepts. Both are pinned so neither reader can be dropped
// without a test noticing.
const ciTriggeredPayloadNested = `{"context":{"forge_job":{
  "repo":"grinco/headmatch","number":54,
  "head_ref":"refs/pull/54/head","default_branch":"main",
  "is_change_request":false}}}`

// THE REGRESSION: a CI-triggered review must take the change-request checkout
// even though it is not a change-request event.
func TestForgeCheckout_CITriggeredReviewMaterializesTheHead(t *testing.T) {
	spec, ok := forgeCheckoutSpec([]byte(ciTriggeredPayload))
	if !ok {
		t.Fatal("a CI-triggered review payload must yield a checkout spec")
	}
	if spec.IsChangeRequest {
		t.Error("IsChangeRequest must stay FALSE — it routes the webhook, and " +
			"flipping it would send CI deliveries to change_request_workflow_id")
	}
	if spec.HeadRef != "refs/pull/54/head" {
		t.Fatalf("HeadRef = %q, want refs/pull/54/head — without it the reviewer "+
			"works against the base branch", spec.HeadRef)
	}
	// The branch the executor takes, asserted as the executor asks it — on
	// HeadRef alone, with IsChangeRequest false above.
	if spec.HeadRef == "" {
		t.Error("the pre-work checkout must select on HeadRef alone")
	}
}

// A run with no pull request — a default-branch build, or a fork PR whose
// event carries no pull_requests[] (§3.3) — has no head to materialize and
// must keep taking the rebase.
func TestForgeCheckout_ARunWithNoPullRequestStillRebases(t *testing.T) {
	payload := `{"context":{"forge_job":{"repo":"o/r","number":0,
	  "default_branch":"main","is_change_request":false,
	  "ci":{"run_id":1,"conclusion":"failure"}}}}`
	spec, ok := forgeCheckoutSpec([]byte(payload))
	if !ok {
		t.Fatal("a forge payload with a default branch is still a forge task")
	}
	if spec.HeadRef != "" {
		t.Errorf("HeadRef = %q, want empty — there is no pull request to check out", spec.HeadRef)
	}
	if spec.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", spec.DefaultBranch)
	}
}

// A pull-request event is unchanged: both flags set, same branch taken.
func TestForgeCheckout_PullRequestEventIsUnchanged(t *testing.T) {
	payload := `{"context":{"forge_job":{"repo":"o/r","number":7,
	  "head_ref":"refs/pull/7/head","default_branch":"main","is_change_request":true}}}`
	spec, ok := forgeCheckoutSpec([]byte(payload))
	if !ok || spec.HeadRef != "refs/pull/7/head" || !spec.IsChangeRequest {
		t.Fatalf("a PR event must be untouched: %+v ok=%v", spec, ok)
	}
}

// A non-forge task yields no spec at all.
func TestForgeCheckout_NonForgeTaskYieldsNoSpec(t *testing.T) {
	for _, p := range []string{`{}`, `{"context":{}}`, `{"context":{"forge_job":{}}}`, `not json`} {
		if _, ok := forgeCheckoutSpec([]byte(p)); ok {
			t.Errorf("payload %q must not yield a checkout spec", p)
		}
	}
}

// The fixture carries forge_job where the DAEMON writes it — top level — and
// carries every key the spec reads.
//
// The first version of this test asserted the nested shape, which
// forgeCheckoutSpec also accepts, so it passed while pinning a payload the
// daemon never produces. A fixture that both the code and the test agree on,
// and that production does not emit, tests the agreement rather than the code.
func TestForgeCheckout_FixtureMatchesTheDaemonsShape(t *testing.T) {
	var probe struct {
		ForgeJob map[string]any `json:"forge_job"`
	}
	if err := json.Unmarshal([]byte(ciTriggeredPayload), &probe); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if probe.ForgeJob == nil {
		t.Fatal("forge_job must sit at the TOP LEVEL — that is where the daemon writes it")
	}
	for _, k := range []string{"head_ref", "head_sha", "is_change_request", "ci"} {
		if _, ok := probe.ForgeJob[k]; !ok {
			t.Errorf("fixture is missing %q, which the daemon writes", k)
		}
	}
}

// BOTH accepted shapes resolve identically, so neither reader can be removed
// without a test noticing.
func TestForgeCheckout_TopLevelAndNestedAgree(t *testing.T) {
	top, ok1 := forgeCheckoutSpec([]byte(ciTriggeredPayload))
	nested, ok2 := forgeCheckoutSpec([]byte(ciTriggeredPayloadNested))
	if !ok1 || !ok2 {
		t.Fatalf("both shapes must resolve: top=%v nested=%v", ok1, ok2)
	}
	if top.HeadRef != nested.HeadRef || top.IsChangeRequest != nested.IsChangeRequest {
		t.Errorf("the two accepted shapes disagree: %+v vs %+v", top, nested)
	}
}
