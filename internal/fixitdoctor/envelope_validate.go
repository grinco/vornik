package fixitdoctor

import "strings"

// Guardrail-hit reasons — the label values on
// vornik_fixit_guardrail_hits_total{reason}.
const (
	GuardrailReasonUnknownKind   = "unknown_kind"
	GuardrailReasonParamsInvalid = "params_invalid"
)

// ValidateActions filters an envelope's proposed actions down to the
// ones that resolve to an ActionKind this edition allows AND carry
// valid params for that kind. Every dropped action increments the
// guardrail-hit metric (nil-safe) with a reason label so operators can
// see how often the LLM proposes out-of-vocabulary or malformed
// actions. The caller sees only the surviving, valid proposals — when
// none survive, the envelope is message-only (fix-it-doctor-design.md
// §5.2/§6: "the server DROPS any unknown/param-invalid action").
//
// This is deliberately independent of whether the LLM even HAD
// config_apply in its response_format enum (Community builds never do,
// via EnvelopeResponseFormat) — a compromised/fine-tuned/prompt-
// injected model could still emit it in free text that ParseEnvelope's
// prose fallback happens to pick up as structured JSON, so the server
// re-validates against AllowedActionKinds(edition) regardless of what
// schema was offered.
func ValidateActions(actions []ProposedAction, edition string, metrics *Metrics) []ProposedAction {
	allowed := make(map[ActionKind]bool, len(allActionKinds))
	for _, k := range AllowedActionKinds(edition) {
		allowed[k] = true
	}

	out := make([]ProposedAction, 0, len(actions))
	for _, a := range actions {
		if !allowed[a.Kind] {
			metrics.recordGuardrailHit(GuardrailReasonUnknownKind)
			continue
		}
		if !validActionParams(a.Kind, a.Params) {
			metrics.recordGuardrailHit(GuardrailReasonParamsInvalid)
			continue
		}
		out = append(out, a)
	}
	return out
}

// validActionParams checks the minimal param contract each ActionKind
// needs to be meaningful downstream (task 3.3's dispatcher will
// re-validate more strictly against live state; this is the
// structural/presence-shape gate that keeps a malformed or
// injection-shaped proposal from ever reaching the operator as a
// rendered card).
func validActionParams(kind ActionKind, params map[string]string) bool {
	switch kind {
	case ActionKindConfigApplyGate:
		return nonEmptyParam(params, "key")
	case ActionKindConfigApply:
		return nonEmptyParam(params, "key") && nonEmptyParam(params, "value")
	case ActionKindRetryTask:
		return nonEmptyParam(params, "task_id")
	case ActionKindReprobeIntegration:
		return nonEmptyParam(params, "integration_id")
	case ActionKindSetSecret:
		return nonEmptyParam(params, "key")
	case ActionKindLinkOut:
		return validLinkOutURL(params["url"])
	default:
		return false
	}
}

func nonEmptyParam(params map[string]string, key string) bool {
	return strings.TrimSpace(params[key]) != ""
}

// validLinkOutURL restricts link_out targets to same-origin absolute
// paths ("/ui/..."). Rejects empty values, scheme-qualified URLs
// (http://, javascript:, data:), protocol-relative ("//host/..."), and
// anything containing whitespace — so prompt-injected content in the
// grounding bundle can never turn a "navigate here" proposal into an
// external redirect or an exfiltration channel. link_out is the one
// action kind task 3.2 actually wires end-to-end (pure client-side
// navigation, no server mutation), so this guard is the whole of its
// safety story.
func validLinkOutURL(url string) bool {
	url = strings.TrimSpace(url)
	if url == "" || !strings.HasPrefix(url, "/") {
		return false
	}
	if strings.HasPrefix(url, "//") {
		return false
	}
	if strings.Contains(url, "://") {
		return false
	}
	if strings.ContainsAny(url, " \t\n\r") {
		return false
	}
	return true
}
