package executor

import (
	"context"
	"encoding/json"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fallbackSwarmResolver is the slice of *registry.Registry the
// fallback-override path needs — declared as an interface so callers
// and tests can substitute it.
type fallbackSwarmResolver interface {
	GetProjectWithSwarm(projectID string) (*registry.Project, *registry.Swarm, error)
}

// fallbackTaskWriter is the slice of persistence.TaskRepository the
// fallback-override path needs.
type fallbackTaskWriter interface {
	Update(ctx context.Context, task *persistence.Task) error
}

// ApplyFallbackModelOverride switches every role in the task's swarm
// that has a configured modelFallback onto that fallback, by merging an
// operator model override into the task payload and persisting it. It
// is the shared core behind the UI "Retry on fallback model" button and
// the API steer-hint `model: fallback` keyword. Returns (true, nil)
// when an override was written; (false, nil) when there's nothing to do
// (no swarm, or no role has a fallback); (false, err) on a hard failure.
func ApplyFallbackModelOverride(ctx context.Context, reg fallbackSwarmResolver, taskRepo fallbackTaskWriter, task *persistence.Task) (bool, error) {
	if task == nil || reg == nil || taskRepo == nil {
		return false, nil
	}
	_, sw, err := reg.GetProjectWithSwarm(task.ProjectID)
	if err != nil {
		return false, err
	}
	overrides := FallbackModelOverrides(sw)
	if len(overrides) == 0 {
		return false, nil
	}
	newPayload, err := WithOperatorModelOverride(task.Payload, overrides)
	if err != nil {
		return false, err
	}
	task.Payload = newPayload
	if err := taskRepo.Update(ctx, task); err != nil {
		return false, err
	}
	return true, nil
}

// operatorModelOverrideKey is the task-payload location an operator
// "retry on the fallback model" action writes to. It lives at
// context.operator_model_override (role → model) and is DELIBERATELY
// separate from context.counterfactual.role_model_override: the
// counterfactual block flags the task as a replay (IsReplay=true),
// which engages the MCP side-effect gate. An operator-forced model
// swap is a REAL run — its tool calls must hit the world — so it needs
// a replay-free seam of its own.
const operatorModelOverrideKey = "operator_model_override"

// operatorModelOverride returns the operator-forced model for a role,
// or "" when none is set. Defensive: a missing/malformed block yields
// "" so the caller falls through to native model resolution.
func operatorModelOverride(payload json.RawMessage, role string) string {
	if len(payload) == 0 || role == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	ctxMap, _ := m["context"].(map[string]any)
	if ctxMap == nil {
		return ""
	}
	overrides, _ := ctxMap[operatorModelOverrideKey].(map[string]any)
	if overrides == nil {
		return ""
	}
	v, _ := overrides[role].(string)
	return v
}

// WithOperatorModelOverride merges role→model overrides into a task
// payload under context.operator_model_override, preserving every
// other field. A nil/empty payload becomes a minimal object. Roles
// mapped to "" are skipped (nothing to override). Returns the payload
// unchanged when there is nothing to merge.
//
// Lives here (not in blackbox) so the operator fallback-retry path
// shares one definition with the executor reader above and never
// touches the counterfactual/replay block.
func WithOperatorModelOverride(payload json.RawMessage, roleModels map[string]string) (json.RawMessage, error) {
	clean := make(map[string]string, len(roleModels))
	for role, model := range roleModels {
		if role != "" && model != "" {
			clean[role] = model
		}
	}
	if len(clean) == 0 {
		return payload, nil
	}

	m := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &m); err != nil {
			return nil, err
		}
	}
	ctxMap, _ := m["context"].(map[string]any)
	if ctxMap == nil {
		ctxMap = map[string]any{}
		m["context"] = ctxMap
	}
	overrides, _ := ctxMap[operatorModelOverrideKey].(map[string]any)
	if overrides == nil {
		overrides = map[string]any{}
		ctxMap[operatorModelOverrideKey] = overrides
	}
	for role, model := range clean {
		overrides[role] = model
	}
	return json.Marshal(m)
}

// FallbackModelOverrides builds the role→model map that the "retry on
// the fallback model" action applies: every role with a non-empty
// modelFallback maps to that fallback. Roles without a fallback are
// omitted (they keep their primary). Returns an empty map when no role
// has a fallback configured, so callers can treat len==0 as "nothing
// to do".
func FallbackModelOverrides(sw *registry.Swarm) map[string]string {
	if sw == nil {
		return nil
	}
	out := make(map[string]string, len(sw.Roles))
	for _, role := range sw.Roles {
		if fb := strings.TrimSpace(role.ModelFallback); fb != "" {
			out[role.Name] = fb
		}
	}
	return out
}

// ParseFallbackModelDirective reports whether an operator's steering
// hint asks for the run to use each role's fallback model.
//
// Matches, in a steering-hint context where the operator is deliberately
// instructing the next retry:
//   - the explicit directives `model: fallback` / `model:fallback` / `@fallback`;
//   - the camelCase config identifier `modelFallback` (no space) — this is the
//     exact token the predefined recovery action uses ("Re-run the researcher
//     on its modelFallback …"). Pre-2026-06-13 the parser matched none of these
//     identifier forms, so that canned action submitted as a hint silently
//     re-ran on the SAME model (task …29be);
//   - the natural phrase `fallback model` ("use the fallback model").
//
// Deliberately does NOT match the space-separated, opposite-order phrase
// "model fallback": that's how prose merely *mentions* the feature ("the model
// fallback is in place"), and a steering hint shouldn't trip on a passing
// reference. The no-space identifier and the "fallback model" phrasing both
// carry clear operator intent.
func ParseFallbackModelDirective(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "model: fallback") ||
		strings.Contains(t, "model:fallback") ||
		strings.Contains(t, "@fallback") ||
		strings.Contains(t, "modelfallback") ||
		strings.Contains(t, "fallback model")
}
