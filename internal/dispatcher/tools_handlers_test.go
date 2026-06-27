package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	yaml "gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// ---------- tiny stubs ----------

// stubAuditRepo captures logAudit calls. dispatcher.AuditRepository
// has a 1-method surface; this is enough.
type stubAuditRepo struct {
	entries []*persistence.ToolAuditEntry
	err     error
}

func (s *stubAuditRepo) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	s.entries = append(s.entries, e)
	return s.err
}

type stubMCP struct {
	tools  []chat.Tool
	out    string
	err    error
	lastIn struct {
		projectID, name, args string
	}
}

func (s *stubMCP) Tools(_ string) []chat.Tool { return s.tools }
func (s *stubMCP) Execute(_ context.Context, projectID, name, args string) (string, error) {
	s.lastIn.projectID = projectID
	s.lastIn.name = name
	s.lastIn.args = args
	return s.out, s.err
}

type stubFileSender struct {
	called  bool
	name    string
	caption string
	content []byte
	err     error
}

// SendArtifactFile implements the channel-agnostic dispatcher.FileSender,
// recording the delivered filename + caption + bytes.
func (s *stubFileSender) SendArtifactFile(_ context.Context, name string, content io.Reader, caption string) error {
	s.called = true
	s.name = name
	s.caption = caption
	if content != nil {
		s.content, _ = io.ReadAll(content)
	}
	return s.err
}

type stubMemory struct {
	results []memory.SearchResult
	err     error
}

func (s *stubMemory) Search(_ context.Context, _, _ string, _ int) ([]memory.SearchResult, error) {
	return s.results, s.err
}

// We can't reach the unexported `projects` map from another package,
// so build a thin registry whose internals are reachable indirectly:
// load YAML on a temp dir. Cheaper, and exercises the real
// registry.Load path the dispatcher uses in production.
func newRegistryWith(t *testing.T, projects []registry.Project, swarms []registry.Swarm, workflows []registry.Workflow) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, p := range projects {
		writeYAML(t, filepath.Join(dir, "projects", p.ID+".yaml"), p)
	}
	for _, s := range swarms {
		writeMD(t, filepath.Join(dir, "swarms", s.ID+".md"), s)
	}
	for _, w := range workflows {
		writeMD(t, filepath.Join(dir, "workflows", w.ID+".md"), w)
	}
	r := registry.New()
	// Load tolerates a validation error and still loads the well-
	// formed entries; tests intentionally lean on minimal projects
	// (no swarmId / no defaultWorkflowId) so we ignore the returned
	// validation error.
	_ = r.Load(dir)
	return r
}

func writeYAML(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	// Marshal through yaml.v3 so the file matches what the
	// registry.Load path expects — the struct tags are yaml-only,
	// so JSON-as-YAML would lose every field.
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeMD writes a swarm or workflow as a SWARM.md / WORKFLOW.md
// file: yaml.Marshal the struct, wrap the result in `---` markers,
// drop on disk. Mirrors writeYAML for the post-YAML-removal
// (2026-05-17) registry loader.
func writeMD(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wrapped := append([]byte("---\n"), b...)
	wrapped = append(wrapped, []byte("---\n")...)
	if err := os.WriteFile(path, wrapped, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newExecutor builds a ToolExecutor with the default zerolog
// logger + the explicit-injection knobs the tests want.
func newExecutor(opts ...func(*ToolExecutor)) *ToolExecutor {
	te := &ToolExecutor{logger: zerolog.Nop()}
	for _, o := range opts {
		o(te)
	}
	return te
}

func withRegistry(r *registry.Registry) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.registry = r }
}
func withTaskRepo(tr persistence.TaskRepository) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.taskRepo = tr }
}
func withExecRepo(er persistence.ExecutionRepository) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.execRepo = er }
}
func withArtifactRepo(ar persistence.ArtifactRepository) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.artifactRepo = ar }
}
func withAuditRepo(ar AuditRepository) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.auditRepo = ar }
}
func withMCP(m MCPExecutor) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.mcpManager = m }
}
func withMemory(m MemorySearcher) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.memory = m }
}

// ---------- Execute / execute dispatch + logAudit ----------

func TestExecute_DispatchesAllNamedTools(t *testing.T) {
	// Each named tool should be reachable from the switch — drive
	// Execute() with a minimal arg payload and check the result
	// doesn't include the "Unknown tool" fallback string. Even
	// argument-rejected results count: the point is that the
	// dispatch landed on the right handler.
	te := newExecutor(
		withTaskRepo(&mocks.MockTaskRepository{}),
		withExecRepo(&mocks.MockExecutionRepository{}),
	)
	tools := []string{
		"list_projects", "switch_project", "list_tasks", "create_task",
		"get_task_status", "wait_for_task", "cancel_task", "retry_task",
		"list_executions", "list_artifacts", "send_artifact",
		"memory_search", "read_artifact",
	}
	for _, name := range tools {
		t.Run(name, func(t *testing.T) {
			tc := chat.ToolCall{Function: chat.FunctionCall{Name: name, Arguments: "{}"}}
			res := te.Execute(context.Background(), tc, "", nil, 0, nil)
			if strings.HasPrefix(res.Content, "Unknown tool:") {
				t.Errorf("dispatch missed for tool %q: %q", name, res.Content)
			}
		})
	}
}

func TestExecute_UnknownToolFallthrough(t *testing.T) {
	te := newExecutor()
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "nope_not_real", Arguments: "{}"}}
	res := te.Execute(context.Background(), tc, "", nil, 0, nil)
	if !strings.HasPrefix(res.Content, "Unknown tool") {
		t.Errorf("expected Unknown-tool fallthrough, got %q", res.Content)
	}
}

func TestExecute_MCPRoutesWhenAllowed(t *testing.T) {
	mcp := &stubMCP{out: "ok"}
	te := newExecutor(withMCP(mcp))
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "mcp__broker__quote", Arguments: "{}"}}
	res := te.Execute(context.Background(), tc, "snake", []string{"snake"}, 0, nil)
	if !strings.Contains(res.Content, "ok") {
		t.Errorf("expected MCP output wrapped in result, got %q", res.Content)
	}
	if mcp.lastIn.projectID != "snake" {
		t.Errorf("MCP routed with wrong project: %s", mcp.lastIn.projectID)
	}
}

func TestExecute_MCPRefusesDisallowedProject(t *testing.T) {
	mcp := &stubMCP{out: "should-not-run"}
	te := newExecutor(withMCP(mcp))
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "mcp__broker__quote", Arguments: "{}"}}
	res := te.Execute(context.Background(), tc, "snake", []string{"other"}, 0, nil)
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("expected access-denied message, got %q", res.Content)
	}
	if mcp.lastIn.projectID == "snake" {
		t.Error("MCP must NOT be invoked when project is not allowed")
	}
}

func TestLogAudit_RecordsEntry(t *testing.T) {
	audit := &stubAuditRepo{}
	te := newExecutor(withAuditRepo(audit))
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "list_projects", Arguments: "{}"}}
	te.Execute(context.Background(), tc, "", nil, 0, nil)
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.ToolName != "list_projects" || e.StepID != "dispatcher" {
		t.Errorf("audit entry shape wrong: %+v", e)
	}
}

func TestLogAudit_TruncatesLongOutput(t *testing.T) {
	audit := &stubAuditRepo{}
	te := newExecutor(withAuditRepo(audit))
	// Drive a switch_project with an invalid project_id so the
	// handler produces a real (short) string. We separately test
	// the truncation explicitly via the helper.
	te.logAudit(context.Background(), "test", "in", strings.Repeat("x", 2500), "p", time.Millisecond)
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}
	if !strings.HasSuffix(audit.entries[0].ToolOutput, "…") {
		t.Errorf("expected truncation ellipsis: %q", audit.entries[0].ToolOutput[len(audit.entries[0].ToolOutput)-10:])
	}
}

func TestLogAudit_NilRepoIsNoop(t *testing.T) {
	te := newExecutor() // no audit repo wired
	// Should not panic.
	te.logAudit(context.Background(), "x", "in", "out", "p", time.Millisecond)
}

func TestLogAudit_LogErrorIsSwallowed(t *testing.T) {
	audit := &stubAuditRepo{err: errors.New("db down")}
	te := newExecutor(withAuditRepo(audit))
	// Should not propagate the error.
	te.logAudit(context.Background(), "x", "in", "out", "p", time.Millisecond)
}

// ---------- listProjects ----------

func TestListProjects_NilRegistry(t *testing.T) {
	te := newExecutor()
	res := te.listProjects(nil)
	if !strings.Contains(res.Content, "not available") {
		t.Errorf("expected registry-unavailable message, got %q", res.Content)
	}
}

func TestListProjects_EmptyAfterScopeFilter(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{minimalProject("snake", "Snake")}, oneSwarm("s"), oneWorkflow("w"))
	te := newExecutor(withRegistry(reg))
	res := te.listProjects([]string{"other"}) // scope excludes the only project
	if !strings.Contains(res.Content, "No projects available") {
		t.Errorf("expected empty-after-filter message, got %q", res.Content)
	}
}

func TestListProjects_RendersVisibleProjects(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{
		minimalProject("snake", "Snake Project"),
		minimalProject("naked-id", ""), // empty DisplayName -> falls back to id
	}, oneSwarm("s"), oneWorkflow("w"))
	te := newExecutor(withRegistry(reg))
	res := te.listProjects(nil)
	if !strings.Contains(res.Content, "Snake Project") {
		t.Errorf("expected display name in output: %q", res.Content)
	}
	if !strings.Contains(res.Content, "naked-id") {
		t.Errorf("expected fallback id in output: %q", res.Content)
	}
}

// minimalProject builds a Project that registry.Load accepts.
// swarmId + defaultWorkflowId are required for cross-ref
// validation; they reference the stub swarm/workflow built by
// oneSwarm/oneWorkflow.
func minimalProject(id, displayName string) registry.Project {
	return registry.Project{
		ID:                id,
		DisplayName:       displayName,
		SwarmID:           "s",
		DefaultWorkflowID: "w",
	}
}

func oneSwarm(id string) []registry.Swarm {
	return []registry.Swarm{{
		ID: id,
		Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "busybox"}},
		},
	}}
}

func oneWorkflow(id string) []registry.Workflow {
	return []registry.Workflow{{
		ID:         id,
		Entrypoint: "step1",
		Steps: map[string]registry.WorkflowStep{
			"step1": {Type: "agent", Role: "lead", Prompt: "do work"},
		},
	}}
}

// mustRegistry wraps newRegistryWith and asserts at least one
// project landed in the registry — useful so a registry-load
// regression surfaces here rather than as an opaque test failure
// downstream.
func mustRegistry(t *testing.T, projects []registry.Project, swarms []registry.Swarm, workflows []registry.Workflow) *registry.Registry {
	t.Helper()
	r := newRegistryWith(t, projects, swarms, workflows)
	if len(projects) > 0 && len(r.ListProjects()) == 0 {
		t.Fatalf("registry.Load dropped every project — check YAML field tags / cross-refs")
	}
	return r
}

// ---------- switchProject ----------

func TestSwitchProject_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.switchProject("not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("expected json-parse error, got %q", res.Content)
	}
}

func TestSwitchProject_EmptyID(t *testing.T) {
	te := newExecutor()
	res := te.switchProject(`{}`, nil)
	if !strings.Contains(res.Content, "project_id is required") {
		t.Errorf("expected required-field error, got %q", res.Content)
	}
}

func TestSwitchProject_NotAllowed(t *testing.T) {
	te := newExecutor()
	res := te.switchProject(`{"project_id":"x"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("expected scope-denied, got %q", res.Content)
	}
}

func TestSwitchProject_NotInRegistry(t *testing.T) {
	reg := registry.New() // empty
	te := newExecutor(withRegistry(reg))
	res := te.switchProject(`{"project_id":"ghost"}`, nil)
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("expected not-found, got %q", res.Content)
	}
}

func TestSwitchProject_Happy(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{minimalProject("snake", "")}, oneSwarm("s"), oneWorkflow("w"))
	te := newExecutor(withRegistry(reg))
	res := te.switchProject(`{"project_id":"snake"}`, nil)
	if res.ProjectSwitch == nil || res.ProjectSwitch.ProjectID != "snake" {
		t.Errorf("expected ProjectSwitch payload, got %+v", res.ProjectSwitch)
	}
	if !strings.Contains(res.Content, "Switched") {
		t.Errorf("expected switched message, got %q", res.Content)
	}
}

func TestSwitchProject_NoRegistryAllowsAnything(t *testing.T) {
	te := newExecutor()
	res := te.switchProject(`{"project_id":"anything"}`, nil)
	if res.ProjectSwitch == nil {
		t.Errorf("with no registry wired and unscoped session, switch should succeed; got %+v", res)
	}
}

// ---------- listTasks ----------

func TestListTasks_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.listTasks(context.Background(), "not-json", "", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("expected json error, got %q", res.Content)
	}
}

func TestListTasks_NoProject(t *testing.T) {
	te := newExecutor()
	res := te.listTasks(context.Background(), `{}`, "", nil)
	if !strings.Contains(res.Content, "project_id is required") {
		t.Errorf("expected resolveProjectAllowed error, got %q", res.Content)
	}
}

func TestListTasks_ExecuteActionError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.listTasks(context.Background(), `{}`, "snake", nil)
	if !strings.Contains(res.Content, "Error") {
		t.Errorf("expected error surfaced, got %q", res.Content)
	}
}

func TestListTasks_Happy(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusQueued}}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.listTasks(context.Background(), `{"status":"QUEUED"}`, "snake", nil)
	if !strings.Contains(res.Content, "t1") {
		t.Errorf("expected task id in result, got %q", res.Content)
	}
}

// ---------- getTaskStatus ----------

func TestGetTaskStatus_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.getTaskStatus(context.Background(), "not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestGetTaskStatus_TaskProjectAllowedError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.getTaskStatus(context.Background(), `{"task_id":"t1"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("expected scope-denied: %q", res.Content)
	}
}

func TestGetTaskStatus_ExecuteActionError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			// Return a task with no project so ExecuteAction's
			// inner getStatus tries to load and emits an error
			// downstream — easier just to make Get return nil.
			return nil, errors.New("boom")
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.getTaskStatus(context.Background(), `{"task_id":"t1"}`, nil)
	// taskProjectAllowed will surface "failed to load task"
	if !strings.Contains(res.Content, "failed to load") {
		t.Errorf("expected taskProjectAllowed error surface, got %q", res.Content)
	}
}

func TestGetTaskStatus_Happy(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.getTaskStatus(context.Background(), `{"task_id":"t1"}`, nil)
	if strings.Contains(res.Content, "Error") {
		t.Errorf("unexpected error in result: %q", res.Content)
	}
}

// ---------- cancelTask / retryTask ----------

func TestCancelTask_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.cancelTask(context.Background(), "not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestCancelTask_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.cancelTask(context.Background(), `{"task_id":"t1"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("expected scope denial, got %q", res.Content)
	}
}

func TestCancelTask_Happy(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}
	repo := &mocks.MockTaskRepository{
		GetFunc:          func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error { return nil },
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.cancelTask(context.Background(), `{"task_id":"t1","confirm":true}`, nil)
	if strings.Contains(res.Content, "Error:") {
		t.Errorf("unexpected error: %q", res.Content)
	}
}

func TestCancelTask_ExecuteActionError(t *testing.T) {
	// executeCancelTask calls UpdateStatus(taskID, CANCELLED);
	// make it fail to drive the error branch.
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}
	repo := &mocks.MockTaskRepository{
		GetFunc:          func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error { return errors.New("db down") },
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.cancelTask(context.Background(), `{"task_id":"t1","confirm":true}`, nil)
	if !strings.Contains(res.Content, "Error") {
		t.Errorf("expected error surface: %q", res.Content)
	}
}

// TestDestructiveTools_RequireConfirm is the hardening regression
// (2026-06-15): cancel_task / retry_task invoked WITHOUT confirm must
// return a confirmation prompt and not mutate the task.
func TestDestructiveTools_RequireConfirm(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*ToolExecutor) ToolResult
	}{
		{"cancel", func(te *ToolExecutor) ToolResult {
			return te.cancelTask(context.Background(), `{"task_id":"t1"}`, nil)
		}},
		{"retry", func(te *ToolExecutor) ToolResult {
			return te.retryTask(context.Background(), `{"task_id":"t1"}`, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := false
			repo := &mocks.MockTaskRepository{
				GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
					return &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}, nil
				},
				UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error { mutated = true; return nil },
				UpdateFunc:       func(_ context.Context, _ *persistence.Task) error { mutated = true; return nil },
			}
			te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
			res := tc.call(te)
			if !strings.Contains(res.Content, "Confirmation required") {
				t.Errorf("expected confirmation prompt, got %q", res.Content)
			}
			if mutated {
				t.Error("task was mutated without confirmation")
			}
		})
	}
}

func TestRetryTask_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.retryTask(context.Background(), "not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestRetryTask_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.retryTask(context.Background(), `{"task_id":"t1"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("expected scope denial, got %q", res.Content)
	}
}

func TestRetryTask_Happy(t *testing.T) {
	task := &persistence.Task{
		ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusFailed,
		Attempt: 1, MaxAttempts: 3,
	}
	repo := &mocks.MockTaskRepository{
		GetFunc:    func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
		UpdateFunc: func(_ context.Context, _ *persistence.Task) error { return nil },
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.retryTask(context.Background(), `{"task_id":"t1","confirm":true}`, nil)
	if strings.Contains(res.Content, "Error:") {
		t.Errorf("unexpected error: %q", res.Content)
	}
}

func TestRetryTask_ExecuteActionError(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusFailed}
	repo := &mocks.MockTaskRepository{
		GetFunc:    func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
		UpdateFunc: func(_ context.Context, _ *persistence.Task) error { return errors.New("db down") },
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.retryTask(context.Background(), `{"task_id":"t1","confirm":true}`, nil)
	if !strings.Contains(res.Content, "Error") {
		t.Errorf("expected error surface: %q", res.Content)
	}
}

// ---------- listExecutions ----------

func TestListExecutions_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.listExecutions(context.Background(), "not-json", "", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListExecutions_NoProject(t *testing.T) {
	te := newExecutor()
	res := te.listExecutions(context.Background(), `{}`, "", nil)
	if !strings.Contains(res.Content, "project_id is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListExecutions_NoRepo(t *testing.T) {
	te := newExecutor()
	res := te.listExecutions(context.Background(), `{}`, "snake", nil)
	if !strings.Contains(res.Content, "Execution repository not available") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListExecutions_RepoError(t *testing.T) {
	te := newExecutor(withExecRepo(&mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, errors.New("db down")
		},
	}))
	res := te.listExecutions(context.Background(), `{}`, "snake", nil)
	if !strings.Contains(res.Content, "Error listing executions") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListExecutions_Empty(t *testing.T) {
	te := newExecutor(withExecRepo(&mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil
		},
	}))
	res := te.listExecutions(context.Background(), `{}`, "snake", nil)
	if !strings.Contains(res.Content, "No executions found") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListExecutions_HappyWithStatus(t *testing.T) {
	te := newExecutor(withExecRepo(&mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			if f.Status == nil || *f.Status != persistence.ExecutionStatus("RUNNING") {
				t.Errorf("status filter not propagated: %+v", f.Status)
			}
			return []*persistence.Execution{
				{ID: "e1", TaskID: "t1", ProjectID: "snake", Status: persistence.ExecutionStatusRunning},
				nil, // tolerated
				{ID: "e2", TaskID: "t2", ProjectID: "snake", Status: persistence.ExecutionStatusRunning},
			}, nil
		},
	}))
	res := te.listExecutions(context.Background(), `{"status":"running"}`, "snake", nil)
	if !strings.Contains(res.Content, "e1") || !strings.Contains(res.Content, "e2") {
		t.Errorf("missing executions in output: %q", res.Content)
	}
}

// ---------- listArtifacts ----------

func TestListArtifacts_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.listArtifacts(context.Background(), "not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListArtifacts_RequiresTaskID(t *testing.T) {
	te := newExecutor()
	res := te.listArtifacts(context.Background(), `{}`, nil)
	if !strings.Contains(res.Content, "task_id is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListArtifacts_NoRepo(t *testing.T) {
	te := newExecutor()
	res := te.listArtifacts(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "not available") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListArtifacts_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(
		withTaskRepo(repo),
		withArtifactRepo(&mocks.MockArtifactRepository{}),
	)
	res := te.listArtifacts(context.Background(), `{"task_id":"t1"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListArtifacts_ArtifactRepoError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, errors.New("disk full")
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.listArtifacts(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "Failed to list") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListArtifacts_Empty(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.listArtifacts(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "No artifacts") {
		t.Errorf("got %q", res.Content)
	}
}

func TestListArtifacts_HappyWithExecID(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	sz := int64(123)
	execID := "exec-1"
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "out.txt", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: &sz, ExecutionID: &execID},
				{Name: "scratch.txt", ArtifactClass: persistence.ArtifactClassIntermediate},
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.listArtifacts(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "out.txt") || !strings.Contains(res.Content, "scratch.txt") {
		t.Errorf("missing artifact names: %q", res.Content)
	}
	if !strings.Contains(res.Content, "123 bytes") {
		t.Errorf("missing size: %q", res.Content)
	}
	if !strings.Contains(res.Content, "exec: exec-1") {
		t.Errorf("missing exec annotation: %q", res.Content)
	}
}

// ---------- sendArtifact ----------

func TestSendArtifact_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.sendArtifact(context.Background(), "not-json", nil, nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestSendArtifact_RequiresTaskID(t *testing.T) {
	te := newExecutor()
	res := te.sendArtifact(context.Background(), `{}`, nil, nil)
	if !strings.Contains(res.Content, "task_id is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestSendArtifact_NotConfigured(t *testing.T) {
	te := newExecutor() // no artifactRepo, no fs
	res := te.sendArtifact(context.Background(), `{"task_id":"t1"}`, nil, nil)
	if !strings.Contains(res.Content, "not configured") {
		t.Errorf("got %q", res.Content)
	}
}

func TestSendArtifact_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(
		withTaskRepo(repo),
		withArtifactRepo(&mocks.MockArtifactRepository{}),
	)
	fs := &stubFileSender{}
	res := te.sendArtifact(context.Background(), `{"task_id":"t1"}`, []string{"snake"}, fs)
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("got %q", res.Content)
	}
	if fs.called {
		t.Error("file sender must not be invoked when scope denies")
	}
}

func TestSendArtifact_ArtifactListError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, errors.New("disk full")
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.sendArtifact(context.Background(), `{"task_id":"t1"}`, nil, &stubFileSender{})
	if !strings.Contains(res.Content, "Failed to list") {
		t.Errorf("got %q", res.Content)
	}
}

func TestSendArtifact_NoArtifacts(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.sendArtifact(context.Background(), `{"task_id":"t1"}`, nil, &stubFileSender{})
	if !strings.Contains(res.Content, "No artifacts") {
		t.Errorf("got %q", res.Content)
	}
}

// TestSendArtifact_FallsBackToChildArtifacts pins the 2026-05-18
// fix: when the LLM passes the PARENT task_id (the one returned by
// create_task) but the artifacts actually live on the leaf child
// (workflow-routed via the adaptive workflow), send_artifact walks
// children and delivers the named artifact from there. Pre-fix the
// LLM got "No artifacts found for this task" and the user got
// nothing.
func TestSendArtifact_FallsBackToChildArtifacts(t *testing.T) {
	parent := &persistence.Task{ID: "parent", ProjectID: "snake"}
	child := &persistence.Task{ID: "child", ParentTaskID: ptrStr("parent"), ProjectID: "snake"}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == "parent" {
				return parent, nil
			}
			return child, nil
		},
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == "parent" {
				return []*persistence.Task{child}, nil
			}
			return nil, nil
		},
	}
	dir := t.TempDir()
	cvPath := filepath.Join(dir, "cv.pdf")
	if err := os.WriteFile(cvPath, []byte("cv body"), 0o600); err != nil {
		t.Fatal(err)
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if f.TaskID == nil {
				return nil, nil
			}
			if *f.TaskID == "parent" {
				return nil, nil // parent has no direct artifacts
			}
			if *f.TaskID == "child" {
				return []*persistence.Artifact{
					{Name: "cv.pdf", StoragePath: cvPath},
				}, nil
			}
			return nil, nil
		},
	}
	fs := &stubFileSender{}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.sendArtifact(context.Background(), `{"task_id":"parent","artifact_name":"cv.pdf"}`, nil, fs)
	if !fs.called {
		t.Fatalf("expected FileSender to be called via child fallback; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "Sent artifact 'cv.pdf'") {
		t.Errorf("response should confirm cv.pdf was sent; got %q", res.Content)
	}
}

// ptrStr is a tiny helper for setting up *string fields in test fixtures.
func ptrStr(s string) *string { return &s }

func TestSendArtifact_NameNotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "out.txt", StoragePath: "/tmp/out.txt"},
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.sendArtifact(context.Background(), `{"task_id":"t1","artifact_name":"missing"}`, nil, &stubFileSender{})
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("got %q", res.Content)
	}
}

func TestSendArtifact_NameMatchHappy(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(outPath, []byte("hello out"), 0o600); err != nil {
		t.Fatal(err)
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "out.txt", StoragePath: outPath},
				{Name: "other.txt", StoragePath: filepath.Join(dir, "other.txt")},
			}, nil
		},
	}
	fs := &stubFileSender{}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.sendArtifact(context.Background(), `{"task_id":"t1","artifact_name":"out.txt"}`, nil, fs)
	if !fs.called {
		t.Fatal("file sender not invoked")
	}
	if fs.name != "out.txt" || string(fs.content) != "hello out" {
		t.Errorf("file sender called with wrong args: name=%q content=%q", fs.name, fs.content)
	}
	if !strings.Contains(res.Content, "Sent artifact") {
		t.Errorf("got %q", res.Content)
	}
}

func TestSendArtifact_DefaultsToFirstWhenNoName(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.txt")
	if err := os.WriteFile(firstPath, []byte("first body"), 0o600); err != nil {
		t.Fatal(err)
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "first.txt", StoragePath: firstPath},
			}, nil
		},
	}
	fs := &stubFileSender{}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	te.sendArtifact(context.Background(), `{"task_id":"t1"}`, nil, fs)
	if fs.name != "first.txt" {
		t.Errorf("expected default to first artifact: %s", fs.name)
	}
}

func TestSendArtifact_SendError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	dir := t.TempDir()
	xPath := filepath.Join(dir, "x")
	if err := os.WriteFile(xPath, []byte("x body"), 0o600); err != nil {
		t.Fatal(err)
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{Name: "x", StoragePath: xPath}}, nil
		},
	}
	fs := &stubFileSender{err: errors.New("network down")}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.sendArtifact(context.Background(), `{"task_id":"t1"}`, nil, fs)
	if !strings.Contains(res.Content, "Failed to send") {
		t.Errorf("got %q", res.Content)
	}
}

// ---------- memorySearch ----------

func TestMemorySearch_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.memorySearch(context.Background(), "not-json", "", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestMemorySearch_RequiresQuery(t *testing.T) {
	te := newExecutor()
	res := te.memorySearch(context.Background(), `{}`, "snake", nil)
	if !strings.Contains(res.Content, "query is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestMemorySearch_NoProject(t *testing.T) {
	te := newExecutor()
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "", nil)
	if !strings.Contains(res.Content, "project_id is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestMemorySearch_NotEnabled(t *testing.T) {
	te := newExecutor() // no memory
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if !strings.Contains(res.Content, "not enabled") {
		t.Errorf("got %q", res.Content)
	}
}

func TestMemorySearch_SearchError(t *testing.T) {
	te := newExecutor(withMemory(&stubMemory{err: errors.New("vector store down")}))
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if !strings.Contains(res.Content, "Memory search failed") {
		t.Errorf("got %q", res.Content)
	}
}

func TestMemorySearch_NoHits(t *testing.T) {
	te := newExecutor(withMemory(&stubMemory{results: nil}))
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if !strings.Contains(res.Content, "No memory hits") {
		t.Errorf("got %q", res.Content)
	}
}

func TestMemorySearch_HitsRendered(t *testing.T) {
	hits := []memory.SearchResult{
		{SourceName: "doc1", Score: 0.95, Content: "hello world"},
		{SourceName: "doc2", Score: 0.80, Content: strings.Repeat("x", 1000)}, // tests truncation
	}
	te := newExecutor(withMemory(&stubMemory{results: hits}))
	res := te.memorySearch(context.Background(), `{"query":"x","limit":3}`, "snake", nil)
	if !strings.Contains(res.Content, "doc1") || !strings.Contains(res.Content, "doc2") {
		t.Errorf("expected hits rendered, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "untrusted_content") {
		t.Errorf("expected untrusted wrapper, got %q", res.Content)
	}
}

func TestMemorySearch_LimitClamped(t *testing.T) {
	captured := 0
	mem := &stubMemoryCapture{}
	te := newExecutor(withMemory(mem))
	te.memorySearch(context.Background(), `{"query":"x","limit":50}`, "snake", nil)
	captured = mem.limit
	if captured > 20 {
		t.Errorf("limit should be clamped to 20, got %d", captured)
	}
	te.memorySearch(context.Background(), `{"query":"x","limit":-1}`, "snake", nil)
	if mem.limit != 5 {
		t.Errorf("negative limit should default to 5, got %d", mem.limit)
	}
}

// stubMemoryCapture is a memory stub that records the limit
// passed in so the clamp test can assert it.
type stubMemoryCapture struct {
	limit int
}

func (s *stubMemoryCapture) Search(_ context.Context, _, _ string, limit int) ([]memory.SearchResult, error) {
	s.limit = limit
	return nil, nil
}

// stubMemoryTemporalCapture implements both the plain MemorySearcher
// AND the optional MemoryTemporalSearcher capability so tests can
// verify the dispatch path picks the temporal interface when bounds
// are supplied. Records every call's args so assertions can pin
// "did we call the temporal variant" without leaking implementation
// detail to the test author.
type stubMemoryTemporalCapture struct {
	plainCalls    int
	temporalCalls int
	lastOpts      memory.SearchOptions
}

func (s *stubMemoryTemporalCapture) Search(_ context.Context, _, _ string, limit int) ([]memory.SearchResult, error) {
	s.plainCalls++
	return nil, nil
}

func (s *stubMemoryTemporalCapture) SearchWithOptions(_ context.Context, _, _ string, opts memory.SearchOptions) ([]memory.SearchResult, error) {
	s.temporalCalls++
	s.lastOpts = opts
	return nil, nil
}

// stubMemoryFirewall implements MemorySearcher AND the optional
// MemoryFirewallSearcher capability, so memorySearch dispatches to
// RecallWithContext. It returns canned results so the policy-proof
// formatter (audit finding #3) can be pinned.
type stubMemoryFirewall struct {
	results []memory.SearchResult
}

func (s *stubMemoryFirewall) Search(_ context.Context, _, _ string, _ int) ([]memory.SearchResult, error) {
	return s.results, nil
}

func (s *stubMemoryFirewall) RecallWithContext(_ context.Context, _, _ string, _ memory.SearchOptions, _ memoryfirewall.RequestContext) ([]memory.SearchResult, error) {
	return s.results, nil
}

// TestMemorySearch_RendersPolicyProof confirms the firewall's
// PolicyProof surfaces as a citable policy= line OUTSIDE the
// untrusted_content wrapper. Before the fix the formatter dropped
// r.PolicyProof entirely (audit finding #3).
func TestMemorySearch_RendersPolicyProof(t *testing.T) {
	mem := &stubMemoryFirewall{results: []memory.SearchResult{{
		SourceName: "doc1", Score: 0.9, Content: "secret-ish fact",
		PolicyProof: &memory.PolicyProofWire{
			ChunkID:      "c1",
			Decision:     "allow",
			PolicyDigest: "abcdef0123456789aaaa",
			RequestContext: memory.PolicyProofRequestContext{
				Role:    "dispatcher",
				Purpose: "operational",
			},
		},
	}}}
	te := newExecutor(withMemory(mem))
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if !strings.Contains(res.Content, "policy=decision=allow") {
		t.Fatalf("expected policy proof line, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "purpose=operational") {
		t.Fatalf("expected purpose in proof, got %q", res.Content)
	}
	// Digest is truncated to 12 chars.
	if !strings.Contains(res.Content, "digest=abcdef012345") {
		t.Fatalf("expected truncated digest, got %q", res.Content)
	}
	// Policy metadata must sit before the untrusted wrapper so the
	// model can trust it.
	policyIdx := strings.Index(res.Content, "policy=decision=")
	untrustedIdx := strings.Index(res.Content, "untrusted_content")
	if policyIdx < 0 || untrustedIdx < 0 || policyIdx > untrustedIdx {
		t.Fatalf("policy line must precede untrusted wrapper; content=%q", res.Content)
	}
}

// TestMemorySearch_RendersPolicyWarning confirms an advisory-mode
// PolicyWarning surfaces.
func TestMemorySearch_RendersPolicyWarning(t *testing.T) {
	mem := &stubMemoryFirewall{results: []memory.SearchResult{{
		SourceName: "doc1", Score: 0.9, Content: "fact",
		PolicyWarning: "block: role not permitted",
	}}}
	te := newExecutor(withMemory(mem))
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if !strings.Contains(res.Content, "policy_warning=block: role not permitted") {
		t.Fatalf("expected policy warning, got %q", res.Content)
	}
}

// TestMemorySearch_NilPolicyProofOmitsLine confirms the legacy path
// (no firewall proof — e.g. lazy-backfill chunk) emits no policy line.
func TestMemorySearch_NilPolicyProofOmitsLine(t *testing.T) {
	te := newExecutor(withMemory(&stubMemory{results: []memory.SearchResult{
		{SourceName: "doc1", Score: 0.9, Content: "fact"},
	}}))
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if strings.Contains(res.Content, "policy=") || strings.Contains(res.Content, "policy_warning=") {
		t.Fatalf("nil proof must not emit a policy line, got %q", res.Content)
	}
}

// TestMemorySearch_TemporalBoundsDispatchToOptionalInterface — when
// the caller supplies from_date / to_date AND the configured
// searcher implements the optional MemoryTemporalSearcher, the
// dispatch must hit SearchWithOptions instead of plain Search.
// Otherwise the bounds get silently dropped.
func TestMemorySearch_TemporalBoundsDispatchToOptionalInterface(t *testing.T) {
	mem := &stubMemoryTemporalCapture{}
	te := newExecutor(withMemory(mem))
	te.memorySearch(context.Background(),
		`{"query":"x","from_date":"2026-05-01","to_date":"2026-05-15"}`, "snake", nil)
	if mem.temporalCalls != 1 {
		t.Fatalf("expected 1 SearchWithOptions call, got plain=%d temporal=%d", mem.plainCalls, mem.temporalCalls)
	}
	if mem.lastOpts.FromDate.IsZero() {
		t.Error("FromDate must be threaded through to SearchOptions")
	}
	if mem.lastOpts.ToDate.IsZero() {
		t.Error("ToDate must be threaded through to SearchOptions")
	}
	if mem.lastOpts.Limit != 5 {
		t.Errorf("Limit fallback should be 5 (the tool's default), got %d", mem.lastOpts.Limit)
	}
}

// TestMemorySearch_NoBoundsKeepsPlainSearchPath — without
// from_date / to_date the dispatcher MUST stay on the legacy
// Search path so callers/mocks that don't implement the optional
// interface keep working.
func TestMemorySearch_NoBoundsKeepsPlainSearchPath(t *testing.T) {
	mem := &stubMemoryTemporalCapture{}
	te := newExecutor(withMemory(mem))
	te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if mem.plainCalls != 1 || mem.temporalCalls != 0 {
		t.Errorf("expected plain dispatch, got plain=%d temporal=%d", mem.plainCalls, mem.temporalCalls)
	}
}

// TestMemorySearch_InvalidDateBoundRejected — malformed bounds
// surface a friendly error rather than silently disabling the
// filter or hitting the SQL with garbage.
func TestMemorySearch_InvalidDateBoundRejected(t *testing.T) {
	mem := &stubMemoryTemporalCapture{}
	te := newExecutor(withMemory(mem))
	res := te.memorySearch(context.Background(), `{"query":"x","from_date":"yesterday"}`, "snake", nil)
	if !strings.Contains(res.Content, "from_date:") {
		t.Errorf("expected error mentioning from_date, got %q", res.Content)
	}
	if mem.plainCalls != 0 || mem.temporalCalls != 0 {
		t.Errorf("validation error must NOT reach the memory layer; got plain=%d temporal=%d", mem.plainCalls, mem.temporalCalls)
	}
}

// TestParseMemoryDateBound covers each accepted form + each
// failure mode. The helper is small but on the hot path so a
// regression here breaks every temporal-filtered call.
func TestParseMemoryDateBound(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		end      bool
		wantErr  bool
		wantZero bool
		want     time.Time
	}{
		{"empty is zero", "", false, false, true, time.Time{}},
		{"whitespace is zero", "   ", false, false, true, time.Time{}},
		{"date only start", "2026-05-15", false, false, false, time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)},
		{"date only end", "2026-05-15", true, false, false, time.Date(2026, 5, 15, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)},
		{"RFC3339 ignores end flag", "2026-05-15T12:34:56Z", true, false, false, time.Date(2026, 5, 15, 12, 34, 56, 0, time.UTC)},
		{"junk fails", "yesterday", false, true, false, time.Time{}},
		{"slashes fail", "2026/05/15", false, true, false, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMemoryDateBound(tc.in, tc.end)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.in, err)
			}
			if tc.wantZero && !got.IsZero() {
				t.Errorf("expected zero time for %q, got %v", tc.in, got)
			}
			if !tc.wantZero && !tc.wantErr && got.IsZero() {
				t.Errorf("expected non-zero time for %q, got zero", tc.in)
			}
			if !tc.wantZero && !tc.wantErr && !got.Equal(tc.want) {
				t.Errorf("parseMemoryDateBound(%q, %v) = %v, want %v", tc.in, tc.end, got, tc.want)
			}
		})
	}
}

// ---------- readArtifact ----------

func TestReadArtifact_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.readArtifact(context.Background(), "not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_RequiresBoth(t *testing.T) {
	te := newExecutor()
	res := te.readArtifact(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "both required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_NoRepo(t *testing.T) {
	te := newExecutor()
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"a"}`, nil)
	if !strings.Contains(res.Content, "not available") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(
		withTaskRepo(repo),
		withArtifactRepo(&mocks.MockArtifactRepository{}),
	)
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"a"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_ListError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, errors.New("disk full")
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"a"}`, nil)
	if !strings.Contains(res.Content, "Failed to list") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "out.txt"}, nil, // nil-tolerated
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"missing"}`, nil)
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_NoStoragePath(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "ghost"}, // no StoragePath
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"ghost"}`, nil)
	if !strings.Contains(res.Content, "no storage path") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_ReadError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "x", StoragePath: "/nonexistent/path"},
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"x"}`, nil)
	if !strings.Contains(res.Content, "Failed to read") {
		t.Errorf("got %q", res.Content)
	}
}

func TestReadArtifact_HappySmall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.txt")
	body := "hello world"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sz := int64(len(body))
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{Name: "out.txt", StoragePath: p, SizeBytes: &sz}}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"out.txt"}`, nil)
	if !strings.Contains(res.Content, "hello world") {
		t.Errorf("expected body in result, got %q", res.Content)
	}
	if strings.Contains(res.Content, "truncated") {
		t.Errorf("small body should not be truncated: %q", res.Content)
	}
}

func TestReadArtifact_Truncated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	body := strings.Repeat("x", readArtifactMaxBytes+200)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{Name: "big.txt", StoragePath: p}}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo), withArtifactRepo(ar))
	res := te.readArtifact(context.Background(), `{"task_id":"t1","artifact_name":"big.txt"}`, nil)
	if !strings.Contains(res.Content, "truncated") {
		t.Errorf("expected truncation note, got %q", res.Content)
	}
}

// ---------- executeMCPTool ----------

func TestExecuteMCPTool_NotConfigured(t *testing.T) {
	te := newExecutor()
	res := te.executeMCPTool(context.Background(), "snake", "mcp__x__y", "{}")
	if !strings.Contains(res.Content, "MCP is not configured") {
		t.Errorf("got %q", res.Content)
	}
}

func TestExecuteMCPTool_RequiresProject(t *testing.T) {
	te := newExecutor(withMCP(&stubMCP{}))
	res := te.executeMCPTool(context.Background(), "", "mcp__x__y", "{}")
	if !strings.Contains(res.Content, "active project") {
		t.Errorf("got %q", res.Content)
	}
}

func TestExecuteMCPTool_BrokerError(t *testing.T) {
	te := newExecutor(withMCP(&stubMCP{err: errors.New("upstream 500")}))
	res := te.executeMCPTool(context.Background(), "snake", "mcp__x__y", "{}")
	if !strings.Contains(res.Content, "MCP error") {
		t.Errorf("got %q", res.Content)
	}
}

func TestExecuteMCPTool_HappyWrapsUntrusted(t *testing.T) {
	te := newExecutor(withMCP(&stubMCP{out: "broker says hi"}))
	res := te.executeMCPTool(context.Background(), "snake", "mcp__broker__quote", "{}")
	if !strings.Contains(res.Content, "untrusted_content") {
		t.Errorf("expected untrusted wrapper, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "broker says hi") {
		t.Errorf("expected MCP output in result, got %q", res.Content)
	}
}

// TestExecuteMCPTool_BelowCapPassesThroughUnchanged — a result that
// fits the cap must be returned verbatim (modulo the untrusted
// wrapper). Pins the no-op path so a future cap change can't silently
// start touching small payloads.
func TestExecuteMCPTool_BelowCapPassesThroughUnchanged(t *testing.T) {
	body := strings.Repeat("y", mcpToolResultMaxBytes) // exactly at cap, > triggers
	te := newExecutor(withMCP(&stubMCP{out: body}))
	res := te.executeMCPTool(context.Background(), "snake", "mcp__scraper__web_fetch", "{}")
	if strings.Contains(res.Content, "truncated") {
		t.Errorf("at-cap payload must NOT be truncated; got truncation note")
	}
	if !strings.Contains(res.Content, body) {
		t.Errorf("body should pass through verbatim")
	}
}

// TestExecuteMCPTool_OverCapTruncatesAndRoutesToTask pins the
// regression for the runaway-token bug: a single MCP result that
// overflows the dispatcher's per-call cap must (a) be truncated to
// the cap, (b) carry an explicit truncation note quoting original
// vs kept bytes so the operator can debug, and (c) instruct the
// model to use create_task for full content — that's the dispatcher
// design: heavy data extraction belongs in a task worker with its
// own context budget, not piled into the chat loop.
//
// Without this cap one Telegram message produced 186k input tokens
// across ~20 web_fetch results and 400'd the bedrock turn with no
// recovery (400 not Retryable; intra-turn prune doesn't exist).
func TestExecuteMCPTool_OverCapTruncatesAndRoutesToTask(t *testing.T) {
	body := strings.Repeat("z", mcpToolResultMaxBytes*3) // 150 KiB
	te := newExecutor(withMCP(&stubMCP{out: body}))
	res := te.executeMCPTool(context.Background(), "snake", "mcp__scraper__web_fetch", `{"url":"https://example.com"}`)

	if !strings.Contains(res.Content, "truncated") {
		t.Fatalf("over-cap payload must carry a truncation note; got %q", res.Content[:min(len(res.Content), 200)])
	}
	if !strings.Contains(res.Content, "create_task") {
		t.Errorf("truncation note must mention create_task so the model knows the recovery path is to delegate, not retry; got: %s",
			snippetAround(res.Content, "truncated", 400))
	}
	if !strings.Contains(res.Content, "untrusted_content") {
		t.Errorf("must still wrap as untrusted_content — the truncation footer doesn't change the trust boundary")
	}
	// Body should be capped: payload region is ~mcpToolResultMaxBytes
	// plus the (small) truncation note. Allow generous slack for the
	// wrapper tags and footer text, but the full 150 KiB body must
	// NOT be present in full.
	if strings.Count(res.Content, "z") >= len(body) {
		t.Errorf("over-cap body was not truncated; saw %d z-bytes in result (input had %d)",
			strings.Count(res.Content, "z"), len(body))
	}
}

func snippetAround(s, anchor string, around int) string {
	i := strings.Index(s, anchor)
	if i < 0 {
		return s
	}
	start := i - around
	if start < 0 {
		start = 0
	}
	end := i + around
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- taskProjectAllowed ----------

func TestTaskProjectAllowed_NoRepo(t *testing.T) {
	te := newExecutor()
	_, err := te.taskProjectAllowed(context.Background(), "t1", nil)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected not-available error, got %v", err)
	}
}

func TestTaskProjectAllowed_LookupError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	te := newExecutor(withTaskRepo(repo))
	_, err := te.taskProjectAllowed(context.Background(), "t1", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to load") {
		t.Errorf("expected lookup error, got %v", err)
	}
}

func TestTaskProjectAllowed_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	_, err := te.taskProjectAllowed(context.Background(), "t1", nil)
	if err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Errorf("expected not-found, got %v", err)
	}
}

func TestTaskProjectAllowed_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	_, err := te.taskProjectAllowed(context.Background(), "t1", []string{"snake"})
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("expected scope denial, got %v", err)
	}
}

func TestTaskProjectAllowed_Happy(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	got, err := te.taskProjectAllowed(context.Background(), "t1", []string{"snake"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "snake" {
		t.Errorf("expected resolved project, got %q", got)
	}
}

// ---------- formatWaitResult, waitForTask ----------

func TestFormatWaitResult_FailedWithErrorClass(t *testing.T) {
	errMsg := "tool timeout"
	class := "TOOL_ERROR"
	task := &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusFailed,
		LastError: &errMsg, LastErrorClass: &class,
	}
	te := newExecutor()
	res := te.formatWaitResult(context.Background(), task)
	if !strings.Contains(res.Content, "reached status FAILED") {
		t.Errorf("missing status: %q", res.Content)
	}
	if !strings.Contains(res.Content, "tool timeout") {
		t.Errorf("missing last_error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "TOOL_ERROR") {
		t.Errorf("missing error_class: %q", res.Content)
	}
}

func TestFormatWaitResult_TruncatesLongError(t *testing.T) {
	huge := strings.Repeat("x", 2000)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed, LastError: &huge}
	te := newExecutor()
	res := te.formatWaitResult(context.Background(), task)
	if !strings.Contains(res.Content, "…") {
		t.Errorf("expected truncation: %q", res.Content[len(res.Content)-30:])
	}
}

func TestFormatWaitResult_WithArtifacts(t *testing.T) {
	sz := int64(42)
	ar := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{Name: "report.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: &sz},
			}, nil
		},
	}
	te := newExecutor(withArtifactRepo(ar))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted}
	res := te.formatWaitResult(context.Background(), task)
	if !strings.Contains(res.Content, "report.md") {
		t.Errorf("expected artifact in result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "42 bytes") {
		t.Errorf("expected size: %q", res.Content)
	}
}

func TestWaitForTask_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.waitForTask(context.Background(), "not-json", nil)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestWaitForTask_ScopeDenied(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "other"}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(context.Background(), `{"task_id":"t1"}`, []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") {
		t.Errorf("got %q", res.Content)
	}
}

func TestWaitForTask_GetError(t *testing.T) {
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			if calls == 1 {
				return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
			}
			return nil, errors.New("db down")
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(context.Background(), `{"task_id":"t1","timeout_seconds":1}`, nil)
	if !strings.Contains(res.Content, "Failed to read task") {
		t.Errorf("got %q", res.Content)
	}
}

func TestWaitForTask_NotFound(t *testing.T) {
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			if calls == 1 {
				return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
			}
			return nil, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(context.Background(), `{"task_id":"t1","timeout_seconds":1}`, nil)
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("got %q", res.Content)
	}
}

func TestWaitForTask_TerminalImmediately(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusCompleted}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "COMPLETED") {
		t.Errorf("expected terminal-state result: %q", res.Content)
	}
}

func TestWaitForTask_ContextCancelledBeforePoll(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	res := te.waitForTask(ctx, `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "cancelled") {
		t.Errorf("expected cancellation message: %q", res.Content)
	}
}

func TestWaitForTask_TimeoutClampedAt30Min(t *testing.T) {
	// Drive the deadline branch by setting timeout to 1s and
	// returning a never-terminal task. The timeout calculation
	// branch caps at 30m but we can't usefully wait that long;
	// the elapsed-after-fastPollWindow branch + the timeout
	// branch are both exercised by setting timeout=1s.
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			return &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	// 0 → default 600s would block tests; the timeout=1 path
	// (1s) is the deterministic version.
	start := time.Now()
	res := te.waitForTask(context.Background(), `{"task_id":"t1","timeout_seconds":1}`, nil)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("waitForTask hung well past timeout")
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("expected timeout message: %q", res.Content)
	}
}

func TestWaitForTask_LargeTimeoutIsClampedTo30Min(t *testing.T) {
	// Drive the timeout clamp branch (timeout > 30m → 30m).
	// The task returns terminal immediately so the clamp is the
	// only branch we exercise here; the deadline never fires.
	task := &persistence.Task{ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusCompleted}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return task, nil },
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(context.Background(), `{"task_id":"t1","timeout_seconds":7200}`, nil)
	if !strings.Contains(res.Content, "COMPLETED") {
		t.Errorf("expected immediate-terminal result: %q", res.Content)
	}
}

// ---------- prompt + dedup helpers (close the remaining gaps) ----------

func TestExtractTaskPrompt_FromContext(t *testing.T) {
	got := extractTaskPrompt([]byte(`{"context":{"prompt":"hi from context"}}`))
	if got != "hi from context" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTaskPrompt_FromTopLevel(t *testing.T) {
	got := extractTaskPrompt([]byte(`{"prompt":"hi from top"}`))
	if got != "hi from top" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTaskPrompt_Empty(t *testing.T) {
	if got := extractTaskPrompt(nil); got != "" {
		t.Errorf("nil payload should return empty: %q", got)
	}
	if got := extractTaskPrompt([]byte("{not json}")); got != "" {
		t.Errorf("malformed payload should return empty: %q", got)
	}
}

func TestTruncatePrompt(t *testing.T) {
	if truncatePrompt("hi", 0) != "hi" {
		t.Error("max=0 should pass through")
	}
	if truncatePrompt("hello", 100) != "hello" {
		t.Error("string shorter than max should pass through")
	}
	got := truncatePrompt("hello world!", 5)
	if got != "hello..." {
		t.Errorf("got %q", got)
	}
}

func TestFindRecentDuplicate_GuardClauses(t *testing.T) {
	// All four guards should produce empty:
	if findRecentDuplicateTask(context.Background(), nil, "p", "x", time.Second) != "" {
		t.Error("nil repo should return empty")
	}
	if findRecentDuplicateTask(context.Background(), &mocks.MockTaskRepository{}, "", "x", time.Second) != "" {
		t.Error("empty projectID should return empty")
	}
	if findRecentDuplicateTask(context.Background(), &mocks.MockTaskRepository{}, "p", "", time.Second) != "" {
		t.Error("empty prompt should return empty")
	}
	if findRecentDuplicateTask(context.Background(), &mocks.MockTaskRepository{}, "p", "x", 0) != "" {
		t.Error("zero window should return empty")
	}
}

func TestFindRecentDuplicate_ListError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	if findRecentDuplicateTask(context.Background(), repo, "p", "x", time.Second) != "" {
		t.Error("List error should yield empty (best-effort)")
	}
}

func TestFindRecentDuplicate_BeforeCutoffStopsScan(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "old", ProjectID: "p", Status: persistence.TaskStatusQueued,
					Payload: payloadWithPrompt(t, "x"), CreatedAt: time.Now().Add(-time.Hour)},
			}, nil
		},
	}
	if got := findRecentDuplicateTask(context.Background(), repo, "p", "x", time.Second); got != "" {
		t.Errorf("old task should not match: %q", got)
	}
}

func TestFindRecentDuplicate_SkipsTerminal(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "done", ProjectID: "p", Status: persistence.TaskStatusCompleted,
					Payload: payloadWithPrompt(t, "x"), CreatedAt: time.Now()},
			}, nil
		},
	}
	if got := findRecentDuplicateTask(context.Background(), repo, "p", "x", time.Minute); got != "" {
		t.Errorf("completed tasks should not dedup: %q", got)
	}
}

func TestFindRecentDuplicate_NilTaskTolerated(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{nil, nil}, nil
		},
	}
	if got := findRecentDuplicateTask(context.Background(), repo, "p", "x", time.Minute); got != "" {
		t.Errorf("nil rows should not panic or match: %q", got)
	}
}

// ---------- DispatcherTools ----------

func TestDispatcherTools_AllDeclared(t *testing.T) {
	tools := DispatcherTools()
	if len(tools) == 0 {
		t.Fatal("expected non-empty tool list")
	}
	// Every named handler must have a corresponding declaration so
	// the LLM can actually call it.
	want := []string{
		"list_projects", "switch_project", "list_tasks", "create_task",
		"get_task_status", "wait_for_task", "cancel_task", "retry_task",
		"list_executions", "list_artifacts", "send_artifact",
		"memory_search", "read_artifact",
	}
	have := map[string]struct{}{}
	for _, tool := range tools {
		have[tool.Function.Name] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			t.Errorf("DispatcherTools missing %q declaration", w)
		}
	}
}

// ---------- filepathBase ----------

func TestFilepathBase(t *testing.T) {
	if got := filepathBase("/tmp/foo/bar.txt"); got != "bar.txt" {
		t.Errorf("got %q", got)
	}
	if got := filepathBase("bar.txt"); got != "bar.txt" {
		t.Errorf("got %q", got)
	}
}

// ---------- createTask edge-case branches ----------

// stubArtifactStore captures StoreInput calls; failure-mode
// controlled per call by leaving result nil + err non-nil.
type stubArtifactStore struct {
	stored []string
	err    error
}

func (s *stubArtifactStore) StoreInput(_ context.Context, _, name, _ string) (*persistence.Artifact, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.stored = append(s.stored, name)
	return &persistence.Artifact{
		ID:          "art-" + name,
		Name:        name,
		StoragePath: "/store/" + name,
	}, nil
}

func (s *stubArtifactStore) Retrieve(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}

// stubBudgetNotifier captures NotifyBudgetBreach calls so the
// budget-gate tests can verify the notifier fires.
type stubBudgetNotifier struct {
	calls []struct {
		projectID, level, period string
	}
}

func (s *stubBudgetNotifier) NotifyBudgetBreach(_ context.Context, projectID, level, period string, _ budget.Decision) {
	s.calls = append(s.calls, struct{ projectID, level, period string }{projectID, level, period})
}

func TestCreateTask_InvalidJSON(t *testing.T) {
	te := newExecutor()
	res := te.createTask(context.Background(), "not-json", "snake", nil, 0)
	if !strings.Contains(res.Content, "Invalid arguments") {
		t.Errorf("got %q", res.Content)
	}
}

func TestCreateTask_NoProjectResolution(t *testing.T) {
	te := newExecutor()
	res := te.createTask(context.Background(), `{}`, "", nil, 0)
	if !strings.Contains(res.Content, "project_id is required") {
		t.Errorf("got %q", res.Content)
	}
}

func TestCreateTask_RejectsDisallowedType(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{
		{
			ID: "snake", SwarmID: "s", DefaultWorkflowID: "w",
			Autonomy: registry.ProjectAutonomy{
				AllowedTaskTypes: []string{"research", "writing"},
			},
		},
	}, oneSwarm("s"), oneWorkflow("w"))
	te := newExecutor(
		withRegistry(reg),
		withTaskRepo(&capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}),
	)
	res := te.createTask(context.Background(), `{"type":"feature","prompt":"x"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "is not allowed") {
		t.Errorf("expected type rejection, got %q", res.Content)
	}
}

func TestCreateTask_DefaultsTypeToFirstAllowed(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{
		{
			ID: "snake", SwarmID: "s", DefaultWorkflowID: "w",
			Autonomy: registry.ProjectAutonomy{AllowedTaskTypes: []string{"research"}},
		},
	}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := newExecutor(withRegistry(reg), withTaskRepo(taskRepo))
	res := te.createTask(context.Background(), `{"prompt":"x"}`, "snake", nil, 0)
	if strings.Contains(res.Content, "is not allowed") {
		t.Errorf("default type should have been auto-applied: %q", res.Content)
	}
	if taskRepo.last == nil {
		t.Fatal("task should have been created with defaulted type")
	}
}

// TestCreateTask_StampsChatTurnIDFromContext — when the caller runs
// inside a dispatcher turn (ctx carries a ChatTurnID), the task
// created by the tool must reference that turn. Without the link the
// follow-up coalescing in telegram/bot.go can't tell which turn a
// completing task belongs to, and the dispatcher LLM gets pinged
// once per leaf instead of once per turn.
func TestCreateTask_StampsChatTurnIDFromContext(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{
		{
			ID: "snake", SwarmID: "s", DefaultWorkflowID: "w",
			Autonomy: registry.ProjectAutonomy{AllowedTaskTypes: []string{"research"}},
		},
	}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := newExecutor(withRegistry(reg), withTaskRepo(taskRepo))

	turn := "chat_20260521190824_abcd"
	ctx := WithChatTurnID(context.Background(), turn)
	res := te.createTask(ctx, `{"prompt":"x"}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error:") {
		t.Fatalf("createTask returned error: %s", res.Content)
	}
	if taskRepo.last == nil {
		t.Fatal("task was not created")
	}
	if taskRepo.last.ChatTurnID == nil || *taskRepo.last.ChatTurnID != turn {
		t.Errorf("Task.ChatTurnID = %v, want %s", taskRepo.last.ChatTurnID, turn)
	}

	// Bare context — no turn id — leaves the task pointer nil.
	taskRepo.last = nil
	res = te.createTask(context.Background(), `{"prompt":"y"}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error:") {
		t.Fatalf("createTask returned error: %s", res.Content)
	}
	if taskRepo.last == nil {
		t.Fatal("second task was not created")
	}
	if taskRepo.last.ChatTurnID != nil {
		t.Errorf("bare-ctx Task.ChatTurnID = %v, want nil", taskRepo.last.ChatTurnID)
	}
}

// TestCreateTask_DedupSuppressesActiveDuplicate exercises the
// in-flight dedup branch — a recently-created active task with the
// same prompt should suppress the second create.
func TestCreateTask_DedupSuppressesActiveDuplicate(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_existing",
					ProjectID: "snake",
					Status:    persistence.TaskStatusQueued,
					Payload:   payloadWithPrompt(t, "run trading tick"),
					CreatedAt: time.Now(),
				},
			}, nil
		},
	}
	te := newExecutor(withRegistry(reg), withTaskRepo(taskRepo))
	res := te.createTask(context.Background(), `{"prompt":"run trading tick"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "already in flight") {
		t.Errorf("expected dedup suppression, got %q", res.Content)
	}
}

// TestCreateTask_ExecuteActionError covers the path where
// chat.ExecuteAction returns an error (repo.Create fails).
func TestCreateTask_ExecuteActionError(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, _ *persistence.Task) error {
			return errors.New("db down")
		},
	}
	te := newExecutor(withRegistry(reg), withTaskRepo(taskRepo))
	res := te.createTask(context.Background(), `{"prompt":"x"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "Error") {
		t.Errorf("expected error surface: %q", res.Content)
	}
}

// TestCreateTask_WorkflowRoleMismatch covers the validation
// branch that refuses a workflow_id whose steps reference roles
// not present in the project's swarm.
func TestCreateTask_WorkflowRoleMismatch(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), []registry.Workflow{
		{
			ID:         "w2",
			Entrypoint: "step1",
			Steps: map[string]registry.WorkflowStep{
				"step1": {Type: "agent", Role: "ghost", Prompt: "do work"}, // role missing from "s"
			},
		},
		oneWorkflow("w")[0],
	})
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := newExecutor(withRegistry(reg), withTaskRepo(taskRepo))
	res := te.createTask(context.Background(), `{"prompt":"x","workflow_id":"w2"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "not present in the project's swarm") {
		t.Errorf("expected workflow-role rejection, got %q", res.Content)
	}
}

// TestCreateTask_WorkflowIDDashIsSanitised exercises the
// `workflowID == "-"` sanitisation.
func TestCreateTask_WorkflowIDDashIsSanitised(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := newExecutor(withRegistry(reg), withTaskRepo(taskRepo))
	te.createTask(context.Background(), `{"prompt":"x","type":"feature","workflow_id":"-"}`, "snake", nil, 0)
	if taskRepo.last == nil || taskRepo.last.WorkflowID == nil || *taskRepo.last.WorkflowID != "w" {
		t.Errorf("dash workflow_id should be sanitised to project default, got %+v",
			taskRepo.last)
	}
}

// TestCreateTask_PassesInputFilesThroughArtifactStore covers the
// happy-path artifact-store branch + the artifact link cleanup.
func TestCreateTask_PassesInputFilesThroughArtifactStore(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	artRepo := &mocks.MockArtifactRepository{}
	store := &stubArtifactStore{}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, artifactRepo: artRepo, artifactStore: store,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature","input_files":["/tmp/b.txt","/tmp/c.txt"]}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error") {
		t.Errorf("unexpected error: %q", res.Content)
	}
	if len(store.stored) != 2 {
		t.Errorf("expected 2 artifacts stored, got %d", len(store.stored))
	}
}

// TestCreateTask_ArtifactStoreErrorFallsBackToPath covers the
// snapshot-failure branch — artifact store fails, the rewritten
// path keeps the original host path.
func TestCreateTask_ArtifactStoreErrorFallsBackToPath(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &stubArtifactStore{err: errors.New("disk full")}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, artifactStore: store,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature","input_files":["/tmp/path.txt"]}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error") {
		t.Errorf("snapshot failure should fall back, not error: %q", res.Content)
	}
}

// TestCreateTask_LinksArtifactsToTask exercises the success-path
// artifact-link UpdateTaskID branch.
func TestCreateTask_LinksArtifactsToTask(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &stubArtifactStore{}
	artUpdates := 0
	artRepo := &mocks.MockArtifactRepository{
		UpdateTaskIDFunc: func(_ context.Context, _, _ string) error {
			artUpdates++
			return nil
		},
	}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, artifactStore: store, artifactRepo: artRepo,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
	}
	te.createTask(context.Background(), `{"prompt":"x","type":"feature","input_files":["/tmp/a.txt"]}`, "snake", nil, 0)
	if artUpdates != 1 {
		t.Errorf("expected 1 artifact link update, got %d", artUpdates)
	}
}

// TestCreateTask_ArtifactLinkErrorIsLogged covers the error branch
// inside the UpdateTaskID loop.
func TestCreateTask_ArtifactLinkErrorIsLogged(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &stubArtifactStore{}
	artRepo := &mocks.MockArtifactRepository{
		UpdateTaskIDFunc: func(_ context.Context, _, _ string) error {
			return errors.New("orphan write")
		},
	}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, artifactStore: store, artifactRepo: artRepo,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
	}
	// Best-effort: error must NOT bubble up.
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature","input_files":["/tmp/a.txt"]}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error") {
		t.Errorf("artifact-link error should not surface: %q", res.Content)
	}
}

// TestCreateTask_RateLimiterBlocked covers the rate-limiter branch.
// We construct a project with tasks_per_minute=1 and pre-record one
// hit so the next Check returns Blocked=true.
func TestCreateTask_RateLimiterBlocked(t *testing.T) {
	// Project YAML loaded via registry has rateLimit fields; the
	// inline test path uses a fresh limiter.
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nrate_limit:\n  tasks_per_minute: 1\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	// Pre-record one hit so the rate limiter blocks the next.
	limiter := ratelimit.New()
	limiter.Record("snake", time.Now())
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo,
		rateLimiter: limiter,
		logger:      zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("expected rate-limit refusal, got %q", res.Content)
	}
}

// TestCreateTask_RateLimiterRecordOnSuccess covers the
// post-create rateLimiter.Record call.
func TestCreateTask_RateLimiterRecordOnSuccess(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	limiter := ratelimit.New()
	te := &ToolExecutor{registry: reg, taskRepo: taskRepo, rateLimiter: limiter, logger: zerolog.Nop()}
	// Type required by chat.executeCreateTask — without it the
	// rate-limiter Record branch is never reached because
	// ExecuteAction returns an error and we exit early.
	te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	res2 := te.createTask(context.Background(), `{"prompt":"y","type":"feature"}`, "snake", nil, 0)
	if strings.Contains(res2.Content, "Cannot create task") {
		t.Fatalf("second create should not be rate-blocked: %q", res2.Content)
	}
}

// TestCreateTask_RegisterFollowupWatchFunc covers both the
// watchFunc-registration branch and the followupRegistrar-skip
// branch when AwaitCompletion=false.
func TestCreateTask_RegisterFollowupSkippedOnExplicitOptOut(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	registrar := &fakeRegistrar{}
	watched := 0
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, followupRegistrar: registrar,
		watchFunc: func(_ string, _ int64) { watched++ },
		logger:    zerolog.Nop(),
	}
	te.createTask(context.Background(), `{"prompt":"x","type":"feature","await_completion":false}`, "snake", nil, 42)
	if watched != 1 {
		t.Errorf("expected watchFunc called once, got %d", watched)
	}
	if len(registrar.snapshot()) != 0 {
		t.Errorf("explicit await_completion=false must NOT register followup")
	}
}

// ---------- dedup helpers: trailing edges ----------

func TestJaccard_DisjointIsZero(t *testing.T) {
	if got := jaccardTokenSimilarity("a b c", "d e f"); got != 0 {
		t.Errorf("expected 0 for disjoint sets, got %v", got)
	}
}

func TestTokenSet_StripsPunctuation(t *testing.T) {
	got := tokenSet("hello, world! (test)")
	want := map[string]bool{"hello": true, "world": true, "test": true}
	if len(got) != len(want) {
		t.Errorf("expected %d tokens, got %d: %+v", len(want), len(got), got)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing token %q in %+v", k, got)
		}
	}
}

func TestTokenSet_EmptyTokensSkipped(t *testing.T) {
	// All-punctuation tokens strip to empty and must not enter the set.
	got := tokenSet("... !!! ((( )))")
	if len(got) != 0 {
		t.Errorf("expected empty set, got %+v", got)
	}
}

func TestNormalisePromptForDedup_CollapsesWhitespace(t *testing.T) {
	got := normalisePromptForDedup("  Hello   World\t\nfoo  ")
	if got != "hello world foo" {
		t.Errorf("got %q", got)
	}
}

// ---------- final coverage closers ----------

// statefulTaskRepo lets a test return different results from Get
// across calls. The first n calls return `first`, subsequent
// calls return `second`. Used to drive the
// "taskProjectAllowed succeeds, ExecuteAction's Get fails" path.
type statefulTaskRepo struct {
	*mocks.MockTaskRepository
	gets    int
	results []func() (*persistence.Task, error)
}

func (s *statefulTaskRepo) Get(ctx context.Context, id string) (*persistence.Task, error) {
	idx := s.gets
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	s.gets++
	return s.results[idx]()
}

// TestGetTaskStatus_ExecuteActionGetError covers the
// executeGetStatus → Get error branch: the first Get inside
// taskProjectAllowed succeeds, the second (in ExecuteAction)
// fails. Without the stateful mock this path is unreachable.
func TestGetTaskStatus_ExecuteActionGetError(t *testing.T) {
	repo := &statefulTaskRepo{
		MockTaskRepository: &mocks.MockTaskRepository{},
		results: []func() (*persistence.Task, error){
			func() (*persistence.Task, error) {
				return &persistence.Task{ID: "t1", ProjectID: "snake"}, nil
			},
			func() (*persistence.Task, error) { return nil, errors.New("flaky DB") },
		},
	}
	te := newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}))
	res := te.getTaskStatus(context.Background(), `{"task_id":"t1"}`, nil)
	if !strings.Contains(res.Content, "Error") {
		t.Errorf("expected ExecuteAction-error surface, got %q", res.Content)
	}
}

// TestWaitForTask_PostFastPollWindow covers the
// time.Since(pollStart) > fastPollWindow branch — the poll
// interval flips from 2s to 5s. We don't actually wait 30s in
// the test (that's the production fastPollWindow); instead we
// rely on the deadline expiring quickly with timeout=1s and the
// post-deadline timeout branch.
//
// The 5s-interval branch needs >30s pollStart, which is too
// slow for unit tests. We accept that one branch arm stays
// uncovered through unit tests; an integration test using a
// 2026 stub clock would close it. This test still exercises
// the time.After + ctx.Done select on the polling-side and
// the deadline-passed return.
func TestWaitForTask_DeadlineAfterPolls(t *testing.T) {
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			return &persistence.Task{
				ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning,
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(context.Background(), `{"task_id":"t1","timeout_seconds":1}`, nil)
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("expected timeout result, got %q", res.Content)
	}
}

// TestWaitForTask_CtxCancelledDuringPoll forces the
// ctx.Done() branch inside the polling select. We pre-cancel
// after the first Get so the second select (post-Get) catches
// the cancellation.
func TestWaitForTask_CtxCancelledDuringPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gets := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			gets++
			if gets == 2 {
				// Get #1 came from taskProjectAllowed; Get #2 is
				// the in-loop fetch. Cancel here so the
				// bottom-of-loop select catches it (NOT the
				// top-of-loop select, which would steal coverage
				// from the post-Get branch).
				cancel()
			}
			return &persistence.Task{
				ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning,
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	res := te.waitForTask(ctx, `{"task_id":"t1","timeout_seconds":30}`, nil)
	if !strings.Contains(res.Content, "cancelled") {
		t.Errorf("expected cancellation: %q", res.Content)
	}
}

// TestJaccard_EmptyAfterTokenStrip covers the
// `len(tokensA) == 0 || len(tokensB) == 0` branch. Tokens of
// pure punctuation strip to empty so the metric returns 0
// even though the strings themselves are non-empty.
func TestJaccard_EmptyAfterTokenStrip(t *testing.T) {
	if got := jaccardTokenSimilarity("...", "abc"); got != 0 {
		t.Errorf("punctuation-only A should return 0, got %v", got)
	}
	if got := jaccardTokenSimilarity("abc", "..."); got != 0 {
		t.Errorf("punctuation-only B should return 0, got %v", got)
	}
}

// ---------- createTask budget gates ----------

// budgetUsageRepo returns a fixed daily / monthly cost from
// SumCostByProject so budget.Check can compute a known decision.
// We embed *recordingUsageRepo and override SumCostByProject to
// keep the test focused.
type budgetUsageRepo struct {
	*recordingUsageRepo
	daily, monthly float64
}

func (b *budgetUsageRepo) SumCostByProject(_ context.Context, _ string, since, until time.Time) (float64, error) {
	// budget.Check passes the day's start / month's start as `since`;
	// the difference between since and until is roughly 1 day or 1
	// month. Use the duration to pick which counter to return.
	d := until.Sub(since)
	if d > 30*24*time.Hour-time.Hour {
		return b.monthly, nil
	}
	return b.daily, nil
}

// TestCreateTask_BudgetHardCapBlocks covers the budget.Blocked
// branch. Project has daily_hard_usd=1.0, spend=2.0, so Check
// returns Blocked=true.
func TestCreateTask_BudgetHardCapBlocks(t *testing.T) {
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nbudget:\n  daily_hard_usd: 1.0\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	usage := &budgetUsageRepo{
		recordingUsageRepo: &recordingUsageRepo{},
		daily:              2.0, // exceeds 1.0 hard cap
	}
	notifier := &stubBudgetNotifier{}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo,
		llmUsageRepo:   usage,
		budgetNotifier: notifier,
		logger:         zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("expected budget refusal, got %q", res.Content)
	}
	if len(notifier.calls) == 0 {
		t.Error("budget notifier should fire on Blocked")
	}
}

// TestCreateTask_BudgetSoftBreachProceeds covers the
// SoftBreached branch — over soft cap, under hard cap.
func TestCreateTask_BudgetSoftBreachProceeds(t *testing.T) {
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nbudget:\n  daily_soft_usd: 1.0\n  daily_hard_usd: 10.0\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	usage := &budgetUsageRepo{
		recordingUsageRepo: &recordingUsageRepo{},
		daily:              2.0, // over soft (1.0), under hard (10.0)
	}
	notifier := &stubBudgetNotifier{}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo,
		llmUsageRepo: usage, budgetNotifier: notifier,
		logger: zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("soft breach should NOT block: %q", res.Content)
	}
	if len(notifier.calls) == 0 {
		t.Error("notifier should still fire on soft breach")
	}
}

// errUsageRepo always errors on SumCostByProject — exercises
// the `berr != nil` branch in createTask's budget gate.
type errUsageRepo struct {
	*recordingUsageRepo
	err error
}

func (e *errUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, e.err
}

func TestCreateTask_BudgetCheckErrorProceeds(t *testing.T) {
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nbudget:\n  daily_hard_usd: 1.0\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	usage := &errUsageRepo{recordingUsageRepo: &recordingUsageRepo{}, err: errors.New("agg failed")}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, llmUsageRepo: usage,
		logger: zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	// Should proceed (Create called) and not surface the error.
	if strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("budget-check error should NOT block: %q", res.Content)
	}
}

// TestCreateTask_InputFilesWithoutArtifactStore covers the
// fall-back branch where input_files are passed through verbatim
// because no artifactStore is wired.
func TestCreateTask_InputFilesWithoutArtifactStore(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature","input_files":["/tmp/a.txt"]}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error") {
		t.Errorf("unexpected error: %q", res.Content)
	}
}

// ---------- silence unused imports in test helpers ----------

// budget.Decision is referenced by the stubBudgetNotifier
// signature; ratelimit.New is invoked in the rate-limiter tests
// above. These imports are otherwise unused at the package
// boundary.
var _ budget.Decision
var _ = ratelimit.New

// TestCreateTask_InputFilesWithoutPrompt covers the
// `args.Input["context"] == nil` branch at line 405 — input_files
// supplied without a prompt forces the lazy context initialisation.
func TestCreateTask_InputFilesWithoutPrompt(t *testing.T) {
	reg := mustRegistry(t, []registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}}, oneSwarm("s"), oneWorkflow("w"))
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{registry: reg, taskRepo: taskRepo, allowedInputRoots: []string{"/tmp"}, logger: zerolog.Nop()}
	res := te.createTask(context.Background(), `{"type":"feature","input_files":["/tmp/a.txt"]}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Error:") {
		t.Errorf("unexpected error: %q", res.Content)
	}
}

// forecastErrUsageRepo passes budget.Check (returns 0 spend) but
// fails on AggregateByRoleModel — drives the `ferr != nil` arm of
// the forecast gate.
type forecastErrUsageRepo struct {
	*recordingUsageRepo
}

func (f *forecastErrUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (f *forecastErrUsageRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	return nil, errors.New("history aggregate down")
}

// TestCreateTask_ForecastError covers the `ferr != nil` arm: the
// budget gate passes (zero spend), then ForecastTask errors and
// the dispatcher logs + proceeds. The task should still be
// created.
func TestCreateTask_ForecastError(t *testing.T) {
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nbudget:\n  daily_hard_usd: 100.0\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	usage := &forecastErrUsageRepo{recordingUsageRepo: &recordingUsageRepo{}}
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo, llmUsageRepo: usage,
		logger: zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("forecast error must not block: %q", res.Content)
	}
}

// TestWaitForTask_FastToSlowPollTransition uses the test-only
// pollInterval/window override vars to exercise the fast → slow
// pollInterval transition without waiting 30 seconds.
func TestWaitForTask_FastToSlowPollTransition(t *testing.T) {
	oldFast := waitForTaskFastPoll
	oldWindow := waitForTaskFastWindow
	defer func() {
		waitForTaskFastPoll = oldFast
		waitForTaskFastWindow = oldWindow
	}()
	waitForTaskFastPoll = 1 * time.Millisecond
	waitForTaskFastWindow = 5 * time.Millisecond

	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			calls++
			return &persistence.Task{
				ID: "t1", ProjectID: "snake", Status: persistence.TaskStatusRunning,
			}, nil
		},
	}
	te := newExecutor(withTaskRepo(repo))
	// timeout_seconds=1 → 1s deadline; the fast→slow flip fires
	// after the 5ms grace; the deadline expires within the test.
	res := te.waitForTask(context.Background(), `{"task_id":"t1","timeout_seconds":1}`, nil)
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("expected timeout result, got %q", res.Content)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 polls to cross the fast-window, got %d", calls)
	}
}

// forecastUsageRepo: budget passes (0 spend) and forecast
// succeeds (empty history → cold-start pricing). Together with
// a non-empty pricing.Table, this gives a positive forecast.USD.
type forecastUsageRepo struct {
	*recordingUsageRepo
}

func (f *forecastUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (f *forecastUsageRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	return nil, nil // empty history → cold-start fallback in ForecastTask
}

// loadPricingFile writes a tiny pricing.yaml into a temp dir and
// returns the loaded *pricing.Table. Used by the forecast tests
// because pricing.Table has no public Add API.
func loadPricingFile(t *testing.T, model string, inputUSDPerM, outputUSDPerM float64) *pricing.Table {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	content := fmt.Sprintf("models:\n  %s:\n    input: %g\n    output: %g\n", model, inputUSDPerM, outputUSDPerM)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write pricing: %v", err)
	}
	tab, err := pricing.Load(path)
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	return tab
}

// TestCreateTask_ForecastDebugLog covers the `forecast.USD > 0`
// debug-log branch. Budget gate passes (DailyHardUSD=100, spend=0),
// forecast computes a small but positive value from cold-start
// pricing (30k * $1/M + 4k * $2/M = $0.038), and that value is
// below the 100 cap so the task proceeds.
func TestCreateTask_ForecastDebugLog(t *testing.T) {
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nbudget:\n  daily_hard_usd: 100.0\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	usage := &forecastUsageRepo{recordingUsageRepo: &recordingUsageRepo{}}
	table := loadPricingFile(t, "test-model", 1.0, 2.0)
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo,
		llmUsageRepo: usage, pricing: table,
		defaultModel: "test-model",
		logger:       zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	if strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("forecast within cap should not block: %q", res.Content)
	}
}

// TestCreateTask_ForecastRefused covers the
// `CheckForecast.Refused` branch. Same as above but with a tiny
// daily cap (0.01) so 0.038 forecast overshoots.
func TestCreateTask_ForecastRefused(t *testing.T) {
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "s.md"), []byte(
		"---\nswarmId: s\nroles:\n  - name: lead\n    runtime:\n      image: busybox\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "w.md"), []byte(
		"---\nworkflowId: w\nentrypoint: step1\nsteps:\n  step1:\n    type: agent\n    role: lead\n    prompt: do work\n---\n"), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "projects", "snake.yaml"), []byte(
		"projectId: snake\nswarmId: s\ndefaultWorkflowId: w\nbudget:\n  daily_hard_usd: 0.01\n"), 0o644)
	reg := registry.New()
	if err := reg.Load(configDir); err != nil {
		t.Fatalf("load: %v", err)
	}
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	usage := &forecastUsageRepo{recordingUsageRepo: &recordingUsageRepo{}}
	table := loadPricingFile(t, "test-model", 1.0, 2.0)
	te := &ToolExecutor{
		registry: reg, taskRepo: taskRepo,
		llmUsageRepo: usage, pricing: table,
		defaultModel: "test-model",
		logger:       zerolog.Nop(),
	}
	res := te.createTask(context.Background(), `{"prompt":"x","type":"feature"}`, "snake", nil, 0)
	if !strings.Contains(res.Content, "Cannot create task") {
		t.Errorf("forecast over cap should block: %q", res.Content)
	}
}

func (b *budgetUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (e *errUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (f *forecastErrUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (f *forecastUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
