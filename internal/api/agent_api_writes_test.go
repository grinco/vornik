package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/persistence"
)

// agentWriteTaskRepo is a map-backed TaskRepository fake for the origin walk:
// it embeds the shared no-op mock and overrides only Get + List (the two
// methods walkOrigin + ResolveRequestRootsWithCompleteness touch).
type agentWriteTaskRepo struct {
	*mockTaskRepository
	byID map[string]*persistence.Task
}

func newAgentWriteTaskRepo(tasks ...*persistence.Task) *agentWriteTaskRepo {
	byID := make(map[string]*persistence.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	return &agentWriteTaskRepo{mockTaskRepository: &mockTaskRepository{}, byID: byID}
}

func (r *agentWriteTaskRepo) Get(_ context.Context, id string) (*persistence.Task, error) {
	return r.byID[id], nil // nil when absent — walkOrigin treats a nil task as a walk error
}

func (r *agentWriteTaskRepo) List(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
	out := make([]*persistence.Task, 0, len(f.IDs))
	for _, id := range f.IDs {
		if t := r.byID[id]; t != nil {
			out = append(out, t)
		}
	}
	return out, nil
}

func task(id, parent string, src persistence.TaskCreationSource) *persistence.Task {
	t := &persistence.Task{ID: id, CreationSource: src}
	if parent != "" {
		t.ParentTaskID = &parent
	}
	return t
}

// ---- resolveAgentWrite: the security-critical origin logic ---------------

func TestResolveAgentWrite_OffRefusesWithNoWalk(t *testing.T) {
	// off must never walk — a pure refuse, audited as not_walked so it is
	// distinguishable from "nothing attempted".
	repo := newAgentWriteTaskRepo(task("t1", "", persistence.TaskCreationSourceUser))
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "off", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "t1")
	assert.False(t, res.permit)
	assert.False(t, res.walked)
	assert.Equal(t, walkOutcomeNotWalked, res.walkOutcome)
	assert.Equal(t, creationSourceUnknown, res.creationSource)
	assert.Empty(t, res.rootTaskID)
}

func TestResolveAgentWrite_EmptyModeIsOff(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "", taskRepo: newAgentWriteTaskRepo()}
	res := srv.resolveAgentWrite(context.Background(), "t1")
	assert.Equal(t, agentWritesOff, res.mode)
	assert.False(t, res.permit)
}

func TestResolveAgentWrite_UserPermitsDirectUserRoot(t *testing.T) {
	repo := newAgentWriteTaskRepo(task("t1", "", persistence.TaskCreationSourceUser))
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "t1")
	assert.True(t, res.permit)
	assert.Equal(t, string(persistence.WalkOutcomeCleanRoot), res.walkOutcome)
	assert.Equal(t, "t1", res.rootTaskID)
	assert.Equal(t, "USER", res.creationSource)
}

// TestResolveAgentWrite_UserPermitsFdd2 — the incident this design fixes: a
// ROUTE child spawned by a USER-initiated chat request. The request-root is the
// USER task, so user mode PERMITS the child's write.
func TestResolveAgentWrite_UserPermitsFdd2(t *testing.T) {
	repo := newAgentWriteTaskRepo(
		task("usr-root", "", persistence.TaskCreationSourceUser),
		task("fdd2", "usr-root", persistence.TaskCreationSourceRoute),
	)
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "fdd2")
	assert.True(t, res.permit, "USER-rooted ROUTE child must be permitted under user mode")
	assert.Equal(t, "usr-root", res.rootTaskID)
	assert.Equal(t, "USER", res.creationSource)
}

func TestResolveAgentWrite_UserRefusesNonUserRoot(t *testing.T) {
	// An autonomous/route-rooted tree stays read-only under user.
	repo := newAgentWriteTaskRepo(
		task("auto-root", "", persistence.TaskCreationSourceAutonomous),
		task("c1", "auto-root", persistence.TaskCreationSourceRoute),
	)
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "c1")
	assert.False(t, res.permit)
	assert.Equal(t, string(persistence.WalkOutcomeCleanRoot), res.walkOutcome)
	assert.Equal(t, "AUTONOMOUS", res.creationSource)
}

// TestResolveAgentWrite_UserRefusesIncompleteLineage — the C1/C2 privilege-
// escalation guard: a deleted parent row ABOVE a USER intermediate must NOT
// grant on that intermediate. Incomplete lineage ⇒ refuse.
func TestResolveAgentWrite_UserRefusesIncompleteLineage(t *testing.T) {
	repo := newAgentWriteTaskRepo(
		// "gone" is referenced but absent; "mid" is USER but is NOT the root.
		task("mid", "gone", persistence.TaskCreationSourceUser),
		task("c1", "mid", persistence.TaskCreationSourceRoute),
	)
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "c1")
	assert.False(t, res.permit, "incomplete lineage above a USER intermediate must refuse")
	assert.Equal(t, string(persistence.WalkOutcomeMissingParent), res.walkOutcome)
	assert.Empty(t, res.rootTaskID)
	assert.Equal(t, creationSourceUnknown, res.creationSource)
}

func TestResolveAgentWrite_UserRefusesCycle(t *testing.T) {
	repo := newAgentWriteTaskRepo(
		task("a", "b", persistence.TaskCreationSourceUser),
		task("b", "a", persistence.TaskCreationSourceUser),
	)
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "a")
	assert.False(t, res.permit, "a cyclic lineage must refuse even if nodes are USER")
	assert.Equal(t, string(persistence.WalkOutcomeCycle), res.walkOutcome)
}

// TestResolveAgentWrite_UserRefusesDepthExhausted — completes the api-layer
// privilege-escalation guard across ALL four non-clean_root outcomes (review
// suggestion 1): a USER-rooted chain deeper than MaxRequestRootWalkDepth
// resolves depth_exhausted ⇒ refuse, even though the true root IS a USER task.
func TestResolveAgentWrite_UserRefusesDepthExhausted(t *testing.T) {
	// A linear chain of 30 tasks (> the depth bound of 25) rooted at a USER
	// task. The walk runs out of budget before reaching the root ⇒ incomplete.
	tasks := make([]*persistence.Task, 0, 31)
	tasks = append(tasks, task("root", "", persistence.TaskCreationSourceUser))
	for i := 1; i <= 30; i++ {
		id := "n" + strconv.Itoa(i)
		parent := "root"
		if i > 1 {
			parent = "n" + strconv.Itoa(i-1)
		}
		tasks = append(tasks, task(id, parent, persistence.TaskCreationSourceRoute))
	}
	repo := newAgentWriteTaskRepo(tasks...)
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "n30")
	assert.False(t, res.permit, "a USER-rooted tree deeper than the walk bound must refuse (fail-closed)")
	assert.Equal(t, string(persistence.WalkOutcomeDepthExhausted), res.walkOutcome)
	assert.Equal(t, creationSourceUnknown, res.creationSource)
}

func TestResolveAgentWrite_UserRefusesOnWalkError(t *testing.T) {
	// nil taskRepo ⇒ the walk can't run ⇒ error ⇒ fail closed under user.
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "user", taskRepo: nil}
	res := srv.resolveAgentWrite(context.Background(), "t1")
	assert.False(t, res.permit)
	assert.Equal(t, walkOutcomeError, res.walkOutcome)
}

// TestResolveAgentWrite_AllPermitsRegardlessOfWalk — all is non-blocking: an
// incomplete/errored walk still PERMITS (all trusts any origin); the walk only
// labels the audit row. This is the FOCUS #2 guarantee from the final review.
func TestResolveAgentWrite_AllPermitsRegardlessOfWalk(t *testing.T) {
	// Incomplete lineage (missing parent) under all.
	repo := newAgentWriteTaskRepo(task("c1", "gone", persistence.TaskCreationSourceRoute))
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "all", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "c1")
	assert.True(t, res.permit, "all must permit regardless of an incomplete walk")
	assert.True(t, res.walked, "all still walks for audit")
	assert.Equal(t, string(persistence.WalkOutcomeMissingParent), res.walkOutcome)
	assert.Empty(t, res.rootTaskID)

	// And a hard walk error (nil repo) under all also permits.
	srvErr := &Server{logger: zerolog.Nop(), agentWritesMode: "all", taskRepo: nil}
	resErr := srvErr.resolveAgentWrite(context.Background(), "c1")
	assert.True(t, resErr.permit, "all must permit even when the audit walk errors")
	assert.Equal(t, walkOutcomeError, resErr.walkOutcome)
}

func TestResolveAgentWrite_AllPopulatesRootOnCleanWalk(t *testing.T) {
	repo := newAgentWriteTaskRepo(
		task("auto-root", "", persistence.TaskCreationSourceAutonomous),
		task("c1", "auto-root", persistence.TaskCreationSourceRoute),
	)
	srv := &Server{logger: zerolog.Nop(), agentWritesMode: "all", taskRepo: repo}
	res := srv.resolveAgentWrite(context.Background(), "c1")
	assert.True(t, res.permit)
	// Correlatable post-hoc even though all permits unconditionally.
	assert.Equal(t, "auto-root", res.rootTaskID)
	assert.Equal(t, "AUTONOMOUS", res.creationSource)
}

// ---- recordAgentWrite: the observability counter -------------------------

func TestRecordAgentWrite_LabelsAndOutcome(t *testing.T) {
	m := NewAgentAPIWriteMetrics(prometheus.NewRegistry())
	srv := &Server{logger: zerolog.Nop(), agentWriteMetrics: m}

	srv.recordAgentWrite(writeResolution{mode: "user", creationSource: "USER"}, true)
	srv.recordAgentWrite(writeResolution{mode: "off", creationSource: ""}, false)

	assert.Equal(t, 1.0, testutil.ToFloat64(m.WritesTotal.WithLabelValues("user", "USER", "permitted")))
	// empty creation_source normalizes to unknown for the label.
	assert.Equal(t, 1.0, testutil.ToFloat64(m.WritesTotal.WithLabelValues("off", "unknown", "refused")))
}

func TestRecordAgentWrite_NilMetricsNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("recordAgentWrite panicked with nil metrics: %v", r)
		}
	}()
	srv := &Server{logger: zerolog.Nop(), agentWriteMetrics: nil}
	// Must be a no-op, not a panic, when metrics are unwired.
	srv.recordAgentWrite(writeResolution{mode: "all", creationSource: "USER"}, true)
}

// ---- handler: end-to-end wiring (audit fields + metric + gate) -----------

func TestAgentQueryAPI_OffWriteRefusedAuditsNotWalked(t *testing.T) {
	reg := loadAPIQueryTestRegistry(t, nil) // empty ⇒ all providers
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	audit := &stubAuditRepo{}
	metrics := NewAgentAPIWriteMetrics(prometheus.NewRegistry())
	repo := newAgentWriteTaskRepo(task(agentWriteTestTaskID, "", persistence.TaskCreationSourceUser))
	srv := &Server{
		logger: zerolog.Nop(), apiGatewayClient: gw, projectRegistry: reg,
		toolAuditRepo: audit, taskRepo: repo, agentWritesMode: "off", agentWriteMetrics: metrics,
	}
	req := agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query",
		`{"provider":"maps","method":"POST","path":"/write"}`, "proj")
	rec := httptest.NewRecorder()
	srv.AgentQueryAPI(rec, req)

	resp := decodeQueryResp(t, rec)
	assert.Contains(t, resp.Refusal, "read-only")
	assert.Empty(t, gw.calls, "off must refuse the write before the gateway")

	rows := audit.rows()
	require.Len(t, rows, 1)
	var p agentAPIAuditPayload
	require.NoError(t, json.Unmarshal([]byte(rows[0].ToolInput), &p))
	assert.Equal(t, "off", p.AgentWritesMode)
	assert.Equal(t, walkOutcomeNotWalked, p.WalkOutcome)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.WritesTotal.WithLabelValues("off", "unknown", "refused")))
}

func TestAgentQueryAPI_UserWritePermittedReachesGateway(t *testing.T) {
	reg := loadAPIQueryTestRegistry(t, nil)
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	audit := &stubAuditRepo{}
	metrics := NewAgentAPIWriteMetrics(prometheus.NewRegistry())
	// The task-scoped key binds to agentWriteTestTaskID; make that a
	// USER-rooted ROUTE child (the fdd2 shape).
	repo := newAgentWriteTaskRepo(
		task("usr-root", "", persistence.TaskCreationSourceUser),
		task(agentWriteTestTaskID, "usr-root", persistence.TaskCreationSourceRoute),
	)
	srv := &Server{
		logger: zerolog.Nop(), apiGatewayClient: gw, projectRegistry: reg,
		toolAuditRepo: audit, taskRepo: repo, agentWritesMode: "user", agentWriteMetrics: metrics,
	}
	req := agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query",
		`{"provider":"maps","method":"POST","path":"/write"}`, "proj")
	rec := httptest.NewRecorder()
	srv.AgentQueryAPI(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, gw.calls, 1, "USER-rooted write must reach the gateway under user mode")
	rows := audit.rows()
	require.Len(t, rows, 1)
	var p agentAPIAuditPayload
	require.NoError(t, json.Unmarshal([]byte(rows[0].ToolInput), &p))
	assert.Equal(t, "user", p.AgentWritesMode)
	assert.Equal(t, string(persistence.WalkOutcomeCleanRoot), p.WalkOutcome)
	assert.Equal(t, "usr-root", p.RootTaskID)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.WritesTotal.WithLabelValues("user", "USER", "permitted")))
}

func TestAgentQueryAPI_ReadNeverWalksOrMeters(t *testing.T) {
	reg := loadAPIQueryTestRegistry(t, nil)
	gw := &fakeQueryGateway{resp: apigateway.Response{Body: "ok"}}
	audit := &stubAuditRepo{}
	metrics := NewAgentAPIWriteMetrics(prometheus.NewRegistry())
	// A repo whose Get would panic if the read path ever walked it.
	srv := &Server{
		logger: zerolog.Nop(), apiGatewayClient: gw, projectRegistry: reg,
		toolAuditRepo: audit, taskRepo: newAgentWriteTaskRepo(), agentWritesMode: "user", agentWriteMetrics: metrics,
	}
	req := agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query",
		`{"provider":"maps","method":"GET","path":"/read"}`, "proj")
	rec := httptest.NewRecorder()
	srv.AgentQueryAPI(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, gw.calls, 1)
	rows := audit.rows()
	require.Len(t, rows, 1)
	var p agentAPIAuditPayload
	require.NoError(t, json.Unmarshal([]byte(rows[0].ToolInput), &p))
	// Reads carry no write-policy trail.
	assert.Empty(t, p.AgentWritesMode)
	assert.Empty(t, p.WalkOutcome)
}

// agentWriteTestTaskID is the task the agentTaskReq helper binds its key to (it
// hardcodes "t1"); the write-policy walk resolves origin from it.
const agentWriteTestTaskID = "t1"
