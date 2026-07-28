package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/registry"
)

// Behavioural proof for the T-1089 publish gate. The registry-side test
// (TestShippedWorkflows_PublisherSuccessIsGated) proves the WIRING exists; this
// proves the wiring actually rejects the payload the incident produced.
//
// The real publisher output from the T-1089 fork was:
//
//	{"published":{"ok":false,"reason":"artifacts/out/deliverable.md not found..."},
//	 "message":"Cannot publish — deliverable.md does not exist..."}
//
// That is schema-VALID (the publisher's outputSchema requires only `ok`, plus a
// `reason` when ok is false), so the step SUCCEEDED and `on_success` fired
// straight into the COMPLETED terminal. These tests pin that the gate now sends
// that exact payload to a FAILED destination instead.

// publishGateStep mirrors the `confirm_published` gate the shipped publishing
// workflows now declare. Kept in sync by
// TestConfirmPublishedGate_MatchesShippedDeepResearch below.
func publishGateStep() registry.WorkflowStep {
	return registry.WorkflowStep{
		Type: "gate",
		Gates: []registry.WorkflowGate{
			{Condition: "published.ok == true", Target: "done"},
			{Condition: "published.ok == false", Target: "publish_failed"},
		},
		OnFail: "publish_failed",
	}
}

// The verbatim T-1089 failing publisher payload must route to publish_failed.
func TestPublishGate_RejectsDeclaredFailure_T1089(t *testing.T) {
	payload := json.RawMessage(`{
	  "published": {"ok": false, "reason": "artifacts/out/deliverable.md not found. The writer step failed to produce the actual deliverable."},
	  "message": "Cannot publish — deliverable.md does not exist."
	}`)

	target, trace, err := evaluateGateStepTraced(publishGateStep(), payload)

	require.NoError(t, err, "ok:false is a matched condition, not an evaluation error")
	assert.Equal(t, "publish_failed", target,
		"a publisher declaring ok:false must NOT reach the COMPLETED terminal (T-1089)")
	require.NotEmpty(t, trace.Entries)
	assert.False(t, trace.Entries[0].Matched, "the ok==true branch must not match")
}

// A genuine success must still complete — the gate must not be a blanket block.
func TestPublishGate_AllowsGenuineSuccess(t *testing.T) {
	payload := json.RawMessage(`{
	  "published": {"ok": true, "url": "https://pagedrop.example/abc", "title": "Pricing Research"},
	  "message": "Published."
	}`)

	target, _, err := evaluateGateStepTraced(publishGateStep(), payload)

	require.NoError(t, err)
	assert.Equal(t, "done", target, "a real publish must still reach the COMPLETED terminal")
}

// The dangerous middle case: a malformed result carrying NO published.ok key at
// all (e.g. a model that followed a stale prompt emitting {"publish":{...}}).
// No gate condition matches, so evaluation errors — which runGateStep routes to
// step.OnFail. Because the shipped gates leave on_success UNSET, that cannot
// become a clean fall-through into `done`.
func TestPublishGate_MissingOkKeyDoesNotReachDone(t *testing.T) {
	// The stale `{"publish": ...}` shape the deep-research/parallel-research
	// prompts used to ask for — no `published.ok` anywhere.
	payload := json.RawMessage(`{"publish":{"url":"","title":"Pricing Research"}}`)

	step := publishGateStep()
	target, _, err := evaluateGateStepTraced(step, payload)

	require.Error(t, err, "no condition matches → evaluation error")
	assert.Empty(t, target)
	assert.Empty(t, step.OnSuccess,
		"on_success must stay UNSET or runGateStep treats a non-match as a clean fall-through (T-1089)")
	assert.Equal(t, "publish_failed", step.OnFail,
		"the non-matching route must land on a failing destination")
}

// Guard against the inline fixture drifting from what the shipped workflow
// actually declares — otherwise these behavioural tests could keep passing
// against a gate nobody runs.
func TestConfirmPublishedGate_MatchesShippedDeepResearch(t *testing.T) {
	root := repoRootFromExecutorTest(t)
	path := filepath.Join(root, "configs", "workflows", "deep-research.md")
	content, err := os.ReadFile(path)
	require.NoError(t, err)

	wf, err := registry.ParseWorkflowMarkdown(content, "deep-research.md")
	require.NoError(t, err)
	require.NotNil(t, wf)

	gate, ok := wf.Steps["confirm_published"]
	require.True(t, ok, "deep-research must declare the confirm_published gate")
	assert.Equal(t, publishGateStep().Gates, gate.Gates,
		"inline fixture drifted from the shipped deep-research gate")
	assert.Equal(t, publishGateStep().OnFail, gate.OnFail)
	assert.Empty(t, gate.OnSuccess,
		"confirm_published must not set on_success (clean fall-through would reopen T-1089)")

	// And the target terminals must mean what the gate assumes.
	assert.Equal(t, "COMPLETED", wf.Terminals["done"].Status)
	assert.Equal(t, "FAILED", wf.Terminals["publish_failed"].Status)
}

// repoRootFromExecutorTest walks up to the module root so the test can read the
// shipped configs regardless of the package's working directory.
func repoRootFromExecutorTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate module root from executor test")
	return ""
}
