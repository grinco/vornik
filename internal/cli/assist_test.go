package cli

import (
	"testing"
	"time"
)

func TestAssistClientFromEnv_UsesLongDefaultTimeout(t *testing.T) {
	t.Setenv("VORNIK_API_TIMEOUT", "")

	c := assistClientFromEnv()
	if c.httpClient.Timeout != assistAPITimeout {
		t.Fatalf("assist timeout = %s, want %s", c.httpClient.Timeout, assistAPITimeout)
	}
}

func TestAssistClientFromEnv_HonorsExplicitTimeout(t *testing.T) {
	t.Setenv("VORNIK_API_TIMEOUT", "2m")

	c := assistClientFromEnv()
	if c.httpClient.Timeout != 2*time.Minute {
		t.Fatalf("assist timeout = %s, want 2m", c.httpClient.Timeout)
	}
}
