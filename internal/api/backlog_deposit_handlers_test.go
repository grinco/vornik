package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/backlogfile"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// backlogDepositTestFixture bundles everything a backlog-deposit test
// needs: a registry with one project ("project-1"), a workspace root
// with that project's directory pre-created (backlogfile.Store.Append
// needs the parent dir to exist), and the wired *Server.
type backlogDepositTestFixture struct {
	server        *Server
	workspaceRoot string
}

// newBacklogDepositFixture builds a registry containing a single
// project with the given autonomy.backlogFilePath (empty -> default
// BACKLOG.md), wires a fresh backlogfile.Store + a real secrets
// detector (so the Block-mode path is genuinely exercised) and the
// task/execution repos the caller supplies.
func newBacklogDepositFixture(t *testing.T, backlogFilePath string, taskRepo persistence.TaskRepository, execRepo persistence.ExecutionRepository) *backlogDepositTestFixture {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	autonomyYAML := ""
	if backlogFilePath != "" {
		autonomyYAML = fmt.Sprintf("autonomy:\n  backlogFilePath: %q\n", backlogFilePath)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project.yaml"), []byte(fmt.Sprintf(`
projectId: project-1
displayName: Project
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 42
%s`, autonomyYAML)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))

	workspaceRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceRoot, "project-1"), 0o755))

	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)

	opts := []ServerOption{
		WithLogger(zerolog.Nop()),
		WithProjectRegistry(reg),
		WithConfig(&config.Config{Runtime: config.RuntimeConfig{ProjectWorkspacePath: workspaceRoot}}),
		WithBacklogStore(backlogfile.NewStore()),
		WithSecrets(det, nil),
	}
	if taskRepo != nil {
		opts = append(opts, WithTaskRepository(taskRepo))
	}
	if execRepo != nil {
		opts = append(opts, WithExecutionRepository(execRepo))
	}
	server := NewServer(opts...)
	return &backlogDepositTestFixture{server: server, workspaceRoot: workspaceRoot}
}

func (f *backlogDepositTestFixture) backlogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(f.workspaceRoot, "project-1", "BACKLOG.md")
}

func (f *backlogDepositTestFixture) readBacklog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.backlogPath(t))
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

type backlogDepositReq struct {
	ProjectID   string `json:"project_id"`
	TaskID      string `json:"task_id"`
	ExecutionID string `json:"execution_id,omitempty"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Evidence    string `json:"evidence"`
	Regression  bool   `json:"regression"`
}

func postBacklogDeposit(t *testing.T, server *Server, req backlogDepositReq, ctxFn func(context.Context) context.Context) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/internal/backlog-deposit", bytes.NewReader(body))
	if ctxFn != nil {
		httpReq = httpReq.WithContext(ctxFn(httpReq.Context()))
	}
	rec := httptest.NewRecorder()
	server.BacklogDeposit(rec, httpReq)
	return rec
}

func decodeBacklogDepositResponse(t *testing.T, rec *httptest.ResponseRecorder) backlogDepositResponse {
	t.Helper()
	var resp backlogDepositResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func baseDepositReq(taskID, title string) backlogDepositReq {
	return backlogDepositReq{
		ProjectID: "project-1",
		TaskID:    taskID,
		Kind:      "bug",
		Title:     title,
		Detail:    "some detail describing the issue",
		Evidence:  "log line 42",
	}
}

// --- Step 1: happy path + file creation -------------------------------

func TestBacklogDeposit_AcceptsAndCreatesFileWithHeader(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "Fix the widget cache"), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
	require.Contains(t, resp.Item, "**[bug]** Fix the widget cache")
	require.Contains(t, resp.Item, "(evidence: log line 42; via task-1,")

	content := fx.readBacklog(t)
	require.Contains(t, content, backlogfile.FormatHeader)
	require.Contains(t, content, "- [?] "+resp.Item)
}

// --- Step 1: second identical deposit -> duplicate ---------------------

func TestBacklogDeposit_SecondIdenticalDeposit_Duplicate(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	rec1 := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "Fix the widget cache"), nil)
	require.Equal(t, "accepted", decodeBacklogDepositResponse(t, rec1).Status)

	rec2 := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "Fix The Widget Cache!!"), nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	resp2 := decodeBacklogDepositResponse(t, rec2)
	require.Equal(t, "rejected", resp2.Status)
	require.Equal(t, "duplicate", resp2.Reason)
}

// --- Step 1: same text, different kind, similarity >= 0.95 -> duplicate

func TestBacklogDeposit_SameTextDifferentKindHighSimilarity_Duplicate(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req1 := baseDepositReq("task-1", "connection pool leaks memory under load")
	req1.Kind = "bug"
	rec1 := postBacklogDeposit(t, fx.server, req1, nil)
	require.Equal(t, "accepted", decodeBacklogDepositResponse(t, rec1).Status)

	req2 := baseDepositReq("task-1", "connection pool leaks memory under load")
	req2.Kind = "refactor"
	rec2 := postBacklogDeposit(t, fx.server, req2, nil)
	resp2 := decodeBacklogDepositResponse(t, rec2)
	require.Equal(t, "rejected", resp2.Status)
	require.Equal(t, "duplicate", resp2.Reason)
}

// --- Step 1: per-task cap at 10 -----------------------------------------

func TestBacklogDeposit_PerTaskCap(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	for i := 0; i < 10; i++ {
		req := baseDepositReq("task-1", fmt.Sprintf("distinct issue number %d about topic %d", i, i*7+3))
		rec := postBacklogDeposit(t, fx.server, req, nil)
		resp := decodeBacklogDepositResponse(t, rec)
		require.Equalf(t, "accepted", resp.Status, "deposit %d should be accepted, got reason=%q", i, resp.Reason)
	}
	// 11th deposit for the same task, unique title, hits the cap.
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "yet another unrelated issue about topic Z"), nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "rejected", resp.Status)
	require.Equal(t, "cap", resp.Reason)

	// A different task is unaffected by task-1's cap.
	rec2 := postBacklogDeposit(t, fx.server, baseDepositReq("task-2", "a completely separate matter"), nil)
	resp2 := decodeBacklogDepositResponse(t, rec2)
	require.Equal(t, "accepted", resp2.Status)
}

// --- Step 1: secret in rendered line -> rejected/secret -----------------

func TestBacklogDeposit_SecretInDetail_Rejected(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "leaked credential in log output")
	req.Detail = "found sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR in the output"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "rejected", resp.Status)
	require.Equal(t, "secret", resp.Reason)
	require.Empty(t, fx.readBacklog(t))
}

// --- Step 1: field validation ---------------------------------------------

func TestBacklogDeposit_FieldValidation(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)

	cases := []struct {
		name string
		mut  func(*backlogDepositReq)
	}{
		{"empty title", func(r *backlogDepositReq) { r.Title = "" }},
		{"title too long", func(r *backlogDepositReq) { r.Title = string(make([]byte, 141)) }},
		{"detail too long", func(r *backlogDepositReq) { r.Detail = string(make([]byte, 2001)) }},
		{"evidence too long", func(r *backlogDepositReq) { r.Evidence = string(make([]byte, 501)) }},
		{"unknown kind", func(r *backlogDepositReq) { r.Kind = "feature" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseDepositReq("task-1", "some title")
			tc.mut(&req)
			rec := postBacklogDeposit(t, fx.server, req, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// --- Step 1: guard stack --------------------------------------------------

func TestBacklogDeposit_TaskScopedKeyMismatch_Forbidden(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-Y", "some title"), func(ctx context.Context) context.Context {
		return taskScopedKeyCtx(ctx, "task-X", "project-1")
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestBacklogDeposit_CrossProjectKey_Forbidden(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "some title")
	rec := postBacklogDeposit(t, fx.server, req, func(ctx context.Context) context.Context {
		return context.WithValue(ctx, projectIDKey, []string{"other-project"})
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestBacklogDeposit_UnknownProject_NotFound(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "some title")
	req.ProjectID = "does-not-exist"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestBacklogDeposit_TaskFromOtherProject_Forbidden(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "other-project"}, nil
		},
	}
	fx := newBacklogDepositFixture(t, "", taskRepo, nil)
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestBacklogDeposit_ExecutionFromOtherTask_Forbidden(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, TaskID: "task-other", ProjectID: "project-1"}, nil
		},
	}
	fx := newBacklogDepositFixture(t, "", nil, execRepo)
	req := baseDepositReq("task-1", "some title")
	req.ExecutionID = "exec-other"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// --- Step 1: traversal backlogFilePath -> 400 ----------------------------

func TestBacklogDeposit_TraversalBacklogFilePath_BadRequest(t *testing.T) {
	fx := newBacklogDepositFixture(t, "../../etc/evil.md", nil, nil)
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Step 1: regression / cooldown flow ----------------------------------

// TestBacklogDeposit_ClosedItemWithoutRegressionFlag_RegressionRequired
// seeds a [x] item matching the deposit's (normalized) title, then
// deposits the same issue without regression:true.
func TestBacklogDeposit_ClosedItemWithoutRegressionFlag_RegressionRequired(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	seedBacklogFile(t, fx.backlogPath(t), []string{
		fmt.Sprintf("- [x] **[bug]** flaky retry loop — root cause was a missing jitter (via task-0, %s)", oldDate()),
	})

	req := baseDepositReq("task-1", "flaky retry loop")
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "rejected", resp.Status)
	require.Equal(t, "regression_required", resp.Reason)
}

// TestBacklogDeposit_ClosedItemWithRegressionRecent_Cooldown seeds a
// [x] item dated within the 7-day cooldown window; a regression:true
// re-deposit is still rejected as cooldown.
func TestBacklogDeposit_ClosedItemWithRegressionRecent_Cooldown(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	recentDate := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	seedBacklogFile(t, fx.backlogPath(t), []string{
		fmt.Sprintf("- [x] **[bug]** flaky retry loop — root cause was a missing jitter (via task-0, %s)", recentDate),
	})

	req := baseDepositReq("task-1", "flaky retry loop")
	req.Regression = true
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "rejected", resp.Status)
	require.Equal(t, "cooldown", resp.Reason)
}

// TestBacklogDeposit_ClosedItemWithRegressionOld_Accepted seeds a [x]
// item older than 7 days; a regression:true re-deposit is accepted.
func TestBacklogDeposit_ClosedItemWithRegressionOld_Accepted(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	seedBacklogFile(t, fx.backlogPath(t), []string{
		fmt.Sprintf("- [x] **[bug]** flaky retry loop — root cause was a missing jitter (via task-0, %s)", oldDate()),
	})

	req := baseDepositReq("task-1", "flaky retry loop")
	req.Regression = true
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
}

// oldDate returns a trailer date well outside the 7-day cooldown window.
func oldDate() string {
	return time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
}

// seedBacklogFile writes a BACKLOG.md with the given item lines
// (verbatim, already including their "- [marker] " prefix) so dedup
// tests can pre-populate state without going through the endpoint.
func seedBacklogFile(t *testing.T, path string, lines []string) {
	t.Helper()
	content := backlogfile.FormatHeader + "\n\n"
	for _, l := range lines {
		content += l + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// --- Body-level validation -------------------------------------------

func TestBacklogDeposit_MalformedJSON_BadRequest(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/backlog-deposit", bytes.NewBufferString("{not json"))
	rec := httptest.NewRecorder()
	fx.server.BacklogDeposit(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestBacklogDeposit_OversizedBody_BadRequest(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	body := bytes.Repeat([]byte("x"), (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/backlog-deposit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	fx.server.BacklogDeposit(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestBacklogDeposit_MissingProjectOrTaskID_BadRequest(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("", "some title")
	req.ProjectID = ""
	rec := postBacklogDeposit(t, fx.server, req, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestBacklogDeposit_WrongMethod_MethodNotAllowed(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal/backlog-deposit", nil)
	rec := httptest.NewRecorder()
	fx.server.BacklogDeposit(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// --- Symlinked project workspace escapes root -> 400 -----------------

func TestBacklogDeposit_SymlinkedWorkspaceEscapesRoot_BadRequest(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	// Replace the project-1 workspace dir with a symlink pointing outside
	// workspaceRoot — safepath.JoinUnder must resolve it and reject.
	projectDir := filepath.Join(fx.workspaceRoot, "project-1")
	require.NoError(t, os.RemoveAll(projectDir))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, projectDir))

	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Empty evidence renders without the evidence segment ---------------

func TestBacklogDeposit_EmptyEvidence_OmitsEvidenceSegment(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "a title with no evidence")
	req.Evidence = ""
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
	require.NotContains(t, resp.Item, "evidence:")
	require.Regexp(t, `\(via task-1, \d{4}-\d{2}-\d{2}\)$`, resp.Item)
}

// --- parseBacklogItemDate: unparseable-despite-matching-shape date -----

func TestParseBacklogItemDate_InvalidCalendarDate_TreatedAsUnparseable(t *testing.T) {
	_, ok := parseBacklogItemDate("**[bug]** x — y (via task-1, 2026-13-40)")
	require.False(t, ok)
}

func TestParseBacklogItemDate_NoTrailer_Unparseable(t *testing.T) {
	_, ok := parseBacklogItemDate("**[bug]** x — y (task: task-9)")
	require.False(t, ok)
}

// TestBacklogDeposit_ClosedItemWithRegressionNoDateTrailer_Accepted covers
// the "absent trailer date on a closed match treated as old" fallback: a
// legacy/operator-authored closed item with no "via ..., date)" trailer at
// all must not permanently block a legitimate regression re-deposit.
func TestBacklogDeposit_ClosedItemWithRegressionNoDateTrailer_Accepted(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	seedBacklogFile(t, fx.backlogPath(t), []string{
		"- [x] **[bug]** flaky retry loop — fixed by an operator by hand (task: task-0)",
	})

	req := baseDepositReq("task-1", "flaky retry loop")
	req.Regression = true
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
}

// --- Not-configured 503s ---------------------------------------------

func TestBacklogDeposit_NoProjectRegistry_ServiceUnavailable(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	rec := postBacklogDeposit(t, server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestBacklogDeposit_NoBacklogStore_ServiceUnavailable(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	fx.server.backlogStore = nil
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestBacklogDeposit_NoWorkspacePath_ServiceUnavailable(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	fx.server.config = &config.Config{}
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// --- Secret scan: Redact and Detect actions ------------------------------

func TestBacklogDeposit_SecretRedactAction_AcceptsWithRedactedLine(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	fx.server.secretsActions = map[string]secrets.Action{secrets.CheckpointBacklogDeposit: secrets.ActionRedact}

	req := baseDepositReq("task-1", "leaked credential in log output")
	req.Detail = "found sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR in the output"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
	require.NotContains(t, resp.Item, "sk-ant-api03")
	require.Contains(t, resp.Item, "[REDACTED:")
}

func TestBacklogDeposit_SecretDetectAction_AcceptsWithRawLine(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	fx.server.secretsActions = map[string]secrets.Action{secrets.CheckpointBacklogDeposit: secrets.ActionDetect}

	req := baseDepositReq("task-1", "leaked credential in log output")
	req.Detail = "found sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR in the output"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
	require.Contains(t, resp.Item, "sk-ant-api03")
}

func TestBacklogDeposit_NoSecretsDetector_Accepted(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	fx.server.secretsDetector = nil
	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
}

// --- Metric recording -----------------------------------------------------

func TestBacklogDeposit_RecordsMetric(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	fx.server.apiMetrics = NewAPIMetrics(prometheus.NewRegistry())

	rec := postBacklogDeposit(t, fx.server, baseDepositReq("task-1", "some title"), nil)
	require.Equal(t, "accepted", decodeBacklogDepositResponse(t, rec).Status)

	got := testutil.ToFloat64(fx.server.apiMetrics.BacklogDepositsTotal.WithLabelValues("project-1", "accepted"))
	require.Equal(t, float64(1), got)
}

// --- Detail truncation ----------------------------------------------------

func TestBacklogDeposit_DetailTruncatedTo600Chars(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	longDetail := ""
	for i := 0; i < 700; i++ {
		longDetail += "x"
	}
	req := baseDepositReq("task-1", "a title about a long detail")
	req.Detail = longDetail
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
	require.Contains(t, resp.Item, longDetail[:600])
	require.NotContains(t, resp.Item, longDetail[:601])
}

// TestBacklogDeposit_DetailTruncation_RuneSafe covers Minor-4: a naive
// detail[:600] byte-slice can split a multi-byte rune (e.g. a 3-byte
// UTF-8 character straddling the cut), producing an invalid UTF-8
// rendered line. 599 ASCII chars + one multi-byte rune means the raw
// byte length is 602 and the rune boundary falls inside the cut point
// under the old byte-slicing behaviour. Asserted against the persisted
// BACKLOG.md content (written directly by backlogfile.Store.Append) —
// not resp.Item, since json.Marshal silently substitutes U+FFFD for
// invalid UTF-8 and would mask the bug on the HTTP response path.
func TestBacklogDeposit_DetailTruncation_RuneSafe(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	ascii := ""
	for i := 0; i < 599; i++ {
		ascii += "x"
	}
	// "€" is 3 bytes in UTF-8; placed right at the truncation boundary.
	longDetail := ascii + "€" + "more trailing detail text to pad past the cap so truncation definitely occurs here"
	req := baseDepositReq("task-1", "a title about rune safe truncation")
	req.Detail = longDetail
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)

	content := fx.readBacklog(t)
	require.True(t, utf8.ValidString(content), "persisted BACKLOG.md content must be valid UTF-8, got %q", content)
}

// --- Title flatten+validate -------------------------------------------

// TestBacklogDeposit_TitleWithNewline_FlattenedAndAccepted covers
// Minor-2: a title containing an embedded newline must be flattened
// (like detail/evidence) before validation/rendering — not rejected,
// and not passed through to backlogfile.Store.Append with the newline
// intact (which would 500 PERSIST_FAILED since Append rejects embedded
// newlines).
func TestBacklogDeposit_TitleWithNewline_FlattenedAndAccepted(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "line1\nline2")
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "accepted", resp.Status)
	require.Contains(t, resp.Item, "line1 line2")
	require.NotContains(t, resp.Item, "\n")
}

// TestBacklogDeposit_TitleWithEmDash_NormalizedToHyphen covers M2 (final
// review, 2026-07-09): renderBacklogDepositLine joins title and detail
// with " — ", and both this package's parseBacklogItemKindTitle and
// internal/autonomy's backlogItemTitle recover a rendered item's title
// by cutting at the FIRST " — ". A title that itself contained that
// exact sequence would render fine but come back truncated on parse,
// silently weakening exact-title dedup. The handler must normalize the
// em-dash character out of the title (to a plain hyphen) before
// rendering, so the title can never introduce the separator.
func TestBacklogDeposit_TitleWithEmDash_NormalizedToHyphen(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	req := baseDepositReq("task-1", "cache — rebuild loop")
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "accepted", resp.Status)

	// The rendered line's title segment must carry a plain hyphen, not
	// the em-dash character — otherwise it's indistinguishable from the
	// title/detail separator.
	_, title := parseBacklogItemKindTitle(resp.Item)
	require.Equal(t, "cache - rebuild loop", title,
		"title must parse back whole (via parseBacklogItemKindTitle), not truncated at an em-dash the title itself contained")
}

// --- Regression requires non-empty evidence ----------------------------

// TestBacklogDeposit_ClosedItemRegressionTrueEmptyEvidence_RegressionRequired
// covers the design-compliance gap: https://docs.vornik.io
// 2026-07-09-autonomous-dev-loop-design.md line 232-234 requires
// `regression: true` PLUS non-empty `evidence` for a regression
// re-deposit against a closed item. The handler previously checked
// only the bool, so regression:true with empty evidence incorrectly
// fell through to the cooldown/accept path.
func TestBacklogDeposit_ClosedItemRegressionTrueEmptyEvidence_RegressionRequired(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	seedBacklogFile(t, fx.backlogPath(t), []string{
		fmt.Sprintf("- [x] **[bug]** flaky retry loop — root cause was a missing jitter (via task-0, %s)", oldDate()),
	})

	req := baseDepositReq("task-1", "flaky retry loop")
	req.Regression = true
	req.Evidence = ""
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "rejected", resp.Status)
	require.Equal(t, "regression_required", resp.Reason)
}

// TestBacklogDeposit_ClosedItemRegressionTrueWithEvidence_ProceedsToCooldown
// confirms the companion positive case: regression:true WITH non-empty
// evidence still proceeds to the existing cooldown/accept logic
// (old trailer date -> accepted).
func TestBacklogDeposit_ClosedItemRegressionTrueWithEvidence_ProceedsToCooldown(t *testing.T) {
	fx := newBacklogDepositFixture(t, "", nil, nil)
	seedBacklogFile(t, fx.backlogPath(t), []string{
		fmt.Sprintf("- [x] **[bug]** flaky retry loop — root cause was a missing jitter (via task-0, %s)", oldDate()),
	})

	req := baseDepositReq("task-1", "flaky retry loop")
	req.Regression = true
	req.Evidence = "reproduced again in prod logs at 2026-07-09T10:00Z"
	rec := postBacklogDeposit(t, fx.server, req, nil)
	resp := decodeBacklogDepositResponse(t, rec)
	require.Equal(t, "accepted", resp.Status)
}
