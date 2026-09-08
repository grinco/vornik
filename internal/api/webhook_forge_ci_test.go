package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// CI ingestion on the GENERIC ingress — the door this deployment actually uses
// (LLD 2026-09-08-forge-ci-outcomes-design.md §3.2).

type stubCIIngest struct {
	recorded []forgeci.Run
	out      *persistence.ForgeCIOutcome
	cfg      forgeci.Config
}

func (s *stubCIIngest) Record(_ context.Context, run forgeci.Run) *persistence.ForgeCIOutcome {
	s.recorded = append(s.recorded, run)
	return s.out
}
func (s *stubCIIngest) Cfg() forgeci.Config { return s.cfg }

func ciJob(conclusion string, number int) forge.ForgeJob {
	return forge.ForgeJob{
		Repo: "acme/infra", Number: number,
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
	done := s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d1", ciJob("success", 42))
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
	done := s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d2", ciJob("failure", 42))
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
	if s.applyForgeCIRules(context.Background(), w, &registry.Project{ID: "p"}, "d3", ciJob("failure", 42)) {
		t.Fatal("with ingestion off the delivery must continue down the normal path")
	}
}
