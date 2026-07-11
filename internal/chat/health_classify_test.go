package chat

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// IsUpstreamInfraError is the ONE shared classifier (design §5.3): the
// circuit breaker and the executor both use it so the "provider is down"
// vocabulary never drifts.

func TestIsUpstreamInfraError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"gateway 500", &GatewayError{Status: 500}, true},
		{"gateway 503", &GatewayError{Status: 503}, true},
		{"gateway 429", &GatewayError{Status: 429}, true},
		{"gateway 401 auth", &GatewayError{Status: 401}, true},
		{"gateway 403 auth", &GatewayError{Status: 403}, true},
		{"gateway 400 client", &GatewayError{Status: 400}, false}, // malformed request, not a health signal
		{"gateway 404", &GatewayError{Status: 404}, false},
		{"retryableHTTPError 500", &retryableHTTPError{StatusCode: 500}, true},
		{"retryableHTTPError 429", &retryableHTTPError{StatusCode: 429}, true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"canceled wrapping deadline", fmt.Errorf("gave up: %w", context.Canceled), false},
		{"wrapped gateway 502", fmt.Errorf("call failed: %w", &GatewayError{Status: 502}), true},
		{"connection refused string", errors.New("dial tcp: connection refused"), true},
		{"no such host string", errors.New("lookup foo: no such host"), true},
		{"i/o timeout string", errors.New("read: i/o timeout"), true},
		{"curl 28 timeout string", errors.New("curl: (28) Operation timed out"), true},
		{"PROVIDER_ERROR string", errors.New("PROVIDER_ERROR: upstream provider returned an error"), true},
		{"plain shape error", errors.New("schema violation: role missing required keys"), false},
		{"generic error", errors.New("something else entirely"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsUpstreamInfraError(c.err); got != c.want {
				t.Errorf("IsUpstreamInfraError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// Canceled must win even when wrapped alongside a deadline (caller gave up).
func TestIsUpstreamInfraError_CancelBeatsDeadline(t *testing.T) {
	// A context that is both canceled and past deadline: cancel wins → not a
	// model-health failure.
	if IsUpstreamInfraError(context.Canceled) {
		t.Fatal("context.Canceled must not count as an infra failure")
	}
	if !IsUpstreamInfraError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must count as an infra failure (model too slow)")
	}
}
