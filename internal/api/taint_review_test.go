package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/taintlineage"
)

// configurable step-outcome repo fake for the taint gate tests.
type taintStubOutcomeRepo struct {
	rows map[string][]persistence.TaintedStepRow
}

func (r *taintStubOutcomeRepo) TaintedStepsForTasks(_ context.Context, ids []string) ([]persistence.TaintedStepRow, error) {
	var out []persistence.TaintedStepRow
	for _, id := range ids {
		out = append(out, r.rows[id]...)
	}
	return out, nil
}
func (r *taintStubOutcomeRepo) Record(context.Context, *persistence.ExecutionStepOutcome) error {
	return nil
}
func (r *taintStubOutcomeRepo) FinalizePending(context.Context, string, string, string, string, string, *string) (string, string, error) {
	return "", "", nil
}
func (r *taintStubOutcomeRepo) SweepPending(context.Context, string, string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (r *taintStubOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, nil
}
func (r *taintStubOutcomeRepo) SupersedeAfter(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}
func (r *taintStubOutcomeRepo) CountByRoleModelOutcome(context.Context, string, time.Time, time.Time, string) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, nil
}
func (r *taintStubOutcomeRepo) StepLatencyP95ByStep(context.Context, time.Time) ([]persistence.StepLatencyStat, error) {
	return nil, nil
}

func taintHighRow() persistence.TaintedStepRow {
	return persistence.TaintedStepRow{
		TaskID: "t", RequiresReview: true,
		UntrustedSources: []byte(`[{"tool":"web_fetch","ref":"https://a.example","severity":2}]`),
	}
}

// cleanRootLister returns a MockTaskRepository whose List resolves taskID as a
// parentless (clean-root) task — a complete walk.
func cleanRootTaskRepo() *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			var out []*persistence.Task
			for _, id := range f.IDs {
				out = append(out, &persistence.Task{ID: id}) // nil parent → clean root
			}
			return out, nil
		},
	}
}

func taintServer(mode string, rows map[string][]persistence.TaintedStepRow, msgRepo persistence.TaskMessageRepository) *Server {
	return &Server{
		taintDefaultMode: mode,
		stepOutcomeRepo:  &taintStubOutcomeRepo{rows: rows},
		taskRepo:         cleanRootTaskRepo(),
		taskMessageRepo:  msgRepo,
	}
}

func TestResolveTaintReview_Off_NoQuery(t *testing.T) {
	srv := taintServer("off", map[string][]persistence.TaintedStepRow{"t": {taintHighRow()}}, &tcStubMessageRepo{})
	res := srv.resolveTaintReview(context.Background(), "p", "t")
	if res.mode != taintlineage.ModeOff || res.park || res.requiresReview {
		t.Fatalf("off must not park/flag: %+v", res)
	}
}

func TestResolveTaintReview_Enforce_UnwiredFailsClosed(t *testing.T) {
	srv := &Server{taintDefaultMode: string(taintlineage.ModeEnforce)}
	res := srv.resolveTaintReview(context.Background(), "p", "t")
	if res.mode != taintlineage.ModeEnforce || !res.park || !res.requiresReview || res.walkComplete {
		t.Fatalf("unwired enforce-mode gate must fail closed: %+v", res)
	}
}

func TestResolveTaintReview_Enforce_MissingTaskIDFailsClosed(t *testing.T) {
	srv := taintServer("enforce", nil, &tcStubMessageRepo{})
	res := srv.resolveTaintReview(context.Background(), "p", "")
	if !res.park || !res.requiresReview {
		t.Fatalf("missing task identity must not bypass enforce mode: %+v", res)
	}
}

func TestResolveTaintReview_Advisory_FlagNoPark(t *testing.T) {
	srv := taintServer("advisory", map[string][]persistence.TaintedStepRow{"t": {taintHighRow()}}, &tcStubMessageRepo{})
	res := srv.resolveTaintReview(context.Background(), "p", "t")
	if res.park {
		t.Fatalf("advisory must not park")
	}
	if !res.tainted || !res.requiresReview {
		t.Fatalf("advisory should flag High: %+v", res)
	}
}

func TestResolveTaintReview_Enforce_HighParks(t *testing.T) {
	srv := taintServer("enforce", map[string][]persistence.TaintedStepRow{"t": {taintHighRow()}}, &tcStubMessageRepo{})
	res := srv.resolveTaintReview(context.Background(), "p", "t")
	if !res.park || !res.requiresReview {
		t.Fatalf("enforce + High must park+requireReview: %+v", res)
	}
	if res.sourceSetHash == "" {
		t.Fatalf("expected a source-set hash")
	}
}

func TestResolveTaintReview_Enforce_MatchingLatch_NoPark(t *testing.T) {
	// Pre-compute the hash the reviewed set will produce.
	roll := taintlineage.Rollup([]taintlineage.StepTaint{
		taintlineage.StepTaintFromBlob(taintHighRow().UntrustedSources, true),
	}, nil, true)
	msgRepo := &tcStubMessageRepo{
		listFn: func(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
			return []*persistence.TaskMessage{{
				MessageKind: persistence.TaskMessageKindSystem,
				Metadata:    taintlineage.LatchMarkerMetadata(roll.SourceSetHash),
			}}, nil
		},
	}
	srv := taintServer("enforce", map[string][]persistence.TaintedStepRow{"t": {taintHighRow()}}, msgRepo)
	res := srv.resolveTaintReview(context.Background(), "p", "t")
	if res.park {
		t.Fatalf("matching latch (complete walk) must suppress the park (D7): %+v", res)
	}
}

func TestIsTaintReviewCheckpoint(t *testing.T) {
	if !IsTaintReviewCheckpoint(taintCheckpointMeta("abc")) {
		t.Fatalf("untrusted_review metadata must be recognized")
	}
	if IsTaintReviewCheckpoint(budgetCheckpointMsg("cp1").Metadata) {
		t.Fatalf("budget checkpoint must NOT be a taint checkpoint")
	}
}

// --- answer-path branch ---

func taintCheckpointMeta(hash string) []byte {
	meta, _ := json.Marshal(map[string]any{
		"kind": "decision",
		"decision": map[string]any{
			"kind":            "untrusted_review",
			"reason":          "tainted_write",
			"write_surface":   "forge",
			"source_set_hash": hash,
		},
		"question": "review the sources",
		"options": []map[string]any{
			{"id": "allow", "label": "Reviewed — resume & allow"},
			{"id": "cancel", "label": "Block (cancel)"},
		},
	})
	return meta
}

func taintCheckpointMsg(id, hash string) *persistence.TaskMessage {
	return &persistence.TaskMessage{ID: id, TaskID: tcTaskID, MessageKind: persistence.TaskMessageKindCheckpoint, Metadata: taintCheckpointMeta(hash)}
}

func taintTaskRepo(cp string, transFn func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error)) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			tk := tcTask(persistence.TaskStatusAwaitingInput)
			tk.OpenCheckpointID = &cp
			return tk, nil
		},
		TransitionConditionalFunc: transFn,
	}
}

// allow by a non-admin is refused (§9): no latch, no resolve.
func TestAnswerCheckpoint_TaintAllow_NonAdminForbidden(t *testing.T) {
	cp := "cp1"
	taskRepo := taintTaskRepo(cp, func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
		return true, nil
	})
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) {
		return taintCheckpointMsg(cp, "abc"), nil
	}}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"allow"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin allow must be 403, got %d (%s)", rec.Code, rec.Body.String())
	}
	if msgRepo.resolvedCheckpoint != "" {
		t.Fatalf("checkpoint must not resolve on a refused attempt")
	}
	for _, m := range msgRepo.inserts {
		if _, ok := taintlineage.ParseLatchHash(m.Metadata); ok {
			t.Fatalf("no latch may be recorded on a refused non-admin allow")
		}
	}
}

// admin allow records the latch AND resumes (QUEUED) AND resolves the checkpoint.
func TestAnswerCheckpoint_TaintAllow_AdminRecordsLatchAndResumes(t *testing.T) {
	cp := "cp1"
	var toStatus persistence.TaskStatus
	taskRepo := taintTaskRepo(cp, func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
		toStatus = to
		return true, nil
	})
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) {
		return taintCheckpointMsg(cp, "hash-xyz"), nil
	}}
	srv := tcServer(taskRepo, msgRepo)
	req := authOff(httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"allow"}`)))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin allow want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if toStatus != persistence.TaskStatusQueued {
		t.Fatalf("allow must re-queue (QUEUED), got %v", toStatus)
	}
	if msgRepo.resolvedCheckpoint != cp {
		t.Fatalf("allow must resolve the checkpoint, got %q", msgRepo.resolvedCheckpoint)
	}
	// A latch marker with the reviewed hash was recorded.
	found := false
	for _, m := range msgRepo.inserts {
		if h, ok := taintlineage.ParseLatchHash(m.Metadata); ok && h == "hash-xyz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("admin allow must record the D7 latch (source_set_hash=hash-xyz)")
	}
}

// cancel blocks the write: transition to CANCELLED, checkpoint resolved.
func TestAnswerCheckpoint_TaintCancel_Cancels(t *testing.T) {
	cp := "cp1"
	var toStatus persistence.TaskStatus
	taskRepo := taintTaskRepo(cp, func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
		toStatus = to
		return true, nil
	})
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) {
		return taintCheckpointMsg(cp, "abc"), nil
	}}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"cancel","content":"block it"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if toStatus != persistence.TaskStatusCancelled {
		t.Fatalf("cancel must transition to CANCELLED, got %v", toStatus)
	}
	if msgRepo.resolvedCheckpoint != cp {
		t.Fatalf("cancel must resolve the checkpoint")
	}
}

// I6 race: admin allow when the task already drifted (TransitionConditional
// loses the race → false) returns 409, not a dangling resume.
func TestAnswerCheckpoint_TaintAllow_DriftLosesRace(t *testing.T) {
	cp := "cp1"
	taskRepo := taintTaskRepo(cp, func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
		return false, nil // lost the race
	})
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		tk := tcTask(persistence.TaskStatusAwaitingInput)
		tk.OpenCheckpointID = &cp
		return tk, nil
	}
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) {
		return taintCheckpointMsg(cp, "abc"), nil
	}}
	srv := tcServer(taskRepo, msgRepo)
	req := authOff(httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{"choice":"allow"}`)))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("drift must be 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}
