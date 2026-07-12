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

// TestIsContextOverflow is the regression test for the 2026-07-12 deep-research
// retry storms (task_20260712145902_18667395d2826b72): glm-5 on Bedrock rejected
// a 194561-token prompt with a deterministic ValidationException, the chat proxy
// sanitized it to PROVIDER_ERROR, and the executor's infra ladder re-sent the
// identical prompt ~12 times per execution. Overflow must classify as
// non-infra so the ladder never engages.
func TestIsContextOverflow(t *testing.T) {
	// The literal Bedrock error observed in the incident (abridged).
	bedrockErr := errors.New(`bedrock Converse(zai.glm-5): operation error Bedrock Runtime: Converse, https response error StatusCode: 400, ValidationException: The model returned the following errors: {"error":{"code":"validation_error","message":"ErrorEvent { error: APIError { type: \"BadRequestError\", code: Some(400), message: \"This model's maximum context length is 202752 tokens. However, you requested 8192 output tokens and your prompt contains at least 194561 input tokens...\" } }","type":"invalid_request_error"}}`)

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"bedrock glm-5 incident error", bedrockErr, true},
		{"openai error code", errors.New("400 context_length_exceeded"), true},
		{"anthropic bedrock phrasing", errors.New("ValidationException: Input is too long for requested model"), true},
		{"proxy sanitized code in agent result", errors.New("agent reported FAILED: LLM call failed: CONTEXT_OVERFLOW"), true},
		{"plain provider outage is not overflow", errors.New("PROVIDER_ERROR upstream provider returned an error"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsContextOverflow(tc.err); got != tc.want {
				t.Errorf("IsContextOverflow(%v) = %v, want %v", tc.err, got, tc.want)
			}
			// Overflow must never double as an infra failure.
			if tc.want && IsUpstreamInfraError(tc.err) {
				t.Errorf("IsUpstreamInfraError must be false for context overflow: %v", tc.err)
			}
		})
	}
}
