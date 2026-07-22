package executor

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRetryPromptPressureHigh(t *testing.T) {
	tests := []struct {
		name   string
		result string
		high   bool
	}{
		{
			name: "below threshold",
			result: `{
				"usage": {
					"max_prompt_tokens_estimate": 5000,
					"context_size": 12000,
					"max_tokens": 2000,
					"max_request_bytes": 15000
				}
			}`,
			high: false,
		},
		{
			name: "at shape threshold",
			result: `{
				"usage": {
					"max_prompt_tokens_estimate": 8500,
					"context_size": 12000,
					"max_tokens": 2000,
					"max_request_bytes": 25500
				}
			}`,
			high: true,
		},
		{
			name: "actual prompt tokens win over estimate",
			result: `{
				"usage": {
					"max_prompt_tokens_estimate": 9000,
					"max_prompt_tokens_actual": 4000,
					"context_size": 12000,
					"max_tokens": 2000,
					"max_request_bytes": 27000
				}
			}`,
			high: false,
		},
		{
			name: "missing context",
			result: `{
				"usage": {
					"max_prompt_tokens_estimate": 9000,
					"context_size": 0,
					"max_tokens": 2000
				}
			}`,
			high: false,
		},
		{
			name:   "invalid json",
			result: `{`,
			high:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			high, reason := retryPromptPressureHigh([]byte(tt.result), shapeRetrySuppressPressureRatio)
			if high != tt.high {
				t.Fatalf("retryPromptPressureHigh() high=%v, want %v (reason=%q)", high, tt.high, reason)
			}
			if high && !strings.Contains(reason, "0.850 >= 0.850") {
				t.Fatalf("reason did not include threshold comparison: %q", reason)
			}
		})
	}
}

func TestRetryPromptPressureHighHonorsEnvOverride(t *testing.T) {
	t.Setenv("VORNIK_RETRY_SUPPRESS_PRESSURE_RATIO", "0.5")
	high, reason := retryPromptPressureHigh([]byte(`{
		"usage": {
			"max_prompt_tokens_estimate": 5000,
			"context_size": 12000,
			"max_tokens": 2000,
			"max_request_bytes": 15000
		}
	}`), shapeRetrySuppressPressureRatio)
	if !high {
		t.Fatalf("expected env override to suppress at lower threshold, reason=%q", reason)
	}
}

func TestRetrySuppressPressureRatioIgnoresInvalidEnv(t *testing.T) {
	for _, value := range []string{"", "abc", "0", "1", "-0.1"} {
		t.Run(value, func(t *testing.T) {
			if value == "" {
				_ = os.Unsetenv("VORNIK_RETRY_SUPPRESS_PRESSURE_RATIO")
			} else {
				t.Setenv("VORNIK_RETRY_SUPPRESS_PRESSURE_RATIO", value)
			}
			if got := retrySuppressPressureRatio(0.83); got != 0.83 {
				t.Fatalf("retrySuppressPressureRatio()=%v, want default", got)
			}
		})
	}
}

func TestCostAwareRetrySuppressedErrorWrapsOriginal(t *testing.T) {
	orig := errors.New("PROVIDER_ERROR upstream 500")
	err := costAwareRetrySuppressedError("infra retry", "large request", orig)
	if !errors.Is(err, orig) {
		t.Fatalf("suppression error should wrap original error")
	}
	if !strings.Contains(err.Error(), "cost-aware retry suppressed for infra retry") {
		t.Fatalf("suppression error missing context: %v", err)
	}
}
