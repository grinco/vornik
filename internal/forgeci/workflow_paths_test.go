package forgeci

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// The two path filters (design 2026-09-08-forge-ci-outcomes-design.md §14).
//
// `workflow_paths` decides what is RECORDED; `trigger_workflow_paths` decides
// which of those may TRIGGER. One knob could not express what a real pull
// request needs: a repo running five workflows either posts five reviews on a
// green push, or hides four workflows' results from the one review it posts.

const (
	gate  = ".github/workflows/typecheck.yml"
	other = ".github/workflows/coverage.yml"
)

// recordingOutcomes counts what actually reached the store.
type recordingOutcomes struct {
	markingOutcomes
	upserted []string
}

func (r *recordingOutcomes) Upsert(_ context.Context, o *persistence.ForgeCIOutcome) error {
	r.upserted = append(r.upserted, o.WorkflowPath)
	return nil
}

// countingReader fails the test's real assertion: an unwatched run must not
// cost a provider round-trip, not merely fail to produce a row.
type countingReader struct{ fetches int }

func (c *countingReader) FetchCIRun(context.Context, string, int64) (forge.CIRun, error) {
	c.fetches++
	return forge.CIRun{}, nil
}
func (c *countingReader) ListCIArtifacts(context.Context, string, int64) ([]forge.CIArtifact, error) {
	return nil, nil
}
func (c *countingReader) FetchCIArtifact(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}
func (c *countingReader) VerifyCIAccess(context.Context) error { return nil }

// Both empty is everything — today's behaviour, pinned first because both keys
// are narrowings of it and neither may change a project that sets neither.
func TestPathFilters_EmptyAllowsEverything(t *testing.T) {
	var c Config
	for _, p := range []string{gate, other, ""} {
		if !c.Watches(p) || !c.Triggers(p) {
			t.Errorf("%q: an unset filter must allow everything", p)
		}
	}
}

// An unwatched run is not recorded AND not fetched.
func TestRecord_AnUnwatchedWorkflowCostsNoAPICall(t *testing.T) {
	store := &recordingOutcomes{}
	reader := &countingReader{}
	g := New(store, reader, Config{WorkflowPaths: []string{gate}}, zerolog.Nop())

	if out := g.Record(context.Background(), Run{Repo: "acme/infra", RunID: 1, WorkflowPath: other}); out != nil {
		t.Errorf("an unwatched run must not be recorded, got %+v", out)
	}
	if len(store.upserted) != 0 {
		t.Errorf("upserted = %v, want none", store.upserted)
	}
	if reader.fetches != 0 {
		t.Errorf("fetches = %d — the filter must sit BEFORE the provider round-trip", reader.fetches)
	}

	// And the watched one goes all the way through.
	if out := g.Record(context.Background(), Run{Repo: "acme/infra", RunID: 2, WorkflowPath: gate}); out == nil {
		t.Fatal("a watched run must be recorded")
	}
	if len(store.upserted) != 1 || store.upserted[0] != gate {
		t.Errorf("upserted = %v, want [%s]", store.upserted, gate)
	}
}

// THE CASE NEITHER KEY COULD EXPRESS ALONE: recorded, so a review that runs for
// another reason still sees it — but not a trigger itself.
func TestDecide_RecordedButNotAGateDoesNotTrigger(t *testing.T) {
	cfg := Config{
		ReviewOnFailure:      true,
		SuccessWorkflowID:    "github-review",
		TriggerWorkflowPaths: []string{gate},
	}

	failedOther := &persistence.ForgeCIOutcome{Conclusion: "failure", Number: 42, WorkflowPath: other}
	if d := Decide(failedOther, cfg, false); d.Enqueue || d.Comment {
		t.Errorf("got %+v — a failure on a non-gate workflow must record only", d)
	}

	greenOther := &persistence.ForgeCIOutcome{Conclusion: "success", Number: 42, WorkflowPath: other}
	if d := Decide(greenOther, cfg, false); d.Enqueue {
		t.Errorf("got %+v — this is the five-reviews-per-green-push case", d)
	}

	// The gate itself triggers, both ways.
	failedGate := &persistence.ForgeCIOutcome{Conclusion: "failure", Number: 42, WorkflowPath: gate}
	if d := Decide(failedGate, cfg, false); !d.Enqueue {
		t.Errorf("got %+v — the gate's failure must trigger the review", d)
	}
	greenGate := &persistence.ForgeCIOutcome{Conclusion: "success", Number: 42, WorkflowPath: gate}
	if d := Decide(greenGate, cfg, false); !d.Enqueue || d.WorkflowID != "github-review" {
		t.Errorf("got %+v — the gate's green run must fire the success workflow", d)
	}
}

// An empty trigger list leaves every RECORDED run a trigger, so a project that
// sets only workflow_paths behaves exactly as it did before §14.
func TestDecide_NoTriggerListMeansEveryRecordedRunTriggers(t *testing.T) {
	cfg := Config{ReviewOnFailure: true, WorkflowPaths: []string{gate, other}}
	out := &persistence.ForgeCIOutcome{Conclusion: "failure", Number: 42, WorkflowPath: other}
	if d := Decide(out, cfg, false); !d.Enqueue {
		t.Errorf("got %+v, want a review — an empty trigger list narrows nothing", d)
	}
}
