package projectwizard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// arbitrateTier3 is the tier-3 gate (design §5.1), run on every turn
// right after the envelope is parsed and before any bundle work.
//
// First, unconditionally: if this turn's user message is an explicit
// affirmative reply to a prior "confirm a from-scratch automation"
// bounce, the session's tier-3 unlock is set (persists for the rest
// of the session, overriding the anchor gate — NOT the
// composer.max_tier operator pin, which is a fleet-wide cap no
// session-level state can lift).
//
// Then, only when this turn's envelope is tier-3 (Tier==3 with a
// Bundle):
//   - composer.max_tier < 3 → immediate downgrade, no retry (an
//     operator pin, not a confidence judgment — a retry wouldn't
//     change the outcome).
//   - the session's per-session tier-3 turn cap is already spent →
//     immediate downgrade, no retry (same reasoning).
//   - the session is unlocked → accept as-is.
//   - the anchor score against the accumulated conversation clears the
//     confidence threshold → ONE corrective retry naming the anchored
//     template; if the retry still emits tier-3, fail closed with a
//     plain-language from-scratch confirmation ask (never persisting
//     the original tier-3 bundle); if the retry self-corrects to tier
//     ≤2, that retry's envelope is what proceeds.
//
// Returns the envelope that should actually be processed/persisted
// (which may be a synthesized downgrade/fail-closed envelope, or the
// retry call's envelope) and the chat response usage should be
// recorded against (nil unless a retry call was made).
//
// on the corrective retry is deliberately folded into fail-closed rather than
// bubbled — see the retryResp error-handling below); kept in the signature so
// Converse's call site doesn't need reshaping if a future infra-level check
// (e.g. a config-driven anchor override lookup) needs to fail hard.
//
//nolint:unparam // the error result is always nil today (a transport failure
func (w *Wizard) arbitrateTier3(schemaCtx context.Context, client chat.Provider, msgs []chat.Message, session *persistence.ProjectWizardSession, transcript []Turn, userMessage string, envelope *Envelope) (*Envelope, *chat.ChatResponse, error) {
	if detectTier3Unlock(transcript, userMessage) {
		session.Tier3Unlocked = true
	}
	if envelope.Tier != 3 || envelope.Bundle == nil {
		return envelope, nil, nil
	}

	maxTier := w.Composer.MaxTier
	if maxTier == 0 {
		// Unconfigured composer (e.g. a test Wizard with a zero-value
		// ComposerConfig) behaves permissively — production always
		// loads config.ComposerConfig.applyDefaults, which never
		// leaves MaxTier at 0.
		maxTier = 3
	}
	if maxTier < 3 {
		return downgradeTier3(envelope, fmt.Sprintf("composer.max_tier is pinned to %d on this daemon, so I'll build this as a template + customizations instead.", maxTier)), nil, nil
	}

	maxTier3Turns := w.Composer.MaxTier3Turns
	if maxTier3Turns == 0 {
		maxTier3Turns = config.ComposerDefaultMaxTier3Turns
	}
	if session.Tier3Turns >= maxTier3Turns {
		return downgradeTier3(envelope, fmt.Sprintf("this session already used its %d allotted custom-build turns — starting a fresh session lets you try again, or I can work from the closest template instead.", maxTier3Turns)), nil, nil
	}

	if session.Tier3Unlocked {
		return envelope, nil, nil
	}

	desc := fullUserDescription(transcript)
	slug, score := anchorScore(desc, w.Priors)
	if slug == "" || score < anchorConfidenceThreshold {
		return envelope, nil, nil
	}

	// Corrective re-prompt: exactly one retry, naming the anchored
	// template (design §5.1 — the same one-corrective-retry pattern as
	// the deterministic output-schema shape-retry).
	hint := fmt.Sprintf("[server note] Your last answer proposed a full custom build (tier 3), but the request closely matches the %q template. Prefer tier 1 or 2 (select that template, with params/addons as needed) unless the operator's intent genuinely cannot be expressed that way.", slug)
	retryMsgs := append(append([]chat.Message(nil), msgs...), chat.Message{Role: "user", Content: hint})
	retryResp, err := client.Complete(schemaCtx, retryMsgs)
	if err != nil || retryResp == nil || len(retryResp.Choices) == 0 {
		// A transport failure on the corrective retry is treated the
		// same as "still tier-3" — fail closed rather than surfacing a
		// raw transport error mid-arbitration.
		return failClosedTier3(slug), nil, nil
	}
	retryEnvelope, perr := parseEnvelope(retryResp.Choices[0].Message.Content)
	if perr != nil {
		return failClosedTier3(slug), retryResp, nil
	}
	if retryEnvelope.Tier == 3 && retryEnvelope.Bundle != nil {
		return failClosedTier3(slug), retryResp, nil
	}
	return retryEnvelope, retryResp, nil
}

// downgradeTier3 synthesizes the operator-facing envelope for an
// immediate (no-retry) tier-3 refusal: the ORIGINAL tier-3 bundle is
// never returned or persisted — only this safe envelope is.
func downgradeTier3(envelope *Envelope, reason string) *Envelope {
	msg := reason
	if envelope != nil && strings.TrimSpace(envelope.Message) != "" {
		msg = strings.TrimSpace(envelope.Message) + " (" + reason + ")"
	}
	var suggested string
	var open []string
	if envelope != nil {
		suggested = envelope.SuggestedTemplate
		open = envelope.OpenQuestions
	}
	return &Envelope{Message: msg, Tier: 0, ReadyToCommit: false, SuggestedTemplate: suggested, OpenQuestions: open}
}

// failClosedTier3 synthesizes the fail-closed envelope after a
// corrective retry still emitted tier-3: a plain-language ask for the
// operator to confirm a from-scratch automation. Carries
// tier3ConfirmSentinel so detectTier3Unlock recognises an affirmative
// reply on the NEXT turn.
func failClosedTier3(anchoredSlug string) *Envelope {
	msg := fmt.Sprintf("This looks like it might need a fully custom automation rather than the %q template. Reply to confirm a from-scratch automation, or tell me more and I'll try fitting it to the template instead.", anchoredSlug)
	return &Envelope{
		Message:           msg,
		Tier:              0,
		ReadyToCommit:     false,
		SuggestedTemplate: anchoredSlug,
		OpenQuestions:     []string{"yes, build it from scratch", "use the " + anchoredSlug + " template instead"},
	}
}

// fullUserDescription joins every user turn in the transcript
// (including the just-appended current turn) into one string — the
// anchor score is computed against the accumulated intent of the
// WHOLE conversation, not just the latest message, since a template
// match often only becomes clear after a couple of clarifying turns.
func fullUserDescription(transcript []Turn) string {
	var b strings.Builder
	for _, t := range transcript {
		if t.Role != "user" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t.Content)
	}
	return b.String()
}

// maxConsecutiveTier3ValidationFailures is the QUALITY circuit-breaker
// threshold (design §7 row 1): after this many CONSECUTIVE tier-3
// bundle-pipeline failures within a session, the composer stops
// retrying tier-3 for the turn and downgrades to a tier-2
// nearest-template suggestion instead. Distinct from (and composed
// with) the existing composer.max_tier3_turns turn-BUDGET cap, which
// only counts ACCEPTED turns — this counter is reset by ANY successful
// tier-3 composition, not by the passage of turns.
const maxConsecutiveTier3ValidationFailures = 3

// applyBundle runs the tier-3 bundle pipeline (design §5.2/§5.4) on an
// admissible tier-3 envelope: shape check → live-collision check →
// materialize (archetype expansion) → guardrail pass (fill + violation
// detection) → staged registry validation. Any failure at any stage
// appends a plain-language reason to envelope.Message and forces
// ready_to_commit=false — exactly like the v1 validator / v2 Compose
// paths (applyProposal / applyComposition) — rather than silently
// fixing or partially persisting a broken bundle.
//
// A meaning-changing guardrail violation is NOT retried within this
// same turn (unlike the tier-arbitration anchor gate) — it is bounced
// into the conversation: the violation is named in the message, the
// bundle is not marked valid, and session.Bundle is not updated to the
// offending version. The transcript now carries the violation, so the
// operator's next turn naturally re-prompts the LLM with that
// corrective context. This is a deliberate scope simplification from
// the design's literal "re-prompt once, then bounce" wording (which
// would require a second same-turn LLM call analogous to the anchor
// retry); it preserves the load-bearing invariant — a violation is
// NEVER silently fixed or hidden — at a materially simpler
// implementation cost.
//
// Every failure at any stage also advances
// session.Tier3ConsecutiveValidationFailures (task 1.3, design §7 row
// 1); on the 3rd consecutive failure the turn is downgraded instead of
// bounced (see rejectOrFallbackBundle). A success resets the counter.
func (w *Wizard) applyBundle(session *persistence.ProjectWizardSession, transcript []Turn, envelope *Envelope) string {
	bundle := envelope.Bundle
	session.Bundle = nil

	ids, shapeErrs := shapeCheckBundle(bundle)
	live, err := liveEntityIDsFromConfigDir(w.LiveConfigDir)
	if err != nil {
		shapeErrs = append(shapeErrs, "internal error checking the live registry: "+err.Error())
	} else {
		shapeErrs = append(shapeErrs, collisionCheckBundle(ids, live)...)
	}
	if len(shapeErrs) > 0 {
		return w.rejectOrFallbackBundle(session, transcript, envelope, "shape", shapeErrs)
	}

	mb, toolViolations, err := materializeBundle(bundle, w.RoleLibrary)
	if err != nil {
		return w.rejectOrFallbackBundle(session, transcript, envelope, "bundle", []string{err.Error()})
	}
	// Grounded cost estimate (task 1.3, design §5.2): overwrite the
	// LLM's free-text cost_band with a deterministic server-side
	// figure as soon as the bundle materializes — even if guardrails or
	// staged validation go on to reject this turn, the preview should
	// never show an ungrounded number for a bundle the operator can
	// already see.
	bundle.Plan.CostBand = estimateCostBand(mb)

	gr := applyGuardrails(mb, bundle.Plan, toolViolations, w.Composer.DefaultBudget, session.ScheduleConfirmedCron)
	for _, v := range gr.Violations {
		w.Metrics.recordGuardrailHit(v.Rule)
	}
	if len(gr.Violations) > 0 {
		msgs := make([]string, len(gr.Violations))
		for i, v := range gr.Violations {
			msgs[i] = v.Message
		}
		return w.rejectOrFallbackBundle(session, transcript, envelope, "guardrail", msgs)
	}

	files, err := renderMaterializedBundle(mb)
	if err != nil {
		return w.rejectOrFallbackBundle(session, transcript, envelope, "render", []string{err.Error()})
	}
	staged, err := stageBundleForValidation(w.LiveConfigDir, files)
	if err != nil {
		return w.rejectOrFallbackBundle(session, transcript, envelope, "staged validation", []string{err.Error()})
	}
	if !staged.OK {
		w.Metrics.recordBundleValidated(bundleValidationResultInvalid)
		return w.rejectOrFallbackBundle(session, transcript, envelope, "registry", staged.Errors)
	}
	w.Metrics.recordBundleValidated(bundleValidationResultValid)

	if len(gr.DefaultsApplied) > 0 {
		envelope.Message = strings.TrimSpace(envelope.Message) + "\n\n(defaults applied: " + strings.Join(gr.DefaultsApplied, "; ") + ")"
	}

	// Schedule-confirmation gate (§5.4, structural, not prose):
	// ready_to_commit alone never satisfies it.
	if mb.Project.Autonomy.Enabled && !scheduleConfirmed(session, mb) {
		envelope.Message = strings.TrimSpace(envelope.Message) + "\n\n(schedule: please confirm the cadence above before this can be marked ready to commit)"
		envelope.ReadyToCommit = false
	}

	bundleBytes, _ := json.Marshal(bundle)
	session.Bundle = bundleBytes
	session.Tier3Turns++
	// A successful tier-3 composition resets the quality
	// circuit-breaker (design §7 row 1: "counter resets on a successful
	// tier-3 composition") — the operator getting a valid build back
	// clears any prior run of failures.
	session.Tier3ConsecutiveValidationFailures = 0
	return turnOutcomeAssistantReply
}

// rejectBundle folds a bundle-pipeline failure into the operator-
// visible message and forces ready_to_commit=false, mirroring
// applyComposition / applyProposal's failure handling. session.Bundle
// is left nil (applyBundle already cleared it) so an invalid/violating
// bundle is never what a later Commit would see.
//
// This does NOT record composer_bundles_validated_total itself (I2
// fix, whole-branch review): that counter (design §5.8) counts bundles
// that were RUN THROUGH staged registry validation, exactly once each.
// applyBundle already records the two staged-validation outcomes
// (:246 invalid on !staged.OK, :249 valid) before ever reaching a
// "registry"-stage rejectBundle call — recording again here double-
// counted that case. Worse, rejectBundle is also the rejection path
// for "shape"/"bundle"/"guardrail"/"render" stages, none of which ever
// reached staged validation, so counting them as "invalid" mis-counted
// bounces that never ran the check the metric describes.
func (w *Wizard) rejectBundle(envelope *Envelope, stage string, reasons []string) string {
	envelope.Message = strings.TrimSpace(envelope.Message) + "\n\n(" + stage + ": " + strings.Join(reasons, "; ") + ")"
	envelope.ReadyToCommit = false
	return turnOutcomeValidationError
}

// rejectOrFallbackBundle wraps rejectBundle with the consecutive-
// validation-failure circuit breaker (design §7 row 1). Every call
// advances session.Tier3ConsecutiveValidationFailures; below the
// threshold this behaves exactly like rejectBundle (the turn bounces
// back into the conversation for another attempt). On the Nth
// consecutive failure, it instead mutates envelope in place into a
// tier-2 nearest-template downgrade — reusing anchorScore against the
// full conversation (same template-selection heuristic the tier-
// arbitration anchor gate uses) and the existing downgradeTier3
// helper — and resets the counter, so the composer never retries
// tier-3 forever within a single quality-failure streak.
func (w *Wizard) rejectOrFallbackBundle(session *persistence.ProjectWizardSession, transcript []Turn, envelope *Envelope, stage string, reasons []string) string {
	outcome := w.rejectBundle(envelope, stage, reasons)
	session.Tier3ConsecutiveValidationFailures++
	if session.Tier3ConsecutiveValidationFailures < maxConsecutiveTier3ValidationFailures {
		return outcome
	}
	session.Tier3ConsecutiveValidationFailures = 0

	slug, _ := anchorScore(fullUserDescription(transcript), w.Priors)
	var reason string
	if slug != "" {
		reason = fmt.Sprintf("I've had trouble building this from scratch a few times — let me start from the closest ready-made template instead: %q. You can customise from there.", slug)
		// downgradeTier3 carries through whatever SuggestedTemplate is
		// already on the envelope — set it here so the UI's suggested-
		// template affordance points at the SAME template named in the
		// message (mirrors how the tier-arbitration anchor gate's
		// corrective retry names its anchored slug).
		envelope.SuggestedTemplate = slug
	} else {
		reason = "I've had trouble building this from scratch a few times — try describing it a little differently, or browse the template gallery for a starting point instead."
	}
	downgraded := downgradeTier3(envelope, reason)
	*envelope = *downgraded
	return turnOutcomeFallback
}

// scheduleConfirmed reports whether the bundle's autonomy schedule
// matches a confirmation already on record for this session (§5.4).
// Autonomy-disabled bundles trivially pass (nothing to confirm).
func scheduleConfirmed(session *persistence.ProjectWizardSession, mb *materializedBundle) bool {
	if mb == nil || mb.Project == nil || !mb.Project.Autonomy.Enabled {
		return true
	}
	if session == nil || session.ScheduleConfirmedAt == nil {
		return false
	}
	return schedulesEquivalent(session.ScheduleConfirmedCron, mb.Project.Autonomy.PollInterval)
}

// schedulesEquivalent reports whether two autonomy poll-interval
// strings (registry.ProjectAutonomy.PollInterval — a Go duration
// string, e.g. "24h", "1440m") denote the same cadence (companion
// review finding 2). Raw string equality is brittle: YAML/marshal
// round-tripping can reformat an identical cadence ("24h" vs
// "1440m"), which would force a spurious re-confirmation prompt —
// annoying but safe. The DANGEROUS direction is the reverse: two
// DIFFERENT cadences must never compare equal and let a changed
// schedule go live without re-confirmation.
//
// Both values are normalized through time.ParseDuration — the same
// parser autonomy.Manager and the UI use for this exact field
// (internal/autonomy/manager.go, internal/ui/dashboard.go) — and
// compared as parsed durations. If EITHER value fails to parse, this
// returns false unconditionally, even when both sides are the same
// unparseable string: when normalization is uncertain, the gate must
// fail closed (treat it as a change, require re-confirmation) rather
// than risk wrongly matching a schedule whose meaning it couldn't
// verify.
func schedulesEquivalent(a, b string) bool {
	da, aerr := time.ParseDuration(strings.TrimSpace(a))
	if aerr != nil {
		return false
	}
	db, berr := time.ParseDuration(strings.TrimSpace(b))
	if berr != nil {
		return false
	}
	return da == db
}
