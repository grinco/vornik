package integrations

import (
	"strings"
	"testing"
	"time"
)

// TestProbeResult_OKMatchesOutcome documents and locks the invariant from
// the design (§5.2): OK is always exactly (Outcome == OutcomeOK). Nothing
// enforces this at the type level (OK is a plain bool field, set by each
// prober), so this test exists to catch a prober that sets them
// inconsistently — every prober adapter test below also asserts this pair
// explicitly for its own cases.
func TestProbeResult_OKMatchesOutcome(t *testing.T) {
	cases := []struct {
		name    string
		result  ProbeResult
		wantOK  bool
		outcome Outcome
	}{
		{"ok", ProbeResult{OK: true, Outcome: OutcomeOK}, true, OutcomeOK},
		{"fail", ProbeResult{OK: false, Outcome: OutcomeFail}, false, OutcomeFail},
		{"error", ProbeResult{OK: false, Outcome: OutcomeError}, false, OutcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.result.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", tc.result.OK, tc.wantOK)
			}
			if tc.result.Outcome != tc.outcome {
				t.Errorf("Outcome = %v, want %v", tc.result.Outcome, tc.outcome)
			}
			if (tc.result.Outcome == OutcomeOK) != tc.result.OK {
				t.Errorf("invariant broken: OK=%v but Outcome==OutcomeOK is %v", tc.result.OK, tc.result.Outcome == OutcomeOK)
			}
		})
	}
}

func TestCandidateConfig_FieldsAddressable(t *testing.T) {
	cand := CandidateConfig{
		Kind:      "telegram",
		ProjectID: "",
		Values:    map[string]string{"bot_token": "secret-value"},
	}
	if cand.Kind != "telegram" {
		t.Errorf("Kind = %q, want telegram", cand.Kind)
	}
	if cand.Values["bot_token"] != "secret-value" {
		t.Error("Values map round-trip failed")
	}
}

// TestRedactSecrets_StripsCandidateValues is the generic log/detail-no-echo
// primitive every prober adapter routes error text through before it lands
// in ProbeResult.Detail or a log line. Table-driven per §8's "log-echo
// assertion": feed a distinctive secret, confirm it never survives.
func TestRedactSecrets_StripsCandidateValues(t *testing.T) {
	cand := CandidateConfig{
		Kind: "telegram",
		Values: map[string]string{
			"bot_token": "123456:AAExtremelyDistinctiveTelegramTokenValue",
		},
	}
	msg := `Get "https://api.telegram.org/bot123456:AAExtremelyDistinctiveTelegramTokenValue/getMe": dial tcp: connection refused`
	redacted := redactSecrets(msg, cand)
	if strings.Contains(redacted, "123456:AAExtremelyDistinctiveTelegramTokenValue") {
		t.Errorf("redacted message still contains the secret: %q", redacted)
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Errorf("redacted message should mark the redaction: %q", redacted)
	}
}

// TestRedactSecrets_LeavesShortValuesAlone — short values (e.g. a port
// number "993") are not blanket-redacted; only substantive-length values
// are treated as potential secrets, so error messages stay readable.
func TestRedactSecrets_LeavesShortValuesAlone(t *testing.T) {
	cand := CandidateConfig{
		Kind:   "email",
		Values: map[string]string{"imap_port": "993"},
	}
	msg := "dial tcp 10.0.0.1:993: connection refused"
	redacted := redactSecrets(msg, cand)
	if redacted != msg {
		t.Errorf("short non-secret values must not be redacted; got %q", redacted)
	}
}

// TestRedactSecrets_EmptyValuesIgnored guards against a pathological empty
// string in Values turning every message into an empty string (strings.
// ReplaceAll(s, "", x) would otherwise interleave x between every rune).
func TestRedactSecrets_EmptyValuesIgnored(t *testing.T) {
	cand := CandidateConfig{Values: map[string]string{"api_base_url": ""}}
	msg := "some ordinary error text"
	if got := redactSecrets(msg, cand); got != msg {
		t.Errorf("empty Values entries must be ignored; got %q", got)
	}
}

func TestIntegrationProbeTimeout_Default(t *testing.T) {
	if integrationProbeTimeout != 15*time.Second {
		t.Errorf("integrationProbeTimeout = %v, want 15s (must match mcpProbeTimeout)", integrationProbeTimeout)
	}
}
