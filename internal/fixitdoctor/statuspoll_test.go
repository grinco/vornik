package fixitdoctor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
)

func TestPollResolvedStatus_FailedTask_ReadsTaskStatusNotReExec(t *testing.T) {
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted}
	tasks := &fakeTaskRepo{tasks: map[string]*persistence.Task{"t1": task}}
	a := &Assembler{Tasks: tasks}

	res, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "t1"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy=true for COMPLETED task, got %+v", res)
	}

	task.Status = persistence.TaskStatusFailed
	res, err = a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "t1"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if res.Healthy {
		t.Fatalf("expected healthy=false for FAILED task, got %+v", res)
	}
}

func TestPollResolvedStatus_FailedTask_MissingTaskErrors(t *testing.T) {
	tasks := &fakeTaskRepo{tasks: map[string]*persistence.Task{}}
	a := &Assembler{Tasks: tasks}
	if _, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindFailedTask, ID: "ghost"}); err == nil {
		t.Fatalf("expected error when the task is missing")
	}
}

func TestPollResolvedStatus_DegradedFeature_ReRunsDiagnose(t *testing.T) {
	verifyOK := true
	feature := featuredoctor.Feature{
		ID: "feat-1",
		Verify: func(_ context.Context, _ featuredoctor.Deps) featuredoctor.PrereqResult {
			return featuredoctor.PrereqResult{OK: verifyOK}
		},
	}
	a := &Assembler{Features: []featuredoctor.Feature{feature}, FeatureDeps: featuredoctor.Deps{Config: &fakeConfigReader{}}}

	res, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "feat-1"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy=true when verify passes, got %+v", res)
	}

	verifyOK = false
	res, err = a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "feat-1"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if res.Healthy {
		t.Fatalf("expected healthy=false when verify fails, got %+v", res)
	}
}

func TestPollResolvedStatus_DegradedFeature_UnknownFeatureErrors(t *testing.T) {
	a := &Assembler{Features: []featuredoctor.Feature{}}
	if _, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindDegradedFeature, ID: "nope"}); err == nil {
		t.Fatalf("expected error for unknown feature")
	}
}

func TestPollResolvedStatus_RedIntegration_ReadsLatestProbe(t *testing.T) {
	probes := &fakeIntegrationProbes{result: integrations.ProbeResult{Outcome: integrations.OutcomeOK}, ok: true}
	a := &Assembler{IntegrationProbes: probes}

	res, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "gh"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy=true for ok outcome, got %+v", res)
	}

	probes.result = integrations.ProbeResult{Outcome: integrations.OutcomeFail}
	res, err = a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "gh"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if res.Healthy {
		t.Fatalf("expected healthy=false for fail outcome, got %+v", res)
	}
}

func TestPollResolvedStatus_RedIntegration_NoProbeKnown(t *testing.T) {
	probes := &fakeIntegrationProbes{ok: false}
	a := &Assembler{IntegrationProbes: probes}
	res, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindRedIntegration, ID: "gh"})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if res.Healthy {
		t.Fatalf("expected healthy=false with no probe result known, got %+v", res)
	}
}

func TestPollResolvedStatus_FailedReload_ReadsReloadStatus(t *testing.T) {
	reload := &fakeReloadStatus{ok: true, rv: ReloadValidationError{Message: "bad config"}}
	a := &Assembler{ReloadStatus: reload}

	res, err := a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if res.Healthy {
		t.Fatalf("expected healthy=false while a reload error is on record, got %+v", res)
	}

	reload.ok = false
	res, err = a.PollResolvedStatus(context.Background(), FailureRef{Kind: FailureKindFailedReload})
	if err != nil {
		t.Fatalf("PollResolvedStatus: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy=true once the reload error clears, got %+v", res)
	}
}
