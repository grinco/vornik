// Task 4.2 — "My requests" cards (Outcome Inbox design §5.3/§5.5). Tests
// cover: request grouping, the status-rollup precedence table
// (exhaustive), the status line (narration vs playbook/kind-label
// fallback), OUTPUT-only deliverable chips, the batch-resolved origin
// badge + subtasks pill, in-scope-only rendering, XSS auto-escaping, and
// the attention page-size cap-hit → HasMore signal (review-2627 fix).
package ui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/playbook"
)

// fakeBatchNarrationRepo is a minimal persistence.ExecutionNarrationRepository
// fake — embedding the (nil) interface satisfies every method except the
// one overridden, mirroring internal/fixitdoctor/assembler_test.go's
// fakeBatchNarrationRepo.
type fakeBatchNarrationRepo struct {
	persistence.ExecutionNarrationRepository
	byExecution map[string][]*persistence.ExecutionNarration
	err         error
	calls       int
}

func (f *fakeBatchNarrationRepo) ListByExecution(_ context.Context, executionID string) ([]*persistence.ExecutionNarration, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byExecution[executionID], nil
}

// fakeChatAuditRepo is a minimal persistence.ChatAuditRepository fake
// exposing only GetChatAuditsByTurnIDs (embedding the nil interface for
// everything else), so it also satisfies chatorigin.ChatAuditLookup and
// the local chatAuditBatchLookup assertion inbox.go relies on.
type fakeChatAuditRepo struct {
	persistence.ChatAuditRepository
	byTurn map[string]persistence.ChatAuditEntry
	err    error
	calls  int
}

func (f *fakeChatAuditRepo) GetChatAuditsByTurnIDs(_ context.Context, _ []string) (map[string]persistence.ChatAuditEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byTurn, nil
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------
// Rollup precedence — exhaustive (§5.3 table)
// ---------------------------------------------------------------------

func TestRollupBucketForStatuses_ExhaustivePrecedenceTable(t *testing.T) {
	cases := []struct {
		status persistence.TaskStatus
		bucket string
	}{
		{persistence.TaskStatusAwaitingApproval, requestBucketNeedsYou},
		{persistence.TaskStatusAwaitingInput, requestBucketNeedsYou},
		{persistence.TaskStatusFailed, requestBucketNeedsYou},
		{persistence.TaskStatusRunning, requestBucketWorking},
		{persistence.TaskStatusQueued, requestBucketWorking},
		{persistence.TaskStatusWaitingForChildren, requestBucketWorking},
		{persistence.TaskStatusCompleted, requestBucketDone},
		{persistence.TaskStatusCancelled, requestBucketDone},
	}
	for _, c := range cases {
		bucket, winner, ok := rollupBucketForStatuses([]persistence.TaskStatus{c.status})
		if !ok {
			t.Errorf("%s: expected ok=true", c.status)
			continue
		}
		if bucket != c.bucket {
			t.Errorf("%s: bucket = %q, want %q", c.status, bucket, c.bucket)
		}
		if winner != c.status {
			t.Errorf("%s: winner = %q, want %q", c.status, winner, c.status)
		}
	}
}

func TestRollupBucketForStatuses_AwaitingExternalExcluded(t *testing.T) {
	// Alone: nothing to roll up.
	if _, _, ok := rollupBucketForStatuses([]persistence.TaskStatus{persistence.TaskStatusAwaitingExternal}); ok {
		t.Error("AWAITING_EXTERNAL alone must not produce a rollup result")
	}
	// Mixed: excluded, doesn't block a real status from winning.
	bucket, winner, ok := rollupBucketForStatuses([]persistence.TaskStatus{
		persistence.TaskStatusAwaitingExternal, persistence.TaskStatusRunning,
	})
	if !ok || bucket != requestBucketWorking || winner != persistence.TaskStatusRunning {
		t.Errorf("AWAITING_EXTERNAL+RUNNING = (%q,%q,%v), want (working, RUNNING, true)", bucket, winner, ok)
	}
}

func TestRollupBucketForStatuses_UnlistedStatusExcluded(t *testing.T) {
	if _, _, ok := rollupBucketForStatuses([]persistence.TaskStatus{persistence.TaskStatusPending}); ok {
		t.Error("an unlisted TaskStatus (PENDING) must not produce a rollup result")
	}
}

func TestRollupBucketForStatuses_EmptyInput(t *testing.T) {
	if _, _, ok := rollupBucketForStatuses(nil); ok {
		t.Error("empty status set must return ok=false")
	}
}

func TestRollupBucketForStatuses_PrecedenceAcrossMixedSets(t *testing.T) {
	cases := []struct {
		name   string
		set    []persistence.TaskStatus
		bucket string
		winner persistence.TaskStatus
	}{
		{
			"needs-you always wins over working/done",
			[]persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusRunning, persistence.TaskStatusCompleted},
			requestBucketNeedsYou, persistence.TaskStatusFailed,
		},
		{
			"approval outranks input and failed",
			[]persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusAwaitingInput, persistence.TaskStatusAwaitingApproval},
			requestBucketNeedsYou, persistence.TaskStatusAwaitingApproval,
		},
		{
			"running outranks queued and blocked",
			[]persistence.TaskStatus{persistence.TaskStatusWaitingForChildren, persistence.TaskStatusQueued, persistence.TaskStatusRunning},
			requestBucketWorking, persistence.TaskStatusRunning,
		},
		{
			"completed outranks cancelled",
			[]persistence.TaskStatus{persistence.TaskStatusCancelled, persistence.TaskStatusCompleted},
			requestBucketDone, persistence.TaskStatusCompleted,
		},
	}
	for _, c := range cases {
		bucket, winner, ok := rollupBucketForStatuses(c.set)
		if !ok || bucket != c.bucket || winner != c.winner {
			t.Errorf("%s: got (%q,%q,%v), want (%q,%q,true)", c.name, bucket, winner, ok, c.bucket, c.winner)
		}
	}
}

// ---------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------

func TestBuildRequestCards_GroupsParentAndChildrenIntoOneCard(t *testing.T) {
	now := time.Now()
	root := &persistence.Task{ID: "req-1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: now.Add(-1 * time.Hour)}
	childA := &persistence.Task{ID: "req-1-child-a", ProjectID: "p1", ParentTaskID: strPtr("req-1"), Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	childB := &persistence.Task{ID: "req-1-child-b", ProjectID: "p1", ParentTaskID: strPtr("req-1"), Status: persistence.TaskStatusFailed, CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now.Add(-30 * time.Minute)}
	req2 := &persistence.Task{ID: "req-2", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now}

	seed := []*persistence.Task{root, childA, childB, req2}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
		CountChildrenForParentsFunc: func(_ context.Context, _ []string) (map[string]int, error) {
			return map[string]int{"req-1": 2}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	filtered := []*persistence.Task{childA, childB, req2} // the attention-query subset
	cards := srv.buildRequestCards(context.Background(), filtered)

	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (one per request-root), got %d", len(cards))
	}
	byID := map[string]requestCard{}
	for _, c := range cards {
		byID[c.RequestID] = c
	}
	c1, ok := byID["req-1"]
	if !ok {
		t.Fatalf("expected a card for req-1, got %+v", byID)
	}
	if c1.Subtasks != 2 {
		t.Errorf("req-1 Subtasks = %d, want 2", c1.Subtasks)
	}
	if c1.Bucket != requestBucketNeedsYou {
		t.Errorf("req-1 Bucket = %q, want needs-you (AWAITING_APPROVAL beats FAILED)", c1.Bucket)
	}
	if c1.Href != "/ui/tasks/req-1-child-a" {
		t.Errorf("req-1 Href = %q, want the rollup-winning child (req-1-child-a)", c1.Href)
	}
	c2, ok := byID["req-2"]
	if !ok {
		t.Fatalf("expected a card for req-2, got %+v", byID)
	}
	if c2.Subtasks != 0 {
		t.Errorf("req-2 Subtasks = %d, want 0 (absent from CountChildrenForParents' map)", c2.Subtasks)
	}
}

// ---------------------------------------------------------------------
// Status line: narration vs fallback
// ---------------------------------------------------------------------

func TestBuildRequestCards_StatusLine_NarrationPresentUsesLatestLine(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-3", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	seed := []*persistence.Task{task}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-3": {ID: "exec-1", TaskID: "req-3"}}, nil
		},
	}
	narration := &fakeBatchNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{
		"exec-1": {
			{ExecutionID: "exec-1", Seq: 0, Text: "Starting up"},
			{ExecutionID: "exec-1", Seq: 1, Text: "Writing your summary — step 2 of 3"},
		},
	}}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithExecutionNarrationRepository(narration))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if got := cards[0].StatusLine; got != "Writing your summary — step 2 of 3" {
		t.Errorf("StatusLine = %q, want the latest narration line", got)
	}
}

func TestBuildRequestCards_StatusLine_FailedFallsBackToPlaybookWhenNarrationAbsent(t *testing.T) {
	now := time.Now()
	class := persistence.TaskFailureClassTimeout
	task := &persistence.Task{ID: "req-4", ProjectID: "p1", Status: persistence.TaskStatusFailed, LastErrorClass: &class, CreatedAt: now, UpdatedAt: now}
	seed := []*persistence.Task{task}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	// No execRepo/narrationRepo wired — narration is absent/disabled.
	srv := NewServer(WithTaskRepository(taskRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	want := playbook.Lookup(class).HumanFriendly()
	if got := cards[0].StatusLine; got != want {
		t.Errorf("StatusLine = %q, want playbook fallback %q", got, want)
	}
}

func TestBuildRequestCards_StatusLine_NonFailedFallsBackToKindLabel(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-5", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, CreatedAt: now, UpdatedAt: now}
	seed := []*persistence.Task{task}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if got := cards[0].StatusLine; got != "Blocked on children" {
		t.Errorf("StatusLine = %q, want the rollup kind-label fallback", got)
	}
}

// ---------------------------------------------------------------------
// Deliverable chips — OUTPUT only
// ---------------------------------------------------------------------

func TestBuildRequestCards_DeliverableChips_OutputOnly(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-6", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	seed := []*persistence.Task{task}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-6": {ID: "exec-2", TaskID: "req-6"}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{ID: "a1", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput},
				{ID: "a2", Name: "input.csv", ArtifactClass: persistence.ArtifactClassInput},
				{ID: "a3", Name: "scratch.txt", ArtifactClass: persistence.ArtifactClassIntermediate},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if len(cards[0].Deliverables) != 1 || cards[0].Deliverables[0].Name != "report.pdf" {
		t.Errorf("Deliverables = %+v, want only the OUTPUT artifact (report.pdf)", cards[0].Deliverables)
	}
}

// ---------------------------------------------------------------------
// Origin badge (batch) + subtasks pill
// ---------------------------------------------------------------------

func TestBuildRequestCards_OriginBadge_BatchResolvedSingleCall(t *testing.T) {
	now := time.Now()
	turn1, turn2 := "turn-1", "turn-2"
	task7 := &persistence.Task{ID: "req-7", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, ChatTurnID: &turn1, CreatedAt: now, UpdatedAt: now}
	task8 := &persistence.Task{ID: "req-8", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, ChatTurnID: &turn2, CreatedAt: now, UpdatedAt: now}
	task9 := &persistence.Task{ID: "req-9", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now} // no ChatTurnID
	seed := []*persistence.Task{task7, task8, task9}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
		CountChildrenForParentsFunc: func(_ context.Context, _ []string) (map[string]int, error) {
			return map[string]int{"req-7": 3}, nil
		},
	}
	chatAudit := &fakeChatAuditRepo{byTurn: map[string]persistence.ChatAuditEntry{
		"turn-1": {ID: "turn-1", ChatID: "123456"},         // all-digits -> telegram
		"turn-2": {ID: "turn-2", ChatID: "web-chat:sess1"}, // -> web chat
	}}
	srv := NewServer(WithTaskRepository(taskRepo), WithChatAuditRepository(chatAudit))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task7, task8, task9})
	byID := map[string]requestCard{}
	for _, c := range cards {
		byID[c.RequestID] = c
	}
	if got := byID["req-7"].Origin; got != "from Telegram" {
		t.Errorf("req-7 Origin = %q, want %q", got, "from Telegram")
	}
	if got := byID["req-7"].Subtasks; got != 3 {
		t.Errorf("req-7 Subtasks = %d, want 3", got)
	}
	if got := byID["req-8"].Origin; got != "from web chat" {
		t.Errorf("req-8 Origin = %q, want %q", got, "from web chat")
	}
	if got := byID["req-9"].Origin; got != "created here" {
		t.Errorf("req-9 (no ChatTurnID) Origin = %q, want %q", got, "created here")
	}
	if chatAudit.calls != 1 {
		t.Errorf("GetChatAuditsByTurnIDs called %d times, want exactly 1 (batch, not per-card)", chatAudit.calls)
	}
}

// ---------------------------------------------------------------------
// Scope: only in-scope requests render
// ---------------------------------------------------------------------

func TestInbox_Requests_ScopeExcludesForeignProject(t *testing.T) {
	now := time.Now()
	mine := &persistence.Task{ID: "mine-req", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	other := &persistence.Task{ID: "other-req", ProjectID: "p2", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	seed := []*persistence.Task{mine, other}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"})
	rec := httptest.NewRecorder()
	srv.Inbox(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "/ui/tasks/mine-req") {
		t.Errorf("expected the scoped caller's own request to render:\n%s", body)
	}
	if strings.Contains(body, "/ui/tasks/other-req") {
		t.Errorf("a foreign project's request must not leak into My requests cards:\n%s", body)
	}
}

// ---------------------------------------------------------------------
// XSS: narration + artifact text auto-escaped
// ---------------------------------------------------------------------

func TestInbox_Requests_XSSAutoEscaped(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-xss", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	seed := []*persistence.Task{task}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-xss": {ID: "exec-xss", TaskID: "req-xss"}}, nil
		},
	}
	narration := &fakeBatchNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{
		"exec-xss": {{ExecutionID: "exec-xss", Seq: 0, Text: `<script>alert(1)</script>`}},
	}}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{ID: "a1", Name: `<img src=x onerror=alert(2)>`, ArtifactClass: persistence.ArtifactClassOutput}}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo),
		WithExecutionNarrationRepository(narration), WithArtifactRepository(artifactRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("narration text rendered unescaped — XSS risk")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected the narration text HTML-escaped:\n%s", body)
	}
	if strings.Contains(body, "<img src=x onerror=alert(2)>") {
		t.Error("artifact name rendered unescaped — XSS risk")
	}
}

// ---------------------------------------------------------------------
// Attention page-size cap-hit → HasMore signal + log (review-2627 fix)
// ---------------------------------------------------------------------

func TestInbox_AttentionCap_HitSetsHasMoreAndLogs(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			now := time.Now()
			bulk := make([]*persistence.Task, 0, f.PageSize)
			for i := 0; i < f.PageSize; i++ {
				bulk = append(bulk, &persistence.Task{
					ID: "t" + string(rune('a'+i%26)) + string(rune('0'+i/26)), ProjectID: "p1",
					Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now,
				})
			}
			return bulk, nil
		},
	}
	var buf bytes.Buffer
	srv := NewServer(WithTaskRepository(taskRepo), WithLogger(zerolog.New(&buf).Level(zerolog.WarnLevel)))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "more waiting") {
		t.Errorf("expected a non-silent '…and more waiting' affordance when the cap is hit:\n%s", body)
	}
	if buf.Len() == 0 {
		t.Error("expected a warning logged when the attention query hits its page-size cap")
	}
	if !strings.Contains(buf.String(), "page-size cap") {
		t.Errorf("log line should mention the page-size cap; got %q", buf.String())
	}
}

func TestInbox_AttentionCap_NotHitNoSignalNoLog(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "only-one", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	var buf bytes.Buffer
	srv := NewServer(WithTaskRepository(taskRepo), WithLogger(zerolog.New(&buf).Level(zerolog.WarnLevel)))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	if strings.Contains(body, "more waiting") {
		t.Errorf("did not expect the cap-hit affordance when well under the cap:\n%s", body)
	}
	if buf.Len() != 0 {
		t.Errorf("did not expect a cap-hit warning logged; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------
// Defensive / error-path branches
// ---------------------------------------------------------------------

// A nil task slipping into the query result must never panic — the
// per-row loop drops it (same as byStatus lookup misses).
func TestBuildRequestCards_NilTaskInInputIgnored(t *testing.T) {
	now := time.Now()
	realTask := &persistence.Task{ID: "req-nil-ok", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{realTask}, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{nil, realTask})
	if len(cards) != 1 || cards[0].RequestID != "req-nil-ok" {
		t.Fatalf("expected the nil entry to be ignored and the real task to still render a card, got %+v", cards)
	}
}

func TestRollupWinner_AllMembersExcludedReturnsNotOK(t *testing.T) {
	members := []*persistence.Task{
		{ID: "a", Status: persistence.TaskStatusAwaitingExternal},
		{ID: "b", Status: persistence.TaskStatusPending},
	}
	winner, _, ok := rollupWinner(members)
	if ok || winner != nil {
		t.Errorf("expected ok=false/winner=nil when every member is excluded from the rollup, got winner=%v ok=%v", winner, ok)
	}
}

func TestBuildRequestCards_AllExcludedMembersProduceNoCard(t *testing.T) {
	now := time.Now()
	// AWAITING_EXTERNAL is excluded from the rollup table entirely; a
	// request whose only member is in that status must not render a
	// card (defensive — the attention query never actually returns
	// AWAITING_EXTERNAL, but buildRequestCards must degrade cleanly if
	// a future caller widens the input).
	task := &persistence.Task{ID: "req-external-only", ProjectID: "p1", Status: persistence.TaskStatusAwaitingExternal, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 0 {
		t.Errorf("expected no card for an all-excluded request, got %+v", cards)
	}
}

func TestBuildRequestCards_ResolveRequestRootsErrorSuppressesCards(t *testing.T) {
	now := time.Now()
	parentID := "missing-parent"
	child := &persistence.Task{ID: "child", ProjectID: "p1", ParentTaskID: &parentID, Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if len(f.IDs) > 0 {
				return nil, errors.New("db unavailable")
			}
			return []*persistence.Task{child}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{child})
	if cards != nil {
		t.Errorf("expected nil cards when ResolveRequestRoots fails, got %+v", cards)
	}
}

func TestBuildRequestCards_CountChildrenForParentsErrorLeavesSubtasksZero(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-cc-err", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
		CountChildrenForParentsFunc: func(_ context.Context, _ []string) (map[string]int, error) {
			return nil, errors.New("db unavailable")
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 || cards[0].Subtasks != 0 {
		t.Fatalf("expected the card to still render with Subtasks=0 on a CountChildrenForParents error, got %+v", cards)
	}
}

func TestBuildRequestCards_ExecutionLookupErrorFallsBackToKindLabel(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-exec-err", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return nil, errors.New("db unavailable")
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo))
	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 || cards[0].StatusLine != "Needs approval" {
		t.Fatalf("expected the kind-label fallback when the execution lookup errors, got %+v", cards)
	}
}

// Two cards backed by the SAME execution ID must fetch narration and
// artifacts exactly once each (the per-card cache), not once per card.
func TestBuildRequestCards_NarrationAndArtifactsCachedPerExecution(t *testing.T) {
	now := time.Now()
	taskA := &persistence.Task{ID: "req-share-a", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskB := &persistence.Task{ID: "req-share-b", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{taskA, taskB}, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{
				"req-share-a": {ID: "exec-shared", TaskID: "req-share-a"},
				"req-share-b": {ID: "exec-shared", TaskID: "req-share-b"},
			}, nil
		},
	}
	narration := &fakeBatchNarrationRepo{byExecution: map[string][]*persistence.ExecutionNarration{
		"exec-shared": {{ExecutionID: "exec-shared", Seq: 0, Text: "Shared line"}},
	}}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{ID: "a1", Name: "shared.pdf", ArtifactClass: persistence.ArtifactClassOutput}}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo),
		WithExecutionNarrationRepository(narration), WithArtifactRepository(artifactRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{taskA, taskB})
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(cards))
	}
	for _, c := range cards {
		if c.StatusLine != "Shared line" {
			t.Errorf("card %s StatusLine = %q, want the shared narration line", c.RequestID, c.StatusLine)
		}
		if len(c.Deliverables) != 1 || c.Deliverables[0].Name != "shared.pdf" {
			t.Errorf("card %s Deliverables = %+v, want the shared deliverable", c.RequestID, c.Deliverables)
		}
	}
	if narration.calls != 1 {
		t.Errorf("ListByExecution called %d times, want exactly 1 (cached across cards sharing an execution)", narration.calls)
	}
	if artifactRepo.CallCount.List != 1 {
		t.Errorf("artifact List called %d times, want exactly 1 (cached across cards sharing an execution)", artifactRepo.CallCount.List)
	}
}

func TestBuildRequestCards_NarrationLookupErrorFallsBackToKindLabel(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-narr-err", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-narr-err": {ID: "exec-err", TaskID: "req-narr-err"}}, nil
		},
	}
	narration := &fakeBatchNarrationRepo{err: errors.New("db unavailable")}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithExecutionNarrationRepository(narration))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 || cards[0].StatusLine != "Needs input" {
		t.Fatalf("expected the kind-label fallback on a narration lookup error, got %+v", cards)
	}
}

func TestBuildRequestCards_ArtifactLookupErrorLeavesDeliverablesEmpty(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-art-err", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-art-err": {ID: "exec-art-err", TaskID: "req-art-err"}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, errors.New("db unavailable")
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 || len(cards[0].Deliverables) != 0 {
		t.Fatalf("expected no deliverables on an artifact lookup error, got %+v", cards)
	}
}

func TestBuildRequestCards_OriginBadge_NoChatTurnIDsSkipsBatchCall(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-no-turn", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	chatAudit := &fakeChatAuditRepo{byTurn: map[string]persistence.ChatAuditEntry{}}
	srv := NewServer(WithTaskRepository(taskRepo), WithChatAuditRepository(chatAudit))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 || cards[0].Origin != "created here" {
		t.Fatalf("expected 'created here' with no batch call, got %+v", cards)
	}
	if chatAudit.calls != 0 {
		t.Errorf("GetChatAuditsByTurnIDs called %d times, want 0 (no card has a ChatTurnID)", chatAudit.calls)
	}
}

func TestBuildRequestCards_OriginBadge_BatchErrorLeavesOriginBlank(t *testing.T) {
	now := time.Now()
	turnID := "turn-err"
	task := &persistence.Task{ID: "req-turn-err", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, ChatTurnID: &turnID, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	chatAudit := &fakeChatAuditRepo{err: errors.New("db unavailable")}
	srv := NewServer(WithTaskRepository(taskRepo), WithChatAuditRepository(chatAudit))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 || cards[0].Origin != "" {
		t.Fatalf("expected a blank origin badge on a batch lookup error, got %+v", cards)
	}
}

func TestBuildRequestCards_OriginBadge_MissingRowOrUndecodableChatIDLeavesBlank(t *testing.T) {
	now := time.Now()
	turnMissing, turnBad := "turn-missing", "turn-bad"
	taskMissing := &persistence.Task{ID: "req-missing-row", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, ChatTurnID: &turnMissing, CreatedAt: now, UpdatedAt: now}
	taskBad := &persistence.Task{ID: "req-bad-chatid", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, ChatTurnID: &turnBad, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{taskMissing, taskBad}, f), nil
		},
	}
	chatAudit := &fakeChatAuditRepo{byTurn: map[string]persistence.ChatAuditEntry{
		// turn-missing intentionally absent (no audit row found).
		"turn-bad": {ID: "turn-bad", ChatID: ""}, // undecodable -> DecodeChatID returns ""
	}}
	srv := NewServer(WithTaskRepository(taskRepo), WithChatAuditRepository(chatAudit))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{taskMissing, taskBad})
	byID := map[string]requestCard{}
	for _, c := range cards {
		byID[c.RequestID] = c
	}
	if byID["req-missing-row"].Origin != "" {
		t.Errorf("expected a blank origin badge when the audit row is missing, got %q", byID["req-missing-row"].Origin)
	}
	if byID["req-bad-chatid"].Origin != "" {
		t.Errorf("expected a blank origin badge for an undecodable chat id, got %q", byID["req-bad-chatid"].Origin)
	}
}

func TestHumanChannelName_AllKnownChannelsAndFallback(t *testing.T) {
	cases := map[string]string{
		"telegram": "Telegram",
		"web-chat": "web chat",
		"slack":    "Slack",
		"email":    "email",
		"future":   "future", // unrecognised channel: raw name, not hidden
	}
	for channel, want := range cases {
		if got := humanChannelName(channel); got != want {
			t.Errorf("humanChannelName(%q) = %q, want %q", channel, got, want)
		}
	}
}
