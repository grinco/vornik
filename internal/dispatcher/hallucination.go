package dispatcher

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
)

// buildChatGroundingContext assembles a grounding context from a
// dispatcher turn's in-memory state. Distinct from
// hallucination.BuildForStep (which queries the audit DB) because
// the chat-side audit hasn't been persisted yet at the moment we
// scan — the tool messages are still in `msgs` and the detector
// runs synchronously before they hit the DB.
//
// projectIDs comes from the request's projects snapshot (already
// scoped to the user's allowlist), so the rule's negative space
// is what the user could legitimately reach.
func (a *Agent) buildChatGroundingContext(ctx context.Context, msgs []chat.Message, projectIDs []string, activeProject string) *hallucination.GroundingContext {
	gc := &hallucination.GroundingContext{
		FetchedURLs:        map[string]struct{}{},
		ArtifactNames:      map[string]struct{}{},
		KnownTaskIDs:       map[string]struct{}{},
		KnownProjectIDs:    map[string]struct{}{},
		KnownArtifactNames: map[string]struct{}{},
	}

	var toolOuts strings.Builder
	for _, m := range msgs {
		// Assistant turns carry the model-issued tool calls — name
		// and arguments. Pull arguments here so the
		// hallucinated_tool_format rule can scan them for wrappers
		// and tokenizer specials. The tool-response loop below
		// covers the response side (audit name + output text).
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if tc.Function.Arguments != "" {
					gc.ToolCallInputs = append(gc.ToolCallInputs, tc.Function.Arguments)
				}
			}
			continue
		}
		if m.Role != "tool" {
			continue
		}
		gc.ToolCallNames = append(gc.ToolCallNames, m.Name)
		toolOuts.WriteString(m.Content)
		toolOuts.WriteString("\n")
		// URL extraction from the in-memory tool outputs mirrors
		// the executor-side BuildForStep behaviour. Lower-cased
		// so the rule's membership check works.
		for _, u := range extractURLsFromText(m.Content) {
			gc.FetchedURLs[strings.ToLower(u)] = struct{}{}
		}
	}
	gc.ToolOutputs = toolOuts.String()

	for _, p := range projectIDs {
		gc.KnownProjectIDs[p] = struct{}{}
	}
	if activeProject != "" {
		gc.KnownProjectIDs[activeProject] = struct{}{}
	}

	// Hydrate KnownTaskIDs from a recent-tasks snapshot when a
	// repo is wired. The detector's task_id rule cross-references
	// this set; without it, the rule no-ops on every task ID
	// since absence-evidence isn't conclusive when the snapshot
	// is empty.
	if a.taskRepoForGrounding != nil && activeProject != "" {
		filter := persistence.TaskFilter{ProjectID: &activeProject, PageSize: 50}
		if tasks, err := a.taskRepoForGrounding.List(ctx, filter); err == nil {
			for _, t := range tasks {
				if t == nil {
					continue
				}
				gc.KnownTaskIDs[t.ID] = struct{}{}
			}
		}
	}
	return gc
}

// dispatcherURLRE mirrors the hallucination package's url
// extractor pattern. Kept as a copy rather than exposing the
// internal regex across packages — the surface cost of an
// exported extractor isn't worth saving these few bytes.
var dispatcherURLRE = regexp.MustCompile(`https?://[^\s\)<>"'\]]+`)

// extractURLsFromText pulls URLs out of arbitrary text,
// stripping trailing sentence punctuation. Used to mine the
// dispatcher turn's tool outputs (which often embed URLs in
// JSON like `"url":"https://x"`) for the grounding context's
// FetchedURLs set. Plain word-by-word splitting won't catch
// JSON-embedded URLs because they're concatenated with quotes
// and trailing fields with no whitespace.
func extractURLsFromText(text string) []string {
	matches := dispatcherURLRE.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		m = strings.TrimRight(m, `.,;:!?)"'`)
		if m == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// formatHallucinationRetryPrompt builds the synthetic user-turn
// message the dispatcher injects after detecting High signals on
// its own reply. Phrased as feedback the model can act on:
// "you said X, but X isn't grounded — call the right tool or
// remove the claim". Kept short so the retry's context budget
// isn't dominated by the retry itself.
func formatHallucinationRetryPrompt(signals []hallucination.Signal) string {
	var b strings.Builder
	b.WriteString("Your previous reply contained the following unsupported claim(s) that did not match any tool output for this turn:\n")
	count := 0
	for _, s := range signals {
		if !s.Severity.Block() {
			continue
		}
		fmt.Fprintf(&b, "  - %s: %q (%s)\n", s.ClaimType, s.ClaimValue, s.Detail)
		count++
		if count >= 5 {
			break
		}
	}
	b.WriteString("\nPlease retry: either call the appropriate tool to ground each claim, or remove the unsupported claim(s) from your answer. Do not invent IDs, URLs, or filenames.")
	return b.String()
}

// retainBlockingSignals filters signals down to the High-severity
// ones for the user-facing warning banner. The full slice is
// available in the dispatcher logs and (Phase 1) execution-side
// outcome row, but a chat reply that surfaces every Info signal
// would be noise.
func retainBlockingSignals(signals []hallucination.Signal) []hallucination.Signal {
	var out []hallucination.Signal
	for _, s := range signals {
		if s.Severity.Block() {
			out = append(out, s)
		}
	}
	return out
}

// formatUserWarningBanner is the last-resort surface for a chat
// reply that hallucinated AND retried-then-hallucinated-again.
// Prepended to the model's text so the user sees the warning
// alongside whatever the model did manage to produce. No
// ASCII-art / emoji decoration — operators have asked us
// generally to keep chat output tight.
func formatUserWarningBanner(signals []hallucination.Signal) string {
	var b strings.Builder
	b.WriteString("[hallucination warning] My previous answer contained ")
	if len(signals) == 1 {
		b.WriteString("an unsupported claim")
	} else {
		fmt.Fprintf(&b, "%d unsupported claims", len(signals))
	}
	b.WriteString(" — values I asserted but could not ground in any tool output. Treat this answer with caution.\n\n")
	return b.String()
}
