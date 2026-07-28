package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/sessionstore"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/textutil"
)

// Thread-digest bounds. Small on purpose: the block's job is to let the lead
// pick WHICH earlier thread a terse channel follow-up refers to, then pull that
// thread in full via get_channel_thread. It is a selection aid, not a
// substitute for the conversation.
const (
	maxDigestThreads = 8
	maxDigestExcerpt = 240
)

// channelPrefixFromSessionID returns the "<team>/<channel>#" prefix of a Slack
// SessionID, or "" when the id isn't the expected shape.
//
// This is the security-relevant primitive, not a formatting convenience. Both
// the digest lookup and get_channel_thread's containment check derive scope
// from it, and it must include team AND channel: comparing channel names alone
// would let a caller in one workspace read a same-named channel in another
// (two workspaces both having #general is the common case, not a corner case).
func channelPrefixFromSessionID(sessionID string) string {
	hash := strings.LastIndex(sessionID, "#")
	if hash < 0 {
		return ""
	}
	container := sessionID[:hash]
	// Require both components to be non-empty so a malformed id can never
	// produce a prefix that matches more than it should (e.g. "/" alone).
	slash := strings.Index(container, "/")
	if slash <= 0 || slash == len(container)-1 {
		return ""
	}
	return sessionID[:hash+1]
}

// threadKeyFromSessionID returns the thread component of a Slack SessionID —
// the value the lead passes back to get_channel_thread.
func threadKeyFromSessionID(sessionID string) string {
	hash := strings.LastIndex(sessionID, "#")
	if hash < 0 || hash == len(sessionID)-1 {
		return ""
	}
	return sessionID[hash+1:]
}

// threadDigest is one sibling thread rendered down to what identifies it.
type threadDigest struct {
	ThreadKey  string
	TurnCount  int
	LastActive time.Time
	Asked      string
	Answered   string
}

// deriveThreadDigest reduces a thread's stored history to its identifying
// excerpt: the first thing the human asked and the last thing the bot answered.
//
// Deterministic on purpose — recomputed from current history on every read, so
// there is no summary to regenerate, invalidate, or debounce, and no LLM call on
// the reply path. The known failure mode is a thread that wanders away from its
// opening question or opens with "hi": the excerpt then identifies the thread
// poorly. That is acceptable because the excerpt only has to be good enough to
// SELECT a thread; get_channel_thread supplies comprehension. If operators
// report the lead picking the wrong thread, the upgrade path is an
// LLM-authored digest that supersedes this (the codebase already has the
// pattern in api.Server.SummarizeThread, where the agent supplies the text).
func deriveThreadDigest(sessionID string, history []chat.Message, updatedAt time.Time) threadDigest {
	d := threadDigest{
		ThreadKey:  threadKeyFromSessionID(sessionID),
		LastActive: updatedAt,
	}
	for _, m := range history {
		switch m.Role {
		case "user":
			d.TurnCount++
			if d.Asked == "" {
				d.Asked = digestExcerpt(m.Content)
			}
		case "assistant":
			d.TurnCount++
			if text := digestExcerpt(m.Content); text != "" {
				// Keep overwriting: the LAST assistant turn is the answer the
				// human is most likely following up on.
				d.Answered = text
			}
		}
	}
	return d
}

// digestExcerpt flattens a message to a single bounded line. Newlines collapse
// to " · " so a multi-paragraph answer stays one visually distinct entry rather
// than breaking the block's structure.
func digestExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if trimmed := strings.TrimSpace(f); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return textutil.TruncateRunes(strings.Join(parts, " · "), maxDigestExcerpt)
}

// renderThreadDigests builds the system-prompt block describing what was
// discussed in this channel's other threads. Returns "" when there is nothing
// worth saying, so the caller appends nothing and the prompt stays unchanged.
//
// Injected into the system prompt rather than into stored history: history is
// persisted and replayed, so a digest written there would compound every turn
// and would still be describing threads as they looked at write time.
func renderThreadDigests(digests []threadDigest) string {
	usable := make([]threadDigest, 0, len(digests))
	for _, d := range digests {
		if d.ThreadKey == "" || d.TurnCount == 0 {
			continue
		}
		if d.Asked == "" && d.Answered == "" {
			continue
		}
		usable = append(usable, d)
	}
	if len(usable) == 0 {
		return ""
	}
	// Most recent first — a follow-up almost always refers to recent activity.
	sort.SliceStable(usable, func(i, j int) bool {
		return usable[i].LastActive.After(usable[j].LastActive)
	})
	if len(usable) > maxDigestThreads {
		usable = usable[:maxDigestThreads]
	}

	var b strings.Builder
	b.WriteString("\n\nRECENT THREADS IN THIS CHANNEL\n")
	b.WriteString("  Threads are hard to find in Slack, so people follow up here in the\n")
	b.WriteString("  channel instead of in the thread. These are the channel's recent\n")
	b.WriteString("  threads — they are YOUR earlier conversations, not a third party's.\n")
	b.WriteString("  When a message here refers to something \"you said\" or asks a terse\n")
	b.WriteString("  follow-up, resolve it against these before asking what they mean.\n")
	b.WriteString("  Excerpts are truncated; call get_channel_thread(thread_key=...) for\n")
	b.WriteString("  a thread's full text.\n")
	for _, d := range usable {
		fmt.Fprintf(&b, "\n  [thread_key=%s · %d turns · %s]\n",
			d.ThreadKey, d.TurnCount, d.LastActive.UTC().Format("2006-01-02"))
		if d.Asked != "" {
			fmt.Fprintf(&b, "    asked: %s\n", d.Asked)
		}
		if d.Answered != "" {
			fmt.Fprintf(&b, "    answered: %s\n", d.Answered)
		}
	}
	return b.String()
}

// digestsForChannel turns a prefix listing into digests, excluding the calling
// session itself and any non-thread sibling (another channel-scoped session, a
// slash invocation) — those are not threads and would be self-referential or
// meaningless as "what was discussed in a thread".
func digestsForChannel(siblings []sessionstore.SiblingSession, callerSessionID string) []threadDigest {
	out := make([]threadDigest, 0, len(siblings))
	for _, s := range siblings {
		if s.SessionID == callerSessionID {
			continue
		}
		key := threadKeyFromSessionID(s.SessionID)
		if key == "" || key == slack.ChannelSessionThreadRoot || strings.HasPrefix(key, "slash:") {
			continue
		}
		out = append(out, deriveThreadDigest(s.SessionID, s.History, s.UpdatedAt))
	}
	return out
}

// slackThreadReaderSet fans a get_channel_thread lookup across every wired
// Slack session store, returning the first hit.
//
// Each store's in-memory map holds only its own channel's threads, so a single
// store cannot answer for a multi-workspace deployment. The durable rows are
// kind-scoped rather than project-scoped, so any store resolves those
// identically — the fan-out exists for the in-process half.
//
// This does NOT widen access: the caller's container prefix is enforced in the
// tool before it ever reaches a reader, so a lookup that arrives here is
// already known to name the caller's own channel.
type slackThreadReaderSet []*slackSessionStore

func (set slackThreadReaderSet) ReadThread(ctx context.Context, sessionID string) ([]chat.Message, error) {
	var firstErr error
	for _, store := range set {
		history, err := store.ReadThread(ctx, sessionID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(history) > 0 {
			return history, nil
		}
	}
	// Not found anywhere: report a miss, not an error, so the tool can tell the
	// lead the thread aged out rather than surfacing a transport failure.
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, nil
}
