package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// reminderRepoForTools implements just enough of
// persistence.ReminderRepository for the cancel + update tool
// tests. Records every mutation so assertions can inspect the
// exact wire shape.
type reminderRepoForTools struct {
	mu        sync.Mutex
	rows      map[string]*persistence.Reminder
	getErr    error
	cancels   []string
	updates   []reminderUpdateSpy
	updateErr error
	cancelErr error
	pauses    []string
	pauseErr  error
	resumes   []reminderResumeSpy
	resumeErr error
}

type reminderResumeSpy struct {
	ID         string
	NextFireAt time.Time
}

type reminderUpdateSpy struct {
	ID        string
	FireAt    time.Time
	Content   string
	CronExpr  string
	ProjectID string
}

func newReminderRepoForTools() *reminderRepoForTools {
	return &reminderRepoForTools{rows: map[string]*persistence.Reminder{}}
}

func (r *reminderRepoForTools) Insert(_ context.Context, rem *persistence.Reminder) error {
	r.rows[rem.ID] = rem
	return nil
}
func (r *reminderRepoForTools) Get(_ context.Context, id string) (*persistence.Reminder, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	row, ok := r.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *row
	return &cp, nil
}
func (r *reminderRepoForTools) List(_ context.Context, _ persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	return nil, nil
}
func (r *reminderRepoForTools) LeaseDue(_ context.Context, _ time.Time, _ int) ([]*persistence.Reminder, error) {
	return nil, nil
}
func (r *reminderRepoForTools) MarkFired(_ context.Context, _ string) error      { return nil }
func (r *reminderRepoForTools) MarkErrored(_ context.Context, _, _ string) error { return nil }
func (r *reminderRepoForTools) Reschedule(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (r *reminderRepoForTools) UpdateFields(_ context.Context, id string, upd persistence.ReminderFieldUpdate) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, reminderUpdateSpy{ID: id, FireAt: upd.FireAt, Content: upd.Content, CronExpr: upd.CronExpr, ProjectID: upd.ProjectID})
	if row, ok := r.rows[id]; ok {
		row.FireAt = upd.FireAt
		if upd.Content != "" {
			row.Content = upd.Content
		}
		if upd.CronExpr != "" {
			row.CronExpr = upd.CronExpr
		}
		if upd.ProjectID != "" {
			row.ProjectID = upd.ProjectID
		}
	}
	return nil
}
func (r *reminderRepoForTools) Cancel(_ context.Context, id string) error {
	if r.cancelErr != nil {
		return r.cancelErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels = append(r.cancels, id)
	if row, ok := r.rows[id]; ok {
		row.Status = persistence.ReminderStatusCancelled
	}
	return nil
}
func (r *reminderRepoForTools) Delete(_ context.Context, _ string) error { panic("not used") }
func (r *reminderRepoForTools) CountPendingByOperator(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (r *reminderRepoForTools) MarkTaskSpawned(_ context.Context, _, _ string, _ *time.Time) error {
	return nil
}
func (r *reminderRepoForTools) ClaimDelivery(_ context.Context, _ string) (*persistence.Reminder, bool, error) {
	return nil, false, nil
}
func (r *reminderRepoForTools) FinalizeDelivery(_ context.Context, _, _ string, _ bool) error {
	return nil
}
func (r *reminderRepoForTools) CountTaskByOperator(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (r *reminderRepoForTools) Pause(_ context.Context, id string) error {
	if r.pauseErr != nil {
		return r.pauseErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pauses = append(r.pauses, id)
	if row, ok := r.rows[id]; ok {
		row.Status = persistence.ReminderStatusPaused
	}
	return nil
}
func (r *reminderRepoForTools) Resume(_ context.Context, id string, nextFireAt time.Time) error {
	if r.resumeErr != nil {
		return r.resumeErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumes = append(r.resumes, reminderResumeSpy{ID: id, NextFireAt: nextFireAt})
	if row, ok := r.rows[id]; ok {
		row.Status = persistence.ReminderStatusPending
		row.FireAt = nextFireAt
	}
	return nil
}

// ReclaimStuckFiring is Task 14's crash-recovery sweep addition. Not
// exercised by the pause/resume/cancel tool tests — compile stub only.
func (r *reminderRepoForTools) ReclaimStuckFiring(_ context.Context, _ time.Time, _ int) ([]*persistence.Reminder, error) {
	return nil, nil
}

// TestCancelReminderTool_HappyPath: cancel via reminder_id +
// rationale flips the row to cancelled.
func TestCancelReminderTool_HappyPath(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_xyz"] = &persistence.Reminder{
		ID: "rem_xyz", OperatorID: "telegram:42", ChannelRef: "42", Status: persistence.ReminderStatusPending,
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "cancel_reminder",
		Arguments: `{"reminder_id":"rem_xyz","rationale":"user said never mind"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.cancels) != 1 || repo.cancels[0] != "rem_xyz" {
		t.Errorf("Cancel calls = %v", repo.cancels)
	}
}

// TestCancelReminderTool_UnknownID surfaces a clear refusal so
// the LLM doesn't retry-loop on a nonexistent id.
func TestCancelReminderTool_UnknownID(t *testing.T) {
	repo := newReminderRepoForTools()
	te := &ToolExecutor{reminderRepo: repo}
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "cancel_reminder",
		Arguments: `{"reminder_id":"rem_missing","rationale":"r"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)
	if !strings.Contains(strings.ToLower(res.Content), "not found") {
		t.Errorf("expected not-found refusal, got %q", res.Content)
	}
	if len(repo.cancels) != 0 {
		t.Errorf("Cancel should NOT have fired")
	}
}

// TestCancelReminderTool_RefusesOtherOperator: a Telegram user
// 42 must not be able to cancel reminders belonging to user
// 99. Without this guard one chat could clear another's
// schedule.
func TestCancelReminderTool_RefusesOtherOperator(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_other"] = &persistence.Reminder{
		ID: "rem_other", OperatorID: "telegram:99", ChannelRef: "99", Status: persistence.ReminderStatusPending,
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "cancel_reminder",
		Arguments: `{"reminder_id":"rem_other","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "not yours") &&
		!strings.Contains(strings.ToLower(res.Content), "different operator") {
		t.Errorf("expected cross-operator refusal, got %q", res.Content)
	}
	if len(repo.cancels) != 0 {
		t.Errorf("Cancel must not fire across operators")
	}
}

// TestUpdateReminderTool_RescheduleViaSeconds — change fire
// time by relative offset (matches set_reminder's
// fire_in_seconds shape so the LLM uses one mental model).
func TestUpdateReminderTool_RescheduleViaSeconds(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42", Status: persistence.ReminderStatusPending,
		FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_x","fire_in_seconds":1800,"rationale":"move sooner"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("UpdateFields calls = %d, want 1", len(repo.updates))
	}
	// Within a few seconds of now + 1800s.
	diff := time.Until(repo.updates[0].FireAt) - 30*time.Minute
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("FireAt drift > 2s: %v from expected now+30m", diff)
	}
}

// TestUpdateReminderTool_RescheduleViaRFC3339 — explicit
// timestamp path.
func TestUpdateReminderTool_RescheduleViaRFC3339(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42", Status: persistence.ReminderStatusPending,
		FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_x","fire_at":"2099-01-01T09:00:00Z","rationale":"explicit"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("UpdateFields calls = %d", len(repo.updates))
	}
	want := time.Date(2099, 1, 1, 9, 0, 0, 0, time.UTC)
	if !repo.updates[0].FireAt.Equal(want) {
		t.Errorf("FireAt = %v, want %v", repo.updates[0].FireAt, want)
	}
}

// TestUpdateReminderTool_ContentOnly — update content without
// changing fire_at. fire_at omitted; fire_in_seconds=0.
func TestUpdateReminderTool_ContentOnly(t *testing.T) {
	originalFire := time.Now().Add(time.Hour).Round(time.Second)
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42", Status: persistence.ReminderStatusPending,
		FireAt: originalFire,
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_x","content":"updated body","rationale":"fix typo"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 || repo.updates[0].Content != "updated body" {
		t.Errorf("update call = %+v", repo.updates)
	}
	// FireAt should be carried forward from the existing row
	// when neither fire_at nor fire_in_seconds is supplied.
	if !repo.updates[0].FireAt.Equal(originalFire) {
		t.Errorf("FireAt drifted: got %v want %v", repo.updates[0].FireAt, originalFire)
	}
}

// TestUpdateReminderTool_RefusesNonPending — the heartbeat may
// already be sending the row; refuse rather than race.
func TestUpdateReminderTool_RefusesNonPending(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_firing"] = &persistence.Reminder{
		ID: "rem_firing", OperatorID: "telegram:42", ChannelRef: "42", Status: persistence.ReminderStatusFiring,
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_firing","fire_in_seconds":60,"rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "only pending") &&
		!strings.Contains(strings.ToLower(res.Content), "can't be modified") {
		t.Errorf("expected non-pending refusal, got %q", res.Content)
	}
}

// TestUpdateReminderTool_RefusesCrossOperator: same cross-
// operator guard the cancel path uses.
func TestUpdateReminderTool_RefusesCrossOperator(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_other"] = &persistence.Reminder{
		ID: "rem_other", OperatorID: "telegram:99", ChannelRef: "99", Status: persistence.ReminderStatusPending,
		FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_other","fire_in_seconds":60,"rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "not yours") &&
		!strings.Contains(strings.ToLower(res.Content), "different operator") {
		t.Errorf("expected refusal, got %q", res.Content)
	}
}

// TestCancelReminderTool_RepoErrorSurfaces ensures DB blips
// don't silently succeed.
func TestCancelReminderTool_RepoErrorSurfaces(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42", Status: persistence.ReminderStatusPending,
	}
	repo.cancelErr = errors.New("disk full")
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "cancel_reminder",
		Arguments: `{"reminder_id":"rem_x","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "disk full") &&
		!strings.Contains(strings.ToLower(res.Content), "failed") {
		t.Errorf("expected failure to propagate, got %q", res.Content)
	}
}

// TestDispatcherTools_ReminderChangeRegistered: cancel + update
// tools must appear in DispatcherTools() so the LLM sees them.
func TestDispatcherTools_ReminderChangeRegistered(t *testing.T) {
	tools := DispatcherTools()
	want := map[string]bool{
		"cancel_reminder": false, "update_reminder": false,
		"pause_reminder": false, "resume_reminder": false,
	}
	for _, tool := range tools {
		if _, ok := want[tool.Function.Name]; ok {
			want[tool.Function.Name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("tool %s not registered in DispatcherTools()", name)
		}
	}
}

// TestPauseReminderTool_HappyPath — pause_reminder calls
// repo.Pause on a pending reminder the caller owns.
func TestPauseReminderTool_HappyPath(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 9 * * *",
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "pause_reminder",
		Arguments: `{"reminder_id":"rem_x","rationale":"traveling this week"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.pauses) != 1 || repo.pauses[0] != "rem_x" {
		t.Errorf("Pause calls = %v", repo.pauses)
	}
}

// TestPauseReminderTool_MidRunFriendlyMessage — Task 10's
// Pause returns ErrNotFound when the row isn't in a pausable
// (pending) state, e.g. it's currently firing/awaiting_task. The
// tool must translate that into a friendly "try again after the
// run finishes" message rather than a raw not-found.
func TestPauseReminderTool_MidRunFriendlyMessage(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusAwaitingTask, CronExpr: "0 9 * * *",
	}
	repo.pauseErr = persistence.ErrNotFound
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "pause_reminder",
		Arguments: `{"reminder_id":"rem_x","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(res.Content, "mid-run") {
		t.Errorf("expected mid-run friendly message, got %q", res.Content)
	}
}

// TestPauseReminderTool_RefusesOtherOperator mirrors the
// cancel/update cross-operator guard.
func TestPauseReminderTool_RefusesOtherOperator(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_other"] = &persistence.Reminder{
		ID: "rem_other", OperatorID: "telegram:99", ChannelRef: "99",
		Status: persistence.ReminderStatusPending, CronExpr: "0 9 * * *",
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "pause_reminder",
		Arguments: `{"reminder_id":"rem_other","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "not yours") &&
		!strings.Contains(strings.ToLower(res.Content), "different operator") {
		t.Errorf("expected cross-operator refusal, got %q", res.Content)
	}
	if len(repo.pauses) != 0 {
		t.Errorf("Pause must not fire across operators")
	}
}

// TestResumeReminderTool_RecurringComputesNextFire — resume on a
// paused recurring row recomputes NextFireAt from CronExpr and
// calls repo.Resume with it.
func TestResumeReminderTool_RecurringComputesNextFire(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPaused, CronExpr: "*/5 * * * *",
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "resume_reminder",
		Arguments: `{"reminder_id":"rem_x","rationale":"back from vacation"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.resumes) != 1 || repo.resumes[0].ID != "rem_x" {
		t.Fatalf("Resume calls = %v", repo.resumes)
	}
	if !repo.resumes[0].NextFireAt.After(time.Now()) {
		t.Errorf("NextFireAt = %v, want a future time", repo.resumes[0].NextFireAt)
	}
}

// TestResumeReminderTool_OneShotRefused — one-shot reminders
// (empty CronExpr) can't be paused/resumed meaningfully; the
// tool must refuse rather than call repo.Resume.
func TestResumeReminderTool_OneShotRefused(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "resume_reminder",
		Arguments: `{"reminder_id":"rem_x","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "one-shot") {
		t.Errorf("expected one-shot refusal, got %q", res.Content)
	}
	if len(repo.resumes) != 0 {
		t.Errorf("Resume should NOT have fired for a one-shot reminder")
	}
}

// TestResumeReminderTool_RefusesOtherOperator mirrors the
// cancel/update/pause cross-operator guard.
func TestResumeReminderTool_RefusesOtherOperator(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_other"] = &persistence.Reminder{
		ID: "rem_other", OperatorID: "telegram:99", ChannelRef: "99",
		Status: persistence.ReminderStatusPaused, CronExpr: "0 9 * * *",
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "resume_reminder",
		Arguments: `{"reminder_id":"rem_other","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "not yours") &&
		!strings.Contains(strings.ToLower(res.Content), "different operator") {
		t.Errorf("expected cross-operator refusal, got %q", res.Content)
	}
	if len(repo.resumes) != 0 {
		t.Errorf("Resume must not fire across operators")
	}
}

// TestResumeReminderTool_NotPausedFriendlyMessage — repo.Resume
// returns ErrNotFound when the row's status has drifted away
// from 'paused' between Get and Resume (race with the heartbeat
// or a second chat message). The tool must surface a friendly
// "isn't paused" message.
func TestResumeReminderTool_NotPausedFriendlyMessage(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_x"] = &persistence.Reminder{
		ID: "rem_x", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 9 * * *",
	}
	repo.resumeErr = persistence.ErrNotFound
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "resume_reminder",
		Arguments: `{"reminder_id":"rem_x","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "isn't paused") {
		t.Errorf("expected not-paused friendly message, got %q", res.Content)
	}
}

// TestUpdateReminderTool_EditsTaskKindContent — Task 11 scope
// item 4: confirm update_reminder can edit a recurring
// task-kind reminder's content (the prompt it runs) while it's
// pending between fires. UpdateFields only guards on
// status=='pending', which a recurring task-kind row satisfies
// between runs — no code change was needed for this to work,
// this test documents/locks in that behavior.
func TestUpdateReminderTool_EditsTaskKindContent(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "proj_1",
		Content: "daily news digest", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","content":"daily news digest, focus on AI + markets","rationale":"refine prompt"}`}}
	res := te.Execute(context.Background(), tc, "", nil, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Errorf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 || repo.updates[0].Content != "daily news digest, focus on AI + markets" {
		t.Errorf("update call = %+v", repo.updates)
	}
	if repo.rows["rem_task"].Content != "daily news digest, focus on AI + markets" {
		t.Errorf("row content not updated: %q", repo.rows["rem_task"].Content)
	}
}

// TestUpdateReminderTool_EditsCron — a recurring reminder's cadence can
// be changed via `cron`; the new expression is persisted and fire_at is
// recomputed from it (mirrors set_reminder's cron-wins schedule
// resolution). Backlog: "edit a reminder's cron schedule via chat".
func TestUpdateReminderTool_EditsCron(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "digest", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","cron":"0 9 * * *","rationale":"later slot"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news"}, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 || repo.updates[0].CronExpr != "0 9 * * *" {
		t.Fatalf("expected cron persisted, update call = %+v", repo.updates)
	}
	if !repo.updates[0].FireAt.After(time.Now()) {
		t.Errorf("fire_at should be recomputed to a future cron slot, got %v", repo.updates[0].FireAt)
	}
	if repo.rows["rem_task"].CronExpr != "0 9 * * *" {
		t.Errorf("row cron not updated: %q", repo.rows["rem_task"].CronExpr)
	}
}

// TestUpdateReminderTool_CronWinsOverFireInSeconds — when both `cron`
// and `fire_in_seconds` are supplied, cron wins (recomputed next fire),
// matching the tool-schema contract and set_reminder's resolution.
// Locks the precedence so a refactor can't regress to last-field-wins.
func TestUpdateReminderTool_CronWinsOverFireInSeconds(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	// fire_in_seconds=60 would put fire_at ~1m out; the daily cron's next
	// fire is materially further than 2 minutes away, so we can assert the
	// cron value took effect rather than the seconds offset.
	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","cron":"0 9 * * *","fire_in_seconds":60,"rationale":"both"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news"}, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 || repo.updates[0].CronExpr != "0 9 * * *" {
		t.Fatalf("cron should win + persist, got %+v", repo.updates)
	}
	if time.Until(repo.updates[0].FireAt) < 2*time.Minute {
		t.Errorf("fire_at should be the cron next-fire, not the 60s offset: %v", repo.updates[0].FireAt)
	}
}

// TestUpdateReminderTool_RejectsInvalidCron — a malformed cron is
// refused with a friendly message and nothing is written.
func TestUpdateReminderTool_RejectsInvalidCron(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","cron":"not a cron","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news"}, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "cron") {
		t.Errorf("expected cron validation error, got %q", res.Content)
	}
	if len(repo.updates) != 0 {
		t.Errorf("no update should have been written, got %+v", repo.updates)
	}
}

// TestUpdateReminderTool_EditsProjectReAuthAllows — changing a task-kind
// reminder's target project re-runs the same session ACL set_reminder
// applies; an allowed project is accepted and persisted.
func TestUpdateReminderTool_EditsProjectReAuthAllows(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","project":"markets","rationale":"move project"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news", "markets"}, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") || strings.Contains(strings.ToLower(res.Content), "not permitted") {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 || repo.updates[0].ProjectID != "markets" {
		t.Fatalf("expected project persisted, update call = %+v", repo.updates)
	}
	if repo.rows["rem_task"].ProjectID != "markets" {
		t.Errorf("row project not updated: %q", repo.rows["rem_task"].ProjectID)
	}
}

// TestUpdateReminderTool_EditsProjectDeniedByACL — a project OUTSIDE the
// session's allowedProjects is refused; nothing is written. This is the
// security-critical case: without re-auth, editing project would be an
// escalation path around set_reminder's create-time ACL.
func TestUpdateReminderTool_EditsProjectDeniedByACL(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","project":"secret","rationale":"sneaky"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news", "markets"}, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "not permitted") {
		t.Errorf("expected ACL refusal, got %q", res.Content)
	}
	if len(repo.updates) != 0 {
		t.Errorf("denied edit must not write; got %+v", repo.updates)
	}
	if repo.rows["rem_task"].ProjectID != "news" {
		t.Errorf("row project must be unchanged, got %q", repo.rows["rem_task"].ProjectID)
	}
}

// TestUpdateReminderTool_ProjectEditRejectedOnTextKind — a text-kind
// reminder runs nothing, so `project` is meaningless; editing it is
// refused rather than silently stored.
func TestUpdateReminderTool_ProjectEditRejectedOnTextKind(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_text"] = &persistence.Reminder{
		ID: "rem_text", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, Kind: persistence.ReminderKindText,
		FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_text","project":"news","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news"}, 42, nil)

	if !strings.Contains(strings.ToLower(res.Content), "task-kind") {
		t.Errorf("expected text-kind rejection mentioning task-kind, got %q", res.Content)
	}
	if len(repo.updates) != 0 {
		t.Errorf("no update expected, got %+v", repo.updates)
	}
}

// TestUpdateReminderTool_ContentOnlyKeepsCronAndProject — a content-only
// edit must pass empty cron/project so COALESCE(NULLIF...) leaves the
// recurring schedule and project untouched. Locks the "empty == keep"
// contract the security review asked to scrutinize.
func TestUpdateReminderTool_ContentOnlyKeepsCronAndProject(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "old", FireAt: time.Now().Add(time.Hour),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","content":"new body","rationale":"x"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news"}, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "error") {
		t.Fatalf("expected success, got %q", res.Content)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("update calls = %d, want 1", len(repo.updates))
	}
	if repo.updates[0].CronExpr != "" || repo.updates[0].ProjectID != "" {
		t.Errorf("content-only edit must pass empty cron/project (keep-if-empty), got cron=%q project=%q",
			repo.updates[0].CronExpr, repo.updates[0].ProjectID)
	}
	if repo.rows["rem_task"].CronExpr != "0 7 * * *" || repo.rows["rem_task"].ProjectID != "news" {
		t.Errorf("cron/project must be preserved, got cron=%q project=%q",
			repo.rows["rem_task"].CronExpr, repo.rows["rem_task"].ProjectID)
	}
}

// TestUpdateReminderTool_CarryForwardIgnoresStaleFireAt — a content/
// project-only edit (no schedule field) on a recurring reminder whose
// fire_at has momentarily slipped past (not yet leased) must NOT be
// rejected with "fire time is in the past"; the carry-forward path skips
// the past-guard (review finding 3).
func TestUpdateReminderTool_CarryForwardIgnoresStaleFireAt(t *testing.T) {
	repo := newReminderRepoForTools()
	repo.rows["rem_task"] = &persistence.Reminder{
		ID: "rem_task", OperatorID: "telegram:42", ChannelRef: "42",
		Status: persistence.ReminderStatusPending, CronExpr: "0 7 * * *",
		Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "digest", FireAt: time.Now().Add(-2 * time.Minute),
	}
	te := &ToolExecutor{reminderRepo: repo}

	tc := chat.ToolCall{Function: chat.FunctionCall{Name: "update_reminder",
		Arguments: `{"reminder_id":"rem_task","content":"tweaked prompt","rationale":"edit only"}`}}
	res := te.Execute(context.Background(), tc, "", []string{"news"}, 42, nil)

	if strings.Contains(strings.ToLower(res.Content), "past") || strings.Contains(strings.ToLower(res.Content), "error") {
		t.Fatalf("carry-forward edit must not fail on a stale fire_at, got %q", res.Content)
	}
	if len(repo.updates) != 1 || repo.updates[0].Content != "tweaked prompt" {
		t.Fatalf("update = %+v", repo.updates)
	}
}
