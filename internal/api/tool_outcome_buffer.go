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
	defaultToolOutcomeMaxAge    = 5 * time.Minute
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

// Forget drops every observation for an execution. Called when an execution
// reaches a terminal state so a long-lived daemon does not accumulate one map
// entry per tool per finished task.
func (b *toolOutcomeBuffer) Forget(executionID string) {
	if b == nil || executionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.entries {
		if key.executionID == executionID {
			delete(b.entries, key)
		}
	}
}
