package executor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

func mkExecWithState(t *testing.T, st executionState) *persistence.Execution {
	t.Helper()
	snap, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return &persistence.Execution{ID: "e1", StateSnapshot: snap}
}

// TestReattachInFlightContainer is the crash-recovery idempotency regression:
// the step handler adopts an in-flight container ONLY when there's a matching
// record AND the container still exists — every other case falls back to a
// fresh run (re-spawn), so normal execution is never altered.
func TestReattachInFlightContainer(t *testing.T) {
	e, rt, _, _, _ := setup()
	ctx := context.Background()

	// No in-flight record → run fresh.
	if _, _, ok := e.reattachInFlightContainer(ctx, mkExecWithState(t, executionState{}), "stepA"); ok {
		t.Fatal("no record must not re-attach")
	}

	// Record for a DIFFERENT step → run fresh.
	diff := executionState{InFlightStepID: "stepB", InFlightContainerID: "c1", InFlightTempRoot: "/tmp/r"}
	if _, _, ok := e.reattachInFlightContainer(ctx, mkExecWithState(t, diff), "stepA"); ok {
		t.Fatal("different-step record must not re-attach")
	}

	match := executionState{InFlightStepID: "stepA", InFlightContainerID: "c1", InFlightTempRoot: "/tmp/r"}

	// Matching record but the container is gone (InspectContainer → nil) →
	// run fresh (the temp root is gone too, e.g. host reboot).
	rt.inspectByID = nil
	if _, _, ok := e.reattachInFlightContainer(ctx, mkExecWithState(t, match), "stepA"); ok {
		t.Fatal("missing container must not re-attach")
	}

	// Matching record + container still exists → ADOPT it.
	rt.inspectByID = map[string]*runtime.Container{"c1": {Status: runtime.StatusRunning}}
	cid, outDir, ok := e.reattachInFlightContainer(ctx, mkExecWithState(t, match), "stepA")
	if !ok {
		t.Fatal("matching record + live container must re-attach")
	}
	if cid != "c1" {
		t.Errorf("containerID = %q, want c1", cid)
	}
	if outDir != filepath.Join("/tmp/r", "output") {
		t.Errorf("outputDir = %q, want /tmp/r/output", outDir)
	}
}

// TestMarkStepInFlight records the running container into the snapshot so a
// later reattach can find it.
func TestMarkStepInFlight(t *testing.T) {
	e, _, er, _, _ := setup()
	exec := &persistence.Execution{ID: "e1"}
	_ = er.Create(context.Background(), exec)

	e.markStepInFlight(context.Background(), exec, "stepA", "c1", "/tmp/r")

	st := loadExecutionState(exec)
	if st.InFlightStepID != "stepA" || st.InFlightContainerID != "c1" || st.InFlightTempRoot != "/tmp/r" {
		t.Fatalf("in-flight not recorded: %+v", st)
	}

	// Empty container ID is a no-op (nothing to re-attach to).
	exec2 := &persistence.Execution{ID: "e2"}
	_ = er.Create(context.Background(), exec2)
	e.markStepInFlight(context.Background(), exec2, "stepA", "", "/tmp/r")
	if len(exec2.StateSnapshot) != 0 {
		if st2 := loadExecutionState(exec2); st2.InFlightContainerID != "" {
			t.Errorf("empty container id must not record: %+v", st2)
		}
	}
}
