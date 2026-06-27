package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// signedGitHubRequest builds an httptest request bearing a valid
// X-Hub-Signature-256 header for the given body — local copy of
// the helper in internal/github (unexported there) so this slice's
// integration tests can drive HandleWebhook directly.
func signedGitHubRequest(secret, event, delivery string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	return req
}

// recordingTaskRepo wraps the persistence-package mock with a
// concurrent-safe slice of every successfully created task and an
// in-memory idempotency map. It satisfies persistence.TaskRepository
// via the embedded *mocks.MockTaskRepository (all unscripted methods
// return zero values) and adds the small amount of stateful
// behaviour the adapter exercises:
//
//   - GetByIdempotencyKey looks up the in-memory map.
//   - Create rejects duplicate idempotency keys so the adapter's
//     race-recovery branch can be tested.
//   - createCalls is a public counter for assertion convenience.
type recordingTaskRepo struct {
	*mocks.MockTaskRepository
	mu          sync.Mutex
	tasks       []*persistence.Task
	byIdem      map[string]*persistence.Task
	createCalls atomic.Int64
}

func newRecordingTaskRepo() *recordingTaskRepo {
	r := &recordingTaskRepo{
		MockTaskRepository: &mocks.MockTaskRepository{},
		byIdem:             map[string]*persistence.Task{},
	}
	r.CreateFunc = r.create
	r.GetByIdempotencyKeyFunc = r.getByIdempotencyKey
	return r
}

func (r *recordingTaskRepo) create(_ context.Context, t *persistence.Task) error {
	r.createCalls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.IdempotencyKey != nil {
		if _, dup := r.byIdem[*t.IdempotencyKey]; dup {
			return errors.New("duplicate idempotency key")
		}
		r.byIdem[*t.IdempotencyKey] = t
	}
	r.tasks = append(r.tasks, t)
	return nil
}

func (r *recordingTaskRepo) getByIdempotencyKey(_ context.Context, _, key string) (*persistence.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.byIdem[key]; ok {
		return t, nil
	}
	return nil, persistence.ErrNotFound
}

func (r *recordingTaskRepo) snapshotTasks() []*persistence.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*persistence.Task, len(r.tasks))
	copy(out, r.tasks)
	return out
}

// projectForTaskCreator returns a Project with the minimum fields
// the adapter exercises: ID for project_id, DefaultPriority for
// priority, DefaultWorkflowID for workflow_id.
func projectForTaskCreator(id string) *registry.Project {
	return &registry.Project{
		ID:                id,
		SwarmID:           "s-1",
		DefaultWorkflowID: "w-1",
		DefaultPriority:   30,
	}
}

// TestGitHubTaskCreator_Create_NilReceiverReturnsError — defensive:
// a zero-value pointer receiver must not panic, returns a clear
// error.
func TestGitHubTaskCreator_Create_NilReceiverReturnsError(t *testing.T) {
	var g *githubTaskCreator
	err := g.Create(context.Background(), github.TaskCreationEvent{Kind: "issues.labeled"})
	if err == nil {
		t.Fatal("expected error on nil receiver")
	}
}

// TestGitHubTaskCreator_Create_NilTaskRepoReturnsError — when the
// service container builds the adapter before the repo is wired,
// the Create path surfaces a clean error rather than NPE.
func TestGitHubTaskCreator_Create_NilTaskRepoReturnsError(t *testing.T) {
	g := newGitHubTaskCreator(nil, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		IdempotencyKey: "github-app:d-1",
	})
	if err == nil || !strings.Contains(err.Error(), "task repository") {
		t.Errorf("err = %v, want 'task repository is not configured'", err)
	}
}

// TestGitHubTaskCreator_Create_NilProjectReturnsError — same
// defensive check on the pinned project — without one we don't
// know where to land the row.
func TestGitHubTaskCreator_Create_NilProjectReturnsError(t *testing.T) {
	g := newGitHubTaskCreator(newRecordingTaskRepo(), nil, nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:   "issues.labeled",
		Labels: []string{"vornik-task"},
	})
	if err == nil || !strings.Contains(err.Error(), "no project") {
		t.Errorf("err = %v, want 'no project pinned'", err)
	}
}

// TestGitHubTaskCreator_IssuesLabeled_CreatesTask — happy path:
// a wired adapter receives an event and the underlying repo gets
// a Task row with every important column populated.
func TestGitHubTaskCreator_IssuesLabeled_CreatesTask(t *testing.T) {
	repo := newRecordingTaskRepo()
	proj := projectForTaskCreator("p-1")
	g := newGitHubTaskCreator(repo, proj, nil, zerolog.Nop())

	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		SessionID:      "acme/api#issues/5",
		Title:          "bug",
		Body:           "details",
		Labels:         []string{"vornik-task", "urgent"},
		SenderLogin:    "vadim",
		Repo:           "acme/api",
		Number:         5,
		InstallationID: 9000,
		IdempotencyKey: "github-app:d-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks created = %d, want 1", len(tasks))
	}
	tk := tasks[0]
	if tk.ProjectID != "p-1" {
		t.Errorf("ProjectID = %q, want p-1", tk.ProjectID)
	}
	if tk.Status != persistence.TaskStatusQueued {
		t.Errorf("Status = %q, want QUEUED", tk.Status)
	}
	if tk.Priority != 30 {
		t.Errorf("Priority = %d, want 30 (project default)", tk.Priority)
	}
	if tk.WorkflowID == nil || *tk.WorkflowID != "w-1" {
		t.Errorf("WorkflowID = %v, want pointer to w-1", tk.WorkflowID)
	}
	if tk.IdempotencyKey == nil || *tk.IdempotencyKey != "github-app:d-1" {
		t.Errorf("IdempotencyKey = %v, want pointer to github-app:d-1", tk.IdempotencyKey)
	}
	if tk.CreationSource != persistence.TaskCreationSourceUser {
		t.Errorf("CreationSource = %q, want USER", tk.CreationSource)
	}
	if tk.Attempt != 1 || tk.MaxAttempts != 3 {
		t.Errorf("Attempt/MaxAttempts = %d/%d, want 1/3", tk.Attempt, tk.MaxAttempts)
	}

	// Payload contents — task type is the first label, source tag
	// is github_app, github sub-object carries the event verbatim.
	var payload map[string]any
	if err := json.Unmarshal(tk.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got := payload["taskType"]; got != "vornik-task" {
		t.Errorf("payload.taskType = %v, want vornik-task", got)
	}
	if got := payload["source"]; got != githubTaskCreatorSource {
		t.Errorf("payload.source = %v, want github_app", got)
	}
	gh, _ := payload["github"].(map[string]any)
	if gh == nil {
		t.Fatalf("payload.github missing: %v", payload)
	}
	if gh["repo"] != "acme/api" || gh["kind"] != "issues.labeled" {
		t.Errorf("payload.github mismatch: %+v", gh)
	}
	if gh["sender_login"] != "vadim" {
		t.Errorf("payload.github.sender_login = %v, want vadim", gh["sender_login"])
	}
	if gh["installation_id"].(float64) != 9000 {
		t.Errorf("payload.github.installation_id = %v, want 9000", gh["installation_id"])
	}
	ctxObj, _ := payload["context"].(map[string]any)
	if ctxObj == nil || !strings.Contains(ctxObj["prompt"].(string), "bug") {
		t.Errorf("payload.context.prompt should contain title: %+v", ctxObj)
	}
}

// TestGitHubTaskCreator_ReplyWorkflowID_Overrides confirms a
// github_app.reply_workflow_id override routes the created task to
// the configured workflow instead of the project default (N7).
func TestGitHubTaskCreator_ReplyWorkflowID_Overrides(t *testing.T) {
	repo := newRecordingTaskRepo()
	proj := projectForTaskCreator("p-1")
	proj.GitHubApp.ReplyWorkflowID = "reply-wf"
	g := newGitHubTaskCreator(repo, proj, nil, zerolog.Nop())

	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		Repo:           "acme/api",
		IdempotencyKey: "github-app:d-reply",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks created = %d, want 1", len(tasks))
	}
	if tk := tasks[0]; tk.WorkflowID == nil || *tk.WorkflowID != "reply-wf" {
		t.Errorf("WorkflowID = %v, want pointer to reply-wf (the override)", tk.WorkflowID)
	}
	// Payload's workflow_id must match the resolved override too.
	var payload map[string]any
	if err := json.Unmarshal(tasks[0].Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got := payload["workflowId"]; got != "reply-wf" {
		t.Errorf("payload.workflowId = %v, want reply-wf", got)
	}
}

// TestGitHubTaskCreator_ReplyWorkflowID_FallsBackToDefault confirms
// that with no override, the task still runs under the project
// default workflow (N7 fallback).
func TestGitHubTaskCreator_ReplyWorkflowID_FallsBackToDefault(t *testing.T) {
	repo := newRecordingTaskRepo()
	proj := projectForTaskCreator("p-1") // DefaultWorkflowID = w-1, no override
	g := newGitHubTaskCreator(repo, proj, nil, zerolog.Nop())

	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		Repo:           "acme/api",
		IdempotencyKey: "github-app:d-default",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks created = %d, want 1", len(tasks))
	}
	if tk := tasks[0]; tk.WorkflowID == nil || *tk.WorkflowID != "w-1" {
		t.Errorf("WorkflowID = %v, want pointer to w-1 (project default)", tk.WorkflowID)
	}
}

// TestGitHubTaskCreator_PullRequestOpened_FixedReviewType — PR-
// opened events always produce a "review" task regardless of any
// label set.
func TestGitHubTaskCreator_PullRequestOpened_FixedReviewType(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "pull_request.opened",
		SessionID:      "acme/api#pulls/12",
		Title:          "PR title",
		Body:           "PR body",
		Labels:         []string{"needs-review", "documentation"},
		SenderLogin:    "alice",
		Repo:           "acme/api",
		Number:         12,
		InstallationID: 9001,
		IdempotencyKey: "github-app:d-pr",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	var payload map[string]any
	_ = json.Unmarshal(tasks[0].Payload, &payload)
	if got := payload["taskType"]; got != pullRequestReviewTaskType {
		t.Errorf("taskType = %v, want %q", got, pullRequestReviewTaskType)
	}
}

// TestGitHubTaskCreator_LabelMapping_OverridesLabelName — when an
// operator supplies a mapping, the matched label resolves to the
// mapped task type rather than the literal label name.
func TestGitHubTaskCreator_LabelMapping_OverridesLabelName(t *testing.T) {
	repo := newRecordingTaskRepo()
	mapping := map[string]string{"vornik-task": "investigate"}
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), mapping, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		Repo:           "acme/api",
		Number:         1,
		IdempotencyKey: "github-app:d-map",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	var payload map[string]any
	_ = json.Unmarshal(tasks[0].Payload, &payload)
	if got := payload["taskType"]; got != "investigate" {
		t.Errorf("taskType = %v, want investigate (mapped)", got)
	}
}

// TestGitHubTaskCreator_LabelMapping_EmptyValueFallsBackToLabel —
// a configured-but-empty mapping entry falls back to the literal
// label name rather than producing an empty task type.
func TestGitHubTaskCreator_LabelMapping_EmptyValueFallsBackToLabel(t *testing.T) {
	repo := newRecordingTaskRepo()
	mapping := map[string]string{"vornik-task": "   "} // whitespace-only
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), mapping, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		Repo:           "acme/api",
		Number:         1,
		IdempotencyKey: "github-app:d-empty-map",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	var payload map[string]any
	_ = json.Unmarshal(tasks[0].Payload, &payload)
	if got := payload["taskType"]; got != "vornik-task" {
		t.Errorf("taskType = %v, want vornik-task (fallback)", got)
	}
}

// TestGitHubTaskCreator_IssuesLabeled_NoLabels_ReturnsError —
// malformed event with empty Labels: surface as error so the
// channel logs the rejection.
func TestGitHubTaskCreator_IssuesLabeled_NoLabels_ReturnsError(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         nil,
		IdempotencyKey: "github-app:d-empty",
	})
	if err == nil {
		t.Fatal("expected error on labels-empty event")
	}
	if len(repo.snapshotTasks()) != 0 {
		t.Error("repo got a row despite the error")
	}
}

// TestGitHubTaskCreator_IssuesLabeled_EmptyMatchedLabel_ReturnsError —
// defensive: a non-empty Labels slice whose first entry is a
// whitespace-only string still produces an error rather than an
// empty task type.
func TestGitHubTaskCreator_IssuesLabeled_EmptyMatchedLabel_ReturnsError(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"   "},
		IdempotencyKey: "github-app:d-blank-label",
	})
	if err == nil || !strings.Contains(err.Error(), "empty matched label") {
		t.Errorf("err = %v, want 'empty matched label'", err)
	}
}

// TestGitHubTaskCreator_UnknownKind_ReturnsError — defensive: a
// future trigger that hasn't been added here should produce an
// error rather than silently land an unfiltered row.
func TestGitHubTaskCreator_UnknownKind_ReturnsError(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.assigned",
		Labels:         []string{"x"},
		IdempotencyKey: "github-app:d-?",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported event kind") {
		t.Errorf("err = %v, want 'unsupported event kind'", err)
	}
}

// TestGitHubTaskCreator_Idempotent_DuplicateDelivery — the same
// IdempotencyKey twice creates exactly one row. Mirrors GitHub's
// at-least-once delivery contract.
func TestGitHubTaskCreator_Idempotent_DuplicateDelivery(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	ev := github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		Repo:           "acme/api",
		Number:         5,
		IdempotencyKey: "github-app:d-dup",
	}
	if err := g.Create(context.Background(), ev); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := g.Create(context.Background(), ev); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if got := repo.createCalls.Load(); got != 1 {
		t.Errorf("repo.Create called %d times, want 1", got)
	}
	if len(repo.snapshotTasks()) != 1 {
		t.Errorf("tasks stored = %d, want 1", len(repo.snapshotTasks()))
	}
}

// TestGitHubTaskCreator_DuplicateKeyRace_RecoversFromGet —
// post-Create error path: lookup misses (lookup race), Create
// fails with a duplicate-key error, second lookup finds the
// concurrently-inserted row and the adapter treats it as success.
func TestGitHubTaskCreator_DuplicateKeyRace_RecoversFromGet(t *testing.T) {
	idem := "github-app:d-race"
	mock := &mocks.MockTaskRepository{}
	var getCalls atomic.Int64
	preexisting := &persistence.Task{ID: "task-pre", IdempotencyKey: &idem, ProjectID: "p-1"}
	mock.GetByIdempotencyKeyFunc = func(_ context.Context, _, k string) (*persistence.Task, error) {
		n := getCalls.Add(1)
		if n == 1 {
			// First lookup (pre-Create): miss so the adapter
			// proceeds to insert.
			return nil, persistence.ErrNotFound
		}
		// Second lookup (post-Create-error): find the row
		// the racing inserter committed.
		return preexisting, nil
	}
	mock.CreateFunc = func(_ context.Context, _ *persistence.Task) error {
		return errors.New("duplicate key value violates unique constraint")
	}
	g := newGitHubTaskCreator(mock, projectForTaskCreator("p-1"), nil, zerolog.Nop())

	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		IdempotencyKey: idem,
	})
	if err != nil {
		t.Fatalf("Create after race: %v", err)
	}
	if getCalls.Load() < 2 {
		t.Errorf("GetByIdempotencyKey called %d times, want >=2 (lookup + recovery)", getCalls.Load())
	}
}

// TestGitHubTaskCreator_CreateError_NonDuplicate_Propagates — a
// real Create failure that ISN'T a duplicate-key race must
// propagate up so the channel logs it.
func TestGitHubTaskCreator_CreateError_NonDuplicate_Propagates(t *testing.T) {
	mock := &mocks.MockTaskRepository{}
	mock.GetByIdempotencyKeyFunc = func(_ context.Context, _, _ string) (*persistence.Task, error) {
		return nil, persistence.ErrNotFound
	}
	mock.CreateFunc = func(_ context.Context, _ *persistence.Task) error {
		return errors.New("backend down")
	}
	g := newGitHubTaskCreator(mock, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		IdempotencyKey: "github-app:d-fail",
	})
	if err == nil || !strings.Contains(err.Error(), "create github task") {
		t.Errorf("err = %v, want create-failure", err)
	}
}

// TestGitHubTaskCreator_NoIdempotencyKey_Inserts — a missing key
// (defensive: should never happen in practice, the channel always
// sets it) still produces a row. The lookup-then-insert path
// degrades to plain insert.
func TestGitHubTaskCreator_NoIdempotencyKey_Inserts(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:   "issues.labeled",
		Labels: []string{"vornik-task"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(repo.snapshotTasks()) != 1 {
		t.Errorf("tasks = %d, want 1", len(repo.snapshotTasks()))
	}
	if repo.snapshotTasks()[0].IdempotencyKey != nil {
		t.Errorf("IdempotencyKey = %v, want nil when not provided", repo.snapshotTasks()[0].IdempotencyKey)
	}
}

// TestGitHubTaskCreator_BuildPrompt_TitleFallback — when the
// title is empty, the prompt synthesizes a "PR #N" / "issue #N"
// stub so the runtime always has something to read.
func TestGitHubTaskCreator_BuildPrompt_TitleFallback(t *testing.T) {
	cases := []struct {
		name string
		ev   github.TaskCreationEvent
		want string
	}{
		{
			name: "issue with title and body",
			ev:   github.TaskCreationEvent{Kind: "issues.labeled", Title: "bug", Body: "details", Repo: "acme/api", Number: 5},
			want: "bug\n\ndetails",
		},
		{
			name: "issue with title only",
			ev:   github.TaskCreationEvent{Kind: "issues.labeled", Title: "bug", Repo: "acme/api", Number: 5},
			want: "bug",
		},
		{
			name: "issue with neither",
			ev:   github.TaskCreationEvent{Kind: "issues.labeled", Repo: "acme/api", Number: 5},
			want: "issue #5 in acme/api",
		},
		{
			name: "PR with neither",
			ev:   github.TaskCreationEvent{Kind: "pull_request.opened", Repo: "acme/api", Number: 12},
			want: "PR #12 in acme/api",
		},
		{
			name: "unknown kind fallback",
			ev:   github.TaskCreationEvent{Kind: "issues.assigned", Repo: "acme/api", Number: 9},
			want: "issues.assigned #9 in acme/api",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGitHubPrompt(tc.ev)
			if got != tc.want {
				t.Errorf("buildGitHubPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGitHubTaskCreator_ImplementsInterface — compile-time + run-time
// guard against drift on the github.TaskCreator contract.
func TestGitHubTaskCreator_ImplementsInterface(t *testing.T) {
	var _ github.TaskCreator = (*githubTaskCreator)(nil)
}

// TestTaskCreatorFromRepo_NilRepoReturnsNilCreator — defensive:
// when the container hasn't wired a task repo yet, the closure
// returns nil so the channel logs "TaskCreator not wired".
func TestTaskCreatorFromRepo_NilRepoReturnsNilCreator(t *testing.T) {
	factory := taskCreatorFromRepo(nil, nil, zerolog.Nop())
	if tc := factory(projectForTaskCreator("p-1")); tc != nil {
		t.Errorf("factory(p) = %v with nil repo, want nil", tc)
	}
}

// TestTaskCreatorFromRepo_NilProjectReturnsNilCreator — same
// for an unpinned project.
func TestTaskCreatorFromRepo_NilProjectReturnsNilCreator(t *testing.T) {
	factory := taskCreatorFromRepo(newRecordingTaskRepo(), nil, zerolog.Nop())
	if tc := factory(nil); tc != nil {
		t.Errorf("factory(nil) = %v, want nil", tc)
	}
}

// TestTaskCreatorFromRepo_BuildsRealCreator — happy path:
// both repo + project present produces a working adapter.
func TestTaskCreatorFromRepo_BuildsRealCreator(t *testing.T) {
	repo := newRecordingTaskRepo()
	factory := taskCreatorFromRepo(repo, nil, zerolog.Nop())
	tc := factory(projectForTaskCreator("p-1"))
	if tc == nil {
		t.Fatal("factory returned nil for fully wired input")
	}
	err := tc.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"vornik-task"},
		IdempotencyKey: "github-app:d-factory",
	})
	if err != nil {
		t.Errorf("Create: %v", err)
	}
	if len(repo.snapshotTasks()) != 1 {
		t.Errorf("tasks created = %d, want 1", len(repo.snapshotTasks()))
	}
}

// TestBuildGitHubChannelWithTaskCreator_WiresIntoChannel — the
// integration test the user asked for: drive the channel's
// HandleWebhook with a signed issues.labeled delivery and assert
// a task lands in the fake repo. Validates both the buildGitHub*
// closure plumbing AND that the channel's TaskCreator field is
// invoked end-to-end.
func TestBuildGitHubChannelWithTaskCreator_WiresIntoChannel(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	repo := newRecordingTaskRepo()
	proj := inboundOnlyProject("p-1")
	proj.GitHubApp.TaskLabels = []string{"vornik-task"}

	ch, picked, err := buildGitHubChannelWithTaskCreator(
		[]*registry.Project{proj},
		taskCreatorFromRepo(repo, nil, zerolog.Nop()),
	)
	if err != nil {
		t.Fatalf("buildGitHubChannelWithTaskCreator: %v", err)
	}
	if ch == nil || picked == nil {
		t.Fatalf("build returned (%v, %v)", ch, picked)
	}

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 9000},
		"issue": {"number": 5, "title": "bug", "body": "details", "labels": [{"name": "vornik-task"}]},
		"label": {"name": "vornik-task"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedGitHubRequest("shhh", "issues", "d-int-1", body))
	if rec.Code != http.StatusOK {
		t.Errorf("HTTP = %d, want 200", rec.Code)
	}

	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks created = %d, want 1", len(tasks))
	}
	if tasks[0].ProjectID != "p-1" {
		t.Errorf("task.ProjectID = %q, want p-1", tasks[0].ProjectID)
	}
	if tasks[0].IdempotencyKey == nil || *tasks[0].IdempotencyKey != "github-app:d-int-1" {
		t.Errorf("IdempotencyKey = %v, want github-app:d-int-1", tasks[0].IdempotencyKey)
	}
}

// TestBuildGitHubChannelWithTaskCreator_PRPath — same end-to-end
// validation for the pull_request.opened trigger; the resulting
// task carries the fixed "review" task type.
func TestBuildGitHubChannelWithTaskCreator_PRPath(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	repo := newRecordingTaskRepo()
	proj := inboundOnlyProject("p-1")

	ch, _, err := buildGitHubChannelWithTaskCreator(
		[]*registry.Project{proj},
		taskCreatorFromRepo(repo, nil, zerolog.Nop()),
	)
	if err != nil {
		t.Fatalf("buildGitHubChannelWithTaskCreator: %v", err)
	}

	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "alice"},
		"installation": {"id": 9001},
		"pull_request": {"number": 12, "title": "fix flake", "body": "ok"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedGitHubRequest("shhh", "pull_request", "d-int-pr", body))
	if rec.Code != http.StatusOK {
		t.Errorf("HTTP = %d, want 200", rec.Code)
	}

	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	var payload map[string]any
	_ = json.Unmarshal(tasks[0].Payload, &payload)
	if got := payload["taskType"]; got != pullRequestReviewTaskType {
		t.Errorf("taskType = %v, want review", got)
	}
}

// TestBuildGitHubChannelWithTaskCreator_IdempotentRetry — the
// same webhook delivered twice (e.g. GitHub retried after a
// transient timeout) creates exactly one task.
func TestBuildGitHubChannelWithTaskCreator_IdempotentRetry(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	repo := newRecordingTaskRepo()
	proj := inboundOnlyProject("p-1")
	proj.GitHubApp.TaskLabels = []string{"vornik-task"}

	ch, _, err := buildGitHubChannelWithTaskCreator(
		[]*registry.Project{proj},
		taskCreatorFromRepo(repo, nil, zerolog.Nop()),
	)
	if err != nil {
		t.Fatalf("buildGitHubChannelWithTaskCreator: %v", err)
	}

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 9000},
		"issue": {"number": 5, "title": "bug", "body": "details", "labels": [{"name": "vornik-task"}]},
		"label": {"name": "vornik-task"}
	}`)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		ch.HandleWebhook(rec, signedGitHubRequest("shhh", "issues", "d-retry", body))
		if rec.Code != http.StatusOK {
			t.Errorf("retry %d: HTTP = %d, want 200", i, rec.Code)
		}
	}

	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Errorf("tasks created after duplicate delivery = %d, want 1", len(tasks))
	}
	if got := repo.createCalls.Load(); got != 1 {
		t.Errorf("repo.Create calls = %d, want 1 (idempotency short-circuit)", got)
	}
}

// TestBuildGitHubChannel_NilTaskCreatorFactory_KeepsLogPath — the
// channel must still construct when the factory closure is nil
// (early-boot, no task repo yet); inbound deliveries log
// "TaskCreator not wired" rather than crashing.
func TestBuildGitHubChannel_NilTaskCreatorFactory_KeepsLogPath(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	proj := inboundOnlyProject("p-1")
	proj.GitHubApp.TaskLabels = []string{"vornik-task"}

	ch, _, err := buildGitHubChannelWithTaskCreator([]*registry.Project{proj}, nil)
	if err != nil {
		t.Fatalf("buildGitHubChannelWithTaskCreator: %v", err)
	}
	if ch == nil {
		t.Fatal("channel nil with nil factory")
	}

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 5, "title": "bug", "labels": [{"name": "vornik-task"}]},
		"label": {"name": "vornik-task"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedGitHubRequest("shhh", "issues", "d-no-factory", body))
	if rec.Code != http.StatusOK {
		t.Errorf("HTTP = %d, want 200 (log path swallows the event)", rec.Code)
	}
}

// TestBuildGitHubChannel_FactoryReturnsNil_KeepsLogPath — a
// non-nil factory that returns nil (e.g. taskRepo is nil under
// the closure) is treated as "not wired" by buildGitHubChannel:
// channel constructs without a TaskCreator and the channel logs
// the no-op path.
func TestBuildGitHubChannel_FactoryReturnsNil_KeepsLogPath(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	proj := inboundOnlyProject("p-1")
	proj.GitHubApp.TaskLabels = []string{"vornik-task"}

	factory := func(_ *registry.Project) github.TaskCreator { return nil }
	ch, _, err := buildGitHubChannelWithTaskCreator([]*registry.Project{proj}, factory)
	if err != nil {
		t.Fatalf("buildGitHubChannelWithTaskCreator: %v", err)
	}
	if ch == nil {
		t.Fatal("channel nil with nil-returning factory")
	}
}

// TestGitHubTaskCreator_StampsForgeJob: the created task payload carries a
// top-level forge_job for the deterministic forge.* system steps.
func TestGitHubTaskCreator_StampsForgeJob(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	if err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		Labels:         []string{"bug"},
		Repo:           "acme/api",
		Number:         5,
		DefaultBranch:  "main",
		IdempotencyKey: "github-app:fj-1",
	}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(repo.snapshotTasks()[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	fj, _ := payload["forge_job"].(map[string]any)
	if fj == nil {
		t.Fatalf("payload.forge_job missing: %+v", payload)
	}
	if fj["provider"] != "github" || fj["repo"] != "acme/api" || fj["action"] != "labeled" {
		t.Errorf("forge_job mismatch: %+v", fj)
	}
	if fj["default_branch"] != "main" || fj["number"].(float64) != 5 {
		t.Errorf("forge_job number/branch: %+v", fj)
	}
	if fj["is_change_request"] != false {
		t.Errorf("issue should not be a change request: %+v", fj)
	}
}

// TestGitHubTaskCreator_PRRoutesToReviewWorkflow: an opened PR runs the
// pr_review_workflow_id, while issues keep the reply/router workflow; the PR's
// forge_job is flagged is_change_request.
func TestGitHubTaskCreator_PRRoutesToReviewWorkflow(t *testing.T) {
	repo := newRecordingTaskRepo()
	proj := projectForTaskCreator("p-1")
	proj.GitHubApp.ReplyWorkflowID = "github-router"
	proj.GitHubApp.PRReviewWorkflowID = "github-review"
	g := newGitHubTaskCreator(repo, proj, nil, zerolog.Nop())

	if err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind: "pull_request.opened", Repo: "acme/api", Number: 12, DefaultBranch: "main",
		IdempotencyKey: "github-app:pr-1",
	}); err != nil {
		t.Fatal(err)
	}
	tk := repo.snapshotTasks()[0]
	if tk.WorkflowID == nil || *tk.WorkflowID != "github-review" {
		t.Errorf("PR task workflow = %v, want github-review", tk.WorkflowID)
	}
	var payload map[string]any
	_ = json.Unmarshal(tk.Payload, &payload)
	fj, _ := payload["forge_job"].(map[string]any)
	if fj == nil || fj["is_change_request"] != true || fj["action"] != "opened" {
		t.Errorf("PR forge_job should be a change request opened: %+v", fj)
	}
}
