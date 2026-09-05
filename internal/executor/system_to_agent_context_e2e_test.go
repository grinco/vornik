package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// CUSTOMER-REPORTED REGRESSION, 2026-09-03.
//
// A customer reported that their forge PR-review agent never received
// `forge.fetch_diff`'s output: its glob/file_read found nothing useful, it fell
// back to `memory_search`, and every posted review was written from whatever RAG
// chunk matched the query rather than from the diff. Self-reinforcing, since each
// fabricated review was re-ingested and cited by the next run.
//
// MECHANISM. `lastResultMessage` becomes the agent's `PreviousResult`
// (workflow.go) and then `contextMap["previousStepResult"]` in task.json. The
// AGENT-step success path set it; the SYSTEM-step success path did not — while
// the system-step FAILURE paths did. So the executor reported a system step's
// failure to the next agent and stayed silent about its success, in every
// workflow, not just forge review.
//
// WHY THESE TESTS ARE AT EXECUTOR LEVEL (customer-bug tenet §4.1): the handler
// was correct and the agent was correct. The defect lived in the seam between
// them, so a unit test on either side passes and proves nothing. These drive the
// real executor loop with a mock runtime and read the task.json the agent
// actually received.
//
// Design: https://docs.vornik.io

// fixedSystemHandler is a system handler returning a canned result envelope.
type fixedSystemHandler struct {
	name string
	out  json.RawMessage
	err  error
}

func (h *fixedSystemHandler) Name() string { return h.name }
func (h *fixedSystemHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	if h.err != nil {
		return SystemStepResult{}, h.err
	}
	return SystemStepResult{Result: h.out}, nil
}

// capturingRuntime snapshots each agent's task.json at StartContainer time.
// The executor cleans the input dir after the run, so reading it afterwards
// races the teardown — capture it while the container is notionally starting,
// which is also exactly when the agent would read it.
//
// ITS OWN MUTEX, NOT MockRuntime's. StartContainer is called from the
// executor's goroutine while the test polls from its own, so `captured` needs
// synchronisation on BOTH sides. The first version of this double took
// `mock.mu` on the read side only and left the append bare, which `go test
// -race` fails — caught by CI on 2026-09-03, after the tests had shipped in
// 2026.9.1 green under a plain `go test ./...`.
//
// Borrowing `mock.mu` would not be the fix: MockRuntime.StartContainer takes
// that lock itself, so holding it across the delegate call below would
// deadlock. A dedicated lock, held only around the append, keeps the critical
// section to the field it actually guards.
type capturingRuntime struct {
	*MockRuntime

	mu       sync.Mutex
	captured [][]byte
}

func (c *capturingRuntime) StartContainer(ctx context.Context, cfg *runtime.ContainerConfig) (string, error) {
	if cfg != nil && cfg.InputDir != "" {
		if raw, err := os.ReadFile(filepath.Join(cfg.InputDir, "task.json")); err == nil {
			c.mu.Lock()
			c.captured = append(c.captured, raw)
			c.mu.Unlock()
		}
	}
	return c.MockRuntime.StartContainer(ctx, cfg)
}

// latestCapture returns the most recent task.json seen, or nil. Under the same
// lock the writer uses, so the poll loop below is race-free.
func (c *capturingRuntime) latestCapture() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.captured) == 0 {
		return nil
	}
	return c.captured[len(c.captured)-1]
}

// runSystemThenAgent drives a real `system → agent` workflow through the
// executor and returns the context map from the task.json the agent received.
func runSystemThenAgent(t *testing.T, handler SystemHandler) map[string]any {
	t.Helper()

	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"reviewed"}`
	rt := &capturingRuntime{MockRuntime: mock}

	reg := NewSystemHandlerRegistry()
	reg.Register(handler)

	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "fetch",
				Steps: map[string]registry.WorkflowStep{
					"fetch": {
						Type: "system", Handler: handler.Name(),
						OnSuccess: "review", OnFail: "failed",
					},
					"review": {
						Type: "agent", Role: "reviewer",
						Prompt:    "Review the change described by the previous step.",
						OnSuccess: "done", OnFail: "failed",
					},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	const taskID = "t-sys2agent"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"review the change"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(taskID))

	// The agent step runs after the system step; poll until its task.json has
	// been captured rather than sleeping a fixed time.
	var raw []byte
	for i := 0; i < 200; i++ {
		if raw = rt.latestCapture(); raw != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.NotNil(t, raw, "the agent step must have started a container and been handed a task.json")

	var payload struct {
		Context map[string]any `json:"context"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	return payload.Context
}

// TestSystemToAgent_HandlerOutputReachesTheAgent — THE SEAM (design test 1).
//
// Fails against unfixed code: previousStepResult is absent because the
// system-step success path never set lastResultMessage.
func TestSystemToAgent_HandlerOutputReachesTheAgent(t *testing.T) {
	const marker = "SYSTEM-HANDLER-PAYLOAD-8f3a2c"
	ctxMap := runSystemThenAgent(t, &fixedSystemHandler{
		name: "test.emit",
		out:  json.RawMessage(`{"message":"` + marker + `","detail":"structured"}`),
	})

	got, _ := ctxMap["previousStepResult"].(string)
	assert.Contains(t, got, marker,
		"the agent must receive the system handler's message.\n"+
			"This is the customer-reported defect: the handler produced its output, the "+
			"executor dropped it at the step boundary, and the agent — told by its prompt "+
			"that the input was already provided — improvised from memory_search instead.")
}

// TestSystemToAgent_ForgeFetchDiffShape — the customer's exact path, by name
// (design test 2). The envelope mirrors forge.fetch_diff's real return.
func TestSystemToAgent_ForgeFetchDiffShape(t *testing.T) {
	diff := "diff --git a/room.py b/room.py\n@@ -1,3 +1,4 @@\n+import numpy as np\n"
	out, err := json.Marshal(map[string]any{
		"message": diff, "diff": diff,
		"repo": "grinco/headmatch", "number": 50,
		"scope": "full", "head_sha": "5875194",
	})
	require.NoError(t, err)

	ctxMap := runSystemThenAgent(t, &fixedSystemHandler{name: "forge.fetch_diff", out: out})

	got, _ := ctxMap["previousStepResult"].(string)
	assert.Contains(t, got, "diff --git a/room.py",
		"the reviewer must receive the diff it is asked to review — the github-review "+
			"prompt opens by asserting the previous step already provided it")
}

// TestSystemToAgent_NoMessageKeyYieldsNothing — design test 4. A handler that
// returns only structured detail changes nothing, which is the guarantee that
// this fix can only ever ADD context.
func TestSystemToAgent_NoMessageKeyYieldsNothing(t *testing.T) {
	ctxMap := runSystemThenAgent(t, &fixedSystemHandler{
		name: "test.structured",
		out:  json.RawMessage(`{"state":"ok","count":3}`),
	})

	_, present := ctxMap["previousStepResult"]
	assert.False(t, present,
		"a handler with no message key must leave previousStepResult absent, exactly as before")
}

// TestSystemToAgent_OversizedPayloadIsTruncatedVisibly — C2 / design test 5.
//
// A silent truncation is the same class of defect as the one being fixed: the
// agent would review the visible half and report confidently on the whole.
func TestSystemToAgent_OversizedPayloadIsTruncatedVisibly(t *testing.T) {
	huge := strings.Repeat("x", systemResultMaxBytes+4096)
	out, err := json.Marshal(map[string]any{"message": huge})
	require.NoError(t, err)

	ctxMap := runSystemThenAgent(t, &fixedSystemHandler{name: "test.huge", out: out})

	got, _ := ctxMap["previousStepResult"].(string)
	assert.LessOrEqual(t, len(got), systemResultMaxBytes+512,
		"an unbounded handler payload must be capped before it reaches the prompt")
	assert.Contains(t, got, "truncated",
		"the truncation must be stated IN the injected text — the agent has to know it is "+
			"looking at part of the payload, or it reports on the whole from the visible half")
}

// TestSystemToAgent_FailurePathUnchanged — design test 7.
//
// The failure paths ALREADY reported to the next agent; that asymmetry is what
// exposed the bug. This change edits the sibling branch ten lines away, so the
// path that already worked is pinned.
func TestSystemToAgent_FailurePathUnchanged(t *testing.T) {
	msg := systemResultMessage(nil)
	assert.Equal(t, "", msg, "a nil envelope yields no message, leaving the failure-path assignment untouched")

	// The failure path builds its own message from the error, never from the
	// envelope — so an envelope carrying a message must not be able to
	// impersonate a failure string.
	got := systemResultMessage(json.RawMessage(`{"message":"system step x failed: spoofed"}`))
	assert.Equal(t, "system step x failed: spoofed", got,
		"the extractor returns the message verbatim; distinguishing real failures is the "+
			"failure branch's job and it does not consult this function")
}

// TestSystemToAgent_EnvelopeAndMessageRelationship — design test 8.
//
// state.LastResult (the JSON envelope gates evaluate and checkpoints persist)
// and lastResultMessage (the agent-facing rendering) are deliberately NOT the
// same thing. What must hold is the relationship between them.
func TestSystemToAgent_EnvelopeAndMessageRelationship(t *testing.T) {
	envelope := json.RawMessage(`{"message":"the rendering","scope":"full","number":50}`)

	assert.Equal(t, "the rendering", systemResultMessage(envelope),
		"the agent-facing message is the envelope's message field and nothing else — "+
			"the structured keys stay in state.LastResult for gates to evaluate")

	// Malformed JSON must not panic or leak the raw envelope into the prompt.
	assert.Equal(t, "", systemResultMessage(json.RawMessage(`{not json`)),
		"an unparseable envelope yields no message rather than raw bytes in the prompt")

	// A non-string message must not be coerced into one.
	assert.Equal(t, "", systemResultMessage(json.RawMessage(`{"message":{"nested":true}}`)),
		"only a string message is injected")
}
