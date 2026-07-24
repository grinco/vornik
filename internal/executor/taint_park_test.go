package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taintlineage"
)

// --- fakes scoped to the taint reviewer tests ---

type taintOutcomeRepo struct {
	rows map[string][]persistence.TaintedStepRow // taskID → rows
}

func (r *taintOutcomeRepo) TaintedStepsForTasks(_ context.Context, ids []string) ([]persistence.TaintedStepRow, error) {
	var out []persistence.TaintedStepRow
	for _, id := range ids {
		out = append(out, r.rows[id]...)
	}
	return out, nil
}
func (r *taintOutcomeRepo) Record(context.Context, *persistence.ExecutionStepOutcome) error {
	return nil
}
func (r *taintOutcomeRepo) FinalizePending(context.Context, string, string, string, string, string, *string) (string, string, error) {
	return "", "", nil
}
func (r *taintOutcomeRepo) SweepPending(context.Context, string, string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (r *taintOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, nil
}
func (r *taintOutcomeRepo) SupersedeAfter(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}
func (r *taintOutcomeRepo) CountByRoleModelOutcome(context.Context, string, time.Time, time.Time, string) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, nil
}
func (r *taintOutcomeRepo) StepLatencyP95ByStep(context.Context, time.Time) ([]persistence.StepLatencyStat, error) {
	return nil, nil
}

type taintLister struct{ byID map[string]*persistence.Task }

func (l *taintLister) List(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
	var out []*persistence.Task
	for _, id := range f.IDs {
		if t, ok := l.byID[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

type taintMsgRepo struct {
	latches []string // hashes to return as system latch markers
}

func (m *taintMsgRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	var out []*persistence.TaskMessage
	for _, h := range m.latches {
		out = append(out, &persistence.TaskMessage{
			MessageKind: persistence.TaskMessageKindSystem,
			Metadata:    taintlineage.LatchMarkerMetadata(h),
		})
	}
	return out, nil
}
func (m *taintMsgRepo) Insert(context.Context, *persistence.TaskMessage) error { return nil }
func (m *taintMsgRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, nil
}
func (m *taintMsgRepo) MarkCheckpointResolved(context.Context, string, string) error { return nil }

func highRow(taskID string) persistence.TaintedStepRow {
	return persistence.TaintedStepRow{
		TaskID:           taskID,
		RequiresReview:   true,
		UntrustedSources: []byte(`[{"tool":"web_fetch","ref":"https://a.example","severity":2}]`),
	}
}

func newReviewer(rows map[string][]persistence.TaintedStepRow, tasks map[string]*persistence.Task, latches []string, mode taintlineage.Mode) (*TaintReviewer, *[]string) {
	var recorded []string
	rec := func(_ taintlineage.Mode, outcome string) { recorded = append(recorded, outcome) }
	tr := NewTaintReviewer(
		&taintOutcomeRepo{rows: rows},
		&taintLister{byID: tasks},
		&taintMsgRepo{latches: latches},
		func(string) taintlineage.Mode { return mode },
		rec,
	)
	return tr, &recorded
}

func rootTask() *persistence.Task { return &persistence.Task{ID: "t"} }

func TestReviewForgeWrite_Off_NoSignal(t *testing.T) {
	tr, _ := newReviewer(map[string][]persistence.TaintedStepRow{"t": {highRow("t")}},
		map[string]*persistence.Task{"t": rootTask()}, nil, taintlineage.ModeOff)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig != nil {
		t.Fatalf("off must return (nil,nil), got sig=%v err=%v", sig, err)
	}
}

func TestReviewForgeWrite_Advisory_FlagNoPark(t *testing.T) {
	tr, recorded := newReviewer(map[string][]persistence.TaintedStepRow{"t": {highRow("t")}},
		map[string]*persistence.Task{"t": rootTask()}, nil, taintlineage.ModeAdvisory)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig != nil {
		t.Fatalf("advisory must not park, got sig=%v", sig)
	}
	if len(*recorded) != 1 || (*recorded)[0] != "flagged" {
		t.Fatalf("advisory should record flagged, got %v", *recorded)
	}
}

// M6: an untainted allowed write records the `permitted` outcome (the §14
// calibration denominator) — under both advisory and enforce.
func TestReviewForgeWrite_Advisory_Untainted_Permitted(t *testing.T) {
	tr, recorded := newReviewer(map[string][]persistence.TaintedStepRow{}, // no taint
		map[string]*persistence.Task{"t": rootTask()}, nil, taintlineage.ModeAdvisory)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig != nil {
		t.Fatalf("advisory untainted must proceed, got sig=%v", sig)
	}
	if len(*recorded) != 1 || (*recorded)[0] != "permitted" {
		t.Fatalf("advisory untainted must record permitted, got %v", *recorded)
	}
}

func TestReviewForgeWrite_Enforce_Untainted_Permitted(t *testing.T) {
	tr, recorded := newReviewer(map[string][]persistence.TaintedStepRow{}, // no taint, clean walk
		map[string]*persistence.Task{"t": rootTask()}, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig != nil {
		t.Fatalf("enforce untainted must proceed, got sig=%v", sig)
	}
	if len(*recorded) != 1 || (*recorded)[0] != "permitted" {
		t.Fatalf("enforce untainted must record permitted, got %v", *recorded)
	}
}

func TestReviewForgeWrite_Enforce_HighParks(t *testing.T) {
	tr, recorded := newReviewer(map[string][]persistence.TaintedStepRow{"t": {highRow("t")}},
		map[string]*persistence.Task{"t": rootTask()}, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("enforce + High must park, got sig=%v err=%v", sig, err)
	}
	if sig.SourceSetHash == "" || sig.SourceCount != 1 {
		t.Fatalf("signal payload wrong: %+v", sig)
	}
	if len(*recorded) != 1 || (*recorded)[0] != "parked" {
		t.Fatalf("enforce park should record parked, got %v", *recorded)
	}
}

func TestReviewForgeWrite_Enforce_IncompleteWalk_FailsClosed(t *testing.T) {
	// Writing task points at a missing parent → incomplete walk → park (D6),
	// even with NO tainted rows.
	tr, _ := newReviewer(map[string][]persistence.TaintedStepRow{},
		map[string]*persistence.Task{"t": {ID: "t", ParentTaskID: strptrExec("gone")}},
		nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig == nil {
		t.Fatalf("enforce + incomplete walk must park (D6), got sig=%v", sig)
	}
}

func TestReviewForgeWrite_Enforce_MatchingLatch_NoPark(t *testing.T) {
	// Resolve the hash first so the latch matches.
	roll := taintlineage.Rollup([]taintlineage.StepTaint{
		taintlineage.StepTaintFromBlob(highRow("t").UntrustedSources, true),
	}, nil, true)
	tr, _ := newReviewer(map[string][]persistence.TaintedStepRow{"t": {highRow("t")}},
		map[string]*persistence.Task{"t": rootTask()}, []string{roll.SourceSetHash}, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "t")
	if err != nil || sig != nil {
		t.Fatalf("matching latch (complete walk) must suppress the park (D7), got sig=%v", sig)
	}
}

func TestReviewForgeWrite_Enforce_AncestorTaint_Parks(t *testing.T) {
	// The writing task 'c' is clean, but its ancestor 'a' has a High row.
	tasks := map[string]*persistence.Task{
		"c": {ID: "c", ParentTaskID: strptrExec("a")},
		"a": {ID: "a"},
	}
	rows := map[string][]persistence.TaintedStepRow{"a": {highRow("a")}}
	tr, _ := newReviewer(rows, tasks, nil, taintlineage.ModeEnforce)
	sig, err := tr.ReviewForgeWrite(context.Background(), "p", "c")
	if err != nil || sig == nil {
		t.Fatalf("ancestor taint must park the descendant's write, got sig=%v", sig)
	}
}

func TestAsTaintReview_RoundTrip(t *testing.T) {
	sig := &TaintReviewSignal{State: TaintReviewState, SourceSetHash: "abc", SourceCount: 3, ShownCount: 2}
	blob, _ := json.Marshal(sig)
	got, ok := AsTaintReview(blob)
	if !ok || got.SourceSetHash != "abc" || got.SourceCount != 3 {
		t.Fatalf("round-trip failed: %+v ok=%v", got, ok)
	}
	if _, ok := AsTaintReview([]byte(`{"state":"other"}`)); ok {
		t.Fatalf("non-taint result must not parse as taint review")
	}
}

func TestParkForTaintReview_WritesCheckpointAndTransitions(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := &Executor{
		taskMessageRepo: msgs,
		persistTaskRepo: tr,
		logger:          zerolog.Nop(),
	}
	task := &persistence.Task{ID: "task_z", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec_z", TaskID: task.ID}
	sig := &TaintReviewSignal{State: TaintReviewState, SourceSetHash: "deadbeef", SourceCount: 5, ShownCount: 3}

	if err := e.parkForTaintReview(context.Background(), task, exec, sig); err != nil {
		t.Fatalf("parkForTaintReview err=%v", err)
	}
	// A checkpoint task_message with the untrusted_review decision kind.
	if len(msgs.inserted) != 1 {
		t.Fatalf("want 1 checkpoint message, got %d", len(msgs.inserted))
	}
	if !IsTaintReviewCheckpointMetaLocal(msgs.inserted[0].Metadata) {
		t.Fatalf("checkpoint metadata is not an untrusted_review decision: %s", msgs.inserted[0].Metadata)
	}
	if h := taintlineage.CheckpointSourceSetHash(msgs.inserted[0].Metadata); h != "deadbeef" {
		t.Fatalf("checkpoint source_set_hash = %q, want deadbeef", h)
	}
	// Exactly one RUNNING/LEASED → AWAITING_INPUT transition.
	if len(tr.calls) != 1 || tr.calls[0].to != persistence.TaskStatusAwaitingInput {
		t.Fatalf("want 1 transition to AWAITING_INPUT, got %+v", tr.calls)
	}
}

// IsTaintReviewCheckpointMetaLocal wraps the shared metadata check (avoids
// importing internal/api into the executor test).
func IsTaintReviewCheckpointMetaLocal(meta []byte) bool {
	return taintlineage.IsTaintReviewCheckpointMeta(meta)
}

func strptrExec(s string) *string { return &s }
