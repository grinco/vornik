package chat

import (
	"context"
	"errors"
	"strings"
)

// health_classify.go — the ONE shared "is this an upstream/provider infra
// failure?" predicate (LLD 2026-07-11-model-health-circuit-breaker §5.3). The
// circuit breaker (health_gate.go) counts breaker failures with it, and the
// executor's isInfraFailure delegates its shared markers to it, so the
// vocabulary that decides "the provider is down" lives in exactly one place.

// upstreamInfraMarkers are substring signatures of transport/upstream infra
// failures that surface as an error STRING (agent-emitted result.json errors,
// native-HTTP kernel errors) rather than a typed error. Kept in sync with the
// executor's historical infraFailureMarkers (which now delegates here).
var upstreamInfraMarkers = []string{
	// curl exit codes from the agent's chat-proxy call.
	"curl: (6)",  // DNS
	"curl: (7)",  // connection refused
	"curl: (28)", // timeout
	"curl: (35)", // TLS connect
	"curl: (52)", // empty reply / EOF
	"curl: (56)", // recv error
	// Gateway 5xx text (bedrock-access-gateway / vertex / claude-sub).
	"gateway error 502",
	"gateway error 503",
	"gateway error 504",
	// The chat-proxy's sanitized upstream-failure code.
	"PROVIDER_ERROR",
	// Connection-level errors that show up without a curl: prefix.
	"connection refused",
	"Connection refused",
	"no such host",
	"i/o timeout",
}

// IsUpstreamInfraError reports whether err represents a transport/upstream
// infrastructure failure — a "the provider/model is unhealthy" signal — as
// opposed to a client/request error (400/404), a model-QUALITY failure
// (shape/plausibility), or a caller cancellation.
//
// Classification (design §5.3):
//   - caller cancellation (context.Canceled) is NEVER an infra failure — the
//     caller walked away; checked FIRST so a cancel that also wraps a deadline
//     is treated as a cancel.
//   - context.DeadlineExceeded IS an infra failure — the model was too slow to
//     answer within the request budget.
//   - a typed *GatewayError / *retryableHTTPError with status 5xx, 429, or
//     401/403 (auth: infra-shaped and non-transient) is an infra failure;
//     400/404 (malformed/not-found — retrying won't help and it's not the
//     model being down) is not.
//   - otherwise, a substring match against upstreamInfraMarkers.
func IsUpstreamInfraError(err error) bool {
	if err == nil {
		return false
	}
	// Cancel beats everything (including a co-wrapped deadline).
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ge *GatewayError
	if errors.As(err, &ge) {
		return isInfraStatus(ge.Status)
	}
	var rhe *retryableHTTPError
	if errors.As(err, &rhe) {
		return isInfraStatus(rhe.StatusCode)
	}
	msg := err.Error()
	for _, m := range upstreamInfraMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// isInfraStatus reports whether an HTTP status is a provider-health failure:
// 5xx (server down), 429 (throttled to the point of failure), or 401/403
// (revoked/expired credential — infra-shaped, non-transient). 4xx otherwise
// (400/404/…) is a request problem, not the model being down.
func isInfraStatus(code int) bool {
	return code >= 500 || code == 429 || code == 401 || code == 403
}
