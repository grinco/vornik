package api

import (
	"sync"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// The daemon's record of how an MCP tool call actually ended, held between the
// moment the daemon executes it (CallMCPTool) and the moment the agent posts
// its audit row (IngestToolAudit).
//
// WHY A BUFFER AT ALL. tool_audit_log rows are written by the AGENT, which is
// exactly the wrong authority for this question: an agent that mis-narrates a
// connector failure IS the failure mode — the 2026-08-25 P0 was a task that
// completed cleanly while describing a 401 in its own result payload. But the
// daemon executes the MCP call itself, so the daemon holds the truth. This
// carries that truth the short distance from the call to the row.
//
// The trust rule it implements: a daemon-observed class ALWAYS wins; the
// agent's own report is used only for a tool the daemon did not execute (a
// container-local tool), where we have no observation of our own and say so
// rather than claiming an authority we do not have.
//
// Design: https://docs.vornik.io §3.2
type toolOutcomeBuffer struct {
	mu      sync.Mutex
	entries map[toolOutcomeKey][]toolOutcome
	// maxAge bounds how long an unclaimed observation is worth keeping. An
	// agent posts its audit row within milliseconds of the call returning; an
	// observation older than this belongs to a call whose row never arrived
	// (a crashed agent), and keeping it risks stamping it onto an unrelated
	// later call of the same tool.
	maxAge time.Duration
	// lastSweep is when the whole map was last walked for expired keys. See
	// sweepExpiredLocked: pruneLocked only ever touches the key being
	// written or read, so without this walk an observation whose audit row
	// never arrived is never revisited and its key is never freed.
	lastSweep time.Time
	// maxPerKey bounds memory for a step that calls one tool in a tight loop
	// while its audit posts are failing. Oldest observations are dropped
	// first, because the newest is the one most likely to be claimed next.
	maxPerKey int
	now       func() time.Time
}

type toolOutcomeKey struct {
	executionID string
	toolName    string
}

type toolOutcome struct {
	outcome string
	class   mcp.FailureClass
	at      time.Time
}

const (
	defaultToolOutcomeMaxAge = 5 * time.Minute
	// toolOutcomeSweepInterval bounds how often the whole map is walked for
	// expired keys. Nothing can expire faster than maxAge, so sweeping more
	// often than that only makes a hot path O(n) for no gain.
	toolOutcomeSweepInterval    = defaultToolOutcomeMaxAge
	defaultToolOutcomeMaxPerKey = 64
)

func newToolOutcomeBuffer() *toolOutcomeBuffer {
	return &toolOutcomeBuffer{
		entries:   make(map[toolOutcomeKey][]toolOutcome),
		maxAge:    defaultToolOutcomeMaxAge,
		maxPerKey: defaultToolOutcomeMaxPerKey,
		now:       time.Now,
	}
}

// Record stores what the daemon observed for one executed MCP tool call.
//
// executionID may be empty — an operator-driven tool call from the console has
// no execution — in which case there is nothing to correlate against and the
// observation is dropped rather than filed under a key that can never match.
func (b *toolOutcomeBuffer) Record(executionID, toolName string, err error) {
	if b == nil || executionID == "" || toolName == "" {
		return
	}
	out := toolOutcome{outcome: "ok", at: b.now()}
	if err != nil {
		out.outcome = "error"
		// Unclassified failures stay unclassified. Guessing a class from the
		// message is the text-sniffing this design exists to remove; "error
		// with no class" is an honest answer and the doctor check ignores it
		// rather than counting it as auth.
		if class, ok := mcp.ClassOf(err); ok {
			out.class = class
		}
	}

	key := toolOutcomeKey{executionID: executionID, toolName: toolName}
	b.mu.Lock()
	defer b.mu.Unlock()
	list := append(b.pruneLocked(key), out)
	if len(list) > b.maxPerKey {
		list = list[len(list)-b.maxPerKey:]
	}
	b.entries[key] = list
	// Free the keys nobody will ever claim. Rate-limited to once per maxAge,
	// so this is not an O(n) walk on a hot path — see sweepExpiredLocked for
	// why eviction rides Record rather than an execution's terminal state.
	b.sweepExpiredLocked(out.at)
}

// Claim removes and returns the OLDEST unclaimed observation for a call.
//
// Oldest-first because audit rows arrive in call order: the first row the agent
// posts for a tool corresponds to the first call the daemon made with it. A
// mismatch is self-correcting — every observation is for a real call of that
// tool in that execution, so the worst case is attributing the right class to
// the wrong one of two adjacent identical calls.
//
// Returns ok=false when the daemon has no observation, which is the honest
// answer for a container-local tool and tells the caller to fall back to what
// the agent reported.
func (b *toolOutcomeBuffer) Claim(executionID, toolName string) (outcome string, class mcp.FailureClass, ok bool) {
	if b == nil || executionID == "" || toolName == "" {
		return "", "", false
	}
	key := toolOutcomeKey{executionID: executionID, toolName: toolName}
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.pruneLocked(key)
	if len(list) == 0 {
		delete(b.entries, key)
		return "", "", false
	}
	head := list[0]
	if rest := list[1:]; len(rest) == 0 {
		delete(b.entries, key)
	} else {
		b.entries[key] = rest
	}
	return head.outcome, head.class, true
}

// pruneLocked drops observations older than maxAge. Called on both paths so
// the map cannot grow without bound in a daemon whose agents never post.
func (b *toolOutcomeBuffer) pruneLocked(key toolOutcomeKey) []toolOutcome {
	list := b.entries[key]
	if len(list) == 0 {
		return nil
	}
	cutoff := b.now().Add(-b.maxAge)
	i := 0
	for i < len(list) && list[i].at.Before(cutoff) {
		i++
	}
	return list[i:]
}

// sweepExpiredLocked drops every key whose observations are all older than
// maxAge. Caller holds b.mu.
//
// THIS REPLACES A `Forget(executionID)` THAT NOTHING CALLED. Its doc comment
// said it was "called when an execution reaches a terminal state", and no call
// site ever existed (grep-verified across internal/ and cmd/ on 2026-09-04) —
// a documented contract with no implementation, which is the shape the
// 2026-08-26 silent-controls audit is about. The leak it was meant to close
// was real: pruneLocked only runs on a Record or Claim touching the SAME key,
// so an observation whose matching agent audit row never arrives (agent crash
// mid-step, audit POST failure, task killed between the MCP call and the audit
// flush) survived for the life of the daemon.
//
// Age, not terminality, is the right trigger. The buffer already defines when
// an observation is dead — maxAge, five minutes, against an agent that posts
// its audit row within milliseconds — and the API layer, which owns this
// buffer, does not observe when an execution ends. Riding Record also means a
// daemon that has stopped making tool calls has stopped growing, so there is
// no goroutine to start, own, or shut down.
func (b *toolOutcomeBuffer) sweepExpiredLocked(now time.Time) {
	if now.Sub(b.lastSweep) < toolOutcomeSweepInterval {
		return
	}
	b.lastSweep = now
	cutoff := now.Add(-b.maxAge)
	for key, list := range b.entries {
		if len(list) == 0 || !list[len(list)-1].at.After(cutoff) {
			// The newest observation for this key is at or past the cutoff, so
			// every one of them is: nothing here can be claimed any more.
			delete(b.entries, key)
		}
	}
}
