package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// stubReminderRepo captures Insert calls for set_reminder tests.
type stubReminderRepo struct {
	mu        sync.Mutex
	rows      []*persistence.Reminder
	pending   int
	insertErr error
	// taskCount/taskCountErr drive CountTaskByOperator for Task 8's
	// task-kind cap tests.
	taskCount    int
	taskCountErr error
}

func (s *stubReminderRepo) Insert(_ context.Context, r *persistence.Reminder) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == "" {
		r.ID = "rem_test_" + r.OperatorID
	}
	cp := *r
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubReminderRepo) Get(_ context.Context, _ string) (*persistence.Reminder, error) {
	panic("not used")
}
func (s *stubReminderRepo) List(_ context.Context, _ persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	panic("not used")
}
func (s *stubReminderRepo) LeaseDue(_ context.Context, _ time.Time, _ int) ([]*persistence.Reminder, error) {
	panic("not used")
}
func (s *stubReminderRepo) MarkFired(_ context.Context, _ string) error { panic("not used") }
func (s *stubReminderRepo) Reschedule(_ context.Context, _ string, _ time.Time) error {
	panic("not used")
}
func (s *stubReminderRepo) MarkErrored(_ context.Context, _, _ string) error {
	panic("not used")
}
func (s *stubReminderRepo) Cancel(_ context.Context, _ string) error { panic("not used") }
func (s *stubReminderRepo) Delete(_ context.Context, _ string) error { panic("not used") }
func (s *stubReminderRepo) UpdateFields(_ context.Context, _ string, _ persistence.ReminderFieldUpdate) error {
	panic("not used")
}
func (s *stubReminderRepo) CountPendingByOperator(_ context.Context, _ string) (int, error) {
	return s.pending, nil
}

// The following four satisfy the Task 4 additions to
// persistence.ReminderRepository (task-spawn/claim/finalize). Not
// exercised by set_reminder tests yet — compile stubs only.
func (s *stubReminderRepo) MarkTaskSpawned(_ context.Context, _, _ string, _ *time.Time) error {
	panic("not used")
}
func (s *stubReminderRepo) ClaimDelivery(_ context.Context, _ string) (*persistence.Reminder, bool, error) {
	panic("not used")
}
func (s *stubReminderRepo) FinalizeDelivery(_ context.Context, _, _ string, _ bool) error {
	panic("not used")
}
func (s *stubReminderRepo) CountTaskByOperator(_ context.Context, _ string) (int, error) {
	return s.taskCount, s.taskCountErr
}
func (s *stubReminderRepo) Pause(_ context.Context, _ string) error { panic("not used") }
func (s *stubReminderRepo) Resume(_ context.Context, _ string, _ time.Time) error {
	panic("not used")
}

// ReclaimStuckFiring is Task 14's crash-recovery sweep addition. Not
// exercised by set_reminder tests — compile stub only.
func (s *stubReminderRepo) ReclaimStuckFiring(_ context.Context, _ time.Time, _ int) ([]*persistence.Reminder, error) {
	panic("not used")
}

type stubKicker struct{ called int }

func (s *stubKicker) Kick() { s.called++ }

func newSetReminderExecutor(repo *stubReminderRepo, kicker *stubKicker) *ToolExecutor {
	te := &ToolExecutor{
		reminderRepo: repo,
		logger:       zerolog.Nop(),
	}
	// Only wire the interface field when the test actually supplied
	// a kicker — a typed nil (*stubKicker)(nil) would still satisfy
	// the interface != nil check and crash on Kick().
	if kicker != nil {
		te.reminderKicker = kicker
	}
	return te
}

// TestSetReminder_HappyPath_FireInSeconds drives the seconds-offset
// branch — the most common LLM input for "remind me in 2 hours".
func TestSetReminder_HappyPath_FireInSeconds(t *testing.T) {
	repo := &stubReminderRepo{}
	kicker := &stubKicker{}
	te := newSetReminderExecutor(repo, kicker)

	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 7200, "content": "check deploy"}`,
		1234, // Telegram chat id
		"assistant", nil,
	)
	if !strings.Contains(res.Content, "Reminder rem_") {
		t.Errorf("response should include reminder id; got %q", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("repo row count: %d", len(repo.rows))
	}
	r := repo.rows[0]
	if r.OperatorID != "telegram:1234" {
		t.Errorf("operator_id = %q", r.OperatorID)
	}
	if r.Channel != "telegram" {
		t.Errorf("channel = %q", r.Channel)
	}
	if r.ChannelRef != "1234" {
		t.Errorf("channel_ref = %q", r.ChannelRef)
	}
	if r.ProjectID != "assistant" {
		t.Errorf("project_id = %q", r.ProjectID)
	}
	if kicker.called != 1 {
		t.Errorf("kicker should fire once; got %d", kicker.called)
	}
}

// TestSetReminder_SlackSession pins the non-Telegram scheduling path. Slack's
// reminder runner + completion notifier already know how to route a durable
// channel ref; the dispatcher used to reject the turn before inserting it
// merely because Slack has no numeric Telegram-style ChatID.
func TestSetReminder_SlackSession(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	ctx := WithOperatorID(context.Background(), "slack:U_JANKA")

	res := te.setReminderForSession(
		ctx,
		`{"fire_in_seconds": 7200, "content": "research this and write report.pdf", "kind":"task"}`,
		0,
		"slack:T_TEAM/D_JANKA#main",
		"janka", []string{"janka"},
	)
	if !strings.Contains(res.Content, "Scheduled update") {
		t.Fatalf("response = %q, want scheduled update confirmation", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("repo row count = %d, want 1", len(repo.rows))
	}
	row := repo.rows[0]
	if row.OperatorID != "slack:U_JANKA" {
		t.Errorf("operator_id = %q", row.OperatorID)
	}
	if row.Channel != "slack" {
		t.Errorf("channel = %q", row.Channel)
	}
	if row.ChannelRef != "T_TEAM/D_JANKA#main" {
		t.Errorf("channel_ref = %q", row.ChannelRef)
	}
}

// TestSetReminder_RFC3339Path mirrors the timestamp branch.
func TestSetReminder_RFC3339Path(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	fire := time.Now().Add(5 * time.Hour).Format(time.RFC3339)
	res := te.setReminder(
		context.Background(),
		`{"fire_at":"`+fire+`","content":"x"}`,
		1, "p", nil,
	)
	if !strings.Contains(res.Content, "Reminder rem_") {
		t.Errorf("response: %s", res.Content)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("rows: %d", len(repo.rows))
	}
}

// TestSetReminder_RejectsPast covers the "fire time in the past"
// guard.
func TestSetReminder_RejectsPast(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	past := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	res := te.setReminder(
		context.Background(),
		`{"fire_at":"`+past+`","content":"x"}`,
		1, "p", nil,
	)
	if !strings.Contains(res.Content, "past") {
		t.Errorf("expected past-rejection; got %q", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have been written to")
	}
}

// TestSetReminder_RejectsBeyondCap covers the max-future-window
// guard — defends against the LLM emitting "remind me in 50 years".
func TestSetReminder_RejectsBeyondCap(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 99999999999, "content": "x"}`,
		1, "p", nil,
	)
	if !strings.Contains(res.Content, "future") {
		t.Errorf("expected future-cap rejection; got %q", res.Content)
	}
}

// TestSetReminder_RefusesMissingDestination covers synthetic/tool-only calls
// that have neither a channel session key nor Telegram's legacy numeric ID.
func TestSetReminder_RefusesMissingDestination(t *testing.T) {
	repo := &stubReminderRepo{}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 60, "content": "x"}`,
		0, "p", nil,
	)
	if !strings.Contains(res.Content, "no channel of record") {
		t.Errorf("expected missing-destination message; got %q", res.Content)
	}
}

// TestSetReminder_EnforcesPendingCap covers the per-operator
// concurrent-pending cap.
func TestSetReminder_EnforcesPendingCap(t *testing.T) {
	repo := &stubReminderRepo{pending: reminderMaxPendingPerOperator}
	te := newSetReminderExecutor(repo, nil)
	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 60, "content": "x"}`,
		1, "p", nil,
	)
	if !strings.Contains(res.Content, "cap") {
		t.Errorf("expected cap rejection; got %q", res.Content)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo should not have accepted the insert")
	}
}

// TestSetReminder_RepoUnwired the daemon doesn't have the
// reminders subsystem — tool returns a clean message instead
// of 500-ing.
func TestSetReminder_RepoUnwired(t *testing.T) {
	te := &ToolExecutor{logger: zerolog.Nop()}
	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 60, "content": "x"}`,
		1, "p", nil,
	)
	if !strings.Contains(res.Content, "not configured") {
		t.Errorf("expected unwired message; got %q", res.Content)
	}
}

// stubAdminAudit captures Insert calls for the set_reminder
// audit-emit test.
type stubAdminAudit struct {
	rows []*persistence.AdminAuditEntry
}

func (s *stubAdminAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	cp := *e
	s.rows = append(s.rows, &cp)
	return nil
}
func (s *stubAdminAudit) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// TestSetReminder_AuditEmitted confirms the dispatcher writes a
// reminder.set admin-audit row on a successful Insert. Pins the
// shape so /ui/admin/audit filtering on `reminder.set` keeps
// working.
func TestSetReminder_AuditEmitted(t *testing.T) {
	repo := &stubReminderRepo{}
	audit := &stubAdminAudit{}
	te := newSetReminderExecutor(repo, nil)
	te.adminAuditRepo = audit

	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 60, "content": "deploy check"}`,
		1234, "assistant", nil,
	)
	if !strings.Contains(res.Content, "Reminder rem_") {
		t.Fatalf("happy path expected; got %q", res.Content)
	}
	if len(audit.rows) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(audit.rows))
	}
	row := audit.rows[0]
	if row.Action != "reminder.set" {
		t.Errorf("action = %q, want reminder.set", row.Action)
	}
	if row.Principal != "telegram:1234" {
		t.Errorf("principal = %q, want telegram:1234", row.Principal)
	}
	if row.Source != "dispatcher" {
		t.Errorf("source = %q, want dispatcher", row.Source)
	}
	if !strings.Contains(row.After, `"channel":"telegram"`) {
		t.Errorf("after should include channel; got %s", row.After)
	}
}

// TestSetReminder_AuditSkippedOnFailure: when the Insert fails,
// no audit row should be written — we record only successes so
// the action's presence in /ui/admin/audit means the reminder
// actually landed.
func TestSetReminder_AuditSkippedOnFailure(t *testing.T) {
	repo := &stubReminderRepo{insertErr: errors.New("db down")}
	audit := &stubAdminAudit{}
	te := newSetReminderExecutor(repo, nil)
	te.adminAuditRepo = audit
	res := te.setReminder(
		context.Background(),
		`{"fire_in_seconds": 60, "content": "x"}`,
		1234, "p", nil,
	)
	if !strings.Contains(res.Content, "insert failed") {
		t.Fatalf("expected insert-failed response; got %q", res.Content)
	}
	if len(audit.rows) != 0 {
		t.Errorf("audit should not record failed inserts; got %d rows", len(audit.rows))
	}
}

// TestResolveReminderFireAt_BothMissing covers the validation
// branch the LLM would hit if it forgot both timestamp fields.
func TestResolveReminderFireAt_BothMissing(t *testing.T) {
	_, err := resolveReminderFireAt(setReminderArgs{}, time.Now())
	if err == nil {
		t.Errorf("expected error when both fire_at and fire_in_seconds are empty")
	}
}
