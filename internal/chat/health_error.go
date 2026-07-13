package chat

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ModelUnhealthyError is returned by HealthGatedProvider when a
// per-(route, model) circuit is OPEN (or its HALF_OPEN probe permit is
// already taken): the call is rejected WITHOUT reaching the upstream, so it
// returns in microseconds (LLD 2026-07-11-model-health-circuit-breaker §5.4).
// The chat proxy maps it to HTTP 503 MODEL_UNHEALTHY; the executor treats it
// as a signal to skip the retry ladder and fail over immediately.
type ModelUnhealthyError struct {
	Route     string
	Model     string
	State     string // "open" | "half_open" — which reject path (observability)
	OpenSince time.Time
}

func (e *ModelUnhealthyError) Error() string {
	return fmt.Sprintf("MODEL_UNHEALTHY: model %q on route %q circuit %s (open since %s)",
		e.Model, e.Route, e.State, e.OpenSince.Format(time.RFC3339))
}

// IsModelUnhealthy reports whether err is (or wraps) a *ModelUnhealthyError.
func IsModelUnhealthy(err error) bool {
	var m *ModelUnhealthyError
	return errors.As(err, &m)
}

// IsModelUnhealthyFailure reports whether err is a model-health circuit-open
// rejection — the typed *ModelUnhealthyError (in-daemon callers) OR the
// agent-emitted "MODEL_UNHEALTHY" marker (the chat proxy returns 503
// MODEL_UNHEALTHY, which the agent surfaces in result.json and the executor
// reads back as a string error). Shared by the executor's infra-retry fast-
// exit and the agent-container health breaker (LLD 2026-07-12-agent-llm-health-
// breaker §4). Centralises the typed-or-marker check so the two callers don't
// drift.
func IsModelUnhealthyFailure(err error) bool {
	if err == nil {
		return false
	}
	if IsModelUnhealthy(err) {
		return true
	}
	return strings.Contains(err.Error(), "MODEL_UNHEALTHY")
}
