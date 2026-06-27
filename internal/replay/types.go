// Package replay assembles a per-execution timeline from the data
// already in the persistence layer: step outcomes, tool audit
// entries, LLM usage rows, artifacts, task messages, and the
// cached LLM post-mortem if one was generated.
//
// The package is read-only in Phase A — it doesn't write to any
// table. Phase B will add the fork primitive in a separate file
// (fork.go) that mutates executions.
//
// Scope discipline: we surface what's persisted, no more.
// Workflow-step LLM prompt bodies are NOT stored today — only
// token counts + cost on task_llm_usage. So `LLMCalls` carries
// the summary (model, role, tokens, cost, iterations) but not
// the prompt/response text. The dispatcher chat path persists
// full prompts via chat_audit_log; that's a separate surface and
// out of scope for the workflow-execution replay.
package replay

import (
	"encoding/json"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Timeline is the top-level view-model the replay page renders.
// All fields are pre-shaped for the template — no further joins
// happen at render time.
type Timeline struct {
	Execution  *persistence.Execution
	Task       *persistence.Task
	PostMortem *persistence.TaskPostMortem // nil if not generated
	Steps      []Step
	// Artifacts is the full per-execution artifact list. v1 keeps
	// this at the timeline level because artifacts don't carry
	// step_id today — attributing them to specific steps requires
	// a schema change (deferred to v2). Rendered as a separate
	// section beneath the step timeline.
	Artifacts []Artifact
	// Lineage carries the forked-from chain, oldest first. Empty
	// in Phase A (no fork primitive yet); populated in Phase B.
	Lineage []LineageHop
	// CrossProjectHops carries outbound cross-project edges
	// from THIS task — call_project delegations + spawn_project
	// materialisations. Inter-project orchestration Phase C
	// surface; empty when the task didn't issue any. UI
	// renders each entry with a deep-link to the other side.
	CrossProjectHops []CrossProjectHop
	// IncomingCrossProjectCall is non-nil when THIS task was
	// itself called from another project — i.e. when the
	// task's cross_project_call_id column points at a CPC row.
	// Renders as a "called from <caller_project>" breadcrumb
	// at the top of the page.
	IncomingCrossProjectCall *CrossProjectHop
	// Totals aggregates the per-step counters so the page header
	// can show "10 steps, $4.32, 47 tool calls" without re-scanning.
	Totals Totals
}

// Step is one row in the replay timeline. Order is the execution
// order recorded by the executor (ExecutionStepOutcome.RecordedAt
// ascending). A step that didn't run (workflow defined it but
// execution never reached it) is absent — we only show steps
// with a recorded outcome.
type Step struct {
	// StepID is the workflow's step identifier (e.g. "research_2").
	StepID string
	// Role is the swarm role that ran this step (e.g. "researcher").
	Role string
	// Model is the LLM model the role ran on.
	Model string
	// RecordedAt is the outcome's recorded_at — the moment the
	// step finished. We don't have a started_at column, so this
	// stands in for ordering.
	RecordedAt time.Time
	// DurationMs is from ExecutionStepOutcome.DurationMS if
	// recorded; 0 when not (older executions, in-flight rows).
	DurationMs int64
	// Outcome is the step's terminal classification:
	// "ok", "schema_violation", "parse_error", "refused",
	// "hallucination", "timeout", "infra_error", etc.
	Outcome string
	// ErrorClass / ErrorDetail are non-empty when Outcome is not
	// "ok". The page renders them as a structured failure card.
	ErrorClass  string
	ErrorDetail string
	// AttributedToStepID is set when the consumer downstream
	// flipped THIS step's outcome to a failure class. Empty
	// when the step finalized itself or the executor's sweep
	// did. Operators care because "this step's output was
	// rejected by step X" is the actionable signal.
	AttributedToStepID string
	// LLMCalls aggregates task_llm_usage rows for this step.
	// One row per (model, role) tuple in case a step retried on
	// a different model — usually one row.
	LLMCalls []LLMCall
	// ToolCalls is the full tool_audit_log slice for this step,
	// in chronological order. Includes input + output JSON so the
	// page can render them as expandable blocks.
	ToolCalls []ToolCall
	// Messages are task_messages rows scoped to this step (via
	// the metadata.step_id field when present). Operator-visible
	// communications only — checkpoints, plans, notes. Internal
	// LLM prompts are NOT here; see package comment.
	Messages []Message
	// Iterations is the sum of TaskLLMUsage.Iterations across
	// LLMCalls — useful for "step retried 3× before giving up".
	Iterations int
	// CostUSD is the sum of TaskLLMUsage.CostUSD for this step.
	CostUSD float64
	// HallucinationSignals carries the JSON-encoded detector
	// findings the executor wrote on this step's outcome row.
	// Empty when no signals fired or detector not run.
	HallucinationSignals json.RawMessage
}

// LLMCall summarises one (model, role) tuple's spend on a step.
// The full prompt body isn't here because it isn't persisted —
// see package comment.
type LLMCall struct {
	Model            string
	Role             string
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CostUSD          float64
	Iterations       int
	Source           string // workflow_step / dispatcher / post_mortem / ...
}

// ToolCall is one tool_audit_log row, pre-shaped for the
// template. Input/Output are kept as strings because the audit
// layer stores them as TEXT — no JSON parsing on the render path.
type ToolCall struct {
	ToolName   string
	Input      string // truncated to RenderLimit
	Output     string // truncated to RenderLimit
	DurationMs int64
	RecordedAt time.Time
	// InputTruncated/OutputTruncated mark when content was
	// truncated past RenderLimit. The template uses these to
	// render an "[Expand]" link that fetches the full row via
	// a sub-endpoint (Phase A: just shows the truncation hint;
	// expand-fetch is a Phase C polish).
	InputTruncated  bool
	OutputTruncated bool
}

// Artifact is one produced file's metadata. Content lives in the
// artifact store and is fetched via /ui/artifacts/<id>.
type Artifact struct {
	ID        string
	Filename  string
	SizeBytes int64
	Hash      string
	URL       string // /ui/artifacts/<id>
}

// Message is one task_messages row scoped to the step. Author
// + kind + content; the full row stays in the DB.
type Message struct {
	ID         string
	AuthorKind string // operator / lead / system / role:...
	AuthorID   string
	Kind       string // checkpoint / plan / note / answer / ...
	Content    string // truncated to RenderLimit
	Truncated  bool
	CreatedAt  time.Time
}

// LineageHop is one ancestor execution in the fork chain. Phase A
// always returns an empty slice — fork tracking lands in Phase B's
// migration 48.
type LineageHop struct {
	ExecutionID    string
	ForkedFromStep string
	Status         persistence.ExecutionStatus
	StartedAt      *time.Time
	CompletedAt    *time.Time
	URL            string // /ui/executions/<id>/replay
}

// CrossProjectHop is one outbound cross-project edge from the
// current task — either a `call_project` delegation (Phase A)
// or a `spawn_project` materialisation (Phase B). Inter-project
// orchestration Phase C surface; populated by the replay
// builder by joining cross_project_calls + project_spawns
// against the current task ID. Empty slice when the task made
// no cross-project calls / spawns.
//
// The page renders these as expandable rows under the step
// timeline so an operator inspecting the marketing task can
// see the architect / implementation / sales hops without
// drilling into each callee task's replay separately.
type CrossProjectHop struct {
	// Kind distinguishes the edge type: "call" for a
	// call_project delegation, "spawn" for a spawn_project
	// materialisation. UI uses different icons + edge styles
	// per kind.
	Kind string
	// StepID is the caller-side step that issued the edge.
	StepID string

	// Call-only fields (Kind == "call") ------------------------
	CPCId          string
	CalleeProject  string
	CalleeWorkflow string
	CalleeTaskID   string
	ExpectedSchema string
	// CallStatus reflects the CPC's current status — pending /
	// running / completed / failed / rejected / timed_out.
	// Empty on Kind="spawn".
	CallStatus string
	// ErrorMessage is set when CallStatus is failed / rejected /
	// timed_out so the UI can render a one-line reason.
	ErrorMessage string

	// Spawn-only fields (Kind == "spawn") ---------------------
	SpawnID        string
	SpawnedProject string
	TemplateSlug   string
	InitialTaskID  string

	// CreatedAt is when the edge was issued (call) or the
	// spawn landed.
	CreatedAt time.Time
	// ResolvedAt is set on terminal CPC status; nil for
	// pending/running calls and for spawns (always immediate).
	ResolvedAt *time.Time
	// CalleeURL is the deep-link the UI uses to navigate to
	// the other side of the edge. Empty when no execution
	// exists yet (e.g. callee task hasn't been leased).
	CalleeURL string
}

// Totals is the headline aggregate shown at the top of the page.
// Re-derived from Steps[] so we don't have to query twice.
type Totals struct {
	StepCount  int
	OkSteps    int
	FailSteps  int // any outcome != ok
	ToolCalls  int
	Artifacts  int
	Iterations int
	CostUSD    float64
}

// RenderLimit is the byte cap applied to any single Input /
// Output / Content field before insertion into the template.
// 4 KiB is large enough for most prompts to come through whole;
// genuinely large blobs render as "first 4 KiB + [Expand]".
const RenderLimit = 4096

// truncateForRender bounds s to RenderLimit. Returns the
// truncated string + a bool flag the template uses to render the
// [Expand] hint.
func truncateForRender(s string) (string, bool) {
	if len(s) <= RenderLimit {
		return s, false
	}
	return s[:RenderLimit], true
}
