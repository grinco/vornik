// Package chat provides an OpenAI-compatible chat client for vornik.
package chat

import (
	"errors"
	"sync"
	"time"
)

// Compactor condenses a slice of overflow turns (the ones that don't fit the
// LLM payload budget) into a short gist line. Implementations live outside
// internal/chat (e.g. internal/memory's deterministic term-frequency gist) so
// this package stays dependency-free. A nil Compactor on a Conversation means
// legacy behavior: AddMessage token-trims and MessagesForLLM == GetMessages.
type Compactor interface {
	// Gist returns a compact summary of msgs, or "" if there's nothing
	// useful to say. The result is bounded by the implementation; the
	// Conversation truncates defensively to compactionGistReserveChars.
	Gist(msgs []Message) string
}

// compactionMarkerPrefix tags the synthetic gist message inserted by
// MessagesForLLM. It is honest about omission (review-20260623-df8c finding
// #3): the model/operator sees a lossy topic residue, not implied fidelity.
const compactionMarkerPrefix = "[earlier conversation summarized]"

// compactionGistReserveChars bounds the gist message and the budget headroom
// reserved for it (≈128 tokens at chars/4).
const compactionGistReserveChars = 512

// Conversation manages message history.
type Conversation struct {
	mu         sync.RWMutex
	id         string
	messages   []Message
	pinned     []Message // persist across Clear/reset
	maxHistory int       // hard upper bound on message count
	maxTokens  int       // soft token budget for trim; 0 disables token-aware trim
	compactor  Compactor // nil = legacy drop-only; set = read-path compaction
	createdAt  time.Time
	lastUsedAt time.Time
}

// NewConversation creates a new conversation with the given ID and maximum history size.
func NewConversation(id string, maxHistory int) *Conversation {
	if maxHistory <= 0 {
		maxHistory = 100 // default
	}
	return &Conversation{
		id:         id,
		messages:   make([]Message, 0),
		maxHistory: maxHistory,
		createdAt:  time.Now(),
	}
}

// SetMaxTokens sets the soft token budget for conversation history. When
// set (>0), AddMessage drops whole turns from the front until the estimated
// token count fits within the budget. This guards against a bloated payload
// (e.g. large tool results accumulated over many turns) overflowing the
// upstream gateway's context window. Zero disables token-aware trimming
// and leaves only the message-count cap.
//
// Trimming uses the same chars/4 estimator as EstimateTokens — a rough
// approximation, deliberately conservative. Budget should be sized below
// the model's context limit with headroom for the system prompt, tools,
// and the response itself (typically 60-70% of context_size).
func (c *Conversation) SetMaxTokens(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxTokens = n
}

// AddMessage adds a message to the conversation history.
// When the history exceeds maxHistory or maxTokens, whole turns are removed
// from the front (a turn starts at a user message) so tool-call pairs are
// never split. Splitting a turn produces orphaned tool messages that break
// OpenAI-compatible APIs and confuse the LLM about which project context
// is active.
//
// The most recent turn is never dropped, even if it alone exceeds the token
// budget — dropping it would erase the current user question the caller is
// trying to answer. In that case the upstream gateway may still reject the
// oversized payload; the retry-with-prune logic in the dispatcher is the
// backstop for that.
func (c *Conversation) AddMessage(msg Message) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, msg)
	c.lastUsedAt = time.Now()

	for c.exceedsLimitsLocked() {
		nextTurn := c.nextTurnStartLocked()
		if nextTurn < 0 {
			// Only one turn remains — refuse to drop it.
			return
		}
		c.messages = c.messages[nextTurn:]
	}
}

// nextTurnStartLocked returns the index of the second user message in
// c.messages (i.e. the start of the turn *after* the first one), or -1 if
// there is no second turn yet. Caller must hold c.mu.
func (c *Conversation) nextTurnStartLocked() int {
	seenFirstUser := false
	for i, m := range c.messages {
		if m.Role != "user" {
			continue
		}
		if !seenFirstUser {
			seenFirstUser = true
			continue
		}
		return i
	}
	return -1
}

// exceedsLimitsLocked returns true when either the message-count or
// token-budget limit is exceeded. Caller must hold c.mu.
//
// When a compactor is wired, the token budget is NOT enforced here: raw
// history is retained (bounded only by the message-count cap) so the read
// path (MessagesForLLM) has older turns to compact into a gist rather than
// dropping them blind. Without a compactor, behavior is unchanged.
func (c *Conversation) exceedsLimitsLocked() bool {
	if len(c.messages) > c.maxHistory {
		return true
	}
	if c.compactor == nil && c.maxTokens > 0 && c.estimateTokensLocked() > c.maxTokens {
		return true
	}
	return false
}

// SetCompactor wires a read-path compactor. With one set, AddMessage stops
// token-trimming (count cap still bounds storage) and MessagesForLLM derives a
// budget-fitting, gist-prefixed payload. Passing nil restores legacy behavior.
func (c *Conversation) SetCompactor(cp Compactor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compactor = cp
}

// MessagesForLLM returns the payload to send upstream. With no compactor (or no
// token budget) it equals GetMessages(). With a compactor and a token budget,
// when raw history exceeds the budget it returns:
//
//	pinned + [single gist of the older overflow turns] + most-recent turns that fit
//
// The most recent turn is always retained (even if it alone exceeds budget).
// The gist is recomputed fresh from raw history on every call — it is never
// persisted and never mutated, so there is no TOCTOU, no persistence desync,
// and no summary-of-summary drift (review-20260623-df8c findings #2, #4, #5).
func (c *Conversation) MessagesForLLM() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Active messages with the same leading-orphan trim GetMessages applies.
	msgs := c.messages
	for len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = msgs[1:]
	}

	assemble := func(gist *Message, kept []Message) []Message {
		out := make([]Message, 0, len(c.pinned)+len(kept)+1)
		out = append(out, c.pinned...)
		if gist != nil {
			out = append(out, *gist)
		}
		out = append(out, kept...)
		return out
	}

	// Fast path: legacy behavior when no compactor / no budget, or when
	// everything already fits.
	pinnedChars := messagesChars(c.pinned)
	budgetChars := c.maxTokens * 4
	if c.compactor == nil || c.maxTokens <= 0 || pinnedChars+messagesChars(msgs) <= budgetChars {
		return assemble(nil, msgs)
	}

	// Find the largest suffix of whole turns that fits the budget (less the
	// pinned cost and a fixed reserve for the gist). Turns start at "user".
	starts := make([]int, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == "user" {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		// No turn boundary to split on — send as-is rather than gist nothing.
		return assemble(nil, msgs)
	}

	avail := budgetChars - pinnedChars - compactionGistReserveChars
	keepFrom := starts[len(starts)-1] // fallback: newest turn only
	for _, st := range starts {       // earliest first → largest fitting suffix
		if messagesChars(msgs[st:]) <= avail {
			keepFrom = st
			break
		}
	}

	dropped := msgs[:keepFrom]
	kept := msgs[keepFrom:]
	if len(dropped) == 0 {
		return assemble(nil, kept)
	}

	gistText := c.compactor.Gist(dropped)
	if gistText == "" {
		// Compactor had nothing to say — drop silently rather than emit an
		// empty marker (still better-bounded than legacy, payload-wise).
		return assemble(nil, kept)
	}
	content := compactionMarkerPrefix + " " + gistText
	if len(content) > compactionGistReserveChars {
		content = content[:compactionGistReserveChars]
	}
	gist := Message{Role: "system", Content: content}
	return assemble(&gist, kept)
}

// messagesChars sums the approximate character cost of a message slice.
func messagesChars(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += messageTokenChars(m)
	}
	return total
}

// estimateTokensLocked returns the rough token count (chars/4). Caller
// must hold c.mu. Pinned messages are included because they are part of
// the final payload sent to the LLM. Image blocks add a fixed budget
// per image — vision-token costs vary by detail and provider, but a
// flat 800-token estimate covers the typical "low" detail tile (256
// tokens) up to a small "high" detail image without underestimating
// catastrophically. Better to overshoot than to bust the context.
func (c *Conversation) estimateTokensLocked() int {
	total := 0
	for _, m := range c.pinned {
		total += messageTokenChars(m)
	}
	for _, m := range c.messages {
		total += messageTokenChars(m)
	}
	return total / 4
}

// messageTokenChars approximates the character cost of a single message
// for estimateTokensLocked. Returns chars (caller divides by 4).
func messageTokenChars(m Message) int {
	if len(m.Blocks) == 0 {
		return len(m.Content)
	}
	chars := 0
	for _, b := range m.Blocks {
		switch b.Type {
		case "text":
			chars += len(b.Text)
		case "image_url":
			// Flat per-image budget — see comment on estimateTokensLocked.
			chars += 3200 // ≈ 800 tokens at chars/4
		}
	}
	return chars
}

// GetMessages returns pinned messages followed by conversation messages.
// Leading orphaned tool/assistant messages (without a preceding user turn) are
// silently dropped — they can appear in disk-restored sessions or after a mid-turn
// crash and would cause API validation failures if sent to the LLM.
func (c *Conversation) GetMessages() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	msgs := c.messages
	for len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = msgs[1:]
	}

	messages := make([]Message, 0, len(c.pinned)+len(msgs))
	messages = append(messages, c.pinned...)
	messages = append(messages, msgs...)
	return messages
}

// Clear removes all messages from the conversation (pinned messages are kept).
func (c *Conversation) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = make([]Message, 0)
	c.createdAt = time.Now()
}

// ID returns the conversation ID.
func (c *Conversation) ID() string {
	return c.id
}

// Len returns the number of messages in the conversation.
func (c *Conversation) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.messages)
}

// LastMessage returns the most recent message, or an error if the conversation is empty.
func (c *Conversation) LastMessage() (Message, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.messages) == 0 {
		return Message{}, errors.New("conversation is empty")
	}
	return c.messages[len(c.messages)-1], nil
}

// CreatedAt returns when the conversation was created.
func (c *Conversation) CreatedAt() time.Time {
	return c.createdAt
}

// LastUsed returns the time of the most recent AddMessage call, or CreatedAt if
// no messages have been added yet.
func (c *Conversation) LastUsed() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastUsedAt.IsZero() {
		return c.createdAt
	}
	return c.lastUsedAt
}

// ReplaceMessages replaces the conversation history with the given slice.
// Used by /summarize to compact history into a single summary message.
func (c *Conversation) ReplaceMessages(msgs []Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = make([]Message, len(msgs))
	copy(c.messages, msgs)
	c.lastUsedAt = time.Now()
}

// DropLast removes the last n messages from history.
func (c *Conversation) DropLast(n int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n <= 0 {
		return 0
	}
	if n > len(c.messages) {
		n = len(c.messages)
	}
	c.messages = c.messages[:len(c.messages)-n]
	return n
}

// Undo removes the last user message and its assistant response (if any).
// Returns the number of messages removed.
func (c *Conversation) Undo() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	// Remove trailing assistant/tool messages
	for len(c.messages) > 0 {
		last := c.messages[len(c.messages)-1]
		if last.Role == "user" {
			c.messages = c.messages[:len(c.messages)-1]
			removed++
			break
		}
		c.messages = c.messages[:len(c.messages)-1]
		removed++
	}
	return removed
}

// Pin adds a message that persists across Clear/reset.
// Pinned messages are prepended to GetMessages output.
func (c *Conversation) Pin(msg Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned = append(c.pinned, msg)
}

// PinnedMessages returns the pinned messages.
func (c *Conversation) PinnedMessages() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Message, len(c.pinned))
	copy(out, c.pinned)
	return out
}

// EstimateTokens returns a rough token estimate (chars/4).
func (c *Conversation) EstimateTokens() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.estimateTokensLocked()
}
