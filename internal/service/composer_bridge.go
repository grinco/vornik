package service

// composerBridge adapts a *projectwizard.Wizard to the dispatcher's
// narrow dispatcher.ComposerBridge seam (task 1.4; design
// https://docs.vornik.io §5.7 Phase
// 4: "The dispatcher gets a compose_automation tool that opens a
// composer session ... the plan renders as text, the graph as a
// link.").
//
// Thin by design: every synthesis / guardrail / validation / commit
// decision stays inside the Wizard, reached in-process — this
// adapter only (a) maps a chat conversation onto a wizard session id
// (open-or-continue) and (b) renders the returned envelope into chat
// text + a link to that SAME session's web preview, where the
// operator reviews Graph/YAML and commits (v1 never commits from
// chat — see tool_compose_automation.go's package doc in
// internal/dispatcher).
import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/projectwizard"
)

// composerBridge implements dispatcher.ComposerBridge over a
// *projectwizard.Wizard.
type composerBridge struct {
	wizard  *projectwizard.Wizard
	baseURL string // externally-reachable web-UI origin, trimmed; "" = relative links

	mu       sync.Mutex
	sessions map[string]string // "<operatorID>|<conversationID>" -> wizard session id
}

// newComposerBridge builds the dispatcher-facing composer bridge over
// wiz. Returns a nil interface (not a typed-nil *composerBridge) when
// wiz is nil, so dispatcher.Agent.SetComposerBridge sees a clean "not
// wired" signal and the compose_automation tool disables itself
// rather than panicking on a nil-but-non-nil-interface receiver.
// baseURL is the daemon's externally-reachable web-UI origin (empty
// falls back to relative links, still usable from webchat).
func newComposerBridge(wiz *projectwizard.Wizard, baseURL string) dispatcher.ComposerBridge {
	if wiz == nil {
		return nil
	}
	return &composerBridge{
		wizard:   wiz,
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		sessions: map[string]string{},
	}
}

// ComposeTurn implements dispatcher.ComposerBridge.
func (b *composerBridge) ComposeTurn(ctx context.Context, operatorID, conversationID, message string) (string, string, bool, error) {
	if b == nil || b.wizard == nil {
		return "", "", true, errors.New("composer bridge not wired")
	}
	key := b.sessionKey(operatorID, conversationID)

	b.mu.Lock()
	sessionID := b.sessions[key]
	b.mu.Unlock()

	res, err := b.wizard.Converse(ctx, sessionID, operatorID, message)
	if sessionID != "" && (errors.Is(err, projectwizard.ErrSessionCommitted) || errors.Is(err, projectwizard.ErrSessionCancelled)) {
		// The mapped session is done — committed via the web UI, or
		// cancelled. Drop the stale mapping and transparently start a
		// fresh draft rather than wedging this conversation forever.
		b.mu.Lock()
		delete(b.sessions, key)
		b.mu.Unlock()
		res, err = b.wizard.Converse(ctx, "", operatorID, message)
	}
	switch {
	case errors.Is(err, projectwizard.ErrTooManySessions):
		return "You already have several automation drafts open. Finish or cancel one from the web UI's projects page before starting another.", "", true, nil
	case errors.Is(err, projectwizard.ErrTurnsExhausted):
		return "This automation conversation has reached its turn limit — describe the automation again and I'll open a fresh draft.", "", true, nil
	case err != nil:
		return "", "", true, err
	}
	if res == nil || res.Envelope == nil {
		return "", "", true, errors.New("composer returned no result")
	}

	b.mu.Lock()
	b.sessions[key] = res.SessionID
	b.mu.Unlock()

	env := res.Envelope
	if env.Tier == 3 && env.Bundle != nil {
		return formatComposedPlanText(env.Message, env.Bundle.Plan), b.previewURL(res.SessionID), false, nil
	}
	return formatClarifyingText(env.Message, env.OpenQuestions), "", true, nil
}

func (b *composerBridge) sessionKey(operatorID, conversationID string) string {
	return operatorID + "|" + conversationID
}

func (b *composerBridge) previewURL(sessionID string) string {
	path := "/ui/projects/new/wizard?session=" + sessionID
	if b.baseURL == "" {
		return path
	}
	return b.baseURL + path
}

// formatComposedPlanText renders a tier-3 ComposedPlan as chat text —
// numbered steps, human-readable schedule, a cost estimate explicitly
// labelled as an estimate, human-approval points, and any
// ApprovalsBypassed rendered PROMINENTLY — mirroring the wording the
// web wizard preview uses (internal/ui/templates/projects_new_wizard.html)
// so the same plan reads consistently whether the operator is looking
// at chat or the browser. Used for Telegram, webchat, and email alike
// — the dispatcher's ToolResult.Content is channel-agnostic text.
func formatComposedPlanText(message string, plan projectwizard.ComposedPlan) string {
	var b strings.Builder
	if msg := strings.TrimSpace(message); msg != "" {
		b.WriteString(msg)
		b.WriteString("\n\n")
	}
	b.WriteString("Plan:\n")
	for i, step := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	if plan.Schedule != "" {
		fmt.Fprintf(&b, "\nSchedule: %s\n", plan.Schedule)
	}
	costBand := plan.CostBand
	if costBand == "" {
		costBand = "(not estimated)"
	}
	fmt.Fprintf(&b, "\nCost: %s — estimate only; actual spend depends on run length and model.\n", costBand)
	if len(plan.Approvals) > 0 {
		b.WriteString("\nHuman in the loop:\n")
		for _, a := range plan.Approvals {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	if len(plan.ApprovalsBypassed) > 0 {
		// Prominent per design §5.2/§5.4 — matches the web preview's
		// "⚠ Will proceed WITHOUT asking" rose-highlighted block.
		b.WriteString("\n⚠ Will proceed WITHOUT asking:\n")
		for _, a := range plan.ApprovalsBypassed {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatClarifyingText renders a non-tier-3 (still-gathering) turn:
// the composer's own message plus its suggested quick-replies, when
// any, so a chat operator sees the same "reply with one of these"
// affordance the web UI renders as chips.
func formatClarifyingText(message string, openQuestions []string) string {
	msg := strings.TrimSpace(message)
	if len(openQuestions) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\nYou could reply: ")
	b.WriteString(strings.Join(openQuestions, " / "))
	return b.String()
}
