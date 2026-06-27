package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"vornik.io/vornik/internal/persistence"
)

// The narrow repository interfaces below are deliberately smaller
// than the full persistence.*Repository surfaces. The Builder only
// needs Get + List on most tables, and defining the slim version
// here keeps test fakes tractable (~10 methods instead of ~80).
// Concrete persistence.* repos satisfy these interfaces via Go's
// structural typing — no adapter needed.

type executionGetter interface {
	Get(ctx context.Context, id string) (*persistence.Execution, error)
}

type taskGetter interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
}

type outcomeLister interface {
	List(ctx context.Context, filter persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error)
}

type llmUsageLister interface {
	List(ctx context.Context, filter persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error)
}

type toolAuditLister interface {
	List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error)
}

type artifactLister interface {
	List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error)
}

type messageLister interface {
	List(ctx context.Context, filter persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error)
}

type postMortemGetter interface {
	Get(ctx context.Context, taskID string) (*persistence.TaskPostMortem, error)
}

// crossProjectCallLister supports the replay tree's multi-hop
// view (inter-project orchestration Phase C). The Builder
// reads two slices of CPC rows: ones where THIS task is the
// caller (outbound edges) and the single row where THIS task
// is the callee (the incoming breadcrumb).
//
// Defined as a narrow read-only interface so the existing
// persistence.CrossProjectCallRepository satisfies it by
// structural typing — no adapter required, and test fakes
// stay tiny.
type crossProjectCallLister interface {
	// GetByCalleeTaskID returns the CPC row that spawned the
	// given task, or ErrNotFound when the task isn't a CPC
	// callee.
	GetByCalleeTaskID(ctx context.Context, calleeTaskID string) (*persistence.CrossProjectCall, error)
	// Get returns a CPC row by its ID — used to resolve the
	// inbound breadcrumb's caller-project label.
	Get(ctx context.Context, id string) (*persistence.CrossProjectCall, error)
}

// Note: a v1-deferred "find CPC by caller task id" interface
// (crossProjectCallFinder) lived here as a placeholder for a
// future optimised lookup. Removed when the Builder's
// fan-out-via-children path proved sufficient; the typed
// interface gets added back when an actual repo method ships.

// taskChildLister returns the children of a parent task. The
// in-project delegation path already populates ParentTaskID
// on every child task — including CPC callees — so iterating
// the parent's children gives us every outbound CPC + spawn.
type taskChildLister interface {
	GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error)
}

// projectSpawnLister provides read access to project_spawns.
// The Builder asks "what did THIS task spawn?" by listing
// rows where parent_task_id matches. We use the by-spawned
// accessor (a UNIQUE lookup) per child task because that's
// what the repo exposes today; a parent-task-list method
// would be a Phase D optimisation.
type projectSpawnLister interface {
	GetBySpawnedProject(ctx context.Context, spawnedProjectID string) (*persistence.ProjectSpawn, error)
}

// executionByTaskGetter resolves the latest execution for a
// given task, used to build CalleeURL deep-links. The standard
// persistence.ExecutionRepository.GetByTaskID satisfies this.
type executionByTaskGetter interface {
	GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error)
}

// Builder assembles a Timeline by orchestrating reads across the
// repositories that already power the audit, spend, and outcome
// surfaces. No SQL JOINs — each repo serves its own table; we
// merge in Go.
//
// All repository fields are required EXCEPT PostMortems, which is
// optional (older deployments without the post-mortem subsystem
// still get a working timeline, just without the diagnosis card).
type Builder struct {
	Executions  executionGetter
	Tasks       taskGetter
	Outcomes    outcomeLister
	LLMUsage    llmUsageLister
	ToolAudit   toolAuditLister
	Artifacts   artifactLister
	Messages    messageLister
	PostMortems postMortemGetter // optional

	// CrossProjectCalls + ProjectSpawns + TaskChildren +
	// ExecutionByTask drive the inter-project multi-hop replay
	// tree (Phase C; LLD §9.2). All optional — when any is
	// nil the Builder skips the cross-project section and
	// renders a pure single-project timeline (backward-
	// compatible with deployments where the inter-project
	// feature flag is off).
	CrossProjectCalls crossProjectCallLister
	ProjectSpawns     projectSpawnLister
	TaskChildren      taskChildLister
	ExecutionByTask   executionByTaskGetter
}

// ErrExecutionNotFound is returned when the requested execution
// doesn't exist. The handler maps this to 404.
var ErrExecutionNotFound = errors.New("replay: execution not found")

// Build returns the assembled Timeline for executionID. Returns
// ErrExecutionNotFound when the execution row is missing.
//
// Wall-clock budget: ~5 sequential queries on indexed columns;
// 200ms is a generous ceiling. The handler wraps this in a
// context timeout.
func (b *Builder) Build(ctx context.Context, executionID string) (*Timeline, error) {
	if b == nil || b.Executions == nil || b.Tasks == nil ||
		b.Outcomes == nil || b.LLMUsage == nil || b.ToolAudit == nil ||
		b.Artifacts == nil || b.Messages == nil {
		return nil, errors.New("replay: builder not fully wired")
	}

	exec, err := b.Executions.Get(ctx, executionID)
	if err != nil {
		// Treat sql.ErrNoRows / persistence.ErrNotFound as not-found
		// (repo implementations vary on which they return).
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrExecutionNotFound
		}
		return nil, fmt.Errorf("replay: load execution: %w", err)
	}
	if exec == nil {
		return nil, ErrExecutionNotFound
	}

	task, err := b.Tasks.Get(ctx, exec.TaskID)
	if err != nil {
		return nil, fmt.Errorf("replay: load task: %w", err)
	}

	// Fan out the per-execution queries. Each repo serves a single
	// table; sequential is fine for v1 — could parallelise later if
	// the 200ms budget starts pressing.
	outcomes, err := b.loadOutcomes(ctx, executionID)
	if err != nil {
		return nil, err
	}
	llmRows, err := b.loadLLMUsage(ctx, executionID)
	if err != nil {
		return nil, err
	}
	toolRows, err := b.loadToolAudit(ctx, executionID)
	if err != nil {
		return nil, err
	}
	artifacts, err := b.loadArtifacts(ctx, executionID)
	if err != nil {
		return nil, err
	}
	messages, err := b.loadMessages(ctx, exec.TaskID, executionID)
	if err != nil {
		return nil, err
	}

	var pm *persistence.TaskPostMortem
	if b.PostMortems != nil {
		got, perr := b.PostMortems.Get(ctx, exec.TaskID)
		// Not-found is fine — operator hasn't requested one yet.
		if perr == nil {
			pm = got
		}
	}

	steps := buildSteps(outcomes, llmRows, toolRows, messages)

	tl := &Timeline{
		Execution:                exec,
		Task:                     task,
		PostMortem:               pm,
		Steps:                    steps,
		Artifacts:                renderArtifacts(artifacts),
		Lineage:                  b.walkLineage(ctx, exec),
		CrossProjectHops:         b.collectOutboundCrossProjectHops(ctx, task),
		IncomingCrossProjectCall: b.collectIncomingCrossProjectCall(ctx, task),
		Totals:                   buildTotals(steps, len(artifacts)),
	}
	return tl, nil
}

// collectIncomingCrossProjectCall builds the breadcrumb row at
// the top of the replay page when THIS task was called from
// another project. Returns nil when the task wasn't a CPC
// callee (the common case for ordinary tasks).
//
// All steps gated by nil-safe early returns so deployments
// without the inter-project surface wired skip the work
// cleanly.
func (b *Builder) collectIncomingCrossProjectCall(ctx context.Context, task *persistence.Task) *CrossProjectHop {
	if b == nil || task == nil || task.CrossProjectCallID == nil || b.CrossProjectCalls == nil {
		return nil
	}
	cpc, err := b.CrossProjectCalls.Get(ctx, *task.CrossProjectCallID)
	if err != nil || cpc == nil {
		return nil
	}
	hop := &CrossProjectHop{
		Kind:           "call",
		StepID:         cpc.CallerStepID,
		CPCId:          cpc.ID,
		CalleeProject:  cpc.CalleeProject,
		CalleeWorkflow: cpc.CalleeWorkflow,
		ExpectedSchema: cpc.ExpectedSchema,
		CallStatus:     string(cpc.Status),
		CreatedAt:      cpc.CreatedAt,
		ResolvedAt:     cpc.ResolvedAt,
	}
	if cpc.ErrorMessage != nil {
		hop.ErrorMessage = *cpc.ErrorMessage
	}
	// The breadcrumb's deep-link points BACK at the caller —
	// the caller's most-recent execution. Best-effort; an
	// empty URL is fine (the UI omits the link).
	if b.ExecutionByTask != nil {
		if exec, err := b.ExecutionByTask.GetByTaskID(ctx, cpc.CallerTaskID); err == nil && exec != nil {
			hop.CalleeURL = "/ui/executions/" + exec.ID + "/replay"
		}
	}
	// Preserve the caller-project context — the hop's
	// "CalleeProject" field on the incoming breadcrumb is
	// actually the OTHER end of the edge (the CALLER from
	// THIS task's perspective). Stash the caller-project
	// name in CalleeProject so the template can render
	// "called from <name>" without conditional logic per
	// edge direction.
	hop.CalleeProject = cpc.CallerProject
	return hop
}

// collectOutboundCrossProjectHops returns every outbound
// cross-project edge originating from THIS task: each call_project
// delegation (matched via the child task's
// cross_project_call_id) and each spawn_project materialisation
// (matched via project_spawns.parent_task_id by inspecting the
// child task's project ID).
//
// The implementation walks THIS task's children — produced by
// both delegation and spawn-initial-task seeding — and probes
// each for a CPC backreference + each child's project for a
// spawn record. Repos are queried minimally; the per-child
// cost is two indexed lookups.
func (b *Builder) collectOutboundCrossProjectHops(ctx context.Context, task *persistence.Task) []CrossProjectHop {
	if b == nil || task == nil || b.TaskChildren == nil {
		return nil
	}
	children, err := b.TaskChildren.GetChildren(ctx, task.ID)
	if err != nil || len(children) == 0 {
		return nil
	}
	hops := make([]CrossProjectHop, 0, len(children))
	for _, child := range children {
		if child == nil {
			continue
		}
		// CPC edge.
		if b.CrossProjectCalls != nil && child.CrossProjectCallID != nil {
			if cpc, err := b.CrossProjectCalls.Get(ctx, *child.CrossProjectCallID); err == nil && cpc != nil {
				h := CrossProjectHop{
					Kind:           "call",
					StepID:         cpc.CallerStepID,
					CPCId:          cpc.ID,
					CalleeProject:  cpc.CalleeProject,
					CalleeWorkflow: cpc.CalleeWorkflow,
					CalleeTaskID:   child.ID,
					ExpectedSchema: cpc.ExpectedSchema,
					CallStatus:     string(cpc.Status),
					CreatedAt:      cpc.CreatedAt,
					ResolvedAt:     cpc.ResolvedAt,
				}
				if cpc.ErrorMessage != nil {
					h.ErrorMessage = *cpc.ErrorMessage
				}
				if b.ExecutionByTask != nil {
					if ex, err := b.ExecutionByTask.GetByTaskID(ctx, child.ID); err == nil && ex != nil {
						h.CalleeURL = "/ui/executions/" + ex.ID + "/replay"
					}
				}
				hops = append(hops, h)
				continue
			}
		}
		// Spawn edge — the child is the initial_task seeded
		// into a spawned project. Look up the spawn by the
		// child's project ID; a hit means the child was
		// dropped into the spawned project's queue by THIS
		// task's spawn_project step.
		if b.ProjectSpawns != nil {
			if sp, err := b.ProjectSpawns.GetBySpawnedProject(ctx, child.ProjectID); err == nil && sp != nil && sp.ParentTaskID == task.ID {
				h := CrossProjectHop{
					Kind:           "spawn",
					StepID:         sp.ParentStepID,
					SpawnID:        sp.ID,
					SpawnedProject: sp.SpawnedProject,
					TemplateSlug:   sp.TemplateSlug,
					InitialTaskID:  child.ID,
					CreatedAt:      sp.CreatedAt,
				}
				if b.ExecutionByTask != nil {
					if ex, err := b.ExecutionByTask.GetByTaskID(ctx, child.ID); err == nil && ex != nil {
						h.CalleeURL = "/ui/executions/" + ex.ID + "/replay"
					}
				}
				hops = append(hops, h)
			}
		}
	}
	return hops
}

// walkLineage chains backwards from the current execution to the
// original source by following parent_execution_id, oldest first.
// Bounded at maxLineageDepth so a corrupt cycle can't hang the
// page render. Errors loading an ancestor stop the walk
// gracefully (the walker treats missing parents as "lineage ends
// here").
func (b *Builder) walkLineage(ctx context.Context, exec *persistence.Execution) []LineageHop {
	if exec == nil || exec.ParentExecutionID == nil || *exec.ParentExecutionID == "" {
		return nil
	}
	var hops []LineageHop
	const maxLineageDepth = 20
	currentID := *exec.ParentExecutionID
	for i := 0; i < maxLineageDepth; i++ {
		parent, err := b.Executions.Get(ctx, currentID)
		if err != nil || parent == nil {
			break
		}
		hop := LineageHop{
			ExecutionID: parent.ID,
			Status:      parent.Status,
			StartedAt:   parent.StartedAt,
			CompletedAt: parent.CompletedAt,
			URL:         "/ui/executions/" + parent.ID + "/replay",
		}
		if parent.ForkedFromStepID != nil {
			hop.ForkedFromStep = *parent.ForkedFromStepID
		}
		hops = append(hops, hop)
		if parent.ParentExecutionID == nil || *parent.ParentExecutionID == "" {
			break
		}
		currentID = *parent.ParentExecutionID
	}
	// Reverse so oldest comes first — operator reads the breadcrumb
	// "exec_original → exec_fork_1 → you" left-to-right.
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	return hops
}

func (b *Builder) loadOutcomes(ctx context.Context, executionID string) ([]*persistence.ExecutionStepOutcome, error) {
	rows, err := b.Outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &executionID,
		// PageSize 0 → repo default. Outcomes-per-execution is
		// bounded by workflow length (typically <20); no pagination.
	})
	if err != nil {
		return nil, fmt.Errorf("replay: list outcomes: %w", err)
	}
	return rows, nil
}

func (b *Builder) loadLLMUsage(ctx context.Context, executionID string) ([]*persistence.TaskLLMUsage, error) {
	rows, err := b.LLMUsage.List(ctx, persistence.TaskLLMUsageFilter{
		ExecutionID: &executionID,
		PageSize:    1000, // upper bound; most executions are well under
	})
	if err != nil {
		return nil, fmt.Errorf("replay: list llm usage: %w", err)
	}
	return rows, nil
}

func (b *Builder) loadToolAudit(ctx context.Context, executionID string) ([]*persistence.ToolAuditEntry, error) {
	rows, err := b.ToolAudit.List(ctx, persistence.ToolAuditFilter{
		ExecutionID: &executionID,
		PageSize:    1000,
	})
	if err != nil {
		return nil, fmt.Errorf("replay: list tool audit: %w", err)
	}
	return rows, nil
}

func (b *Builder) loadArtifacts(ctx context.Context, executionID string) ([]*persistence.Artifact, error) {
	rows, err := b.Artifacts.List(ctx, persistence.ArtifactFilter{
		ExecutionID: &executionID,
		PageSize:    1000,
	})
	if err != nil {
		return nil, fmt.Errorf("replay: list artifacts: %w", err)
	}
	return rows, nil
}

func (b *Builder) loadMessages(ctx context.Context, taskID, executionID string) ([]*persistence.TaskMessage, error) {
	// task_messages doesn't filter by execution_id at the repo
	// level today — we list for the task and drop rows whose
	// execution_id doesn't match. Tasks have bounded message
	// counts so the over-fetch cost is tolerable.
	rows, err := b.Messages.List(ctx, persistence.TaskMessageFilter{
		TaskID: taskID,
		Limit:  500,
	})
	if err != nil {
		return nil, fmt.Errorf("replay: list messages: %w", err)
	}
	filtered := make([]*persistence.TaskMessage, 0, len(rows))
	for _, m := range rows {
		if m.ExecutionID != nil && *m.ExecutionID == executionID {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

// buildSteps merges the per-execution rows into one ordered slice
// of Steps keyed on StepID. Each step picks up its outcome row's
// metadata + any tool calls / LLM usage / artifacts / messages
// that name the same step_id.
//
// Ordering: ExecutionStepOutcome.RecordedAt ASC. Steps that share
// a step_id (re-runs from a fork-in-place) collapse into one row;
// v1 doesn't distinguish them. v2 may surface iterations as
// nested sub-steps.
func buildSteps(
	outcomes []*persistence.ExecutionStepOutcome,
	llmRows []*persistence.TaskLLMUsage,
	toolRows []*persistence.ToolAuditEntry,
	messages []*persistence.TaskMessage,
) []Step {
	if len(outcomes) == 0 {
		return nil
	}
	// Sort outcomes by recorded_at ASC so the timeline reads
	// top-to-bottom in execution order.
	sort.Slice(outcomes, func(i, j int) bool {
		return outcomes[i].RecordedAt.Before(outcomes[j].RecordedAt)
	})

	// Index sidecar data by step_id for O(1) lookup.
	llmByStep := map[string][]*persistence.TaskLLMUsage{}
	for _, r := range llmRows {
		llmByStep[r.StepID] = append(llmByStep[r.StepID], r)
	}
	toolsByStep := map[string][]*persistence.ToolAuditEntry{}
	for _, r := range toolRows {
		toolsByStep[r.StepID] = append(toolsByStep[r.StepID], r)
	}
	messagesByStep := groupMessagesByStep(messages)

	steps := make([]Step, 0, len(outcomes))
	for _, o := range outcomes {
		s := Step{
			StepID:      o.StepID,
			Role:        o.Role,
			Model:       o.Model,
			RecordedAt:  o.RecordedAt,
			Outcome:     o.Outcome,
			ErrorClass:  o.ErrorClass,
			ErrorDetail: o.ErrorDetail,
		}
		if o.DurationMS != nil {
			s.DurationMs = *o.DurationMS
		}
		if o.AttributedToStepID != nil {
			s.AttributedToStepID = *o.AttributedToStepID
		}
		if len(o.HallucinationSignals) > 0 {
			s.HallucinationSignals = o.HallucinationSignals
		}
		s.LLMCalls = aggregateLLMCalls(llmByStep[o.StepID])
		for _, c := range s.LLMCalls {
			s.Iterations += c.Iterations
			s.CostUSD += c.CostUSD
		}
		s.ToolCalls = renderToolCalls(toolsByStep[o.StepID])
		s.Messages = renderMessages(messagesByStep[o.StepID])
		steps = append(steps, s)
	}
	return steps
}

// aggregateLLMCalls collapses rows with the same (model, role)
// tuple into a single LLMCall — typical case is one row per
// (step, model, role) but a step that switched models mid-run
// gets multiple.
func aggregateLLMCalls(rows []*persistence.TaskLLMUsage) []LLMCall {
	if len(rows) == 0 {
		return nil
	}
	type key struct{ model, role string }
	byKey := map[key]*LLMCall{}
	for _, r := range rows {
		k := key{model: r.Model, role: r.Role}
		c, ok := byKey[k]
		if !ok {
			c = &LLMCall{Model: r.Model, Role: r.Role, Source: r.Source}
			byKey[k] = c
		}
		c.PromptTokens += r.PromptTokens
		c.CompletionTokens += r.CompletionTokens
		c.CacheReadTokens += r.CacheReadTokens
		c.CostUSD += r.CostUSD
		c.Iterations += r.Iterations
	}
	out := make([]LLMCall, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	// Stable order: by model then role so render is deterministic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Role < out[j].Role
	})
	return out
}

func renderToolCalls(rows []*persistence.ToolAuditEntry) []ToolCall {
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	out := make([]ToolCall, 0, len(rows))
	for _, r := range rows {
		input, inputTrunc := truncateForRender(r.ToolInput)
		output, outputTrunc := truncateForRender(r.ToolOutput)
		out = append(out, ToolCall{
			ToolName:        r.ToolName,
			Input:           input,
			Output:          output,
			DurationMs:      r.DurationMs,
			RecordedAt:      r.CreatedAt,
			InputTruncated:  inputTrunc,
			OutputTruncated: outputTrunc,
		})
	}
	return out
}

func renderArtifacts(rows []*persistence.Artifact) []Artifact {
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	out := make([]Artifact, 0, len(rows))
	for _, a := range rows {
		item := Artifact{
			ID:       a.ID,
			Filename: a.Name,
			URL:      "/ui/artifacts/" + a.ID,
		}
		if a.SizeBytes != nil {
			item.SizeBytes = *a.SizeBytes
		}
		if a.ContentHashSHA256 != nil {
			item.Hash = *a.ContentHashSHA256
		}
		out = append(out, item)
	}
	return out
}

func renderMessages(rows []*persistence.TaskMessage) []Message {
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	out := make([]Message, 0, len(rows))
	for _, m := range rows {
		content, trunc := truncateForRender(m.Content)
		authorID := ""
		if m.AuthorID != nil {
			authorID = *m.AuthorID
		}
		out = append(out, Message{
			ID:         m.ID,
			AuthorKind: m.AuthorKind,
			AuthorID:   authorID,
			Kind:       m.MessageKind,
			Content:    content,
			Truncated:  trunc,
			CreatedAt:  m.CreatedAt,
		})
	}
	return out
}

// groupMessagesByStep extracts the step_id from each message's
// Metadata JSON when present. Messages without a step_id pinned
// in metadata are skipped (operator-visible chat messages that
// aren't step-scoped). The metadata shape varies by message kind,
// so we use a minimal JSON probe rather than typed unmarshal.
func groupMessagesByStep(rows []*persistence.TaskMessage) map[string][]*persistence.TaskMessage {
	out := map[string][]*persistence.TaskMessage{}
	for _, m := range rows {
		stepID := extractStepID(m.Metadata)
		if stepID == "" {
			continue
		}
		out[stepID] = append(out[stepID], m)
	}
	return out
}

// extractStepID pulls the step_id field out of a TaskMessage's
// Metadata JSON. Returns "" when missing, malformed, or the
// metadata is empty. We probe rather than typed-unmarshal because
// every message kind has its own metadata shape.
func extractStepID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		StepID string `json:"step_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.StepID
}

// buildTotals re-derives the aggregate counters from the step
// slice. Cheap — bounded by step count which is small. The
// artifact count comes in separately because artifacts live at
// the timeline level, not per-step.
func buildTotals(steps []Step, artifactCount int) Totals {
	t := Totals{StepCount: len(steps), Artifacts: artifactCount}
	for _, s := range steps {
		if s.Outcome == "ok" {
			t.OkSteps++
		} else if s.Outcome != "" && s.Outcome != "pending_validation" {
			t.FailSteps++
		}
		t.ToolCalls += len(s.ToolCalls)
		t.Iterations += s.Iterations
		t.CostUSD += s.CostUSD
	}
	return t
}
