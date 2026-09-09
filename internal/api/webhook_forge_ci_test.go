package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// CI ingestion on the GENERIC ingress — the door this deployment actually uses
// (LLD 2026-09-08-forge-ci-outcomes-design.md §3.2).

type stubCIIngest struct {
	recorded        []forgeci.Run
	out             *persistence.ForgeCIOutcome
	cfg             forgeci.Config
	alreadyReviewed bool
	commented       []int64
	postRefused     bool
}

func (s *stubCIIngest) Record(_ context.Context, run forgeci.Run) *persistence.ForgeCIOutcome {
	s.recorded = append(s.recorded, run)
	return s.out
}
func (s *stubCIIngest) Cfg() forgeci.Config { return s.cfg }

func (s *stubCIIngest) AlreadyReviewed(context.Context, string, *persistence.ForgeCIOutcome) bool {
	return s.alreadyReviewed
}

// postRefused models a run whose comment claim was already taken — what a
// redelivery meets.
func (s *stubCIIngest) Comment(_ context.Context, out *persistence.ForgeCIOutcome) (bool, error) {
	s.commented = append(s.commented, out.RunID)
	if s.postRefused {
		return false, nil
	}
	return true, nil
}

// ciPR is the pull request every case in this file uses. A constant rather than
// a parameter: no case varies it, and what a case DOES vary — whether the
// stubbed outcome carries a PR at all — lives on the outcome, not here.
const ciPR = 42

func ciJob(conclusion string) forge.ForgeJob {
	return forge.ForgeJob{
		Repo: "acme/infra", Number: ciPR,
		CI: &forge.CIRef{RunID: 77, HeadSHA: "sha", Conclusion: conclusion},
	}
}

// A green run with no success workflow is recorded and handled — the delivery
// does not fall through and create a task.
func TestForgeCIRules_GreenRecordsAndStops(t *testing.T) {
	ing := &stubCIIngest{
		out: &persistence.ForgeCIOutcome{Conclusion: "success", Number: 42},
		cfg: forgeci.Config{ReviewOnFailure: true},
	}
	s := &Server{logger: zerolog.Nop(), forgeCI: func(string) ForgeCIIngest { return ing }}

	w := httptest.NewRecorder()
	done := s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d1", ciJob("success"))
	if !done {
		t.Fatal("a recorded, non-triggering run must be fully handled here")
	}
	if len(ing.recorded) != 1 || ing.recorded[0].RunID != 77 {
		t.Errorf("the run must be recorded: %+v", ing.recorded)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "ci_recorded" {
		t.Errorf("status = %q, want ci_recorded", body["status"])
	}
}

// A failure on a PR falls THROUGH so the normal path creates the review task.
func TestForgeCIRules_FailureFallsThroughToTaskCreation(t *testing.T) {
	ing := &stubCIIngest{
		out: &persistence.ForgeCIOutcome{Conclusion: "failure", Number: 42},
		cfg: forgeci.Config{ReviewOnFailure: true},
	}
	s := &Server{logger: zerolog.Nop(), forgeCI: func(string) ForgeCIIngest { return ing }}

	w := httptest.NewRecorder()
	done := s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d2", ciJob("failure"))
	if done {
		t.Fatal("a triggering run must fall through so the task is created")
	}
	if len(ing.recorded) != 1 {
		t.Errorf("it must still be recorded before the decision: %+v", ing.recorded)
	}
}

// Ingestion off for the project: the delivery continues down the normal path
// and records nothing.
func TestForgeCIRules_DisabledFallsThrough(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), forgeCI: func(string) ForgeCIIngest { return nil }}
	w := httptest.NewRecorder()
	if s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d3", ciJob("failure")) {
		t.Fatal("with ingestion off the delivery must continue down the normal path")
	}
}

// The response says what HAPPENED (design §13.6). A redelivery whose claim was
// refused published nothing, and must not answer as though it had — observed
// on headmatch 2026-09-09, where the redelivery correctly posted no second
// comment and still replied "ci_commented".
func TestForgeCIRules_CommentStatusReportsWhatHappened(t *testing.T) {
	newServer := func(refused bool) (*Server, *stubCIIngest) {
		ing := &stubCIIngest{
			out:             &persistence.ForgeCIOutcome{Conclusion: "failure", Number: 42, RunID: 77},
			cfg:             forgeci.Config{ReviewOnFailure: true, CommentOnFailure: true},
			alreadyReviewed: true,
			postRefused:     refused,
		}
		return &Server{logger: zerolog.Nop(), forgeCI: func(string) ForgeCIIngest { return ing }}, ing
	}
	statusOf := func(refused bool) string {
		s, _ := newServer(refused)
		w := httptest.NewRecorder()
		if !s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d4", ciJob("failure")) {
			t.Fatal("an already-reviewed failure must be handled here, not enqueued")
		}
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body["status"]
	}

	if got := statusOf(false); got != "ci_commented" {
		t.Errorf("first delivery status = %q, want ci_commented", got)
	}
	if got := statusOf(true); got != "ci_recorded" {
		t.Errorf("redelivery status = %q, want ci_recorded — it posted nothing", got)
	}
}

// THE REPAIR OF §14.4, ASSERTED ON THIS INGRESS: the generic relay path honours
// the workflow-path filters, because it goes through the REAL forgeci.
//
// It did not. `workflow_paths` was enforced only inside the GitHub App channel,
// so on this ingress — the one the deployment that found this actually uses —
// the key parsed and did nothing. A stub ingest cannot catch that: the stub
// would filter or not filter according to whatever the stub does. So this case
// wires a real *forgeci.Ingest and asserts through it.
func TestForgeCIRules_TheGenericIngressHonoursTheWorkflowFilters(t *testing.T) {
	const gate = ".github/workflows/typecheck.yml"
	const other = ".github/workflows/coverage.yml"

	run := func(cfg forgeci.Config, path string) (handled bool, recorded []string) {
		store := &pathRecordingOutcomes{}
		ing := forgeci.New(store, nil, cfg, zerolog.Nop())
		s := &Server{logger: zerolog.Nop(), forgeCI: func(string) ForgeCIIngest { return ing }}

		job := ciJob("failure")
		job.CI.WorkflowPath = path
		w := httptest.NewRecorder()
		handled = s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d5", job)
		return handled, store.paths
	}

	// Not ingested: recorded nothing, and the delivery is handled here rather
	// than falling through to create a review task.
	handled, recorded := run(forgeci.Config{ReviewOnFailure: true, WorkflowPaths: []string{gate}}, other)
	if !handled || len(recorded) != 0 {
		t.Errorf("an unwatched workflow: handled=%v recorded=%v — workflow_paths must apply on THIS ingress",
			handled, recorded)
	}

	// Ingested but not a gate: recorded, and still no review.
	handled, recorded = run(forgeci.Config{
		ReviewOnFailure: true, TriggerWorkflowPaths: []string{gate},
	}, other)
	if !handled || len(recorded) != 1 {
		t.Errorf("a watched non-gate workflow: handled=%v recorded=%v — it must be recorded and not trigger",
			handled, recorded)
	}

	// The gate falls THROUGH, so the normal path creates the review task.
	handled, recorded = run(forgeci.Config{
		ReviewOnFailure: true, TriggerWorkflowPaths: []string{gate},
	}, gate)
	if handled || len(recorded) != 1 {
		t.Errorf("the gate: handled=%v recorded=%v — it must be recorded AND trigger the review",
			handled, recorded)
	}
}

// pathRecordingOutcomes records which workflow paths reached the store.
type pathRecordingOutcomes struct{ paths []string }

func (r *pathRecordingOutcomes) Upsert(_ context.Context, o *persistence.ForgeCIOutcome) error {
	r.paths = append(r.paths, o.WorkflowPath)
	return nil
}
func (r *pathRecordingOutcomes) ListByHeadSHA(context.Context, string, string, string) ([]*persistence.ForgeCIOutcome, error) {
	return nil, nil
}
func (r *pathRecordingOutcomes) ClaimComment(context.Context, string, string, int64, time.Time) (bool, error) {
	return true, nil
}
func (r *pathRecordingOutcomes) ReleaseComment(context.Context, string, string, int64) error {
	return nil
}
func (r *pathRecordingOutcomes) PruneBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}
