package steering

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeCheckpointStore records what the Answerer wrote so a test can assert on
// the sequence rather than on a mock's call log.
type fakeCheckpointStore struct {
	cp        *persistence.TaskMessage
	getErr    error
	inserted  []*persistence.TaskMessage
	insertErr error
	resolved  []string
}

func (f *fakeCheckpointStore) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	// A miss is (nil, persistence.ErrNotFound) at both backends — see
	// internal/persistence/misscontract. A permissive double here would
	// certify the caller's absent-row path without ever exercising it.
	if f.cp == nil {
		return nil, persistence.ErrNotFound
	}
	return f.cp, nil
}

func (f *fakeCheckpointStore) Insert(_ context.Context, m *persistence.TaskMessage) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, m)
	return nil
}

func (f *fakeCheckpointStore) MarkCheckpointResolved(_ context.Context, _, checkpointID string) error {
	f.resolved = append(f.resolved, checkpointID)
	return nil
}

type fakeTransitioner struct {
	ok     bool
	err    error
	called int
}

func (f *fakeTransitioner) TransitionConditional(_ context.Context, _ string, _ []persistence.TaskStatus,
	_ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
	f.called++
	return f.ok, f.err
}

type fakeWaker struct{ woke int }

func (f *fakeWaker) Wake() { f.woke++ }

// plainDecisionCP is a lead-written decision checkpoint: kind at the TOP level,
// no nested decision.kind. This is the shape SerializeCheckpointMetadata emits.
func plainDecisionCP(id string) *persistence.TaskMessage {
	meta, _ := json.Marshal(map[string]any{
		"kind":     "decision",
		"question": "Which blind type?",
		"options": []map[string]string{
			{"id": "roller", "label": "Roller"},
			{"id": "roman", "label": "Roman"},
			{"id": "venetian", "label": "Venetian"},
		},
	})
	return &persistence.TaskMessage{ID: id, Content: "Which blind type?", Metadata: meta}
}

func newAnswerer(cp *persistence.TaskMessage, transitionOK bool) (*Answerer, *fakeCheckpointStore, *fakeTransitioner, *fakeWaker) {
	store := &fakeCheckpointStore{cp: cp}
	tr := &fakeTransitioner{ok: transitionOK}
	w := &fakeWaker{}
	return NewAnswerer(store, tr, w), store, tr, w
}

// The happy path IS the five-step sequence the three call sites each
// reimplemented: read checkpoint, record the answer threaded under it, resolve
// it, flip AWAITING_INPUT→QUEUED, wake the scheduler.
func TestAnswerer_RecordsResolvesAndRequeues(t *testing.T) {
	a, store, tr, w := newAnswerer(plainDecisionCP("cp1"), true)

	res, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "task_1", CheckpointID: "cp1", OptionID: "roman",
		AuthorID: "slack:U_alice", Source: "slack_slash",
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res.RecordedLabel != "Roman" {
		t.Errorf("RecordedLabel = %q, want %q (label, not id)", res.RecordedLabel, "Roman")
	}
	if res.AlreadyHandled {
		t.Error("AlreadyHandled = true on a winning transition")
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted %d messages, want 1", len(store.inserted))
	}
	got := store.inserted[0]
	if got.MessageKind != persistence.TaskMessageKindAnswer {
		t.Errorf("MessageKind = %q, want answer", got.MessageKind)
	}
	if got.ParentID == nil || *got.ParentID != "cp1" {
		t.Error("the answer must be threaded under the checkpoint it resolves")
	}
	if len(store.resolved) != 1 || store.resolved[0] != "cp1" {
		t.Errorf("resolved = %v, want [cp1]", store.resolved)
	}
	if tr.called != 1 || w.woke != 1 {
		t.Errorf("transition=%d wake=%d, want 1/1", tr.called, w.woke)
	}
}

// A losing conditional transition means somebody else answered first (UI,
// another channel). Report it rather than double-recording.
func TestAnswerer_LostTransitionReportsAlreadyHandled(t *testing.T) {
	a, _, _, w := newAnswerer(plainDecisionCP("cp1"), false)

	res, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "task_1", CheckpointID: "cp1", OptionID: "roller", AuthorID: "slack:U_alice",
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !res.AlreadyHandled {
		t.Error("a lost AWAITING_INPUT→QUEUED race must report AlreadyHandled")
	}
	if w.woke != 0 {
		t.Error("must not wake the scheduler when the transition was lost")
	}
}

// THE SECURITY TEST. Budget and taint-review checkpoints carry their own
// authorization in the API handler — taint `allow` is admin-class only. They
// are written with kind:"decision" at the TOP level and the real kind NESTED
// under decision.kind, so a check that only read the top level would admit
// them and turn any chat answer path into a bypass.
func TestAnswerer_RefusesCheckpointKindsWithTheirOwnAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nestedKind string
	}{
		{"budget raise/abandon", "budget"},
		{"taint review allow/cancel", "untrusted_review"},
		{"an unknown future kind fails CLOSED", "some_future_gated_kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, _ := json.Marshal(map[string]any{
				"kind":     "decision", // top level looks perfectly ordinary
				"decision": map[string]any{"kind": tc.nestedKind},
			})
			cp := &persistence.TaskMessage{ID: "cp1", Metadata: meta}
			a, store, tr, _ := newAnswerer(cp, true)

			_, err := a.Answer(context.Background(), AnswerRequest{
				TaskID: "task_1", CheckpointID: "cp1", OptionID: "increase", AuthorID: "slack:U_alice",
			})
			if !errors.Is(err, ErrCheckpointNotChatAnswerable) {
				t.Fatalf("err = %v, want ErrCheckpointNotChatAnswerable", err)
			}
			if len(store.inserted) != 0 || len(store.resolved) != 0 || tr.called != 0 {
				t.Error("a refused checkpoint must not be written to, resolved, or transitioned")
			}
		})
	}
}

// The plain kinds stay answerable — the allowlist must not be so tight that it
// breaks the feature it is guarding.
func TestAnswerer_AllowsThePlainSteeringKinds(t *testing.T) {
	for _, kind := range []string{"decision", "action_required", "review", ""} {
		t.Run("kind="+kind, func(t *testing.T) {
			meta, _ := json.Marshal(map[string]any{"kind": kind})
			cp := &persistence.TaskMessage{ID: "cp1", Content: "do the thing", Metadata: meta}
			a, _, _, _ := newAnswerer(cp, true)
			if _, err := a.Answer(context.Background(), AnswerRequest{
				TaskID: "t", CheckpointID: "cp1", FreeText: "done", AuthorID: "slack:U_alice",
			}); err != nil {
				t.Errorf("kind %q must be answerable from chat: %v", kind, err)
			}
		})
	}
}

// The primitive takes an option ID, never an index — that is what keeps the
// 1-based text and 0-based Telegram callback from ever meeting in shared code.
// An id that isn't on the checkpoint is a caller bug and must not be recorded.
func TestAnswerer_UnknownOptionIDIsRefused(t *testing.T) {
	a, store, tr, _ := newAnswerer(plainDecisionCP("cp1"), true)

	_, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "t", CheckpointID: "cp1", OptionID: "vertical", AuthorID: "slack:U_alice",
	})
	if !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("err = %v, want ErrUnknownOption", err)
	}
	if len(store.inserted) != 0 || tr.called != 0 {
		t.Error("an unknown option must not be recorded")
	}
}

// An option with no label falls back to its id — the same precedence
// buildSteeringButtons applies, so the button and the text can never disagree.
func TestAnswerer_LabelFallsBackToOptionID(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{
		"kind":    "decision",
		"options": []map[string]string{{"id": "roller"}},
	})
	a, _, _, _ := newAnswerer(&persistence.TaskMessage{ID: "cp1", Metadata: meta}, true)

	res, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "t", CheckpointID: "cp1", OptionID: "roller", AuthorID: "slack:U_alice",
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res.RecordedLabel != "roller" {
		t.Errorf("RecordedLabel = %q, want the id as fallback", res.RecordedLabel)
	}
}

// No open checkpoint: the task was answered elsewhere between notification and
// reply. Distinct from a refusal so the caller can say "already handled".
func TestAnswerer_NoOpenCheckpoint(t *testing.T) {
	a, _, tr, _ := newAnswerer(nil, true)
	_, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "t", CheckpointID: "cp1", FreeText: "hi", AuthorID: "slack:U_alice",
	})
	if !errors.Is(err, ErrNoOpenCheckpoint) {
		t.Fatalf("err = %v, want ErrNoOpenCheckpoint", err)
	}
	if tr.called != 0 {
		t.Error("must not transition when there is no checkpoint")
	}
}

func TestAnswerer_RefusesAStaleCheckpointReference(t *testing.T) {
	a, store, tr, _ := newAnswerer(plainDecisionCP("cp-new"), true)
	_, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "t", CheckpointID: "cp-old", OptionID: "roller", AuthorID: "slack:U_alice",
	})
	if !errors.Is(err, ErrNoOpenCheckpoint) {
		t.Fatalf("err = %v, want stale reference refusal", err)
	}
	if len(store.inserted) != 0 || tr.called != 0 {
		t.Fatal("a stale button wrote an answer against the replacement checkpoint")
	}
}

func TestAnswerer_RefusesBlankFreeText(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"kind": "action_required"})
	a, store, tr, _ := newAnswerer(&persistence.TaskMessage{ID: "cp1", Metadata: meta}, true)
	_, err := a.Answer(context.Background(), AnswerRequest{
		TaskID: "t", CheckpointID: "cp1", FreeText: "  ", AuthorID: "slack:U_alice",
	})
	if !errors.Is(err, ErrEmptyAnswer) {
		t.Fatalf("err = %v, want ErrEmptyAnswer", err)
	}
	if len(store.inserted) != 0 || tr.called != 0 {
		t.Fatal("a blank answer must not resolve and requeue the task")
	}
}
