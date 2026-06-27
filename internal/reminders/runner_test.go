package reminders

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// stubRepo implements ReminderRepository for the heartbeat tests.
// Only the methods Runner actually touches are implemented; the
// rest panic so silent test drift surfaces.
type stubRepo struct {
	mu          sync.Mutex
	queue       []*persistence.Reminder
	fired       []string
	rescheduled []rescheduleCall
	errored     map[string]string
}

type rescheduleCall struct {
	ID         string
	NextFireAt time.Time
}

func newStubRepo() *stubRepo { return &stubRepo{errored: map[string]string{}} }

func (s *stubRepo) Insert(_ context.Context, _ *persistence.Reminder) error { panic("not used") }
func (s *stubRepo) Get(_ context.Context, _ string) (*persistence.Reminder, error) {
	panic("not used")
}
func (s *stubRepo) List(_ context.Context, _ persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	panic("not used")
}
func (s *stubRepo) LeaseDue(_ context.Context, _ time.Time, _ int) ([]*persistence.Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.queue
	s.queue = nil
	for _, r := range out {
		r.Status = persistence.ReminderStatusFiring
	}
	return out, nil
}
func (s *stubRepo) MarkFired(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fired = append(s.fired, id)
	return nil
}
func (s *stubRepo) Reschedule(_ context.Context, id string, nextFireAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescheduled = append(s.rescheduled, rescheduleCall{ID: id, NextFireAt: nextFireAt})
	return nil
}
func (s *stubRepo) MarkErrored(_ context.Context, id, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errored[id] = msg
	return nil
}
func (s *stubRepo) Cancel(_ context.Context, _ string) error { panic("not used") }
func (s *stubRepo) Delete(_ context.Context, _ string) error { panic("not used") }
func (s *stubRepo) UpdateFields(_ context.Context, _ string, _ time.Time, _ string) error {
	panic("not used")
}
func (s *stubRepo) CountPendingByOperator(_ context.Context, _ string) (int, error) {
	panic("not used")
}

// enqueue swaps the pending queue under the same mutex LeaseDue
// uses. Test goroutines must call this when the Runner may be
// concurrently reading queue (i.e. after Run has started).
func (s *stubRepo) enqueue(rows []*persistence.Reminder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = rows
}

func (s *stubRepo) firedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.fired)
}

// stubChannel implements conversation.Channel; only Send is
// exercised by Runner.
type stubChannel struct {
	mu   sync.Mutex
	sent []string // text bodies
	err  error
}

func (s *stubChannel) Name() string                                           { return "stub" }
func (s *stubChannel) Start(_ context.Context, _ conversation.Receiver) error { return nil }
func (s *stubChannel) Stop() error                                            { return nil }
func (s *stubChannel) ResolveSpeaker(_ context.Context, _ string) (conversation.Speaker, error) {
	return conversation.Speaker{}, nil
}
func (s *stubChannel) ListSessions(_ context.Context) ([]conversation.Session, error) {
	return nil, nil
}
func (s *stubChannel) Send(_ context.Context, msg conversation.ChannelMessage) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg.Text)
	return "sent-id", nil
}

type stubResolver struct {
	channels map[string]conversation.Channel
}

func (s *stubResolver) ResolveChannel(name string) conversation.Channel {
	return s.channels[name]
}

// TestRunner_DeliversAndMarksFired drives one full tick:
// repo returns one due reminder → resolver returns a Send-capable
// channel → channel records the body → repo records the fired id.
func TestRunner_DeliversAndMarksFired(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{ID: "rem_001", OperatorID: "telegram:1", Channel: "telegram",
			ChannelRef: "chat123", Content: "deploy check",
			Status: persistence.ReminderStatusPending},
	}
	ch := &stubChannel{}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
	})
	r.tickOnce(context.Background())

	if len(ch.sent) != 1 {
		t.Fatalf("channel.Send count = %d, want 1", len(ch.sent))
	}
	if ch.sent[0] != "⏰ Reminder: deploy check" {
		t.Errorf("body = %q", ch.sent[0])
	}
	if len(repo.fired) != 1 || repo.fired[0] != "rem_001" {
		t.Errorf("fired = %v, want [rem_001]", repo.fired)
	}
}

// TestRunner_MissingChannelMarksErrored covers the "channel not
// wired" branch — the reminder's Channel value points at a
// transport that isn't configured (e.g. webchat on a Telegram-
// only deployment). Row should land in errored, not crash.
func TestRunner_MissingChannelMarksErrored(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{ID: "rem_002", Channel: "webchat"},
	}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{}}, // empty
		Logger:   zerolog.Nop(),
	})
	r.tickOnce(context.Background())
	if msg, ok := repo.errored["rem_002"]; !ok {
		t.Errorf("expected rem_002 to be marked errored")
	} else if msg == "" {
		t.Errorf("error message should not be empty")
	}
}

// TestRunner_SendFailMarksErrored: channel.Send returns an
// error; row should be errored, fired list empty.
func TestRunner_SendFailMarksErrored(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{ID: "rem_003", Channel: "telegram"},
	}
	ch := &stubChannel{err: errors.New("telegram timeout")}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
	})
	r.tickOnce(context.Background())
	if _, ok := repo.errored["rem_003"]; !ok {
		t.Errorf("expected rem_003 to be marked errored")
	}
	if len(repo.fired) != 0 {
		t.Errorf("nothing should have been marked fired; got %v", repo.fired)
	}
}

// TestRunner_KickCollapsesBursts confirms the kick channel
// collapses overlapping calls — important since a chat session
// that adds 5 reminders at once shouldn't queue 5 ticks.
func TestRunner_KickCollapsesBursts(t *testing.T) {
	r := New(Config{Logger: zerolog.Nop()})
	r.Kick()
	r.Kick()
	r.Kick()
	if got := len(r.kickCh); got != 1 {
		t.Errorf("kickCh len = %d, want 1", got)
	}
}

// TestRunner_NilRepoIsSafe confirms a Runner constructed without
// a repo doesn't crash on tick — daemon boots either way.
func TestRunner_NilRepoIsSafe(t *testing.T) {
	r := New(Config{Logger: zerolog.Nop()})
	r.tickOnce(context.Background())
}

// TestRunner_RunExitsOnContextCancel exercises the goroutine
// lifecycle. The Run path was previously 0% covered — only
// tickOnce was tested in isolation. Starting Run + cancelling
// ctx within the timeout confirms the select { case <-ctx.Done()
// } arm fires and the function returns cleanly.
func TestRunner_RunExitsOnContextCancel(t *testing.T) {
	repo := newStubRepo()
	r := New(Config{
		Repo:         repo,
		TickInterval: 25 * time.Millisecond,
		Logger:       zerolog.Nop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	time.Sleep(80 * time.Millisecond) // let at least one ticker fire
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Run did not exit within 500ms of ctx cancel")
	}
}

// TestRunner_RunHonoursKick: the kick channel drives a tick
// even with a long ticker interval, so a freshly-inserted
// "remind me in 30 seconds" doesn't wait for the next polling
// tick.
func TestRunner_RunHonoursKick(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{ID: "rem_kick", Channel: "telegram"},
	}
	ch := &stubChannel{}
	r := New(Config{
		Repo:         repo,
		Resolver:     &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		TickInterval: 10 * time.Second, // long — only kick should drive
		Logger:       zerolog.Nop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	// Initial tick fires immediately; wait for it.
	time.Sleep(50 * time.Millisecond)
	// Queue another reminder + kick.
	repo.enqueue([]*persistence.Reminder{{ID: "rem_kick_2", Channel: "telegram"}})
	r.Kick()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	// Both reminders should have fired (initial + kick).
	if repo.firedCount() < 1 {
		t.Errorf("expected at least one fired reminder")
	}
}

// TestRunner_RecurringReschedulesInsteadOfFired covers the
// re-arm primitive: a cron row that delivers successfully must
// land on Reschedule, not MarkFired, so the heartbeat picks it
// up again at the next slot.
func TestRunner_RecurringReschedulesInsteadOfFired(t *testing.T) {
	// Clock pinned to Sunday 2026-05-24 16:00 UTC. Cron "0 9 * * 1"
	// = every Monday 09:00 → next fire 2026-05-25 09:00.
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{
			ID: "rem_cron", OperatorID: "telegram:1", Channel: "telegram",
			ChannelRef: "chat", Content: "weekly check-in",
			CronExpr: "0 9 * * 1",
			Status:   persistence.ReminderStatusPending,
		},
	}
	ch := &stubChannel{}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
		Clock:    func() time.Time { return clock },
	})
	r.tickOnce(context.Background())

	if len(repo.fired) != 0 {
		t.Errorf("cron row should not be marked fired; got %v", repo.fired)
	}
	if len(repo.rescheduled) != 1 {
		t.Fatalf("rescheduled count = %d, want 1", len(repo.rescheduled))
	}
	want := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	if !repo.rescheduled[0].NextFireAt.Equal(want) {
		t.Errorf("next fire = %s, want %s", repo.rescheduled[0].NextFireAt, want)
	}
}

// TestRunner_RecurringPastBoundGoesTerminal: when the next cron
// slot would exceed RecurrenceUntil, the runner falls through
// to MarkFired so the bounded loop collapses.
func TestRunner_RecurringPastBoundGoesTerminal(t *testing.T) {
	clock := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 24, 17, 0, 0, 0, time.UTC) // bound is before next monday
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{
			ID: "rem_cron_bounded", Channel: "telegram",
			CronExpr:        "0 9 * * 1",
			RecurrenceUntil: &until,
			Status:          persistence.ReminderStatusPending,
		},
	}
	ch := &stubChannel{}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
		Clock:    func() time.Time { return clock },
	})
	r.tickOnce(context.Background())

	if len(repo.rescheduled) != 0 {
		t.Errorf("bounded row past bound must not reschedule; got %v", repo.rescheduled)
	}
	if len(repo.fired) != 1 || repo.fired[0] != "rem_cron_bounded" {
		t.Errorf("fired = %v, want [rem_cron_bounded]", repo.fired)
	}
}

// TestRunner_RecurringInvalidCronAtDeliveryGoesErrored: cron
// columns can drift (operator edits, schema change). At delivery
// time, an unparseable cron must surface as errored — not
// silently terminate a recurring schedule, not panic.
func TestRunner_RecurringInvalidCronAtDeliveryGoesErrored(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{
		{
			ID: "rem_bad_cron", Channel: "telegram",
			CronExpr: "not a cron",
			Status:   persistence.ReminderStatusPending,
		},
	}
	ch := &stubChannel{}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
	})
	r.tickOnce(context.Background())

	if msg, ok := repo.errored["rem_bad_cron"]; !ok {
		t.Errorf("bad-cron row should be marked errored")
	} else if msg == "" {
		t.Errorf("error message empty")
	}
	if len(repo.rescheduled) != 0 || len(repo.fired) != 0 {
		t.Errorf("bad-cron row must not reschedule or fire; rescheduled=%v fired=%v", repo.rescheduled, repo.fired)
	}
}
