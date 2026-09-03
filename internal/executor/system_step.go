package executor

// `system` step type for deterministic, no-LLM workflow steps.
// B-7 — the document-ingest workflow's `extract` and `index`
// steps both use this path so chunking a markdown file into
// memory costs zero LLM tokens.
//
// Design notes
//   - Handler registry is a small map keyed on handler name
//     (e.g. "rag.extract"). The executor's dispatch (case
//     "system" in workflow.go) looks up the handler by
//     step.Handler and calls Execute.
//   - Handlers are stateless. They receive a SystemStepInput
//     carrying task + execution + step + previous result; they
//     return a SystemStepResult whose Result becomes the next
//     step's PrevResult (same shape as agent-step result.json).
//   - The registry is constructed by the service container at
//     boot, populated with the RAG handlers, and passed to
//     NewWithOptions via WithSystemHandlers. Daemons that don't
//     wire any handlers (CLI tools, tests) get an empty registry
//     and any system step fails with "unknown handler".

import (
	"context"
	"encoding/json"
	"fmt"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// SystemHandler is the contract for `system`-typed workflow steps.
// Implementations are pure-Go (no agent container, no LLM call).
// Looked up by name at dispatch time via SystemHandlerRegistry.
type SystemHandler interface {
	// Name is the handler's identifier in workflow YAML
	// (`step.handler: "rag.extract"`). Registry keys on this.
	Name() string

	// Execute runs the handler. The returned SystemStepResult.Result
	// is persisted as the step's LastResult and becomes the
	// PrevResult of the next step in the workflow.
	Execute(ctx context.Context, in SystemStepInput) (SystemStepResult, error)
}

// SystemStepInput is the envelope passed to every SystemHandler.
// Carries the task + execution pointers (handlers need ProjectID,
// task Payload, etc.) plus the workflow step config and the prior
// step's result. Kept narrow so tests don't need to build the full
// executor state.
type SystemStepInput struct {
	Task      *persistence.Task
	Execution *persistence.Execution
	StepID    string
	Step      *registry.WorkflowStep
	// PrevResult carries the previous step's LastResult (JSON
	// bytes). For the entrypoint step this is nil/empty.
	PrevResult json.RawMessage
}

// SystemStepResult is what Execute returns when it succeeds. The
// Result field becomes the step's LastResult — same shape an agent
// step's result.json would produce.
type SystemStepResult struct {
	Result json.RawMessage
}

// SystemStepBlockedState is the sentinel `state` value a system handler sets in
// its (successful, error-free) result to signal that it could NOT complete
// deterministically and the task must PARK awaiting operator action — rather
// than fail (and be retried) or route to on_success. The executor detects it
// after Execute and drives the same AWAITING_INPUT hand-off the lead uses,
// surfacing the handler's operator-facing message + any attached artifact. Used
// by forge.open_change_request when a branch push is rejected (e.g. a missing
// GitHub App permission): the change is captured as a patch artifact and the
// operator either grants the permission and resumes, or applies the patch and
// closes — instead of autonomy looping on an un-pushable task.
const SystemStepBlockedState = "blocked_awaiting_operator"

// PublishBlockedSignal is the result shape a system handler returns to request
// the awaiting-operator park. Marshaled into SystemStepResult.Result and parsed
// by the executor via AsPublishBlocked.
type PublishBlockedSignal struct {
	State        string `json:"state"` // must equal SystemStepBlockedState
	Reason       string `json:"reason,omitempty"`
	Remediation  string `json:"remediation,omitempty"`
	ArtifactID   string `json:"patch_artifact_id,omitempty"`
	ArtifactName string `json:"patch_artifact,omitempty"`
}

// AsPublishBlocked reports whether a system-step result requests the
// awaiting-operator park, returning the parsed signal when so.
func AsPublishBlocked(result json.RawMessage) (*PublishBlockedSignal, bool) {
	if len(result) == 0 {
		return nil, false
	}
	var s PublishBlockedSignal
	if err := json.Unmarshal(result, &s); err != nil || s.State != SystemStepBlockedState {
		return nil, false
	}
	return &s, true
}

// SystemHandlerRegistry is the executor's lookup table for
// system-typed steps. Constructed at daemon boot, then frozen for
// the executor's lifetime — handlers are wiring, not runtime
// config. Concurrency-safe for reads (map only mutated during
// Register, which runs in single-threaded boot code).
type SystemHandlerRegistry struct {
	byName map[string]SystemHandler
}

// NewSystemHandlerRegistry returns an empty registry. Callers
// populate it via Register before passing to WithSystemHandlers.
func NewSystemHandlerRegistry() *SystemHandlerRegistry {
	return &SystemHandlerRegistry{byName: map[string]SystemHandler{}}
}

// Register binds a handler under its Name(). Last-write-wins on
// duplicate names — operators can override the bundled handlers
// from a future plugin path without a code change. Nil handlers
// are a no-op so the service container can pass conditionally
// constructed handlers without nil-guarding each.
func (r *SystemHandlerRegistry) Register(h SystemHandler) {
	if r == nil || h == nil {
		return
	}
	r.byName[h.Name()] = h
}

// Get returns the handler claiming this name. ok=false when no
// handler is registered — the executor surfaces an
// "unknown handler" step outcome.
func (r *SystemHandlerRegistry) Get(name string) (SystemHandler, bool) {
	if r == nil {
		return nil, false
	}
	h, ok := r.byName[name]
	return h, ok
}

// Names returns the registered handler names. Powers `vornikctl
// doctor` warnings + the workflow validator's "unknown handler"
// surfacing.
func (r *SystemHandlerRegistry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	return out
}

// runSystemHandlerSafely invokes a system-step handler behind a panic barrier,
// converting a panic into an ordinary step error.
//
// It exists because on 2026-08-19 a nil dereference inside rag.index escaped the
// step goroutine and killed the daemon — the bench instance crash-looped 28
// times in ten minutes, and any production project running that workflow would
// have done the same. The specific nil is guarded now, but one handler's latent
// bug being able to stop every other project's work is the wrong blast radius.
// A step handler's bug belongs to that step: it fails, on_fail routes, and the
// daemon keeps serving.
//
// The error names the handler and says it panicked, so the failure is not
// mistaken for an ordinary handler rejection during triage. The stack goes to
// the log rather than into the error, which is operator-facing and ends up in
// task records.
//
// Transparent otherwise: a handler's own result and error pass through untouched.
func runSystemHandlerSafely(
	ctx context.Context,
	handler SystemHandler,
	handlerName string,
	in SystemStepInput,
) (res SystemStepResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("system handler %q panicked: %v (this is a handler bug — "+
				"the step failed instead of the daemon)", handlerName, r)
			res = SystemStepResult{}
		}
	}()
	return handler.Execute(ctx, in)
}

// systemResultMaxBytes caps the handler output injected into the next agent's
// prompt.
//
// An agent's message is bounded by the model's output limit; a system handler's
// is bounded by nothing — a large PR diff is megabytes. The in-tree precedent is
// canonicalContextMaxBytes (16 KiB, canonical_context.go), whose comment states
// the same concern: it "would otherwise balloon task.json and push the LLM's
// context budget".
//
// 256 KiB rather than 16 KiB because that cap is per pre-loaded FILE and this is
// one payload: 16 KiB would truncate nearly every real PR diff, making the fix
// useless for the case that motivated it. 256 KiB is roughly 64k tokens, which
// fits alongside a system prompt and workspace reads in the models configured
// here (128k+ context), and a diff larger than that is not reviewable in one
// pass anyway — at which point the truncation marker is the honest signal.
//
// Deliberately STATIC, not context-window-aware: the executor does not know the
// model's window at input-assembly time, and a lookup to derive one number is
// more moving parts than the number is worth.
const systemResultMaxBytes = 256 * 1024

// systemResultTruncationMarker is appended when the cap engages. It goes IN the
// injected text, not just the log: an agent that cannot tell it is holding part
// of a payload will review the visible half and report on the whole — the same
// class of defect this whole path exists to fix.
const systemResultTruncationMarker = "\n\n[... truncated: the previous step's output exceeded " +
	"the context budget. You are seeing the FIRST part only — do not treat it as complete, " +
	"and read the underlying files if you need the remainder.]"

// systemResultMessage extracts the agent-facing rendering from a system
// handler's result envelope, capped.
//
// `message` is the established convention across every handler: the
// human/agent-facing text, with the other keys carrying structured detail. A
// handler that returns only structured detail yields "", which leaves
// previousStepResult absent exactly as before — so this can only ever ADD
// context, never remove it.
//
// CONSTRAINT ON HANDLER AUTHORS: `message` is REFERENCE MATERIAL for the next
// agent — a diff, a fetched document, a scope marker — and must never carry
// instructions. A step that follows one is told to treat this as context to
// consult, not directives to obey; a handler smuggling a prompt in here would be
// injecting instructions the workflow author never wrote.
func systemResultMessage(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(result, &env); err != nil {
		return ""
	}
	if len(env.Message) <= systemResultMaxBytes {
		return env.Message
	}
	return env.Message[:systemResultMaxBytes] + systemResultTruncationMarker
}
