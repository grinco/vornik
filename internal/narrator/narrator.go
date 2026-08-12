// Package narrator implements the Narrated Execution worker (task
// 2.1, https://docs.vornik.io). It
// subscribes to the existing live-execution bus (internal/executor/
// livepubsub) — no executor changes — and turns step/tool milestones
// into short, plain-language lines: persisted first, then
// republished onto the same bus as KindNarrationLine.
//
// Shaped directly on memory.NarrativeWriter (cheap-model, per-call
// model override, task_llm_usage attribution) + memory.
// LLMConsolidateWorker (background loop + metrics), per the design's
// §2 reuse table.
package narrator

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// Default knob values, applied whenever the corresponding Narrator
// field is left at its zero value (design §5.2/§5.4).
const (
	DefaultDebounce         = 2 * time.Second
	DefaultLongToolThresh   = 10 * time.Second
	DefaultMinLineInterval  = 3 * time.Second
	DefaultMaxLines         = 40
	DefaultMaxCostUSD       = 0.02
	defaultIdlePollInterval = 5 * time.Second
	defaultIdleThreshold    = 8 * time.Second
	defaultForceTeardown    = 2 * time.Hour
)

// Subscriber is the narrow slice of livepubsub.Publisher the
// narrator needs to observe every execution's events. Mirrors the
// memory.UsageRecorder narrow-interface convention.
type Subscriber interface {
	SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error)
}

// EventPublisher is the narrow slice of livepubsub.Publisher the
// narrator needs to republish narration lines.
type EventPublisher interface {
	Publish(ctx context.Context, executionID, kind string, payload any) int64
}

// Store persists narration lines. The narrator ALWAYS calls
// Store.Insert before EventPublisher.Publish for the same line
// (persist-then-publish, design §5.3, load-bearing) — Run enforces
// the ordering; implementations don't need to know about it.
type Store interface {
	Insert(ctx context.Context, row *persistence.ExecutionNarration) (seq int64, err error)
}

// ExecutionLookup resolves the (project_id, task_id, status) an
// execution_id needs. Two uses: (1) the FIRST event for a new
// execution_id resolves project_id/task_id (required, NOT NULL,
// columns on execution_narration); (2) the idle-poll sweep samples
// Status to detect a terminal execution, since no bus event fires on
// completion (non-goal #1: no executor changes — this IS the
// "sample the audit/other stores post-hoc" fallback the design
// directs for a signal that isn't on the bus).
type ExecutionLookup interface {
	Get(ctx context.Context, id string) (*persistence.Execution, error)
}

// Narrator is the bus subscriber + narration composer. Shape mirrors
// the design's §5.1 pseudocode field-for-field.
type Narrator struct {
	Sub        Subscriber
	Pub        EventPublisher
	Store      Store
	Executions ExecutionLookup

	Client chat.Provider
	Model  string

	// Spend records one task_llm_usage row per billed narration call. Pricing is
	// still consulted separately below because narrateCost RETURNS the figure to
	// its caller — it is not only a ledger field here.
	Spend   llmspend.Recorder
	Pricing memory.PricingTable
	Scanner SecretScanner

	// --- Chat push (task 2.3, chatpush.go) ---------------------------
	//
	// Tasks / Audit / Resolver are the chatorigin.Resolve collaborators —
	// SHARED with internal/steering's Notifier via internal/chatorigin (see
	// that package's doc comment). Any of the three nil disables chat push
	// entirely; it never errors, it just never pushes.
	Tasks    TaskGetter
	Audit    chatorigin.ChatAuditLookup
	Resolver chatorigin.ChannelResolver
	// Artifacts backs the deliverable-led completion push (§5.7 point 4).
	// Nil ⇒ completion push falls back to the plain narration text.
	Artifacts ArtifactLister
	// ProjectSettings resolves a project's chat_push/no_narration flags
	// (registry.Project.Narrator). Nil ⇒ every project gets the package
	// defaults (chat push off, narration on) — pre-2026-07-10 behaviour.
	ProjectSettings func(projectID string) ProjectNarratorSettings
	// ChatMilestoneKinds overrides which trigger kinds the chat push
	// considers a milestone. Empty ⇒ [step_completed, completion].
	ChatMilestoneKinds []string
	// BaseURL is the external base URL for the completion push's UI deep
	// link, mirroring steering.Notifier.baseURL. Empty omits the link.
	BaseURL string

	// Debounce / LongToolThreshold / MinLineInterval / MaxLines /
	// MaxCostUSD are the trigger + cost-bound knobs (§5.2, §5.4).
	// Zero values fall back to the Default* constants above.
	Debounce         time.Duration
	LongToolThresh   time.Duration
	MinLineInterval  time.Duration
	MaxLines         int
	MaxCostUSD       float64
	IdlePollInterval time.Duration
	IdleThreshold    time.Duration
	ForceTeardown    time.Duration

	Logger  zerolog.Logger
	Metrics *Metrics

	// nowFn is overridable in tests for deterministic min_line_interval
	// / idle-sweep assertions without real sleeps. Defaults to
	// time.Now.
	nowFn func() time.Time

	// afterFunc is overridable in tests so debounce/heartbeat timers
	// fire fast+deterministically. Defaults to time.AfterFunc.
	afterFunc func(d time.Duration, f func()) *time.Timer

	// onLine is an optional test hook invoked (on the Run goroutine,
	// synchronously) after each line is persisted+published.
	onLine func(row *persistence.ExecutionNarration)
	// preLine is an optional test hook invoked immediately before
	// onLine, on the Run goroutine. Lets tests layer extra behaviour
	// (e.g. panic-once-then-behave-normally) on top of the harness's
	// default onLine forwarding without racing a post-construction
	// field overwrite.
	preLine func(row *persistence.ExecutionNarration)
	// onTeardown is an optional test hook invoked (on the Run
	// goroutine) whenever a per-execution state is removed, so tests
	// can observe teardown deterministically instead of polling
	// states from another goroutine.
	onTeardown func(executionID string)

	states map[string]*executionState

	debounceCh  chan debounceFired
	heartbeatCh chan heartbeatFired
}

type debounceFired struct {
	executionID string
	epoch       uint64
}

type heartbeatFired struct {
	executionID string
	callID      string
	epoch       uint64
}

func (n *Narrator) now() time.Time {
	if n.nowFn != nil {
		return n.nowFn()
	}
	return time.Now()
}

func (n *Narrator) arm(d time.Duration, f func()) *time.Timer {
	if n.afterFunc != nil {
		return n.afterFunc(d, f)
	}
	return time.AfterFunc(d, f)
}

func (n *Narrator) debounce() time.Duration {
	if n.Debounce > 0 {
		return n.Debounce
	}
	return DefaultDebounce
}

func (n *Narrator) longToolThreshold() time.Duration {
	if n.LongToolThresh > 0 {
		return n.LongToolThresh
	}
	return DefaultLongToolThresh
}

func (n *Narrator) minLineInterval() time.Duration {
	if n.MinLineInterval > 0 {
		return n.MinLineInterval
	}
	return DefaultMinLineInterval
}

func (n *Narrator) maxLines() int {
	if n.MaxLines > 0 {
		return n.MaxLines
	}
	return DefaultMaxLines
}

func (n *Narrator) maxCostUSD() float64 {
	if n.MaxCostUSD > 0 {
		return n.MaxCostUSD
	}
	return DefaultMaxCostUSD
}

func (n *Narrator) idlePollInterval() time.Duration {
	if n.IdlePollInterval > 0 {
		return n.IdlePollInterval
	}
	return defaultIdlePollInterval
}

func (n *Narrator) idleThreshold() time.Duration {
	if n.IdleThreshold > 0 {
		return n.IdleThreshold
	}
	return defaultIdleThreshold
}

func (n *Narrator) forceTeardownAfter() time.Duration {
	if n.ForceTeardown > 0 {
		return n.ForceTeardown
	}
	return defaultForceTeardown
}

func (n *Narrator) ensureInit() {
	if n.states == nil {
		n.states = map[string]*executionState{}
	}
	if n.debounceCh == nil {
		n.debounceCh = make(chan debounceFired, 64)
	}
	if n.heartbeatCh == nil {
		n.heartbeatCh = make(chan heartbeatFired, 64)
	}
}

// Run drives the subscribe-and-narrate loop until ctx is cancelled.
// Structurally disabled (returns immediately, doing nothing) when
// Sub, Pub, Store, or Executions is nil — the caller (service
// container) gates construction on narrator.enabled and the
// available wiring, matching LLMConsolidateWorker.Run's contract.
//
// The loop body (runOnce) is wrapped in recover() so a narrator
// panic NEVER crashes the daemon (design §5.1): a recovered panic
// increments vornik_narration_panics_total, clears all per-execution
// state (a clean slate — storage already has everything persisted so
// far), and re-subscribes after a short backoff.
func (n *Narrator) Run(ctx context.Context) {
	if n == nil || n.Sub == nil || n.Pub == nil || n.Store == nil || n.Executions == nil {
		return
	}
	n.ensureInit()
	n.Logger.Info().
		Dur("debounce", n.debounce()).
		Dur("long_tool_threshold", n.longToolThreshold()).
		Dur("min_line_interval", n.minLineInterval()).
		Int("max_lines", n.maxLines()).
		Float64("max_cost_usd", n.maxCostUSD()).
		Msg("narrator: worker started")
	for {
		if ctx.Err() != nil {
			return
		}
		n.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (n *Narrator) runOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			n.Logger.Error().Interface("panic", r).
				Msg("narrator: recovered panic in Run loop; clearing state and resubscribing")
			n.metricPanic()
			n.states = map[string]*executionState{}
		}
	}()

	events, cancel, err := n.Sub.SubscribeAll()
	if err != nil {
		n.Logger.Warn().Err(err).Msg("narrator: SubscribeAll failed")
		return
	}
	defer cancel()

	ticker := time.NewTicker(n.idlePollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			n.handleBusEvent(ctx, evt)
		case f := <-n.debounceCh:
			n.handleDebounceFired(ctx, f)
		case f := <-n.heartbeatCh:
			n.handleHeartbeatFired(ctx, f)
		case now := <-ticker.C:
			n.sweepIdle(ctx, now)
		}
	}
}

func (n *Narrator) handleBusEvent(ctx context.Context, evt livepubsub.LiveEvent) {
	if evt.ExecutionID == "" {
		return
	}
	// NEVER react to our own published output (incident 2026-07-11 "narrator
	// spam"): SubscribeAll delivers the KindNarrationLine events emitLine
	// publishes, and reaching stateFor below would re-create state for the
	// already-torn-down execution — the next idle sweep then re-emits the
	// completion line, republishes, and the loop runs one LLM call per
	// idle-poll tick forever (per-execution caps reset with the state).
	// Observed in prod: 180+ "Task completed successfully." lines on one task.
	if evt.Kind == livepubsub.KindNarrationLine {
		return
	}
	st := n.stateFor(ctx, evt.ExecutionID)
	if st == nil {
		n.metricDropped(evt.Kind)
		return
	}
	st.lastEventAt = n.now()

	switch evt.Kind {
	case livepubsub.KindStepStarted:
		n.onStepStarted(ctx, evt, st)
	case livepubsub.KindToolCallStarted:
		n.onToolCallStarted(ctx, evt, st)
	case livepubsub.KindToolCallFinished:
		n.onToolCallFinished(evt, st)
	case livepubsub.KindStepCompleted:
		n.onStepCompleted(ctx, evt, st)
	default:
		// Every other kind (llm_call_started/finished, paused,
		// resumed, file_edit, forked, cross-project-*, ...) is
		// observed but is deliberately NOT a narration trigger per
		// design §5.2's trigger table.
	}
}

func (n *Narrator) stateFor(ctx context.Context, executionID string) *executionState {
	if st, ok := n.states[executionID]; ok {
		return st
	}
	exec, err := n.Executions.Get(ctx, executionID)
	if err != nil || exec == nil {
		return nil
	}
	// Defence in depth for the same feedback-loop class: a straggler event
	// arriving AFTER the completion line + teardown (late tool_call_finished,
	// replayed bus event) must not resurrect the story — the sweep would
	// emit a duplicate completion for it.
	if isTerminalStatus(exec.Status) {
		return nil
	}
	st := newExecutionState(executionID, exec.ProjectID, exec.TaskID, n.now())
	n.states[executionID] = st
	return st
}

func (n *Narrator) onStepStarted(ctx context.Context, evt livepubsub.LiveEvent, st *executionState) {
	p, ok := evt.Payload.(livepubsub.StepStartedPayload)
	if !ok {
		return
	}
	if !st.seenSteps[p.StepID] {
		st.seenSteps[p.StepID] = true
		st.stepIdx++
	}
	st.currentStepID = p.StepID
	st.currentRole = p.Role

	st.pendingEpoch++
	epoch := st.pendingEpoch
	st.pendingStart = &pendingLine{stepID: p.StepID, role: p.Role, stepIdx: st.stepIdx, epoch: epoch}

	execID := evt.ExecutionID
	n.arm(n.debounce(), func() {
		select {
		case n.debounceCh <- debounceFired{executionID: execID, epoch: epoch}:
		case <-ctx.Done():
		}
	})
}

func (n *Narrator) handleDebounceFired(ctx context.Context, f debounceFired) {
	st, ok := n.states[f.executionID]
	if !ok {
		return
	}
	if st.pendingStart == nil || st.pendingStart.epoch != f.epoch {
		return // cancelled (superseded by StepCompleted) or superseded by a newer start
	}
	pending := st.pendingStart
	st.pendingStart = nil
	n.emitLine(ctx, f.executionID, st, triggerStepStarted,
		templateInput{Role: pending.role, StepIdx: pending.stepIdx, StepTotal: st.stepTotal},
		pending.stepID, "", persistence.ExecutionNarrationKindStep)
}

func (n *Narrator) onToolCallStarted(ctx context.Context, evt livepubsub.LiveEvent, st *executionState) {
	p, ok := evt.Payload.(livepubsub.ToolCallStartedPayload)
	if !ok {
		return
	}
	st.toolEpoch++
	epoch := st.toolEpoch
	st.toolTimers[p.CallID] = &toolTimerState{stepID: p.StepID, tool: p.Tool, epoch: epoch}

	execID := evt.ExecutionID
	callID := p.CallID
	n.arm(n.longToolThreshold(), func() {
		select {
		case n.heartbeatCh <- heartbeatFired{executionID: execID, callID: callID, epoch: epoch}:
		case <-ctx.Done():
		}
	})
}

func (n *Narrator) onToolCallFinished(evt livepubsub.LiveEvent, st *executionState) {
	p, ok := evt.Payload.(livepubsub.ToolCallFinishedPayload)
	if !ok {
		return
	}
	// Cancels the heartbeat: deleting the map entry makes the
	// eventual timer fire a no-op (epoch/presence check in
	// handleHeartbeatFired) even though the real time.Timer itself
	// keeps running — cheaper than tracking + calling Stop() and
	// exactly as correct, since the fired signal is ignored either way.
	delete(st.toolTimers, p.CallID)
}

func (n *Narrator) handleHeartbeatFired(ctx context.Context, f heartbeatFired) {
	st, ok := n.states[f.executionID]
	if !ok {
		return
	}
	tt, ok := st.toolTimers[f.callID]
	if !ok || tt.epoch != f.epoch {
		return // finished already, or superseded
	}
	delete(st.toolTimers, f.callID) // one-shot: fires at most once per call
	n.emitLine(ctx, f.executionID, st, triggerToolHeartbeat,
		templateInput{Role: st.currentRole, Tool: tt.tool, StepIdx: st.stepIdx, StepTotal: st.stepTotal},
		tt.stepID, tt.tool, persistence.ExecutionNarrationKindTool)
}

func (n *Narrator) onStepCompleted(ctx context.Context, evt livepubsub.LiveEvent, st *executionState) {
	p, ok := evt.Payload.(livepubsub.StepCompletedPayload)
	if !ok {
		return
	}
	// Supersede a pending step-start line — design §5.2: "step-start
	// lines older than the completion they precede are dropped,
	// never both shown."
	if st.pendingStart != nil && st.pendingStart.stepID == p.StepID {
		st.pendingStart = nil
	}
	n.emitLine(ctx, evt.ExecutionID, st, triggerStepCompleted,
		templateInput{Role: st.currentRole, StepIdx: st.stepIdx, StepTotal: st.stepTotal, Outcome: p.Outcome},
		p.StepID, "", persistence.ExecutionNarrationKindStep)
}

// sweepIdle is the idle-poll terminal-detection fallback (see
// ExecutionLookup's doc comment): for every execution that has gone
// quiet for at least idleThreshold, sample its status once. Terminal
// → emit the completion line and tear down; still running → leave it
// for the next tick; unresolvable for longer than forceTeardownAfter
// → drop the state anyway (bounded memory, matches livepubsub's own
// idle-stream eviction philosophy).
func (n *Narrator) sweepIdle(ctx context.Context, now time.Time) {
	for execID, st := range n.states {
		idle := now.Sub(st.lastEventAt)
		// Lag tradeoff: a completed execution's completion line isn't
		// emitted the instant it finishes — it's discovered on the
		// next tick after idleThreshold has elapsed with no bus
		// activity (up to ~idleThreshold seconds of extra delay,
		// worst case idleThreshold + idlePollInterval). This is the
		// accepted cost of the non-goal #1 constraint (design §4: "no
		// new executor events / no executor core changes" — completion
		// isn't a bus event today, so it's sampled post-hoc here
		// instead of the executor emitting one).
		if idle < n.idleThreshold() {
			continue
		}
		exec, err := n.Executions.Get(ctx, execID)
		if err != nil || exec == nil {
			if idle >= n.forceTeardownAfter() {
				n.teardown(execID)
			}
			continue
		}
		if isTerminalStatus(exec.Status) {
			// An execution reaching a terminal status does NOT always mean the
			// TASK is done: a recover/checkpoint flow ends an execution at an
			// intermediate step and spawns a continuation execution, so the
			// task is still mid-flight. Narrating "completed successfully"
			// there is a false completion (incident 2026-07-11: a research
			// task narrated success while it was in recovery). Only emit the
			// completion line when the TASK itself is terminal. When the task
			// is still active, DON'T tear down immediately — this also covers
			// a lagging terminal-flip (exec terminal, task not yet): a later
			// sweep re-checks and emits once the task settles, or the state is
			// force-torn-down after ForceTeardown if a continuation took over.
			active, known := n.taskStillActive(ctx, st.taskID)
			if !known {
				// Transient task-lookup error: we can't tell if this is a
				// genuine completion or a mid-recovery execution-terminal.
				// Emitting here risks a false completion — suppress and retry
				// on the next tick (force-teardown remains the backstop).
				if idle >= n.forceTeardownAfter() {
					n.teardown(execID)
				}
				continue
			}
			if active {
				// The task isn't terminal (retry / recovery / continuation).
				// For a COMPLETED execution this could be a premature-success
				// false completion (recovery incident) — suppress it. But a
				// FAILED/CANCELLED execution must still be narrated truthfully:
				// without it the story ends on the last step's "Finished step N"
				// and reads as success (headmatch task ...9417765). Emit a
				// distinct "attempt failed — retrying" line, then tear down so
				// the retry gets a fresh story.
				if exec.Status != persistence.ExecutionStatusCompleted {
					n.emitLine(ctx, execID, st, triggerAttemptFailed,
						templateInput{Role: st.currentRole, StepIdx: st.stepIdx, StepTotal: st.stepTotal, Success: false},
						"", "", persistence.ExecutionNarrationKindCompletion)
					n.teardown(execID)
					continue
				}
				if idle >= n.forceTeardownAfter() {
					n.teardown(execID)
				}
				continue
			}
			n.emitLine(ctx, execID, st, triggerCompletion,
				templateInput{
					Role: st.currentRole, StepIdx: st.stepIdx, StepTotal: st.stepTotal,
					Success: exec.Status == persistence.ExecutionStatusCompleted,
				},
				"", "", persistence.ExecutionNarrationKindCompletion)
			n.teardown(execID)
			continue
		}
		if idle >= n.forceTeardownAfter() {
			n.teardown(execID)
		}
	}
}

// teardown removes an execution's state and notifies the optional
// test hook. All mutation happens on the Run goroutine, so no lock
// is needed; the hook lets white-box tests observe the removal
// deterministically instead of polling n.states from another
// goroutine (which would race the very state this method mutates).
func (n *Narrator) teardown(executionID string) {
	delete(n.states, executionID)
	if n.onTeardown != nil {
		n.onTeardown(executionID)
	}
}

func isTerminalStatus(s persistence.ExecutionStatus) bool {
	switch s {
	case persistence.ExecutionStatusCompleted, persistence.ExecutionStatusFailed, persistence.ExecutionStatusCancelled:
		return true
	default:
		return false
	}
}

// isTerminalTaskStatus reports whether a TASK status is a settled end-state.
// PAUSED / AWAITING_* / RUNNING / QUEUED / LEASED are all "still active".
func isTerminalTaskStatus(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
		return true
	default:
		return false
	}
}

// taskStillActive reports whether the execution's TASK is positively known to
// be non-terminal — the signal that an execution's terminal status is an
// intermediate checkpoint, not task completion. Fail-open: when the task
// getter isn't wired or a lookup errors, it returns false so the sweep keeps
// its prior "emit on execution-terminal" behavior (never suppress a real
// completion on uncertainty).
// Returns (active, known). known is false ONLY when a task lookup errored —
// a TRANSIENT failure where we cannot tell if the task is terminal, so the
// caller must NOT emit a (possibly false) completion; it retries next tick
// (review-20260716-cea0). nil-Tasks / empty-taskID / not-found are deterministic
// "not active" but KNOWN, preserving the legacy emit-on-execution-terminal
// behavior when task status can't be consulted by design.
func (n *Narrator) taskStillActive(ctx context.Context, taskID string) (active, known bool) {
	if n.Tasks == nil || taskID == "" {
		return false, true
	}
	t, err := n.Tasks.Get(ctx, taskID)
	if err != nil {
		return false, false // transient lookup error — unknown
	}
	if t == nil {
		return false, true // not found — deterministically not active
	}
	return !isTerminalTaskStatus(t.Status), true
}

// emitLine is the single choke point every trigger funnels through:
// cap checks, compose (LLM or template), PERSIST, then PUBLISH
// (design §5.3, load-bearing — never the reverse), then metrics.
func (n *Narrator) emitLine(ctx context.Context, executionID string, st *executionState, kind triggerKind, in templateInput, stepID, toolName, storageKind string) {
	if st.lineCapped {
		return // hard backstop — narration stopped entirely (design §8)
	}
	if n.projectSettings(st.projectID).NoNarration {
		// Per-project opt-out (design §9 Q3) — this project produces no
		// lines at all, checked before any composition (LLM or template) so
		// an opted-out project never pays even the template-render cost.
		return
	}
	now := n.now()
	if !st.lastLineAt.IsZero() && now.Sub(st.lastLineAt) < n.minLineInterval() {
		return // min_line_interval coalescing — dropped, not queued (story is advisory)
	}

	var text string
	var degraded bool
	if st.costCapped {
		text, degraded = fallbackTemplate(kind, in), true
	} else {
		text, degraded = n.composeLine(ctx, kind, in, stepID, toolName, st)
	}
	if text == "" {
		text, degraded = fallbackTemplate(kind, in), true
	}

	row := &persistence.ExecutionNarration{
		ID:          persistence.GenerateID("nar"),
		ProjectID:   st.projectID,
		TaskID:      st.taskID,
		ExecutionID: executionID,
		StepID:      stepID,
		Kind:        storageKind,
		Text:        text,
		Degraded:    degraded,
	}

	// PERSIST FIRST.
	seq, err := n.Store.Insert(ctx, row)
	if err != nil {
		n.Logger.Warn().Err(err).Str("execution_id", executionID).Msg("narrator: persist failed; dropping line")
		n.metricDropped("persist_error")
		return
	}
	row.Seq = seq

	st.lastLineAt = now
	st.linesEmitted++
	n.metricLine(storageKind, degraded)
	if st.linesEmitted >= n.maxLines() && !st.lineCapped {
		st.lineCapped = true
		n.metricCapped("lines")
	}

	// THEN PUBLISH — a crash between the two lines above and here
	// leaves the row in storage, absent from the live bus, never the
	// reverse (design §5.3).
	n.Pub.Publish(ctx, executionID, livepubsub.KindNarrationLine, livepubsub.NarrationLinePayload{
		Seq: seq, StepID: stepID, Kind: storageKind, Text: text, Degraded: degraded,
	})

	// Chat push (task 2.3, chatpush.go) — pushes a SUBSET of already-
	// produced lines (milestone kinds only) to the task's originating
	// chat, opt-in per project. Reuses row.Text; makes no extra LLM call.
	n.pushChatMilestone(ctx, kind, st, row)

	if n.preLine != nil {
		n.preLine(row)
	}
	if n.onLine != nil {
		n.onLine(row)
	}
}
