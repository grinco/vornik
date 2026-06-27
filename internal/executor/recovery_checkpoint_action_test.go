package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// These tests pin the APPLY half of LLD §9 "Structured
// recovery-checkpoint actions": resolving the operator's chosen option
// from the conversation, and applying its action via the existing seams
// (delegateSelectedWorkflow / ApplyFallbackModelOverride) while
// preserving every guardrail (allow-list, operator-gated, fail-safe).

// checkpointMsgID is the fixed id the test checkpoint message uses; the
// paired answerMsg("cp1", …) threads to it.
const checkpointMsgID = "cp1"

// checkpointMsg builds a persisted checkpoint task_message (id=cp1)
// carrying the supplied decision options (with their structured actions
// serialized).
func checkpointMsg(t *testing.T, opts []CheckpointOption) *persistence.TaskMessage {
	t.Helper()
	meta, err := SerializeCheckpointMetadata(&CheckpointPayload{
		Kind:     CheckpointKindDecision,
		Question: "What now?",
		Options:  opts,
	})
	require.NoError(t, err)
	return &persistence.TaskMessage{
		ID:          checkpointMsgID,
		MessageKind: persistence.TaskMessageKindCheckpoint,
		AuthorKind:  "lead",
		Metadata:    meta,
	}
}

// answerMsg builds an operator answer threaded to a checkpoint, with the
// chosen option id in metadata ({"choice":...}) as the API writes it.
func answerMsg(parentID, choice string) *persistence.TaskMessage {
	meta, _ := json.Marshal(map[string]any{"choice": choice})
	return &persistence.TaskMessage{
		ID:          "answer_" + choice,
		ParentID:    &parentID,
		MessageKind: persistence.TaskMessageKindAnswer,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		Metadata:    meta,
	}
}

func TestResolveOperatorCheckpointAction(t *testing.T) {
	opts := []CheckpointOption{
		{ID: "reroute", Label: "Re-run via planner", Action: &CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "research-planner"}},
		{ID: "plain", Label: "Just retry"},
	}

	t.Run("resolves the chosen option's action via threaded parent", func(t *testing.T) {
		msgs := []*persistence.TaskMessage{
			checkpointMsg(t, opts),
			answerMsg("cp1", "reroute"),
		}
		got := resolveOperatorCheckpointAction(msgs)
		require.NotNil(t, got)
		assert.Equal(t, CheckpointActionRerouteWorkflow, got.Type)
		assert.Equal(t, "research-planner", got.Workflow)
	})

	t.Run("chosen option without an action yields nil (prose path)", func(t *testing.T) {
		msgs := []*persistence.TaskMessage{
			checkpointMsg(t, opts),
			answerMsg("cp1", "plain"),
		}
		assert.Nil(t, resolveOperatorCheckpointAction(msgs))
	})

	t.Run("falls back to most-recent checkpoint when parent id missing", func(t *testing.T) {
		ans := answerMsg("", "reroute") // no ParentID
		msgs := []*persistence.TaskMessage{checkpointMsg(t, opts), ans}
		got := resolveOperatorCheckpointAction(msgs)
		require.NotNil(t, got)
		assert.Equal(t, CheckpointActionRerouteWorkflow, got.Type)
	})

	t.Run("latest answer wins when operator re-answers", func(t *testing.T) {
		msgs := []*persistence.TaskMessage{
			checkpointMsg(t, opts),
			answerMsg("cp1", "reroute"),
			answerMsg("cp1", "plain"), // newer answer picks the action-less option
		}
		assert.Nil(t, resolveOperatorCheckpointAction(msgs))
	})

	t.Run("no answer message yields nil", func(t *testing.T) {
		msgs := []*persistence.TaskMessage{checkpointMsg(t, opts)}
		assert.Nil(t, resolveOperatorCheckpointAction(msgs))
	})

	t.Run("unknown choice id yields nil", func(t *testing.T) {
		msgs := []*persistence.TaskMessage{checkpointMsg(t, opts), answerMsg("cp1", "does-not-exist")}
		assert.Nil(t, resolveOperatorCheckpointAction(msgs))
	})

	t.Run("empty conversation yields nil", func(t *testing.T) {
		assert.Nil(t, resolveOperatorCheckpointAction(nil))
	})

	t.Run("answer with malformed metadata yields nil", func(t *testing.T) {
		bad := &persistence.TaskMessage{
			ID:          "a-bad",
			MessageKind: persistence.TaskMessageKindAnswer,
			AuthorKind:  persistence.TaskMessageAuthorOperator,
			Metadata:    json.RawMessage(`{not json`),
		}
		assert.Nil(t, resolveOperatorCheckpointAction([]*persistence.TaskMessage{checkpointMsg(t, opts), bad}))
	})

	t.Run("answer with no choice key yields nil", func(t *testing.T) {
		noChoice := &persistence.TaskMessage{
			ID:          "a-nc",
			MessageKind: persistence.TaskMessageKindAnswer,
			AuthorKind:  persistence.TaskMessageAuthorOperator,
			Metadata:    json.RawMessage(`{"freeform":"text"}`),
		}
		assert.Nil(t, resolveOperatorCheckpointAction([]*persistence.TaskMessage{checkpointMsg(t, opts), noChoice}))
	})

	t.Run("checkpoint with malformed metadata yields nil", func(t *testing.T) {
		badCP := &persistence.TaskMessage{
			ID:          "cp-bad",
			MessageKind: persistence.TaskMessageKindCheckpoint,
			Metadata:    json.RawMessage(`{broken`),
		}
		assert.Nil(t, resolveOperatorCheckpointAction([]*persistence.TaskMessage{badCP, answerMsg("cp-bad", "reroute")}))
	})

	t.Run("answer with no resolvable checkpoint yields nil", func(t *testing.T) {
		// Only an answer, no checkpoint message at all.
		assert.Nil(t, resolveOperatorCheckpointAction([]*persistence.TaskMessage{answerMsg("missing", "reroute")}))
	})
}

func TestCheckpointAnswerChoice(t *testing.T) {
	assert.Equal(t, "x", checkpointAnswerChoice(json.RawMessage(`{"choice":"x"}`)))
	assert.Equal(t, "", checkpointAnswerChoice(nil))
	assert.Equal(t, "", checkpointAnswerChoice(json.RawMessage(`not-json`)))
	assert.Equal(t, "", checkpointAnswerChoice(json.RawMessage(`{}`)))
}

// TestCheckpointOptionAction_Valid pins the validation predicate directly,
// including the unknown-type default branch (the parser's demotion hinges
// on it).
func TestCheckpointOptionAction_Valid(t *testing.T) {
	var nilAction *CheckpointOptionAction
	assert.False(t, nilAction.valid())
	assert.True(t, (&CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "wf"}).valid())
	assert.False(t, (&CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow}).valid())
	assert.False(t, (&CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "   "}).valid())
	assert.True(t, (&CheckpointOptionAction{Type: CheckpointActionModelFallback}).valid())
	assert.True(t, (&CheckpointOptionAction{Type: CheckpointActionRetry}).valid())
	assert.True(t, (&CheckpointOptionAction{Type: CheckpointActionSkip}).valid())
	assert.False(t, (&CheckpointOptionAction{Type: "teleport"}).valid())
}

func TestApplyRecoveryCheckpointAction_RerouteWorkflow(t *testing.T) {
	t.Run("delegates a child on the operator-chosen candidate workflow", func(t *testing.T) {
		tr := NewMockTaskRepo()
		parentWF := "adaptive"
		parent := &persistence.Task{
			ID:         "t-parent",
			ProjectID:  "p1",
			WorkflowID: &parentWF,
			Payload:    json.RawMessage(`{"context":{"prompt":"x"}}`),
		}
		tr.AddTask(parent)
		e := &Executor{taskRepo: tr, logger: zerolog.Nop()}
		project := &registry.Project{ID: "p1", AdaptiveCandidateWorkflows: []string{"research-planner"}}

		applied := e.applyRecoveryCheckpointAction(context.Background(), parent, project, nil,
			&CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "research-planner"})
		require.True(t, applied, "a valid in-allow-list reroute must apply")

		children, err := tr.GetChildren(context.Background(), "t-parent")
		require.NoError(t, err)
		require.Len(t, children, 1, "exactly one child task must be delegated")
		require.NotNil(t, children[0].WorkflowID)
		assert.Equal(t, "research-planner", *children[0].WorkflowID)
	})

	t.Run("workflow outside the allow-list is demoted (no crash, no child)", func(t *testing.T) {
		tr := NewMockTaskRepo()
		parentWF := "adaptive"
		parent := &persistence.Task{ID: "t-parent", ProjectID: "p1", WorkflowID: &parentWF}
		tr.AddTask(parent)
		e := &Executor{taskRepo: tr, logger: zerolog.Nop()}
		// Empty candidate list — the financial-strict default — must reject.
		project := &registry.Project{ID: "p1"}

		applied := e.applyRecoveryCheckpointAction(context.Background(), parent, project, nil,
			&CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "research-planner"})
		assert.False(t, applied, "reroute outside the allow-list must demote to prose, not apply")
		children, _ := tr.GetChildren(context.Background(), "t-parent")
		assert.Empty(t, children, "no child task may be spawned on a rejected reroute")
	})

	t.Run("nil project demotes to prose", func(t *testing.T) {
		e := &Executor{logger: zerolog.Nop()}
		applied := e.applyRecoveryCheckpointAction(context.Background(), &persistence.Task{ID: "t"}, nil, nil,
			&CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "wf"})
		assert.False(t, applied)
	})
}

func TestApplyRecoveryCheckpointAction_ModelFallback(t *testing.T) {
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "researcher", Model: "primary", ModelFallback: "fallback-model"},
	}}
	t.Run("writes operator_model_override for roles with a fallback", func(t *testing.T) {
		writer := newFakeTaskRepo()
		e := &Executor{persistTaskRepo: writer, logger: zerolog.Nop()}
		task := &persistence.Task{ID: "t1", ProjectID: "p1", Payload: json.RawMessage(`{"context":{"prompt":"x"}}`)}

		applied := e.applyRecoveryCheckpointAction(context.Background(), task, nil, swarm,
			&CheckpointOptionAction{Type: CheckpointActionModelFallback})
		require.True(t, applied)
		persisted := writer.parents["t1"]
		require.NotNil(t, persisted, "the override must be persisted")
		assert.Equal(t, "fallback-model", operatorModelOverride(persisted.Payload, "researcher"))
	})

	t.Run("no role has a fallback demotes to prose", func(t *testing.T) {
		writer := newFakeTaskRepo()
		e := &Executor{persistTaskRepo: writer, logger: zerolog.Nop()}
		noFb := &registry.Swarm{Roles: []registry.SwarmRole{{Name: "lead", Model: "m"}}}
		applied := e.applyRecoveryCheckpointAction(context.Background(),
			&persistence.Task{ID: "t2"}, nil, noFb,
			&CheckpointOptionAction{Type: CheckpointActionModelFallback})
		assert.False(t, applied)
		assert.Empty(t, writer.parents, "no DB write when nothing to override")
	})

	t.Run("persistence error demotes to prose (fail-safe)", func(t *testing.T) {
		writer := newErrTaskWriter(errTaskWriterBoom)
		e := &Executor{persistTaskRepo: writer, logger: zerolog.Nop()}
		task := &persistence.Task{ID: "t3", ProjectID: "p1", Payload: json.RawMessage(`{"context":{}}`)}
		applied := e.applyRecoveryCheckpointAction(context.Background(), task, nil, swarm,
			&CheckpointOptionAction{Type: CheckpointActionModelFallback})
		assert.False(t, applied, "a persistence failure must demote to prose, not crash recovery")
	})
}

// TestApplyRecoveryCheckpointAction_UnknownTypeIsNoop guards the
// defensive default branch (the parser normally demotes unknown types,
// but the apply path must also be inert if one slips through).
func TestApplyRecoveryCheckpointAction_UnknownTypeIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	assert.False(t, e.applyRecoveryCheckpointAction(context.Background(),
		&persistence.Task{ID: "t"}, nil, nil, &CheckpointOptionAction{Type: "teleport"}))
}

var errTaskWriterBoom = errors.New("update failed")

// errTaskWriter embeds *fakeTaskRepo (a full persistence.TaskRepository)
// and overrides Update to return an error, exercising the model_fallback
// persistence-failure fail-safe.
type errTaskWriter struct {
	*fakeTaskRepo
	err error
}

func newErrTaskWriter(err error) *errTaskWriter {
	return &errTaskWriter{fakeTaskRepo: newFakeTaskRepo(), err: err}
}

func (w *errTaskWriter) Update(context.Context, *persistence.Task) error { return w.err }

func TestApplyRecoveryCheckpointAction_RetrySkipAndNil(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1"}
	// retry / skip keep the prose-hint path — applied=false (no new seam).
	assert.False(t, e.applyRecoveryCheckpointAction(context.Background(), task, nil, nil,
		&CheckpointOptionAction{Type: CheckpointActionRetry}))
	assert.False(t, e.applyRecoveryCheckpointAction(context.Background(), task, nil, nil,
		&CheckpointOptionAction{Type: CheckpointActionSkip}))
	// nil action is a no-op.
	assert.False(t, e.applyRecoveryCheckpointAction(context.Background(), task, nil, nil, nil))
}
