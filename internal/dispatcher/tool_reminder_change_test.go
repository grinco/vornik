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
}

type reminderUpdateSpy struct {
	ID      string
	FireAt  time.Time
	Content string
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
func (r *reminderRepoForTools) UpdateFields(_ context.Context, id string, fireAt time.Time, content string) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, reminderUpdateSpy{ID: id, FireAt: fireAt, Content: content})
	if row, ok := r.rows[id]; ok {
		row.FireAt = fireAt
		if content != "" {
			row.Content = content
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

	if !strings.Contains(strings.ToLower(res.Content), "no longer pending") &&
		!strings.Contains(strings.ToLower(res.Content), "already") {
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
	want := map[string]bool{"cancel_reminder": false, "update_reminder": false}
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
