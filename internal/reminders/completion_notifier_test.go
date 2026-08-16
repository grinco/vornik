// Task 6: reminderCompletionNotifier. Reuses runner_test.go's stubRepo /
// stubChannel / stubResolver fakes rather than hand-rolling new ones — see
// docs/plans/2026-07-19-scheduled-task-notifications-plan.md Task 6.
package reminders

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type captureReminderFileSender struct {
	names []string
	data  [][]byte
}

func (s *captureReminderFileSender) SendArtifactFile(_ context.Context, name string, content io.Reader, _ string) error {
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	s.names = append(s.names, name)
	s.data = append(s.data, b)
	return nil
}

type stubReminderFileResolver struct{ sender ReminderFileSender }

func (r stubReminderFileResolver) ResolveReminderFileSender(_, _ string) ReminderFileSender {
	return r.sender
}

type stubReminderArtifactReader map[string][]byte

func (r stubReminderArtifactReader) Retrieve(_ context.Context, id string) ([]byte, error) {
	b, ok := r[id]
	if !ok {
		return nil, errors.New("missing artifact bytes")
	}
	return bytes.Clone(b), nil
}

// TestCompletionNotifierDeliversAndFinalizes: a successful task
// completion for a recurring task-kind reminder sends exactly one
// message and finalizes with terminal=false (recurring rows go back to
// pending, not fired — see design §2.3 / Reminder.IsRecurring).
func TestCompletionNotifierDeliversAndFinalizes(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_1", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Daily digest", CronExpr: "0 7 * * *",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_1"}, true, "here is your digest")

	if len(ch.sent) != 1 {
		t.Fatalf("channel.Send count = %d, want 1", len(ch.sent))
	}
	if !strings.Contains(ch.sent[0], "Daily digest") {
		t.Errorf("body = %q, want it to include the reminder label", ch.sent[0])
	}
	if !strings.Contains(ch.sent[0], "here is your digest") {
		t.Errorf("body = %q, want it to include the outcome message", ch.sent[0])
	}
	if !repo.finalized {
		t.Fatal("expected FinalizeDelivery to be called")
	}
	if repo.finalizedTerminal {
		t.Error("recurring reminder must finalize with terminal=false")
	}
}

// TestCompletionNotifierDeliversOutputArtifacts pins the scheduled-report
// promise: when a Slack task produces PDF/HTML outputs, completion delivers
// those files into the original thread instead of only posting a task link.
func TestCompletionNotifierDeliversOutputArtifacts(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_report", Kind: persistence.ReminderKindTask, Channel: "slack",
		ChannelRef: "T123/D_JANKA#main", Content: "Research and write a report",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if f.TaskID == nil || *f.TaskID != "task_report" {
				t.Fatalf("artifact filter task = %v, want task_report", f.TaskID)
			}
			return []*persistence.Artifact{
				{ID: "a_pdf", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput},
				{ID: "a_tmp", Name: "notes.tmp", ArtifactClass: persistence.ArtifactClassIntermediate},
			}, nil
		},
	}
	files := &captureReminderFileSender{}
	n := NewCompletionNotifier(
		repo,
		&stubResolver{channels: map[string]conversation.Channel{"slack": ch}},
		nil, zerolog.Nop(), time.Now,
		WithArtifactDelivery(
			artifactRepo,
			stubReminderArtifactReader{"a_pdf": []byte("pdf bytes")},
			stubReminderFileResolver{sender: files},
		),
	)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_report"}, true, "done")
	// A duplicate executor callback loses ClaimDelivery and must not upload the
	// same report again. This is the attachment-specific side of the reminder's
	// existing at-most-once delivery contract.
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_report"}, true, "done")

	if len(files.names) != 1 || files.names[0] != "report.pdf" {
		t.Fatalf("delivered names = %v, want [report.pdf]", files.names)
	}
	if string(files.data[0]) != "pdf bytes" {
		t.Errorf("delivered bytes = %q", files.data[0])
	}
	if !repo.finalized {
		t.Fatal("successful attachment delivery must finalize the reminder")
	}
}

// TestCompletionNotifierOneShotFinalizesTerminal: a one-shot (no
// CronExpr) task-kind reminder finalizes terminal=true so it lands on
// fired, not pending.
func TestCompletionNotifierOneShotFinalizesTerminal(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_2", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "One-off report",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_2"}, true, "done")

	if !repo.finalized || !repo.finalizedTerminal {
		t.Fatalf("one-shot finalize wrong: finalized=%v terminal=%v", repo.finalized, repo.finalizedTerminal)
	}
}

// TestCompletionNotifierBoundedRecurrenceDoneFinalizesTerminal covers
// review Finding 1: a bounded recurring reminder (CronExpr set AND
// RecurrenceUntil set) whose next slot falls AFTER the bound must
// finalize terminal=true, even though rem.IsRecurring() is still true.
// Without the bound check, terminal=false would send this row back to
// 'pending' with its stale/past fire_at (deliverTask armed fire_at=nil
// for the last-in-bound fire; MarkTaskSpawned's COALESCE left it
// unchanged) — LeaseDue would then re-fire it on every tick forever,
// never honoring "until". Mirrors runner.finalize's own bound check
// (runner.go's finalize, ~line 345).
//
// Clock pinned to Sunday 2026-05-24 16:00 UTC; cron "0 9 * * 1" (every
// Monday 09:00) next-fires 2026-05-25 09:00 — AFTER the bound below.
func TestCompletionNotifierBoundedRecurrenceDoneFinalizesTerminal(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 24, 17, 0, 0, 0, time.UTC) // before next Monday
	rem := &persistence.Reminder{
		ID: "rem_bound_done", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Bounded digest",
		CronExpr: "0 9 * * 1", RecurrenceUntil: &until,
		FireAt: clock, // stale/past: last-in-bound fire left it unarmed
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	before := testutil.ToFloat64(metricTaskSkipped)
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), func() time.Time { return clock })

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_bound_done"}, true, "done")

	if !repo.finalized || !repo.finalizedTerminal {
		t.Fatalf("bounded-done finalize wrong: finalized=%v terminal=%v, want finalized=true terminal=true", repo.finalized, repo.finalizedTerminal)
	}
	if got := testutil.ToFloat64(metricTaskSkipped) - before; got != 0 {
		t.Errorf("metricTaskSkipped delta = %v, want 0 (bounded-done must not falsely trip the skip metric)", got)
	}
}

// TestCompletionNotifierBoundedRecurrenceStillWithinBound: same shape
// as above but RecurrenceUntil is far enough out that the next slot is
// still within bound — must finalize terminal=false (re-arms normally).
func TestCompletionNotifierBoundedRecurrenceStillWithinBound(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // after next Monday 2026-05-25
	rem := &persistence.Reminder{
		ID: "rem_bound_ok", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Bounded digest",
		CronExpr: "0 9 * * 1", RecurrenceUntil: &until,
		FireAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC), // armed for the NEXT slot, still future
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), func() time.Time { return clock })

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_bound_ok"}, true, "done")

	if !repo.finalized || repo.finalizedTerminal {
		t.Fatalf("bounded-still-active finalize wrong: finalized=%v terminal=%v, want finalized=true terminal=false", repo.finalized, repo.finalizedTerminal)
	}
}

// TestCompletionNotifierDeliverableUsesResultEnvelope covers Finding 2:
// when the completed task carries a ResultEnvelope, the delivered body
// must contain it verbatim rather than falling back to the executor's
// plain message string.
func TestCompletionNotifierDeliverableUsesResultEnvelope(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_envelope", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Daily digest", CronExpr: "0 7 * * *",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	task := &persistence.Task{ID: "task_envelope", ResultEnvelope: []byte(`{"headline":"3 deploys shipped"}`)}
	n.NotifyTaskCompleted(context.Background(), task, true, "fallback message should not appear")

	if len(ch.sent) != 1 {
		t.Fatalf("channel.Send count = %d, want 1", len(ch.sent))
	}
	if !strings.Contains(ch.sent[0], `"headline":"3 deploys shipped"`) {
		t.Fatalf("body = %q, want it to contain the ResultEnvelope", ch.sent[0])
	}
	if strings.Contains(ch.sent[0], "fallback message should not appear") {
		t.Fatalf("body = %q, must prefer ResultEnvelope over the plain message", ch.sent[0])
	}
}

// TestCompletionNotifierSkipMetricIncrementsOnOverrun covers Finding 4:
// an unbounded recurring reminder whose armed FireAt already sits in
// the past (the task overran its slot) must bump metricTaskSkipped.
func TestCompletionNotifierSkipMetricIncrementsOnOverrun(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	rem := &persistence.Reminder{
		ID: "rem_overrun", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Digest", CronExpr: "0 7 * * *",
		FireAt: clock.Add(-time.Hour), // already past when we deliver
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	before := testutil.ToFloat64(metricTaskSkipped)
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), func() time.Time { return clock })

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_overrun"}, true, "done")

	if got := testutil.ToFloat64(metricTaskSkipped) - before; got != 1 {
		t.Fatalf("metricTaskSkipped delta = %v, want 1", got)
	}
}

// TestLabelIsRuneSafe covers Finding 3: label() must not split a
// multi-byte UTF-8 rune when truncating. 70 "é" (2 bytes each) plus
// emoji exceeds the 60-rune cap at a non-ASCII boundary — a byte-index
// slice (s[:57]) would produce invalid UTF-8 or a garbled rune here.
func TestLabelIsRuneSafe(t *testing.T) {
	longNonASCII := strings.Repeat("é", 70) + "📊"
	rem := &persistence.Reminder{Content: longNonASCII}

	got := label(rem)

	if !strings.HasSuffix(got, "...") {
		t.Fatalf("label() = %q, want truncated with ellipsis", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("label() = %q, not valid UTF-8 (rune boundary split)", got)
	}
	// 57 runes + "..." = 60 runes total.
	if got := []rune(got); len(got) != 60 {
		t.Fatalf("label() rune length = %d, want 60", len(got))
	}
}

// TestCompletionNotifierIgnoresUnclaimed covers the at-most-once guard:
// when ClaimDelivery reports ok=false (not ours / already delivered /
// duplicate HA callback) the notifier must not send anything.
func TestCompletionNotifierIgnoresUnclaimed(t *testing.T) {
	repo := newStubRepo() // claim == nil => ClaimDelivery returns ok=false
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "other"}, true, "x")

	if len(ch.sent) != 0 {
		t.Fatal("must not deliver a task it didn't spawn")
	}
	if repo.finalized {
		t.Fatal("must not finalize a delivery it never claimed")
	}
}

// TestCompletionNotifierClaimIsAtMostOnce drives NotifyTaskCompleted
// twice for the same task id, mirroring a duplicate completion callback
// (e.g. an HA-racing executor instance). The stubRepo hands out its
// claim exactly once, so the second call must be a no-op.
func TestCompletionNotifierClaimIsAtMostOnce(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_3", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Digest",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	task := &persistence.Task{ID: "task_3"}
	n.NotifyTaskCompleted(context.Background(), task, true, "first")
	n.NotifyTaskCompleted(context.Background(), task, true, "second")

	if len(ch.sent) != 1 {
		t.Fatalf("channel.Send count = %d, want exactly 1 (at-most-once)", len(ch.sent))
	}
}

// TestCompletionNotifierFailureNotice covers success=false: the
// operator gets a distinct failure-notice body rather than the
// deliverable, and delivery still finalizes (the outcome — success or
// failure — is what's being delivered).
func TestCompletionNotifierFailureNotice(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_4", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Daily digest", CronExpr: "0 7 * * *",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_4"}, false, "boom")

	if len(ch.sent) != 1 {
		t.Fatalf("channel.Send count = %d, want 1", len(ch.sent))
	}
	if !strings.Contains(ch.sent[0], "couldn't complete") {
		t.Fatalf("expected failure notice, got %v", ch.sent[0])
	}
	if !strings.Contains(ch.sent[0], "boom") {
		t.Fatalf("expected failure notice to include the message, got %v", ch.sent[0])
	}
	if !repo.finalized {
		t.Error("a delivered failure notice must still finalize (it's the outcome delivery)")
	}
}

// TestCompletionNotifierMissingChannelMarksErrored: the resolver has no
// channel registered for rem.Channel — mirrors runner.go's own
// "channel not configured" handling. Must not panic, must not finalize
// (delivery never happened).
func TestCompletionNotifierMissingChannelMarksErrored(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_5", Kind: persistence.ReminderKindTask, Channel: "webchat",
		ChannelRef: "chat123", Content: "Digest",
	}
	repo := newStubRepo()
	repo.claim = rem
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{}}, nil, zerolog.Nop(), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_5"}, true, "x")

	if msg, ok := repo.errored["rem_5"]; !ok || msg == "" {
		t.Errorf("expected rem_5 to be marked errored, got %q (ok=%v)", msg, ok)
	}
	if repo.finalized {
		t.Error("must not finalize when the channel isn't configured")
	}
}

// TestCompletionNotifierSendFailMarksErrored: the channel's Send call
// returns an error. Must mark errored, bump the deliver-errors metric
// path, and not finalize.
func TestCompletionNotifierSendFailMarksErrored(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_6", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "chat123", Content: "Digest",
	}
	repo := newStubRepo()
	repo.claim = rem
	ch := &stubChannel{err: errors.New("telegram timeout")}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_6"}, true, "x")

	if msg, ok := repo.errored["rem_6"]; !ok || msg == "" {
		t.Errorf("expected rem_6 to be marked errored, got %q (ok=%v)", msg, ok)
	}
	if repo.finalized {
		t.Error("must not finalize on a send failure")
	}
}

// TestCompletionNotifierNilSafe: a nil receiver / nil repo / nil task
// must not panic — the executor's fan-out may hold a nil-typed
// CompletionNotifier when reminders aren't wired on this deployment.
func TestCompletionNotifierNilSafe(_ *testing.T) {
	var n *CompletionNotifier
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "x"}, true, "x") // must not panic

	n2 := NewCompletionNotifier(nil, nil, nil, zerolog.Nop(), time.Now)
	n2.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "x"}, true, "x") // repo nil, must not panic

	n3 := NewCompletionNotifier(newStubRepo(), nil, nil, zerolog.Nop(), time.Now)
	n3.NotifyTaskCompleted(context.Background(), nil, true, "x") // task nil, must not panic
}
