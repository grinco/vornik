package dispatcher

// Agent-driven write path for the per-operator profile
// (roadmapped "Per-operator persistent profile / memory" follow-
// up to the read-path). The tool is exposed sparingly: the
// description below tells the model to call ONLY when the
// operator makes an explicit, durable preference statement.
//
// Each call:
//   1. Validates the key is in the allow-list (the same list
//      the dispatcher's <operator_profile> prompt block reads —
//      a future "leak" via a non-allow-listed key would never
//      reach the system prompt, but rejecting at the boundary
//      keeps the invariant tight at every layer).
//   2. Reads the existing row (so multi-key updates compose).
//   3. Merges the new key/value into structured (or replaces
//      the notes column when key="notes").
//   4. Upserts via the OperatorProfileRepository.
//   5. Returns a concise confirmation including the rationale
//      so the model can cite it back to the operator.
//
// Audit goes through the existing tool_audit_log path (every
// tool call is captured via te.auditRepo); the rationale lands
// in the audit row's output field for later inspection by
// `vornikctl operator profile audit`.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
)

// updateOperatorProfileAllowedKeys mirrors the dispatcher's
// operatorProfileKnownKeys list — the structured-blob fields
// that survive into the system-prompt <operator_profile>
// block. "notes" is a parallel key that targets the notes
// column directly. Anything else returns a refusal.
var updateOperatorProfileAllowedKeys = map[string]bool{
	"tone":                true,
	"verbosity":           true,
	"time_zone":           true,
	"communication_style": true,
	"preferred_channel":   true,
	"notes":               true,
}

// updateOperatorProfileArgs is the parsed tool-arguments shape.
type updateOperatorProfileArgs struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Rationale string `json:"rationale"`
}

// updateOperatorProfile handles the update_operator_profile
// tool call. Pure error-message-as-content semantics — no
// panics, no nil-deref; the dispatcher loop expects a string
// reply on every path.
func (te *ToolExecutor) updateOperatorProfile(ctx context.Context, rawArgs string) ToolResult {
	if te.operatorProfiles == nil {
		return ToolResult{Content: "Operator-profile updates are not configured on this daemon — the OperatorProfileRepository isn't wired (SQLite / pre-migration-60 deployment)."}
	}
	operatorID, _ := operatorIDFromContext(ctx)
	if operatorID == "" {
		return ToolResult{Content: "Cannot update operator profile: this turn was not initiated by an identified operator (synthetic / autonomy / post-mortem context have no operator_id)."}
	}
	// Resolve to canonical operator id so an operator who's
	// linked their channels sees writes land on the same row
	// the read path injects from. Falls back to operatorID
	// verbatim when no link table is wired or no link exists.
	operatorID = te.resolveCanonicalOperatorID(ctx, operatorID)

	var args updateOperatorProfileArgs
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("update_operator_profile: invalid arguments JSON: %v", err)}
	}
	key := strings.TrimSpace(args.Key)
	rationale := strings.TrimSpace(args.Rationale)
	if key == "" {
		return ToolResult{Content: "update_operator_profile: 'key' is required."}
	}
	if !updateOperatorProfileAllowedKeys[key] {
		// Sorted list in the refusal message so the model knows
		// the exact allow-list. Rebuilds on every refusal — the
		// table is six entries and the sort is trivial.
		allowed := make([]string, 0, len(updateOperatorProfileAllowedKeys))
		for k := range updateOperatorProfileAllowedKeys {
			allowed = append(allowed, k)
		}
		sort.Strings(allowed)
		return ToolResult{Content: fmt.Sprintf("update_operator_profile: %q is not a recognised key. Allowed: %s.", key, strings.Join(allowed, ", "))}
	}
	if rationale == "" {
		return ToolResult{Content: "update_operator_profile: 'rationale' is required — every profile change carries an explanation for the audit log."}
	}

	// Load + merge. The dispatcher's per-turn read path doesn't
	// cache between turns, so any drift is bounded by the
	// network round-trip latency to Postgres.
	current, err := te.operatorProfiles.Get(ctx, operatorID)
	if err != nil && err != persistence.ErrNotFound {
		return ToolResult{Content: fmt.Sprintf("update_operator_profile: load failed: %v", err)}
	}
	if current == nil {
		current = &persistence.OperatorProfile{OperatorID: operatorID}
	}

	if key == "notes" {
		current.Notes = strings.TrimSpace(args.Value)
	} else {
		structured := map[string]any{}
		if len(current.Structured) > 0 {
			_ = json.Unmarshal(current.Structured, &structured)
		}
		value := strings.TrimSpace(args.Value)
		if value == "" {
			delete(structured, key)
		} else {
			structured[key] = value
		}
		raw, err := json.Marshal(structured)
		if err != nil {
			return ToolResult{Content: fmt.Sprintf("update_operator_profile: marshal failed: %v", err)}
		}
		current.Structured = raw
	}

	if err := te.operatorProfiles.Upsert(ctx, current); err != nil {
		return ToolResult{Content: fmt.Sprintf("update_operator_profile: persist failed: %v", err)}
	}

	// Audit row — surfaces on the operator-profile detail
	// page's audit panel. Principal + Target both carry
	// operator_id so the UI can filter on either field. The
	// after_state JSON captures the (key, value, rationale)
	// tuple for later review.
	if te.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{
			"key":       key,
			"value":     strings.TrimSpace(args.Value),
			"rationale": rationale,
		})
		_ = te.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: operatorID,
			Source:    "dispatcher",
			Action:    "operator_profile.updated",
			Target:    operatorID,
			After:     string(afterJSON),
		})
	}

	if key == "notes" {
		return ToolResult{Content: fmt.Sprintf("Operator profile updated. Notes set to: %q. Rationale: %s.", current.Notes, rationale), Provenance: outputguard.ProvenanceFirstParty}
	}
	if v := strings.TrimSpace(args.Value); v == "" {
		return ToolResult{Content: fmt.Sprintf("Operator profile updated. Removed key %q. Rationale: %s.", key, rationale), Provenance: outputguard.ProvenanceFirstParty}
	}
	return ToolResult{Content: fmt.Sprintf("Operator profile updated. %s = %q. Rationale: %s.", key, strings.TrimSpace(args.Value), rationale), Provenance: outputguard.ProvenanceFirstParty}
}

// operatorIDContextKey is the unexported context-value key.
// Avoids the stringly-typed collision pattern that bites
// downstream when two packages choose the same string.
type operatorIDContextKey struct{}

// WithOperatorID stamps the operator id on the request
// context. Agent.Process calls this at the top of every turn
// when req.OperatorID is non-empty; tools read the value via
// operatorIDFromContext.
func WithOperatorID(ctx context.Context, operatorID string) context.Context {
	if operatorID == "" {
		return ctx
	}
	return context.WithValue(ctx, operatorIDContextKey{}, operatorID)
}

// operatorIDFromContext is the corresponding reader. Returns
// ("", false) when no id is stamped.
func operatorIDFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(operatorIDContextKey{}).(string)
	return v, ok && v != ""
}
