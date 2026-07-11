package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// seedTasks is the fixture Statuses-aware ListFunc closures below filter
// with mocks.FilterTasks — real WHERE-clause semantics instead of a
// hand-rolled switch, so these tests exercise the actual Statuses
// contract the refactored Inbox now relies on (the false-pass risk the
// Outcome Inbox design review flagged: a naive closure that only checks
// filter.Status stops matching the moment the caller switches to
// filter.Statuses, and the test would never notice).
//
// TestInbox_RanksAndFiltersItems — approvals/checkpoints rank above failures;
// FAILED older than 24h is excluded; each row links to the task detail.
func TestInbox_RanksAndFiltersItems(t *testing.T) {
	now := time.Now()
	staleClass := "context_timeout"
	seed := []*persistence.Task{
		{ID: "t-approve", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now},
		{ID: "t-failed-fresh", ProjectID: "p1", Status: persistence.TaskStatusFailed, LastErrorClass: &staleClass, CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "t-failed-old", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)},
	}
	var filters []persistence.TaskFilter
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			filters = append(filters, f)
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Approval ranks before the failure.
	ai := strings.Index(body, "t-approve")
	fi := strings.Index(body, "t-failed-fresh")
	if ai < 0 || fi < 0 || ai > fi {
		t.Errorf("approval should rank before failure (ai=%d fi=%d)", ai, fi)
	}
	// 24h-stale failure excluded — from BOTH the pinned attention queue
	// and the broader "Your requests" list (task 4.4's recent-any-status
	// query applies the same recency window to FAILED tasks).
	if strings.Contains(body, "t-failed-old") {
		t.Error("FAILED older than 24h must be excluded from the inbox")
	}
	// Row links to the task detail + shows the error class.
	if !strings.Contains(body, `/ui/tasks/t-approve`) {
		t.Error("inbox row missing task-detail link")
	}
	if !strings.Contains(body, "context_timeout") {
		t.Error("failure row should surface the error class")
	}
	// The Outcome Inbox design's load-bearing invariant: the pinned
	// attention queue is ONE scoped query, not one List call per status
	// (§5.2). Task 4.4 adds a SECOND, separate query for the broader
	// "Your requests" list (any status) — so List is called exactly
	// twice overall, and the FIRST call is the 4-status attention query.
	if taskRepo.CallCount.List != 2 {
		t.Errorf("List called %d times, want exactly 2 (attention query + broader recent-requests query)", taskRepo.CallCount.List)
	}
	if len(filters) < 1 || len(filters[0].Statuses) != 4 {
		t.Errorf("expected the first (attention) query to request all 4 attention statuses, got %v", filters)
	}
	for _, st := range filters[0].Statuses {
		if st == persistence.TaskStatusAwaitingExternal {
			t.Error("AWAITING_EXTERNAL must never be part of the attention query's Statuses set")
		}
	}
}

// TestInbox_ScopedUserSeesOwnRowsPastGlobalCap pins the cross-project
// visibility scope audit follow-up: a project-scoped session must query
// its own project(s) directly, not
// the latest-N-across-all-projects slice. With the old global-200 query
// a busy instance's other-project rows fill the page and the scoped
// user's own actionable rows fall past the cap → invisible.
func TestInbox_ScopedUserSeesOwnRowsPastGlobalCap(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if !containsInboxStatus(f.Statuses, persistence.TaskStatusAwaitingApproval) {
				return nil, nil
			}
			// Scoped single-query path (E3): project_id IN (ids). The
			// caller's project surfaces their row.
			for _, pid := range f.ProjectIDs {
				if pid == "p1" {
					return []*persistence.Task{{ID: "mine", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}}, nil
				}
			}
			// Per-project query for the caller's project surfaces their row.
			if f.ProjectID != nil && *f.ProjectID == "p1" {
				return []*persistence.Task{{ID: "mine", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now}}, nil
			}
			// Global query (no project filter) returns a full page of
			// OTHER-project rows that bury the caller's row past the cap.
			if f.ProjectID == nil || *f.ProjectID == "" {
				bulk := make([]*persistence.Task, 0, f.PageSize)
				for i := 0; i < f.PageSize; i++ {
					bulk = append(bulk, &persistence.Task{ID: "other", ProjectID: "p2", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now})
				}
				return bulk, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"})
	rec := httptest.NewRecorder()
	srv.Inbox(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/tasks/mine") {
		t.Fatalf("scoped caller must see their own row even when other projects fill the global page:\n%s", body)
	}
	if strings.Contains(body, "/ui/tasks/other") {
		t.Fatal("foreign-project rows leaked into a scoped inbox")
	}
}

// TestInbox_EmptyState — nothing actionable renders the all-clear state.
func TestInbox_EmptyState(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if !strings.Contains(rec.Body.String(), "All clear") {
		t.Errorf("expected all-clear empty state:\n%s", rec.Body.String())
	}
}

// TestInbox_AttentionQuery_AllFourKindsScopedExcludingExternal is the
// Outcome Inbox design's attention-query contract (§5.2/§4): one call
// surfaces all four kinds (approval, input, failed, blocked-on-children)
// scoped to the viewer; AWAITING_EXTERNAL never appears even though a
// task in that status exists in the backing store; FAILED honors the
// 24h recency window.
func TestInbox_AttentionQuery_AllFourKindsScopedExcludingExternal(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "approve", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now},
		{ID: "input", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now},
		{ID: "failed-fresh", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: now, UpdatedAt: now},
		{ID: "blocked", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, CreatedAt: now, UpdatedAt: now},
		{ID: "external", ProjectID: "p1", Status: persistence.TaskStatusAwaitingExternal, CreatedAt: now, UpdatedAt: now},
		{ID: "failed-stale", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)},
		{ID: "other-project", ProjectID: "p2", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now},
	}
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

	for _, want := range []string{"approve", "input", "failed-fresh", "blocked"} {
		if !strings.Contains(body, "/ui/tasks/"+want) {
			t.Errorf("expected %s in the attention queue:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/ui/tasks/external") {
		t.Error("AWAITING_EXTERNAL must be excluded from the attention queue")
	}
	if strings.Contains(body, "/ui/tasks/failed-stale") {
		t.Error("FAILED older than 24h must be excluded")
	}
	if strings.Contains(body, "/ui/tasks/other-project") {
		t.Error("a foreign project's row must not leak into a scoped viewer's queue")
	}
	// Attention query (4 statuses) + task 4.4's broader recent-requests
	// query (any status) = 2 List calls total.
	if taskRepo.CallCount.List != 2 {
		t.Errorf("List called %d times, want exactly 2 (attention query + broader recent-requests query)", taskRepo.CallCount.List)
	}
}

func containsInboxStatus(haystack []persistence.TaskStatus, needle persistence.TaskStatus) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Task 4.4 — the broader "Your requests" list (design §5.7): recent
// request-root cards in ANY status, not just the attention subset. This
// is what makes a purely COMPLETED/RUNNING request (no attention-flagged
// descendant) reachable as a card — the gap task 4.2 deferred.
// ---------------------------------------------------------------------

// TestInbox_BroaderList_CompletedAndRunningRequestsReachable pins the
// core 4.4 fix: neither task ever enters the attention query's four
// statuses, so pre-4.4 neither ever got a "Your requests" card at all.
func TestInbox_BroaderList_CompletedAndRunningRequestsReachable(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-done", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "t-running", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour)},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/tasks/t-done") {
		t.Errorf("expected a COMPLETED-only request to render a card (previously unreachable via the attention query alone):\n%s", body)
	}
	if !strings.Contains(body, "/ui/tasks/t-running") {
		t.Errorf("expected a RUNNING-only request to render a card:\n%s", body)
	}
	if strings.Contains(body, "No requests yet") || strings.Contains(body, "Describe what you want and Vornik will set it up") {
		t.Errorf("empty state must not render when the broader list has cards:\n%s", body)
	}
}

// TestInbox_BroaderList_RecencyOrder — the broader list orders by
// recency (freshest first), NOT the rollup-rank order the pinned
// attention section uses (needs-you > working > done) — otherwise an
// old "needs you" card would always sort ahead of a fresh "done" one,
// defeating the "what's recently happened" purpose of this section.
func TestInbox_BroaderList_RecencyOrder(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-old-done", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)},
		{ID: "t-fresh-running", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour)},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	freshIdx := strings.Index(body, "/ui/tasks/t-fresh-running")
	oldIdx := strings.Index(body, "/ui/tasks/t-old-done")
	if freshIdx < 0 || oldIdx < 0 {
		t.Fatalf("expected both cards to render:\n%s", body)
	}
	if freshIdx > oldIdx {
		t.Errorf("expected the fresher RUNNING request before the older COMPLETED one (recency order), got fresh=%d old=%d", freshIdx, oldIdx)
	}
}

// TestInbox_BroaderList_ScopedUserSeesOwnRequestsPastGlobalCap mirrors
// TestInbox_ScopedUserSeesOwnRowsPastGlobalCap for the broader (any
// status) query: a project-scoped session must query its own project(s)
// directly for the recent-requests window too, not a global latest-N
// slice that a busy instance's other-project rows could bury it past.
func TestInbox_BroaderList_ScopedUserSeesOwnRequestsPastGlobalCap(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if len(f.Statuses) > 0 {
				// The pinned attention query — irrelevant here.
				return nil, nil
			}
			for _, pid := range f.ProjectIDs {
				if pid == "p1" {
					return []*persistence.Task{{ID: "mine-done", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: now, UpdatedAt: now}}, nil
				}
			}
			if f.ProjectID != nil && *f.ProjectID == "p1" {
				return []*persistence.Task{{ID: "mine-done", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: now, UpdatedAt: now}}, nil
			}
			if f.ProjectID == nil || *f.ProjectID == "" {
				bulk := make([]*persistence.Task, 0, f.PageSize)
				for i := 0; i < f.PageSize; i++ {
					bulk = append(bulk, &persistence.Task{ID: "other-done", ProjectID: "p2", Status: persistence.TaskStatusCompleted, CreatedAt: now, UpdatedAt: now})
				}
				return bulk, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := scopedUIRequest(http.MethodGet, "/ui/inbox", []string{"p1"})
	rec := httptest.NewRecorder()
	srv.Inbox(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/tasks/mine-done") {
		t.Fatalf("scoped caller must see their own recent request even when other projects fill the global page:\n%s", body)
	}
	if strings.Contains(body, "/ui/tasks/other-done") {
		t.Fatal("foreign-project rows leaked into a scoped viewer's broader requests list")
	}
}

// TestInbox_BroaderList_HasMoreWhenCapped mirrors the attention query's
// review-2627 non-silent-cap discipline for the broader query: a
// full-budget result sets HasMoreRecent and surfaces the note instead of
// quietly dropping requests past inboxRecentRequestsPageSize.
func TestInbox_BroaderList_HasMoreWhenCapped(t *testing.T) {
	now := time.Now()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if len(f.Statuses) > 0 {
				return nil, nil
			}
			bulk := make([]*persistence.Task, 0, f.PageSize)
			for i := 0; i < f.PageSize; i++ {
				bulk = append(bulk, &persistence.Task{
					ID:        "t-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
					ProjectID: "p1",
					Status:    persistence.TaskStatusCompleted,
					CreatedAt: now,
					UpdatedAt: now,
				})
			}
			return bulk, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "…and more requests waiting") {
		t.Errorf("expected the non-silent HasMoreRecent note when the broader query hits its page-size cap:\n%s", body)
	}
}

// TestInbox_BroaderList_DedupedAgainstAttention — a request already
// pinned in the attention section (Requests) must not also appear in
// the broader list (RecentRequests): same request-root, one card.
func TestInbox_BroaderList_DedupedAgainstAttention(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-approve", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	// The card's own "View →" link, not the attention row's inline
	// approve/reject hx-post targets (which share the "/ui/tasks/t-approve"
	// prefix via .../approve and .../reject).
	const cardLink = `href="/ui/tasks/t-approve"`
	if got := strings.Count(body, cardLink); got != 1 {
		t.Errorf("expected the pinned request's card to render exactly once (deduped from the broader list), got %d occurrences of %s:\n%s",
			got, cardLink, body)
	}
}

// TestInbox_EmptyState_NoRequestsRendersOnboardingCard — zero requests
// (attention or broader) renders the onboarding card linking to the
// composer's stand-in, the project wizard (task 4.4, design §5.7).
func TestInbox_EmptyState_NoRequestsRendersOnboardingCard(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Describe what you want and Vornik will set it up") {
		t.Errorf("expected the onboarding empty-state title:\n%s", body)
	}
	if !strings.Contains(body, `href="/ui/projects/new"`) {
		t.Errorf("expected the onboarding CTA to link to the project wizard:\n%s", body)
	}
}

// TestInbox_RecordsViewMetricWithRoleLabel — vornik_ui_inbox_views_total
// increments once per render, labelled by the viewer's session role
// (design §5.8).
func TestInbox_RecordsViewMetricWithRoleLabel(t *testing.T) {
	m := NewInboxMetrics(prometheus.NewRegistry())
	srv := NewServer(WithInboxMetrics(m))

	srv.Inbox(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	if got := testutil.ToFloat64(m.ViewsTotal.WithLabelValues("none")); got != 1 {
		t.Errorf("views total for role=none = %v, want 1 (no session role in an unauthenticated request)", got)
	}

	req2 := setupAuthRequest(http.MethodGet, "/ui/inbox", "admin")
	srv.Inbox(httptest.NewRecorder(), req2)
	if got := testutil.ToFloat64(m.ViewsTotal.WithLabelValues("admin")); got != 1 {
		t.Errorf("views total for role=admin = %v, want 1", got)
	}
}
