package narrator

import "time"

// pendingLine is the armed step-start candidate awaiting the debounce
// window (design §5.2). epoch guards against a stale timer firing
// after the pending line was cancelled (StepCompleted arrived first)
// or replaced by a newer step-start.
type pendingLine struct {
	stepID  string
	role    string
	stepIdx int
	epoch   uint64
}

// toolTimerState is the armed one-shot long-tool heartbeat timer for
// a single tool call (design §5.2 — "a single timer armed on
// ToolCallStarted and cancelled on ToolCallFinished"). epoch guards
// the same race as pendingLine's.
type toolTimerState struct {
	stepID string
	tool   string
	epoch  uint64
}

// executionState is the narrator's per-execution debounce/coalesce +
// budget bookkeeping (design §5.1 "per-execution state machine",
// §5.4 cost bound). All mutation happens on the single Run goroutine
// — timers only ever communicate back into that goroutine via
// channels, so no locking is needed.
type executionState struct {
	executionID string
	projectID   string
	taskID      string

	// stepIdx counts DISTINCT steps started so far (1-based once the
	// first StepStarted lands); stepTotal is the workflow's total
	// step count when known. No resolver is wired in this task (no
	// executor changes, and the bus doesn't carry a total), so
	// stepTotal stays 0 ("unknown") and templates render "step N"
	// without "of M" — see fallbackTemplate / stepOf.
	stepIdx   int
	stepTotal int
	seenSteps map[string]bool

	currentStepID string
	currentRole   string

	// pendingStart is the debounced step-start candidate; nil when
	// none is armed. pendingEpoch is bumped every time a new one is
	// armed so a stale timer fire is recognisable.
	pendingStart *pendingLine
	pendingEpoch uint64

	// toolTimers tracks in-flight long-tool heartbeat timers keyed
	// by call_id. toolEpoch is bumped per arm for the same stale-
	// timer defence as pendingEpoch.
	toolTimers map[string]*toolTimerState
	toolEpoch  uint64

	// lastLineAt + linesEmitted + spendUSD are the coalesce +
	// budget bookkeeping (§5.2 min_line_interval, §5.4 caps).
	// NOT crash-recovered — a narrator restart resets these
	// in-memory counters (documented accepted tradeoff, §5.4); the
	// persisted seq (store-computed) is the crash-safe half.
	lastLineAt   time.Time
	linesEmitted int
	spendUSD     float64
	// lineCapped is the hard backstop: once true, emitLine refuses
	// to produce ANY further line (not even a template) for this
	// execution (design §8: "line cap stops narration").
	lineCapped bool
	// costCapped flips narration to deterministic template-only
	// (still producing lines, just no more LLM spend) once the
	// per-execution budget is exhausted (design §8: "cost budget
	// flips to template-only").
	costCapped bool

	// lastEventAt drives the idle-poll terminal-detection sweep
	// (see narrator.go sweepIdle) — the bus carries no execution-
	// completed/failed kind (non-goal #1: no executor changes), so
	// the narrator samples ExecutionRepository.Get post-hoc once an
	// execution has gone quiet for a while, exactly the "sample the
	// audit/other stores post-hoc" fallback the design directs for
	// signals that aren't on the bus.
	lastEventAt time.Time
}

func newExecutionState(executionID, projectID, taskID string, now time.Time) *executionState {
	return &executionState{
		executionID: executionID,
		projectID:   projectID,
		taskID:      taskID,
		seenSteps:   map[string]bool{},
		toolTimers:  map[string]*toolTimerState{},
		lastEventAt: now,
	}
}
