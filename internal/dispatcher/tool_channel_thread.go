package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/textutil"
)

// getChannelThreadName is the tool the lead calls to read one of its own
// earlier conversations in the same chat container (a Slack thread in the same
// channel).
const getChannelThreadName = "get_channel_thread"

// maxChannelThreadBytes bounds the rendered transcript. A thread that exceeds it
// is truncated from the FRONT — the tail carries the conclusion, which is what a
// follow-up is almost always about.
const maxChannelThreadBytes = 8000

// ChannelThreadReader reads the stored history of a sibling conversation in the
// same channel. Implemented by the service container over the channel session
// store; nil disables the tool.
//
// The implementation is responsible ONLY for lookup. Access scoping is enforced
// here in the tool (see getChannelThread) so a future reader implementation
// cannot accidentally widen it.
type ChannelThreadReader interface {
	// ReadThread returns the stored history for sessionID, or nil when the
	// session is unknown.
	ReadThread(ctx context.Context, sessionID string) ([]chat.Message, error)
}

// getChannelThread returns the transcript of a sibling thread in the caller's
// own channel.
//
// Containment: the requested thread is resolved against the CALLER's session id
// prefix — "<team>/<channel>#" — never against a channel name alone. Two
// workspaces routinely both have a #general, so comparing channel names would
// let a lead in one workspace read the other's conversation. The caller's
// session comes from the request context, not from tool arguments, so the model
// cannot nominate its own scope.
func (te *ToolExecutor) getChannelThread(ctx context.Context, argsJSON string) ToolResult {
	var args struct {
		ThreadKey string `json:"thread_key"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	threadKey := strings.TrimSpace(args.ThreadKey)
	if threadKey == "" {
		return ToolResult{Content: "thread_key is required (take it from the RECENT THREADS block)."}
	}
	if te.channelThreads == nil {
		return ToolResult{Content: "Channel thread history is not available on this deployment."}
	}

	_, callerSession := originatingChannelFromContext(ctx)
	prefix := chatContainerPrefix(callerSession)
	if prefix == "" {
		return ToolResult{Content: "This conversation has no channel context, so other threads cannot be resolved."}
	}

	// Accept either a bare thread key or a fully-qualified session id, but a
	// qualified id must sit inside the caller's own container.
	target := threadKey
	if strings.Contains(threadKey, "#") {
		if !strings.HasPrefix(threadKey, prefix) {
			return ToolResult{Content: "That thread belongs to a different channel or workspace; it cannot be read from here."}
		}
	} else {
		target = prefix + threadKey
	}
	if target == callerSession {
		return ToolResult{Content: "That is the current conversation — its history is already in context."}
	}

	history, err := te.channelThreads.ReadThread(ctx, target)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Could not read that thread: %v", err)}
	}
	if len(history) == 0 {
		return ToolResult{Content: fmt.Sprintf("No stored history for thread %q. It may have aged out; ask the user to restate what they need.", threadKey)}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Transcript of thread %s (%d messages):\n\n", threadKey, len(history))
	for _, m := range history {
		role := m.Role
		switch role {
		case "user":
			role = "them"
		case "assistant":
			role = "you"
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", role, content)
	}
	out := b.String()
	if len(out) > maxChannelThreadBytes {
		// Keep the tail: the conclusion is what a follow-up refers to.
		out = "…(earlier messages truncated)…\n" +
			textutil.TruncateRunes(out[len(out)-maxChannelThreadBytes:], maxChannelThreadBytes)
	}
	return ToolResult{Content: out}
}

// chatContainerPrefix returns the container portion of a session id up to and
// including the separator — for Slack, "<team>/<channel>#".
//
// Returns "" for any id that isn't that shape, which fails the containment
// check closed: a channel whose session ids carry no container (webchat cookie
// hashes, email message-ids) gets no cross-thread access at all rather than an
// accidentally-wide one.
func chatContainerPrefix(sessionID string) string {
	hash := strings.LastIndex(sessionID, "#")
	if hash < 0 {
		return ""
	}
	container := sessionID[:hash]
	slash := strings.Index(container, "/")
	if slash <= 0 || slash == len(container)-1 {
		return ""
	}
	return sessionID[:hash+1]
}
