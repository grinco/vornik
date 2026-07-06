package projectdoctor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/registry"
)

type fakeResolver struct {
	proj *registry.Project
	err  error
}

func (f fakeResolver) ResolveProjectConfig(_ string) (*registry.Project, *registry.Swarm, *registry.Workflow, error) {
	return f.proj, nil, nil, f.err
}

type fakeSmoke struct {
	last    SmokeStatus
	hasLast bool
	fired   string
}

func (f *fakeSmoke) Trigger(_ context.Context, _ string, prompt string) (string, error) {
	f.fired = prompt
	return "task_1", nil
}
func (f *fakeSmoke) Latest(_ string) (SmokeStatus, bool) { return f.last, f.hasLast }

func TestRun_AssemblesSixChecksAndCompleteness(t *testing.T) {
	proj := &registry.Project{
		ID:          "p",
		Permissions: registry.ProjectPermissions{Secrets: []string{"TOK"}},
		Autonomy:    registry.ProjectAutonomy{Enabled: true, Mode: "llm", PollInterval: "4h", Goal: "g"},
	}
	d := New(Deps{
		Registry: fakeResolver{proj: proj},
		Secrets:  fakeSecrets{"TOK": true},
		Model:    fakePinger{},
		MCP:      fakeSnap{},
		Smoke:    &fakeSmoke{},
	})
	rep := d.Run(context.Background(), "p")
	if len(rep.Checks) != 6 {
		t.Fatalf("want 6 checks, got %d", len(rep.Checks))
	}
	// canonical order
	wantOrder := []string{"config_valid", "secrets", "model", "mcp", "schedule", "smoke"}
	for i, w := range wantOrder {
		if rep.Checks[i].Key != w {
			t.Fatalf("check %d = %q, want %q", i, rep.Checks[i].Key, w)
		}
	}
	if !rep.Complete {
		t.Fatalf("all-green project must be complete: %+v", rep)
	}
}

func TestRun_UnresolvedProjectIsIncomplete(t *testing.T) {
	d := New(Deps{Registry: fakeResolver{err: errFake("boom")}})
	rep := d.Run(context.Background(), "p")
	if rep.Complete {
		t.Fatal("unresolved project must be incomplete")
	}
	if rep.Checks[0].Key != "config_valid" || rep.Checks[0].Status != StatusRed {
		t.Fatalf("config_valid must be red: %+v", rep.Checks[0])
	}
}

func TestTriggerSmoke_UsesGoalThenFallback(t *testing.T) {
	fs := &fakeSmoke{}
	withGoal := &registry.Project{ID: "p", Autonomy: registry.ProjectAutonomy{Goal: "track pricing"}}
	d := New(Deps{Registry: fakeResolver{proj: withGoal}, Smoke: fs})
	if _, err := d.TriggerSmoke(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if fs.fired != "track pricing" {
		t.Fatalf("smoke prompt = %q, want the autonomy goal", fs.fired)
	}
	noGoal := &registry.Project{ID: "p"}
	d = New(Deps{Registry: fakeResolver{proj: noGoal}, Smoke: fs})
	if _, err := d.TriggerSmoke(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if fs.fired != "Reply with exactly: OK" {
		t.Fatalf("smoke fallback prompt = %q", fs.fired)
	}
}

func TestTriggerSmoke_InFlightGuardShortCircuits(t *testing.T) {
	fs := &fakeSmoke{last: SmokeStatus{TaskID: "task_existing", Running: true}, hasLast: true}
	proj := &registry.Project{ID: "p"}
	d := New(Deps{Registry: fakeResolver{proj: proj}, Smoke: fs})
	taskID, err := d.TriggerSmoke(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if taskID != "task_existing" {
		t.Fatalf("taskID = %q, want the in-flight task id", taskID)
	}
	if fs.fired != "" {
		t.Fatalf("Trigger must not be called while a smoke run is in flight, got prompt %q", fs.fired)
	}
}

func TestTriggerSmoke_NotRunningStillTriggers(t *testing.T) {
	fs := &fakeSmoke{last: SmokeStatus{TaskID: "task_old", Running: false}, hasLast: true}
	proj := &registry.Project{ID: "p", Autonomy: registry.ProjectAutonomy{Goal: "g"}}
	d := New(Deps{Registry: fakeResolver{proj: proj}, Smoke: fs})
	taskID, err := d.TriggerSmoke(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if taskID != "task_1" || fs.fired != "g" {
		t.Fatalf("expected a new trigger, got taskID=%q fired=%q", taskID, fs.fired)
	}
}

func TestTriggerSmoke_NoPriorRunTriggers(t *testing.T) {
	fs := &fakeSmoke{hasLast: false}
	proj := &registry.Project{ID: "p", Autonomy: registry.ProjectAutonomy{Goal: "g"}}
	d := New(Deps{Registry: fakeResolver{proj: proj}, Smoke: fs})
	taskID, err := d.TriggerSmoke(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if taskID != "task_1" || fs.fired != "g" {
		t.Fatalf("expected a new trigger, got taskID=%q fired=%q", taskID, fs.fired)
	}
}

// recordingSecretWriter records every Set call as "name=value" so a
// test can assert exactly how many times (and with what arguments)
// the writer was invoked.
type recordingSecretWriter struct {
	calls []string
}

func (w *recordingSecretWriter) Set(name, value string) error {
	w.calls = append(w.calls, name+"="+value)
	return nil
}

// TestSetSecret_RejectsUndeclaredName is the regression for the
// companion review finding (2026-07-04): SetSecret used to write any
// name unconditionally, so setting a name the project doesn't declare
// in Permissions.Secrets was a silent no-op from the operator's point
// of view — checkSecrets only iterates declared names, so the value
// never surfaces anywhere. A declared name must still reach the
// writer; an undeclared name must be rejected before the writer is
// ever called.
func TestSetSecret_RejectsUndeclaredName(t *testing.T) {
	proj := &registry.Project{ID: "p", Permissions: registry.ProjectPermissions{Secrets: []string{"TOK"}}}
	w := &recordingSecretWriter{}
	d := New(Deps{Registry: fakeResolver{proj: proj}, SecretWriter: w})

	if err := d.SetSecret("p", "TOK", "shh"); err != nil {
		t.Fatalf("declared secret must be accepted: %v", err)
	}
	if len(w.calls) != 1 || w.calls[0] != "TOK=shh" {
		t.Fatalf("writer calls = %v, want exactly one Set(TOK, shh)", w.calls)
	}

	if err := d.SetSecret("p", "EVIL", "x"); err == nil {
		t.Fatal("undeclared secret name must be rejected")
	}
	if len(w.calls) != 1 {
		t.Fatalf("writer must not be called for an undeclared secret name, calls=%v", w.calls)
	}
}

func TestQuickStatus_SkipsNetworkChecks(t *testing.T) {
	proj := &registry.Project{ID: "p"} // no secrets, autonomy off, resolves clean
	// Nil Model/MCP would surface in a full Run (Model → non-blocking
	// neutral, MCP → its own degraded state), but QuickStatus skips
	// both network checks and must still report complete.
	d := New(Deps{Registry: fakeResolver{proj: proj}, Secrets: fakeSecrets{}})
	if !d.QuickStatus("p") {
		t.Fatal("clean no-secrets autonomy-off project must be quick-complete")
	}
}
