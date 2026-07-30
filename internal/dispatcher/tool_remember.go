package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SLICE 1 of the chat memory-write design
// (https://docs.vornik.io), after three review
// rounds. This slice is the HARD GATE and nothing else — there is deliberately NO write
// path yet, because the gate has to exist before anything can write.
//
// WHY THE GATE IS THE DESIGN. Review round 1 rejected the original proposal for offering
// downranking as the injection mitigation: "The difference between 'never writes' and
// 'writes with low weight' is not a security boundary — it's a ranking problem with an
// adversarial optimizer on the input side." A chat turn is untrusted input, and once it is
// in project memory it enters the lead's prompt and every future task, outliving the
// conversation that carried it. §5.1's channel-level opt-in is the boundary that replaced
// the ranking story.
//
// WHAT THE GATE DOES NOT BUY, per review 2: authorization containment, not blast-radius
// containment. A misleading `remember` inside a granted channel still writes something that
// persists until per-data-subject Art 17 deletion. That is accepted — the write was
// authorized. The gate bounds WHO may write, never WHAT a permitted writer may say.

// MemoryWriteGate answers whether a given channel session has been granted the capability
// to write durable project memory.
//
// Deny-by-default at both levels: a nil gate means the capability is absent from this
// deployment, and a gate that returns false means this channel was never granted it. The
// two are indistinguishable to the caller on purpose (see rememberUnavailable).
type MemoryWriteGate interface {
	CanWriteMemory(ctx context.Context, channel, sessionID string) bool
}

// SetMemoryWriteGate wires the capability check.
//
// Writes through to the tool executor as well: NewAgent builds a.toolExecutor once, so a
// setter that assigned only the agent field would leave the executor holding nil — the bug
// that made the chat bug-report tool permanently dark (fixed 1234aea37, found in
// production rather than in tests).
func (a *Agent) SetMemoryWriteGate(g MemoryWriteGate) {
	if a == nil {
		return
	}
	a.memoryWrite = g
	if a.toolExecutor != nil {
		a.toolExecutor.memoryWrite = g
	}
}

// rememberUnavailable is the single refusal text.
//
// IDENTICAL whether the capability is absent or merely ungranted for this channel (review
// 2 minor). Two different messages would form a capability-existence oracle: a caller could
// learn which deployments have the feature configured but withheld. It also matches the
// shape `get_channel_thread` already uses, so the surface reads consistently.
const rememberUnavailable = "Saving to memory is not available on this deployment."

type rememberArgs struct {
	Content string `json:"content"`
	// Scope is the model's REQUEST for where the fact goes. Not authoritative — see
	// resolveMemoryScope: anything unrecognised, including silence, is personal.
	Scope string `json:"scope"`
}

// remember is the chat-facing memory-write tool.
//
// SLICE 1 BEHAVIOUR: gate, validate, then say plainly that the write path is not built.
// It deliberately does NOT accept-and-discard — a tool that silently swallows the content
// is exactly the "it said it would remember and nothing happened" report that started this
// work, and shipping that shape again while the write path is pending would recreate the
// original bug with extra steps.
func (te *ToolExecutor) remember(ctx context.Context, args string) ToolResult {
	channel, sessionID := originatingChannelFromContext(ctx)

	// No originating channel (API, A2A, an autonomy tick) means there is no channel whose
	// grant could be checked. Refuse rather than fall through to a default.
	if te.memoryWrite == nil || channel == "" || sessionID == "" {
		return ToolResult{Content: rememberUnavailable}
	}
	if !te.memoryWrite.CanWriteMemory(ctx, channel, sessionID) {
		return ToolResult{Content: rememberUnavailable}
	}

	var in rememberArgs
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return ToolResult{Content: fmt.Sprintf(
				"remember: could not parse arguments (%v). Pass the `content` to remember.", err)}
		}
	}
	if strings.TrimSpace(in.Content) == "" {
		return ToolResult{Content: "remember needs `content`: the fact to keep."}
	}

	scope := resolveMemoryScope(in.Scope)

	// Slice 2 ends here. Remaining: the confirmation record a shared-scope write cannot
	// commit without (§5.3), named-entity extraction pre-commit with medium confidence
	// blocking (§6.1, §6.2), and class registration plus TTL-deletes verification from the
	// first-write checklist (§9).
	//
	// The resolved scope is reported so the model can tell the user WHERE the fact would
	// go. Without that, someone learns their scope was wrong only when a colleague reads it.
	where := "your personal profile (only you)"
	if scope == memoryScopeShared {
		where = "shared project memory (everyone in this project)"
	}
	return ToolResult{Content: fmt.Sprintf(
		"Scope resolved to %s — it would go to %s. The save path is NOT implemented yet, so "+
			"nothing has been kept. Tell the user plainly that you could not save it, and "+
			"which scope you would have used, rather than implying you did.", scope, where)}
}

// WithCallSiteForTest sets the originating channel + session on a context so tests can
// exercise channel-scoped tools without reaching into the unexported context key.
//
// Test-only helper kept beside the production code it serves, rather than duplicated in
// each test file that needs it.
func WithCallSiteForTest(ctx context.Context, channel, sessionID string) context.Context {
	return withOriginatingChannel(ctx, channel, sessionID)
}
