package narrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// --- fakes ------------------------------------------------------------

type fakeTaskGetterCP struct {
	byID map[string]*persistence.Task
}

func (f fakeTaskGetterCP) Get(_ context.Context, id string) (*persistence.Task, error) {
	return f.byID[id], nil
}

type fakeChatAuditCP struct {
	row *persistence.ChatAuditEntry
	err error
}

func (f fakeChatAuditCP) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return f.row, f.err
}

type fakeChannelCP struct {
	name    string
	sent    []conversation.ChannelMessage
	sendErr error
}

func (f *fakeChannelCP) Name() string                                       { return f.name }
func (f *fakeChannelCP) Start(context.Context, conversation.Receiver) error { return nil }
func (f *fakeChannelCP) Stop() error                                        { return nil }
func (f *fakeChannelCP) Send(_ context.Context, m conversation.ChannelMessage) (string, error) {
	f.sent = append(f.sent, m)
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "sent-1", nil
}
func (f *fakeChannelCP) ListSessions(context.Context) ([]conversation.Session, error) {
	return nil, nil
}
func (f *fakeChannelCP) ResolveSpeaker(context.Context, string) (conversation.Speaker, error) {
	return conversation.Speaker{}, nil
}

type fakeResolverCP struct {
	byName map[string]conversation.Channel
}

func (f fakeResolverCP) ResolveChannel(name string) conversation.Channel {
	if f.byName == nil {
		return nil
	}
	return f.byName[name]
}

type fakeArtifactListerCP struct {
	byExec map[string][]*persistence.Artifact
	err    error
}

func (f fakeArtifactListerCP) List(_ context.Context, _, executionID string) ([]*persistence.Artifact, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byExec[executionID], nil
}

func strp(s string) *string { return &s }

// newChatPushNarrator builds a minimal Narrator (no Sub/Run loop — tests
// call emitLine directly, matching persist_publish_test.go's convention)
// wired for chat push against the given task/audit/resolver fakes. Returns
// the fakeStore too since several tests assert the persisted row's text
// matches the pushed text.
func newChatPushNarrator(t *testing.T, task *persistence.Task, row *persistence.ChatAuditEntry, ch *fakeChannelCP, chatPush bool) (*Narrator, *fakeStore) {
	t.Helper()
	store := newFakeStore(nil)
	res := fakeResolverCP{byName: map[string]conversation.Channel{}}
	if ch != nil {
		res.byName[ch.name] = ch
	}
	n := &Narrator{
		Sub:        newFakeSub(),
		Pub:        newFakePub(nil),
		Store:      store,
		Executions: newFakeExecutions(),
		Tasks:      fakeTaskGetterCP{byID: map[string]*persistence.Task{task.ID: task}},
		Audit:      fakeChatAuditCP{row: row},
		Resolver:   res,
		ProjectSettings: func(string) ProjectNarratorSettings {
			return ProjectNarratorSettings{ChatPush: chatPush}
		},
	}
	n.ensureInit()
	return n, store
}

// --- opt-in gating ----------------------------------------------------

// TestChatPush_OptInOff_NoSend pins design §5.7 point 1: chat_push is
// opt-in per project, default off. A milestone line for a chat-originated
// task must NOT push when the project hasn't opted in.
func TestChatPush_OptInOff_NoSend(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, row, ch, false) // opt-in OFF

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	if len(ch.sent) != 0 {
		t.Fatalf("chat_push=false must not push, got %d sends", len(ch.sent))
	}
}

// TestChatPush_OptInOn_MilestoneKind_OneSend pins the positive case: opted
// in + a milestone kind (step_completed, the default) pushes exactly one
// message carrying the already-produced line text.
func TestChatPush_OptInOn_MilestoneKind_OneSend(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, store := newChatPushNarrator(t, task, row, ch, true) // opt-in ON

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	if len(ch.sent) != 1 {
		t.Fatalf("want exactly 1 push, got %d", len(ch.sent))
	}
	rows := store.all()
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 persisted row, got %d", len(rows))
	}
	if ch.sent[0].Text != rows[0].Text {
		t.Errorf("pushed text = %q, want the already-produced line %q", ch.sent[0].Text, rows[0].Text)
	}
	if ch.sent[0].SessionID != "555" {
		t.Errorf("SessionID = %q, want 555", ch.sent[0].SessionID)
	}
}

// TestChatPush_NonMilestoneKind_NotPushed pins the coarser cadence (design
// §5.7 point 2): step_started is not a milestone kind by default, so an
// opted-in project still doesn't get a chat push for it (only the UI story
// panel shows every line).
func TestChatPush_NonMilestoneKind_NotPushed(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepStarted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)
	n.emitLine(context.Background(), "exec1", st, triggerToolHeartbeat,
		templateInput{Role: "worker", Tool: "read_file", StepIdx: 1}, "s1", "read_file", persistence.ExecutionNarrationKindTool)

	if len(ch.sent) != 0 {
		t.Fatalf("non-milestone kinds must not push, got %d sends", len(ch.sent))
	}
}

// TestChatPush_NonChatOriginated_Skip pins design §5.7 point 2 / §7's
// failure-mode table: a task with no ChatTurnID and no chat-originated
// ancestor never pushes, even opted in — and must never error/panic.
func TestChatPush_NonChatOriginated_Skip(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"} // no ChatTurnID, no parent
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, &persistence.ChatAuditEntry{}, ch, true)

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerCompletion,
		templateInput{Role: "worker", Success: true}, "", "", persistence.ExecutionNarrationKindCompletion)

	if len(ch.sent) != 0 {
		t.Fatalf("non-chat-originated task must not push, got %d sends", len(ch.sent))
	}
}

// TestChatPush_MissingCollaborators_NoPanic covers the nil-Tasks/Audit/
// Resolver gate — an un-wired chat push must be a pure no-op.
func TestChatPush_MissingCollaborators_NoPanic(t *testing.T) {
	store := newFakeStore(nil)
	n := &Narrator{
		Sub:        newFakeSub(),
		Pub:        newFakePub(nil),
		Store:      store,
		Executions: newFakeExecutions(),
		ProjectSettings: func(string) ProjectNarratorSettings {
			return ProjectNarratorSettings{ChatPush: true}
		},
	}
	n.ensureInit()
	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerCompletion,
		templateInput{Success: true}, "", "", persistence.ExecutionNarrationKindCompletion)
	if len(store.all()) != 1 {
		t.Fatalf("line must still be produced even though chat push is un-wired")
	}
}

// TestChatPush_CustomMilestoneKinds_OverridesDefault pins
// config.NarratorConfig.ChatMilestoneKinds: when set, it REPLACES the
// default [step_completed, completion] set rather than extending it.
func TestChatPush_CustomMilestoneKinds_OverridesDefault(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)
	n.ChatMilestoneKinds = []string{"step_started"}

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	// step_started is now the (only) configured milestone kind.
	n.emitLine(context.Background(), "exec1", st, triggerStepStarted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)
	if len(ch.sent) != 1 {
		t.Fatalf("configured milestone kind step_started must push, got %d sends", len(ch.sent))
	}
	// step_completed is no longer in the configured set, so it must NOT push.
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s2", "", persistence.ExecutionNarrationKindStep)
	if len(ch.sent) != 1 {
		t.Fatalf("step_completed dropped from the configured set must not push, got %d sends", len(ch.sent))
	}
}

// TestCompletionPush_ArtifactListError_FallsBackToNarrationText pins
// outputArtifacts' error handling: a List failure must not fail the push,
// it just falls back to the plain narration text (no attachments).
func TestCompletionPush_ArtifactListError_FallsBackToNarrationText(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, store := newChatPushNarrator(t, task, row, ch, true)
	n.Artifacts = fakeArtifactListerCP{err: context.DeadlineExceeded}

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerCompletion,
		templateInput{Success: true}, "", "", persistence.ExecutionNarrationKindCompletion)

	rows := store.all()
	if len(ch.sent) != 1 {
		t.Fatalf("want exactly 1 push despite the artifact-list error, got %d", len(ch.sent))
	}
	if ch.sent[0].Text != rows[0].Text {
		t.Errorf("text = %q, want plain narration fallback %q", ch.sent[0].Text, rows[0].Text)
	}
	if len(ch.sent[0].Attachments) != 0 {
		t.Errorf("must carry no attachments on a list error, got %d", len(ch.sent[0].Attachments))
	}
}

// --- no-narration opt-out (§9 Q3) --------------------------------------

// TestNoNarrationOptOut_NoLinesProduced pins design §9 Q3: a project that
// opted out of narration entirely produces ZERO lines — not persisted, not
// published, chat push moot.
func TestNoNarrationOptOut_NoLinesProduced(t *testing.T) {
	store := newFakeStore(nil)
	pub := newFakePub(nil)
	n := &Narrator{
		Sub:        newFakeSub(),
		Pub:        pub,
		Store:      store,
		Executions: newFakeExecutions(),
		ProjectSettings: func(string) ProjectNarratorSettings {
			return ProjectNarratorSettings{NoNarration: true}
		},
	}
	n.ensureInit()
	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	if len(store.all()) != 0 {
		t.Fatalf("no_narration=true must produce 0 persisted rows, got %d", len(store.all()))
	}
	if len(pub.all()) != 0 {
		t.Fatalf("no_narration=true must publish 0 lines, got %d", len(pub.all()))
	}
}

// --- deliverable-led completion push (§5.7 point 4 / §5.8) -------------

func mimePtr(s string) *string { return &s }

// TestCompletionPush_LeadsWithSingleArtifact pins the single-deliverable
// phrasing ("Here's your <name>:") plus one Attachment carrying
// ArtifactID.
func TestCompletionPush_LeadsWithSingleArtifact(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)
	n.Artifacts = fakeArtifactListerCP{byExec: map[string][]*persistence.Artifact{
		"exec1": {
			{ID: "art1", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput, MimeType: mimePtr("application/pdf")},
			{ID: "art2", Name: "scratch.tmp", ArtifactClass: persistence.ArtifactClassIntermediate},
		},
	}}

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerCompletion,
		templateInput{Success: true}, "", "", persistence.ExecutionNarrationKindCompletion)

	if len(ch.sent) != 1 {
		t.Fatalf("want exactly 1 completion push, got %d", len(ch.sent))
	}
	msg := ch.sent[0]
	if !strings.HasPrefix(msg.Text, "Here's your report.pdf:") {
		t.Errorf("completion text = %q, want it to lead with the single deliverable", msg.Text)
	}
	if !strings.Contains(msg.Text, "All done") {
		t.Errorf("completion text must still include the narration line: %q", msg.Text)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("want exactly 1 attachment (the OUTPUT artifact only), got %d", len(msg.Attachments))
	}
	if msg.Attachments[0].ArtifactID != "art1" || msg.Attachments[0].Name != "report.pdf" {
		t.Errorf("attachment = %+v, want art1/report.pdf", msg.Attachments[0])
	}
	if msg.Attachments[0].MimeType != "application/pdf" {
		t.Errorf("attachment MimeType = %q, want application/pdf", msg.Attachments[0].MimeType)
	}
}

// TestCompletionPush_LeadsWithMultipleArtifacts pins the plural phrasing
// and the full name-list fallback for text-only channels.
func TestCompletionPush_LeadsWithMultipleArtifacts(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)
	n.Artifacts = fakeArtifactListerCP{byExec: map[string][]*persistence.Artifact{
		"exec1": {
			{ID: "art1", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput},
			{ID: "art2", Name: "summary.md", ArtifactClass: persistence.ArtifactClassOutput},
		},
	}}

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerCompletion,
		templateInput{Success: true}, "", "", persistence.ExecutionNarrationKindCompletion)

	msg := ch.sent[0]
	if !strings.Contains(msg.Text, "2 files") {
		t.Errorf("completion text = %q, want the plural file-count phrasing", msg.Text)
	}
	if !strings.Contains(msg.Text, "report.pdf") || !strings.Contains(msg.Text, "summary.md") {
		t.Errorf("completion text must list every deliverable name: %q", msg.Text)
	}
	if len(msg.Attachments) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(msg.Attachments))
	}
}

// TestCompletionPush_NoArtifacts_FallsBackToNarrationText pins the no-
// deliverable fallback (§5.8: "If none, the story ends with the completion
// narration text as the deliverable").
func TestCompletionPush_NoArtifacts_FallsBackToNarrationText(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, store := newChatPushNarrator(t, task, row, ch, true)
	n.Artifacts = fakeArtifactListerCP{} // no artifacts at all

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerCompletion,
		templateInput{Success: true}, "", "", persistence.ExecutionNarrationKindCompletion)

	rows := store.all()
	msg := ch.sent[0]
	if msg.Text != rows[0].Text {
		t.Errorf("no-artifact completion text = %q, want the plain narration line %q", msg.Text, rows[0].Text)
	}
	if len(msg.Attachments) != 0 {
		t.Errorf("no-artifact completion must carry no attachments, got %d", len(msg.Attachments))
	}
}

// --- email recovery -----------------------------------------------------

// TestChatPush_EmailRecoversToAndSubject mirrors steering/notifier.go's
// email precedent: the channel-specific to/subject are recovered from the
// durable audit row's UserID, not an in-memory session.
func TestChatPush_EmailRecoversToAndSubject(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_2")}
	row := &persistence.ChatAuditEntry{ID: "chat_2", ChatID: "email:<thread@x.com>", UserID: "email:ops@x.com", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "email"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	if len(ch.sent) != 1 {
		t.Fatalf("want exactly 1 push, got %d", len(ch.sent))
	}
	msg := ch.sent[0]
	if msg.SessionID != "<thread@x.com>" {
		t.Errorf("SessionID = %q, want <thread@x.com>", msg.SessionID)
	}
	if msg.ChannelSpecific["to"] != "ops@x.com" {
		t.Errorf("email To = %q, want ops@x.com", msg.ChannelSpecific["to"])
	}
	if msg.ChannelSpecific["subject"] == "" {
		t.Errorf("email subject must be set")
	}
}

// --- no extra LLM call --------------------------------------------------

// TestChatPush_NoExtraLLMCall pins design §5.7's closing constraint: the
// chat push reuses the already-composed line; it never triggers a second
// LLM call beyond the one composeLine already made for the line itself.
func TestChatPush_NoExtraLLMCall(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram"}
	n, _ := newChatPushNarrator(t, task, row, ch, true)
	provider := &fakeProvider{replies: []string{"Finished the step."}}
	n.Client = provider
	n.Model = "cheap-model"

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)

	if len(ch.sent) != 1 {
		t.Fatalf("want exactly 1 push, got %d", len(ch.sent))
	}
	if provider.calls != 1 {
		t.Fatalf("want exactly 1 LLM call (line composition only, none from the chat push), got %d", provider.calls)
	}
	if ch.sent[0].Text != "Finished the step." {
		t.Errorf("pushed text = %q, want the LLM-composed line reused verbatim", ch.sent[0].Text)
	}
}

// TestChatPush_SendError_NoPanic pins the best-effort contract: a channel
// Send failure must log and return, never propagate/panic.
func TestChatPush_SendError_NoPanic(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannelCP{name: "telegram", sendErr: context.DeadlineExceeded}
	n, _ := newChatPushNarrator(t, task, row, ch, true)

	st := newExecutionState("exec1", "p1", "t1", time.Now())
	n.states["exec1"] = st
	n.emitLine(context.Background(), "exec1", st, triggerStepCompleted,
		templateInput{Role: "worker", StepIdx: 1}, "s1", "", persistence.ExecutionNarrationKindStep)
	// Must not panic; nothing further to assert.
}
