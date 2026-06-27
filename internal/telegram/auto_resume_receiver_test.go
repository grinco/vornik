// Tests the auto-resume conversation continuation (triggerFollowup)
// after a watched task completes. With a Receiver wired, the
// synthetic user turn flows through receiver.Receive — the only
// dispatcher-bound path after the legacy inbox was excised.

package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func sizePtr(n int64) *int64 { return &n }

// filterAwareArtifactRepo honours the TaskID filter so the
// descendant-aggregation tests can simulate a parent that owns no
// artifacts but whose child does. The base stubArtifactRepo (in
// forum_notify_test.go) returns every row regardless of filter,
// which doesn't let us distinguish "parent has zero" from "child
// has rows". Other methods panic so a wrong call shows up loudly.
type filterAwareArtifactRepo struct {
	byTask map[string][]*persistence.Artifact
}

func (s *filterAwareArtifactRepo) Create(context.Context, *persistence.Artifact) error {
	panic("unused")
}
func (s *filterAwareArtifactRepo) Get(context.Context, string) (*persistence.Artifact, error) {
	panic("unused")
}
func (s *filterAwareArtifactRepo) GetByHash(context.Context, string) (*persistence.Artifact, error) {
	panic("unused")
}
func (s *filterAwareArtifactRepo) List(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	if f.TaskID == nil {
		return nil, nil
	}
	return s.byTask[*f.TaskID], nil
}
func (s *filterAwareArtifactRepo) Delete(context.Context, string) error              { panic("unused") }
func (s *filterAwareArtifactRepo) DeleteByExecutionID(context.Context, string) error { panic("unused") }
func (s *filterAwareArtifactRepo) UpdateTaskID(context.Context, string, string) error {
	panic("unused")
}

// TestTriggerFollowup_ReceiverPath_FiresReceive — when a Receiver
// is wired and a followup is pending for the completed task,
// triggerFollowup routes the synthetic user turn through
// receiver.Receive on a goroutine.
func TestTriggerFollowup_ReceiverPath_FiresReceive(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "task_xyz", "demo-project")
	bot.triggerFollowup(&persistence.Task{ID: "task_xyz"}, true, "task completed cleanly")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire within 2s")
	}
	if r.calls != 1 {
		t.Errorf("Receive calls = %d, want 1", r.calls)
	}
	// SpeakerID should be empty — auto-resume is server-internal,
	// not user-authenticated. The SessionStore relies on this to
	// skip its allowlist check.
	if r.last.SpeakerID != "" {
		t.Errorf("SpeakerID = %q, want empty (synthetic UserID==0)", r.last.SpeakerID)
	}
	if r.last.SessionID != "100" {
		t.Errorf("SessionID = %q, want 100", r.last.SessionID)
	}
	// Synthetic text renders the task ref via idfmt.Short — "task_xyz"
	// shortens to "T-_xyz" (last 4 chars after the typed prefix).
	if !strings.Contains(r.last.Text, "T-_xyz") {
		t.Errorf("synthetic text missing short task_id: %q", r.last.Text)
	}
}

// TestTriggerFollowup_NotificationStatusDerivedFromSuccess — the
// synthetic turn must derive "completed"/"failed" from the `success`
// bool, NOT interpolate task.Status. Pre-fix, a stale in-memory
// Task left over from the executor's lease-time load (still
// "LEASED") rendered as "[Task X reached terminal status: LEASED.]"
// in the 2026-05-21 watchlist incident. Verifies both paths:
// success → "completed", task.Status="LEASED" must not appear; and
// !success → existing "did NOT complete" copy is preserved.
func TestTriggerFollowup_NotificationStatusDerivedFromSuccess(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "task_xyz", "p1")
	// Deliberately stale Status="LEASED" on the in-memory task —
	// mirrors what the executor used to pass after taskRepo.UpdateStatus
	// flipped the DB row to COMPLETED without mutating the struct.
	bot.triggerFollowup(&persistence.Task{ID: "task_xyz", Status: persistence.TaskStatusLeased}, true, "done")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	if !strings.Contains(r.last.Text, "completed successfully") {
		t.Errorf("expected literal 'completed successfully', got: %q", r.last.Text)
	}
	if strings.Contains(r.last.Text, "LEASED") {
		t.Errorf("stale task.Status leaked into notification: %q", r.last.Text)
	}
	if strings.Contains(r.last.Text, "terminal status") {
		t.Errorf("legacy 'terminal status' phrasing still present: %q", r.last.Text)
	}
}

// TestTriggerFollowup_ReceiverNotWired_NoOp — without a Receiver
// the auto-resume path logs and bails (the legacy inbox path is
// gone; there's no other LLM hop available).
func TestTriggerFollowup_ReceiverNotWired_NoOp(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	// Intentionally NO SetReceiver.
	bot.RegisterFollowup(100, "task_abc", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task_abc"}, true, "ok")
	// Nothing observable to assert beyond "didn't panic" — the
	// followup is silently dropped with a warn log.
}

// TestTriggerFollowup_NoFollowup_NoOp — defensive: the function
// should not fire receiver.Receive when there's no pending
// followup for the task.
func TestTriggerFollowup_NoFollowup_NoOp(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.triggerFollowup(&persistence.Task{ID: "no-followup-here"}, true, "ok")

	if r.waitReceive(t, 200*time.Millisecond) {
		t.Errorf("receiver fired despite no pending followup")
	}
}

// TestTriggerFollowup_ArtifactList_FiltersToOutputClass — the
// synthetic auto-resume turn must only mention OUTPUT-class
// artifacts. Transient files (handover.json, debug logs, internal
// snapshots) leaking into the LLM's context window cause the model
// to forward them to the user via send_artifact — that's the
// noise customers complain about.
func TestTriggerFollowup_ArtifactList_FiltersToOutputClass(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	rep := &stubArtifactRepo{rows: []*persistence.Artifact{
		{Name: "report.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: sizePtr(123)},
		{Name: "handover.json", ArtifactClass: persistence.ArtifactClassIntermediate, SizeBytes: sizePtr(456)},
		{Name: "debug.log", ArtifactClass: persistence.ArtifactClassLog, SizeBytes: sizePtr(789)},
		{Name: "snapshot.bin", ArtifactClass: persistence.ArtifactClassSnapshot},
		{Name: "metadata.json", ArtifactClass: persistence.ArtifactClassMetadata},
	}}
	bot.artifactRepo = rep
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "task_xyz", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task_xyz"}, true, "done")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	if !strings.Contains(r.last.Text, "report.md") {
		t.Errorf("OUTPUT artifact missing from synthetic text: %q", r.last.Text)
	}
	for _, forbidden := range []string{"handover.json", "debug.log", "snapshot.bin", "metadata.json"} {
		if strings.Contains(r.last.Text, forbidden) {
			t.Errorf("non-OUTPUT artifact %q leaked into synthetic text: %s", forbidden, r.last.Text)
		}
	}
}

// TestTriggerFollowup_ArtifactList_NoOutputArtifactsOmitsBlock —
// when nothing OUTPUT-class is produced, the synthetic text must
// skip the entire "Produced N artifact(s)" block so the LLM
// doesn't tell the user about phantom files.
func TestTriggerFollowup_ArtifactList_NoOutputArtifactsOmitsBlock(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	rep := &stubArtifactRepo{rows: []*persistence.Artifact{
		{Name: "handover.json", ArtifactClass: persistence.ArtifactClassIntermediate},
		{Name: "trace.log", ArtifactClass: persistence.ArtifactClassLog},
	}}
	bot.artifactRepo = rep
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "task_q", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task_q"}, true, "done")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	if strings.Contains(r.last.Text, "Produced") {
		t.Errorf("synthetic text mentions artifacts when none are OUTPUT-class: %s", r.last.Text)
	}
}

// TestTriggerFollowup_CoalescesSameTurn — when two tasks scheduled
// by the same dispatcher turn complete close together, only ONE
// synthetic turn must be fed back to the dispatcher, mentioning
// both tasks. Reproduces the 2026-05-21 incident's tail: the
// T-8408 follow-up turn spawned T-0918 and T-8e47; both terminated
// before the dispatcher reply for T-8408 finished, and pre-fix
// they each triggered their own synthetic turn (3 turns total for
// one user question). Post-fix the second + third land as a single
// coalesced turn.
func TestTriggerFollowup_CoalescesSameTurn(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	const chatID = int64(100)
	const turn = "chat_20260521190824_aaaa"
	turnPtr := turn

	bot.RegisterFollowup(chatID, "task_0918", "p1")
	bot.RegisterFollowup(chatID, "task_8e47", "p1")

	// Reproduce the incident shape deterministically: the dispatcher
	// reply for the originating turn is still holding the chat's
	// receiver lock while both tasks terminate. The deliverer spawned
	// by the first outcome blocks on that lock; the second outcome
	// joins the same bucket; releasing the lock drains BOTH as one
	// batch. Pre-2026-06-04 this test fired both outcomes without
	// holding the lock and relied on goroutine-spawn latency to land
	// them in one batch — on slow CI runners the deliverer drained the
	// first outcome alone (two deliveries, which IS correct behaviour
	// for well-separated completions) and the test flaked.
	lock := bot.receiverLock(chatID)
	lock.Lock()
	bot.triggerFollowup(&persistence.Task{ID: "task_0918", ChatTurnID: &turnPtr}, true, "first done")
	bot.triggerFollowup(&persistence.Task{ID: "task_8e47", ChatTurnID: &turnPtr}, true, "second done")
	lock.Unlock()

	// Wait for the coalesced delivery.
	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire within 2s")
	}
	// Give any other potential deliveries a chance to also fire —
	// they MUST NOT. Reads go through the guarded accessors: a
	// hypothetical extra delivery would otherwise race these
	// assertions (the unguarded r.calls / r.last reads were the data
	// race CI flagged on 2026-06-04).
	time.Sleep(200 * time.Millisecond)
	if got := r.callCount(); got != 1 {
		t.Errorf("expected ONE coalesced delivery, got %d Receive calls — coalescing regressed", got)
	}
	text := r.lastMessage().Text
	// idfmt.Short("task_0918") → "T-0918" (typed prefix + trailing 4).
	for _, want := range []string{"T-0918", "T-8e47", "completed successfully"} {
		if !strings.Contains(text, want) {
			t.Errorf("coalesced text missing %q: %q", want, text)
		}
	}
	// Coalesced batch must declare itself so the dispatcher LLM
	// knows to handle several outcomes in one turn.
	if !strings.Contains(text, "tasks from this turn terminated") {
		t.Errorf("coalesced preamble missing: %q", text)
	}
}

// TestTriggerFollowup_DifferentTurnsStillSeparate — tasks with
// different chat_turn_ids must NOT coalesce. Each gets its own
// synthetic turn. This protects multi-tenant chats and unrelated
// turns from accidentally getting merged.
func TestTriggerFollowup_DifferentTurnsStillSeparate(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 4)}
	bot.SetReceiver(r)

	const chatID = int64(100)
	turnA, turnB := "chat_A", "chat_B"

	bot.RegisterFollowup(chatID, "task_a1", "p1")
	bot.RegisterFollowup(chatID, "task_b1", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task_a1", ChatTurnID: &turnA}, true, "A done")
	bot.triggerFollowup(&persistence.Task{ID: "task_b1", ChatTurnID: &turnB}, true, "B done")

	// Drain two deliveries via the done channel rather than polling
	// r.calls — the latter races with Receive's writes when several
	// turn goroutines fire concurrently.
	for i := 0; i < 2; i++ {
		if !r.waitReceive(t, 2*time.Second) {
			t.Fatalf("delivery %d did not arrive within 2s", i+1)
		}
	}
	if got := r.callCount(); got != 2 {
		t.Fatalf("expected two separate deliveries (one per turn), got %d", got)
	}
}

// TestTriggerFollowup_NoChatTurnID_StillImmediate — legacy /
// API-initiated tasks without a chat_turn_id keep the per-task
// delivery shape (no coalescing).
func TestTriggerFollowup_NoChatTurnID_StillImmediate(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "task_legacy", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task_legacy" /* ChatTurnID: nil */}, true, "done")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	if !strings.Contains(r.last.Text, "T-gacy") {
		t.Errorf("single-task delivery missing short id: %q", r.last.Text)
	}
	// Single delivery → no coalesced preamble.
	if strings.Contains(r.last.Text, "tasks from this turn terminated") {
		t.Errorf("non-coalesced delivery wrongly added coalesced preamble: %q", r.last.Text)
	}
}

// TestTriggerFollowup_CoalescesDuringHeldLock — the more subtle
// guarantee: a second completion that arrives WHILE the first
// deliverer is blocked on the receiver lock must still join the
// batch, not spawn a duplicate delivery. Uses blockingReceiver to
// hold the lock long enough for the second triggerFollowup to fire.
func TestTriggerFollowup_CoalescesDuringHeldLock(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	// First fire a non-coalesced turn to occupy the receiver lock
	// for the chat; while that turn is "in flight" inside Receive,
	// we'll enqueue two coalesced outcomes for a different turn.
	br := &blockingReceiver{started: make(chan string, 4), release: make(chan struct{})}
	bot.SetReceiver(br)

	const chatID = int64(100)
	bot.RegisterFollowup(chatID, "task_holder", "p1")
	// Send the holder turn through handleReceiverTurn to take the lock.
	go bot.handleReceiverTurn(br, &Message{ChatID: chatID, UserID: 0, Text: "holder"})
	select {
	case <-br.started:
	case <-time.After(2 * time.Second):
		t.Fatal("holder turn did not start within 2s")
	}

	turn := "chat_coalesce_test"
	turnPtr := turn
	bot.RegisterFollowup(chatID, "task_first", "p1")
	bot.RegisterFollowup(chatID, "task_second", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task_first", ChatTurnID: &turnPtr}, true, "done1")
	// Give the goroutine time to start waiting on the receiver lock.
	time.Sleep(50 * time.Millisecond)
	bot.triggerFollowup(&persistence.Task{ID: "task_second", ChatTurnID: &turnPtr}, true, "done2")

	// Release the holder; the queued deliverer wakes, drains BOTH
	// outcomes, and fires ONE coalesced turn.
	close(br.release)

	// Holder turn finishes (1), then the coalesced one fires (2).
	deadline := time.Now().Add(2 * time.Second)
	for br.maxInFlight() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Read the coalesced text from the channel.
	var coalescedText string
	for {
		select {
		case got := <-br.started:
			if got != "holder" {
				coalescedText = got
			}
		case <-time.After(1 * time.Second):
			if coalescedText == "" {
				t.Fatal("coalesced turn did not arrive within 1s after holder release")
			}
		}
		if coalescedText != "" {
			break
		}
	}
	if !strings.Contains(coalescedText, "T-irst") || !strings.Contains(coalescedText, "T-cond") {
		t.Errorf("coalesced text missing both task short refs: %q", coalescedText)
	}
	if !strings.Contains(coalescedText, "tasks from this turn terminated") {
		t.Errorf("coalesced preamble missing: %q", coalescedText)
	}
	// Drain remaining started channel so the blockingReceiver
	// doesn't leak — there should be NO additional turns.
	drained := 0
	for {
		select {
		case <-br.started:
			drained++
		case <-time.After(200 * time.Millisecond):
			if drained > 0 {
				t.Errorf("unexpected extra turn(s) after coalesce: %d", drained)
			}
			return
		}
	}
}

// TestTriggerFollowup_RouteParent_AggregatesDescendantArtifacts —
// pins the fix for the 2026-05-21 incident (T-6da5 / T-b129 /
// T-3d1e): strict-adaptive route parents have ZERO own-artifacts
// because the route step just delegates. Pre-fix the synthetic
// turn carried "Produced 0 artifact(s)" and the dispatcher LLM
// told the user "no research data was produced". Post-fix the
// followup walks the descendants and lists their OUTPUT artifacts
// with explicit task_id attribution.
func TestTriggerFollowup_RouteParent_AggregatesDescendantArtifacts(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})

	// Parent has no own-artifacts; child has the real deliverable.
	// filterAwareArtifactRepo honours the TaskID filter so the
	// parent's List call returns empty while the child's returns
	// the deliverable. Without that filter the fallback path would
	// never fire (parent would appear to have its child's rows).
	bot.artifactRepo = &filterAwareArtifactRepo{
		byTask: map[string][]*persistence.Artifact{
			"child-1": {
				{ID: "art-1", Name: "deliverable.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: sizePtr(4830)},
				{ID: "art-2", Name: "research.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: sizePtr(11182)},
			},
		},
	}
	bot.taskRepo = &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == "parent-route" {
				return []*persistence.Task{{ID: "child-1", Status: persistence.TaskStatusCompleted}}, nil
			}
			return nil, nil
		},
	}
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "parent-route", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "parent-route"}, true, "route already executed")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	body := r.last.Text
	for _, want := range []string{
		"Produced 2 artifact(s) on descendant tasks:",
		"deliverable.md",
		"research.md",
		// The full task id of the child must ride on each row so
		// the LLM passes the right value to read_artifact.
		"task_id=child-1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n----- body -----\n%s", want, body)
		}
	}
	// Legacy single-task hint must NOT appear when we're routing
	// from descendants — it sets the wrong expectation about which
	// task_id to use.
	if strings.Contains(body, "Use read_artifact(task_id, artifact_name) to fetch any of these") {
		t.Errorf("legacy single-task wording leaked into descendant aggregation: %s", body)
	}
}

// TestTriggerFollowup_OwnArtifactsTakePrecedence — when the
// terminating task DOES have OUTPUT artifacts, the rendering uses
// the legacy single-task shape: no per-row task_id annotation, no
// "on descendant tasks" wording. This is the common case for
// non-route tasks. (GetChildren may still be called by the
// pre-existing findActiveDescendant defense-in-depth probe; we
// only assert the rendering shape.)
func TestTriggerFollowup_OwnArtifactsTakePrecedence(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.artifactRepo = &filterAwareArtifactRepo{
		byTask: map[string][]*persistence.Artifact{
			"task-with-output": {
				{ID: "art-1", Name: "result.md", ArtifactClass: persistence.ArtifactClassOutput, SizeBytes: sizePtr(800)},
			},
		},
	}
	bot.taskRepo = &mocks.MockTaskRepository{} // GetChildren returns nil
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "task-with-output", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "task-with-output"}, true, "done")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	body := r.last.Text
	if !strings.Contains(body, "Produced 1 artifact(s):") {
		t.Errorf("expected own-artifact rendering, got: %s", body)
	}
	if strings.Contains(body, "task_id=") {
		t.Errorf("per-row task_id annotation must NOT render for own artifacts: %s", body)
	}
	if strings.Contains(body, "on descendant tasks") {
		t.Errorf("descendant-aggregation wording leaked into own-artifact case: %s", body)
	}
}

// TestTriggerFollowup_RouteParent_NoDescendantsEither — defensive
// shape: parent has no artifacts AND no children (genuinely empty
// task). The block must be omitted entirely rather than rendering
// "Produced 0 artifact(s)".
func TestTriggerFollowup_RouteParent_NoDescendantsEither(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.artifactRepo = &filterAwareArtifactRepo{}
	bot.taskRepo = &mocks.MockTaskRepository{} // GetChildren returns nil
	r := &handleMessageReceiver{done: make(chan struct{}, 1)}
	bot.SetReceiver(r)

	bot.RegisterFollowup(100, "lonely-task", "p1")
	bot.triggerFollowup(&persistence.Task{ID: "lonely-task"}, true, "done")

	if !r.waitReceive(t, 2*time.Second) {
		t.Fatal("receiver did not fire")
	}
	if strings.Contains(r.last.Text, "Produced") {
		t.Errorf("empty-task synthetic turn must skip the 'Produced N' block: %s", r.last.Text)
	}
}

// TestTriggerFollowup_NilTask_NoOp — defensive guard.
func TestTriggerFollowup_NilTask_NoOp(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.SetReceiver(&handleMessageReceiver{done: make(chan struct{}, 1)})
	bot.triggerFollowup(nil, true, "ok")
	// Should not panic and should not fire anything.
	// No explicit assert: panic-free run is the assertion.
	_ = context.Background()
}
