package dispatcher

// Operator-profile system-prompt injection. The dispatcher
// reads the operator's profile from
// persistence.OperatorProfileRepository at the start of every
// turn (when a profile exists + the repo is wired) and appends
// a <operator_profile> block to the system prompt so the model
// can read tone / verbosity / time_zone / accumulated notes.
//
// Read-path-first slice: this file only builds the block. The
// agent-driven update_operator_profile tool ships in a follow-
// up; profiles populate via that path + the future
// `vornikctl operator set` CLI.
//
// Security: only well-known keys from the structured JSONB are
// rendered. An attacker who somehow writes a free-form key into
// the column ("prompt_injection") doesn't get verbatim
// rendering into the system prompt. The notes column IS
// free-form by design — operators trust the assistant's
// accumulated notes are their own writing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// operatorProfileKnownKeys is the allow-list of structured
// fields rendered into the prompt. Pinned here so a future
// "add field X" requires a code change + review.
var operatorProfileKnownKeys = []string{
	"tone",
	"verbosity",
	"time_zone",
	"communication_style",
	"preferred_channel",
}

// operatorProfileCitationFooter is the in-prompt instruction
// that asks the model to prefix any reply sentence shaped by a
// profile field with a citation marker. The marker syntax is
// byte-stable plain text — channel surfaces (UI / Telegram /
// webchat) can optionally style it but it survives any plain-
// text fallback.
//
// Refusal pattern: when the current turn directly contradicts a
// profile field, the model emits "[overriding profile: <key> — ...]"
// instead so the operator sees the override explicitly.
const operatorProfileCitationFooter = `
# When a reply relies on any of the fields above, prefix the
# sentence with [from your profile: <key>] (or [from your notes]
# for the notes line). If the current turn contradicts a profile
# field, use [overriding profile: <key>] instead so the operator
# can see the override explicitly.`

// appendOperatorProfileBlock returns the system prompt with an
// <operator_profile>...</operator_profile> block appended when
// profile carries useful content. nil or empty profile returns
// the base prompt unchanged so behaviour stays uniform across
// "never seen this operator" and "no preferences yet".
//
// Thin wrapper around buildOperatorProfileBlock that drops the
// "what was used" telemetry — kept for tests + the small
// number of callers that don't write the audit row.
func appendOperatorProfileBlock(systemPrompt string, profile *persistence.OperatorProfile) string {
	out, _, _ := buildOperatorProfileBlock(systemPrompt, profile)
	return out
}

// buildOperatorProfileBlock is the audit-aware version: returns
// the rewritten system prompt PLUS the set of keys + whether
// notes were used. Phase-B audit (profile_use_audit) consumes
// the second/third returns to record which keys influenced the
// turn.
func buildOperatorProfileBlock(systemPrompt string, profile *persistence.OperatorProfile) (prompt string, usedKeys []string, usedNotes bool) {
	if profile == nil {
		return systemPrompt, nil, false
	}

	// Decode structured JSONB; tolerate garbage. The profile's
	// notes column is still useful even if the structured side
	// is corrupt.
	var structured map[string]any
	if len(profile.Structured) > 0 {
		// json.Unmarshal returns an error on garbage — leave
		// structured nil and continue to the notes path.
		_ = json.Unmarshal(profile.Structured, &structured)
	}

	rendered := renderKnownKeys(structured)
	notes := strings.TrimSpace(profile.Notes)
	if len(rendered) == 0 && notes == "" {
		return systemPrompt, nil, false
	}

	keys := extractKeysFromRendered(rendered)

	var b strings.Builder
	b.WriteString(systemPrompt)
	if !strings.HasSuffix(systemPrompt, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("\n<operator_profile>\n")
	for _, line := range rendered {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if notes != "" {
		if len(rendered) > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("notes: ")
		b.WriteString(notes)
		b.WriteByte('\n')
	}
	b.WriteString(strings.TrimLeft(operatorProfileCitationFooter, "\n"))
	b.WriteByte('\n')
	b.WriteString("</operator_profile>\n")
	return b.String(), keys, notes != ""
}

// extractKeysFromRendered splits the "key: value" lines back
// into the bare key names. The rendered slice is already sorted
// + allow-listed by renderKnownKeys so the output is a stable
// sorted set of known keys.
func extractKeysFromRendered(rendered []string) []string {
	if len(rendered) == 0 {
		return nil
	}
	out := make([]string, 0, len(rendered))
	for _, line := range rendered {
		if i := strings.IndexByte(line, ':'); i > 0 {
			out = append(out, strings.TrimSpace(line[:i]))
		}
	}
	return out
}

// renderKnownKeys turns the structured map into stable
// "key: value" lines for the allow-listed keys. Keys not in
// operatorProfileKnownKeys are dropped. Output sorted for
// deterministic prompt assembly (LLM cache + test stability).
func renderKnownKeys(structured map[string]any) []string {
	if len(structured) == 0 {
		return nil
	}
	out := make([]string, 0, len(operatorProfileKnownKeys))
	for _, key := range operatorProfileKnownKeys {
		v, ok := structured[key]
		if !ok {
			continue
		}
		s := scalarToString(v)
		if s == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s", key, s))
	}
	sort.Strings(out)
	return out
}

// maybeInjectOperatorProfile fetches the operator's profile
// (when both the repo + operatorID are wired) and appends the
// <operator_profile> block to the supplied system prompt.
// Failure to fetch (DB blip, ErrNotFound for fresh operator)
// degrades silently — the turn continues without the block
// rather than failing the operator's request.
//
// The operator id passed in is the channel-specific speaker id
// (Telegram user_id, webchat session, etc.); we walk the
// identity-link table once to resolve it onto the canonical
// operator id so a linked operator sees one profile across
// every channel.
func (a *Agent) maybeInjectOperatorProfile(ctx context.Context, systemPrompt, operatorID string) string {
	if a == nil || a.operatorProfiles == nil || operatorID == "" {
		return systemPrompt
	}
	canonical := a.resolveCanonicalOperatorID(ctx, operatorID)
	profile, err := a.operatorProfiles.Get(ctx, canonical)
	if err != nil {
		// ErrNotFound is the expected "fresh operator" path —
		// no log noise. Anything else surfaces as a debug-
		// level log so operators can investigate without
		// burning the user's turn.
		if !errors.Is(err, persistence.ErrNotFound) {
			a.logger.Debug().Err(err).
				Str("operator_id", canonical).
				Msg("dispatcher: operator profile read failed; injecting empty block")
		}
		return systemPrompt
	}
	out, usedKeys, usedNotes := buildOperatorProfileBlock(systemPrompt, profile)
	// Audit row is best-effort — a transient DB blip on the
	// audit path must NEVER break the user's turn. The Insert
	// gets the canonical operator id so a future
	// `vornikctl operator audit <canonical>` matches the row
	// the profile read produced. Empty (usedKeys + !usedNotes)
	// means the block was suppressed; skip the audit in that
	// case so the audit table reflects actual injections.
	if a.profileUseAudit != nil && (len(usedKeys) > 0 || usedNotes) {
		go func(opID string, keys []string, notes bool) {
			// 5s cap so the goroutine never outlives an operator
			// session by much.
			auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.profileUseAudit.Insert(auditCtx, &persistence.ProfileUseAudit{
				OperatorID: opID,
				UsedKeys:   keys,
				UsedNotes:  notes,
			}); err != nil {
				a.logger.Debug().Err(err).
					Str("operator_id", opID).
					Msg("dispatcher: profile-use audit insert failed; the turn continues regardless")
			}
		}(canonical, usedKeys, usedNotes)
	}
	return out
}

// scalarToString coerces JSON scalars to display strings. Maps
// + slices are skipped (returned empty) — known-key values are
// always strings or numbers in the documented schema; nested
// shapes shouldn't sneak into the prompt.
func scalarToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}
