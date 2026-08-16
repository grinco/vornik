package steering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Answerer records an operator's answer to an open steering checkpoint and
// re-queues the task. It is the single implementation of a sequence that was
// written out three times — the API/UI handler, the Telegram button callback,
// and (as of the Slack `/vornik answer` path) chat — each of which loaded the
// checkpoint, inserted an `answer` task_message threaded under it, marked it
// resolved, flipped AWAITING_INPUT→QUEUED under a conditional update, and woke
// the scheduler. See https://docs.vornik.io
// §v1.1/4.
//
// It deliberately takes an option **ID**, never an index. The rendered text
// numbers options from 1 for humans; the Telegram callback encodes them from 0;
// letting either convention reach shared code is how an off-by-one becomes a
// wrong-answer-recorded bug. Each adapter maps its own numbering to an id
// first, so there is no index arithmetic here to get wrong.
type Answerer struct {
	msgs  CheckpointStore
	tasks TaskTransitioner
	waker Waker
}

// CheckpointStore is the task_messages surface the Answerer needs.
type CheckpointStore interface {
	GetOpenCheckpoint(ctx context.Context, taskID string) (*persistence.TaskMessage, error)
	Insert(ctx context.Context, msg *persistence.TaskMessage) error
	MarkCheckpointResolved(ctx context.Context, taskID, checkpointID string) error
}

// TaskTransitioner is the guarded status flip. The conditional update IS the
// concurrency control: two operators answering at once means one wins the
// `WHERE status='AWAITING_INPUT'` and the other is told it was already handled.
type TaskTransitioner interface {
	TransitionConditional(ctx context.Context, taskID string, from []persistence.TaskStatus,
		to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error)
}

// Waker nudges the scheduler so the re-queued task runs now rather than at the
// next tick. Optional; nil is fine.
type Waker interface{ Wake() }

// Sentinel errors so callers can render a channel-appropriate message without
// string-matching.
var (
	// ErrNoOpenCheckpoint — nothing is waiting, typically because the
	// checkpoint was answered somewhere else since the prompt was delivered.
	ErrNoOpenCheckpoint = errors.New("steering: no open checkpoint")
	// ErrUnknownOption — the option id is not on this checkpoint.
	ErrUnknownOption = errors.New("steering: unknown option")
	// ErrCheckpointNotChatAnswerable — the checkpoint is a kind that carries
	// its own authorization and side effects, and must be answered where that
	// authorization is enforced (the web UI). See chatAnswerableKind.
	ErrCheckpointNotChatAnswerable = errors.New("steering: checkpoint kind is not answerable from chat")
	// ErrEmptyAnswer prevents a blank chat reply from resolving a checkpoint
	// and waking a task with no operator decision to consume.
	ErrEmptyAnswer = errors.New("steering: answer is empty")
)

// NewAnswerer wires the primitive. waker may be nil.
func NewAnswerer(msgs CheckpointStore, tasks TaskTransitioner, waker Waker) *Answerer {
	return &Answerer{msgs: msgs, tasks: tasks, waker: waker}
}

// AnswerRequest is one operator answer. Exactly one of OptionID (a decision
// checkpoint) or FreeText (action_required / review) is meaningful; OptionID
// wins when both are set.
type AnswerRequest struct {
	TaskID       string
	CheckpointID string
	OptionID     string // decision checkpoints — an option id, NOT an index
	FreeText     string // free-text checkpoints
	AuthorID     string // channel-prefixed operator id, e.g. "slack:U123"
	Source       string // provenance tag recorded on the message, e.g. "slack_slash"
}

// AnswerResult reports what was recorded. AlreadyHandled means the answer was
// written but the task had already left AWAITING_INPUT — someone answered
// first. The caller should tell the operator rather than claim success.
type AnswerResult struct {
	RecordedLabel  string
	AlreadyHandled bool
}

// checkpointMeta covers BOTH metadata shapes in the wild:
//
//   - lead-written checkpoints marshal executor.CheckpointPayload, which puts
//     the kind at the TOP level: {"kind":"decision","options":[…]}
//   - budget and taint-review checkpoints (executor/task_budget_gate.go,
//     executor/taint_park.go) write {"kind":"decision","decision":{"kind":"budget"}}
//     — top-level "decision" AND the real kind nested underneath.
//
// The nested field is therefore the discriminator, and a check that read only
// the top level would wave budget and taint checkpoints straight through.
type checkpointMeta struct {
	Kind     string `json:"kind"`
	Question string `json:"question"`
	Options  []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"options"`
	Decision struct {
		Kind string `json:"kind"`
	} `json:"decision"`
}

// chatAnswerableKind is a POSITIVE allowlist: only the plain steering kinds a
// lead produces may be answered from a chat channel.
//
// Budget checkpoints move money (a per-task spend cap) and taint-review
// `allow` is admin-class ONLY — "no self-serving clear", authorized before any
// write, in internal/api. Neither authorization exists on a chat path, so
// answering them from chat would be a privilege bypass reachable by anyone the
// channel's sender allowlist admits.
//
// Positive, not negative, so a future gated kind is refused by default instead
// of silently inheriting chat access. And it lives HERE rather than in each
// adapter, so a new channel cannot forget it.
func chatAnswerableKind(m checkpointMeta) bool {
	if m.Decision.Kind != "" {
		return false // a nested kind means a specialised, separately-authorized checkpoint
	}
	switch m.Kind {
	case "decision", "action_required", "review", "":
		// "" covers checkpoints written before the kind field existed; they are
		// plain by definition, since every specialised kind sets decision.kind.
		return true
	default:
		return false
	}
}

func answerLabel(req AnswerRequest, meta checkpointMeta) (string, error) {
	if req.OptionID == "" {
		label := strings.TrimSpace(req.FreeText)
		if label == "" {
			return "", ErrEmptyAnswer
		}
		return label, nil
	}
	for _, option := range meta.Options {
		if option.ID != req.OptionID {
			continue
		}
		if option.Label != "" {
			return option.Label, nil
		}
		return option.ID, nil // same fallback buildSteeringButtons uses
	}
	return "", ErrUnknownOption
}

// Answer records the operator's reply to the open checkpoint and re-queues the
// task. Errors are sentinels (see above); a nil error with
// AlreadyHandled=true means the answer landed but somebody beat us to the
// transition.
func (a *Answerer) Answer(ctx context.Context, req AnswerRequest) (AnswerResult, error) {
	if a == nil || a.msgs == nil || a.tasks == nil {
		return AnswerResult{}, errors.New("steering: answerer not wired")
	}
	cp, err := a.msgs.GetOpenCheckpoint(ctx, req.TaskID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("load open checkpoint: %w", err)
	}
	if cp == nil {
		return AnswerResult{}, ErrNoOpenCheckpoint
	}
	if req.CheckpointID != "" && req.CheckpointID != cp.ID {
		// The caller answered a prompt that has since been replaced. Never map
		// its positional/button choice onto the new checkpoint.
		return AnswerResult{}, ErrNoOpenCheckpoint
	}

	var meta checkpointMeta
	if len(cp.Metadata) > 0 {
		// A checkpoint whose metadata will not parse is treated as unparseable
		// rather than plain — fail closed, same direction as the allowlist.
		if err := json.Unmarshal(cp.Metadata, &meta); err != nil {
			return AnswerResult{}, ErrCheckpointNotChatAnswerable
		}
	}
	if !chatAnswerableKind(meta) {
		return AnswerResult{}, ErrCheckpointNotChatAnswerable
	}

	// Resolve the recorded content BEFORE writing anything, so a bad option id
	// leaves no trace.
	label, err := answerLabel(req, meta)
	if err != nil {
		return AnswerResult{}, err
	}

	checkpointID := req.CheckpointID
	if checkpointID == "" {
		checkpointID = cp.ID
	}
	authorID := req.AuthorID
	metaBytes, _ := json.Marshal(map[string]any{
		"source": req.Source,
		"choice": req.OptionID,
	})
	msg := &persistence.TaskMessage{
		TaskID:      req.TaskID,
		MessageKind: persistence.TaskMessageKindAnswer,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		AuthorID:    &authorID,
		ParentID:    &checkpointID,
		Content:     label,
		Metadata:    metaBytes,
		CreatedAt:   time.Now().UTC(),
	}
	if err := a.msgs.Insert(ctx, msg); err != nil {
		return AnswerResult{}, fmt.Errorf("record answer: %w", err)
	}
	// Best-effort, matching the pre-extraction call sites: the answer row is
	// the durable record; a failed resolve leaves the checkpoint open but the
	// transition below is what actually unblocks the task.
	_ = a.msgs.MarkCheckpointResolved(ctx, req.TaskID, checkpointID)

	ok, err := a.tasks.TransitionConditional(ctx, req.TaskID,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingInput},
		persistence.TaskStatusQueued,
		persistence.TransitionOpts{ClearLease: true})
	if err != nil {
		return AnswerResult{RecordedLabel: label}, fmt.Errorf("requeue: %w", err)
	}
	if !ok {
		return AnswerResult{RecordedLabel: label, AlreadyHandled: true}, nil
	}
	if a.waker != nil {
		a.waker.Wake()
	}
	return AnswerResult{RecordedLabel: label}, nil
}
