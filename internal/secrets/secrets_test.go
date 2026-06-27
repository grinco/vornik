package secrets

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDefaultDetector is the test helper every case uses. Defaults
// match production: curated patterns + default allowlist + entropy
// at 40-char/4.5-bit thresholds.
func newDefaultDetector(t *testing.T) *MultiDetector {
	t.Helper()
	d, err := NewMultiDetector(Config{})
	require.NoError(t, err)
	return d
}

// TestScan_KnownPatterns is the headline matrix: each curated
// pattern must fire on a representative positive sample. Negative
// samples ensure the regexes don't fire on ordinary code or prose.
// One row per pattern keeps this readable and the test names map
// 1:1 to the pattern names so a regression is easy to localize.
func TestScan_KnownPatterns(t *testing.T) {
	cases := []struct {
		name     string
		positive string
		negative string
		typeName string
	}{
		{
			// Note: the test fixture deliberately avoids "EXAMPLE"
			// because the default allowlist suppresses
			// documentation placeholders — using AWS's published
			// AKIAIOSFODNN7EXAMPLE here would be (correctly)
			// suppressed by the allowlist and confuse the test.
			name:     "aws_access_key",
			positive: "creds: AKIAQWERTYUIOPASDFGH in env",
			negative: "AKIAIO is too short for a key",
			typeName: "aws_access_key",
		},
		{
			name:     "aws_session_token",
			positive: "token=ASIAQWERTYUIOPASDFGH",
			negative: "ASIA stands for the region prefix",
			typeName: "aws_session_token",
		},
		{
			// Google API keys are AIza + exactly 35 chars; the
			// fixture below totals 39 chars including the prefix.
			name:     "google_api_key",
			positive: "key: AIzaSyA_QrSt1234567890abcdefghijklmnopq",
			negative: "AIza is just the prefix",
			typeName: "google_api_key",
		},
		{
			name:     "github_pat",
			positive: "Authorization: token ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			negative: "ghp_short",
			typeName: "github_pat",
		},
		{
			name:     "gitlab_pat",
			positive: "token glpat-abcdefABCDEF1234567z",
			negative: "glpat- prefix only",
			typeName: "gitlab_pat",
		},
		{
			name:     "slack_token",
			positive: "xoxb-1111111111-2222222222-AbCdEfGhIjKlMnOpQrStUvWxYz",
			negative: "xox- not a real prefix",
			typeName: "slack_token",
		},
		{
			name:     "openai_key",
			positive: "OPENAI_API_KEY=sk-proj1234567890abcdefghijklmnopqrstuv",
			negative: "sk-short",
			typeName: "openai_key",
		},
		{
			name:     "anthropic_key",
			positive: "ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234",
			negative: "sk-ant- prefix only",
			typeName: "anthropic_key",
		},
		{
			name:     "private_key_block",
			positive: "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
			negative: "BEGIN PUBLIC KEY isn't a private key",
			typeName: "private_key_block",
		},
		{
			name:     "jwt",
			positive: "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
			negative: "eyJ alone isn't a JWT",
			typeName: "jwt",
		},
		{
			name:     "connection_string",
			positive: "DATABASE_URL=postgres://admin:hunter2@db.internal:5432/prod",
			negative: "postgres://localhost/prod has no credentials",
			typeName: "connection_string",
		},
		{
			name:     "generic_kv",
			positive: `password = "hunter2hunter2hunter2"`,
			negative: `password = "x"`,
			typeName: "generic_kv",
		},
	}

	d := newDefaultDetector(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPos := d.Scan([]byte(tc.positive))
			require.NotEmpty(t, gotPos, "positive sample for %s must produce at least one finding", tc.name)
			foundType := false
			for _, f := range gotPos {
				if f.Type == tc.typeName {
					foundType = true
					break
				}
			}
			assert.True(t, foundType, "positive sample for %s must include a finding of that type — got %+v", tc.name, gotPos)

			gotNeg := d.Scan([]byte(tc.negative))
			for _, f := range gotNeg {
				assert.NotEqual(t, tc.typeName, f.Type,
					"negative sample for %s must NOT trigger that pattern — got %+v", tc.name, f)
			}
		})
	}
}

// TestScan_EntropyDetectsLongHighEntropyTokens — fallback for
// custom tokens the regex list doesn't know about. A 60-char
// base64-shaped string with high entropy must fire even when no
// regex matches.
func TestScan_EntropyDetectsLongHighEntropyTokens(t *testing.T) {
	d := newDefaultDetector(t)
	// 60-char random-looking base64-ish token, NOT matching any
	// curated pattern (no aws/github/etc. prefix).
	custom := "f7Hq2KpWnVx9rTzL3mYsBdGcJaQuXoCePlAjMfNbDvIeRtSuKgVwHnFiZyOhUx"
	findings := d.Scan([]byte("CUSTOM_TOKEN=" + custom + " in env"))
	require.NotEmpty(t, findings, "custom high-entropy token must fire entropy detector")
	saw := false
	for _, f := range findings {
		if f.Type == "entropy" || f.Type == "generic_kv" {
			saw = true
		}
	}
	assert.True(t, saw,
		"custom token must produce an entropy or generic_kv finding — got %+v", findings)
}

// TestScan_AllowlistSuppressesGitSHA — without the allowlist a
// 40-char hex string would fire the entropy detector (Shannon ≈
// 4.0 — borderline) or look generic-kv-ish. The default allowlist
// suppresses git SHAs explicitly so commit-discussing prose
// doesn't drown the operator in false positives.
func TestScan_AllowlistSuppressesGitSHA(t *testing.T) {
	d := newDefaultDetector(t)
	body := "see commit a1b2c3d4e5f6789012345678901234567890abcd for context"
	findings := d.Scan([]byte(body))
	for _, f := range findings {
		assert.NotEqual(t, "entropy", f.Type,
			"git SHA must be allowlisted — got entropy finding %+v", f)
	}
}

// TestScan_AllowlistSuppressesUUID — UUIDs frequently appear in
// task IDs, execution IDs, and request traces; they must not
// trigger the entropy or generic_kv detectors.
func TestScan_AllowlistSuppressesUUID(t *testing.T) {
	d := newDefaultDetector(t)
	body := "task uuid: 123e4567-e89b-12d3-a456-426614174000 ran in 1.2s"
	findings := d.Scan([]byte(body))
	for _, f := range findings {
		assert.NotEqual(t, "entropy", f.Type, "UUID must be allowlisted: %+v", f)
	}
}

// TestScan_AllowlistSuppressesPlaceholderTokens — LLMs frequently
// emit "your_api_key_here" or "<API_KEY>" in example prose. These
// must not fire the generic_kv pattern.
func TestScan_AllowlistSuppressesPlaceholderTokens(t *testing.T) {
	d := newDefaultDetector(t)
	body := `Replace your-api-key with your real value: api_key="<YOUR_API_KEY_HERE>"`
	findings := d.Scan([]byte(body))
	for _, f := range findings {
		assert.NotEqual(t, "generic_kv", f.Type,
			"documentation placeholder must be allowlisted: %+v", f)
	}
}

// TestScan_ShortBodySkipped — empty + tiny bodies short-circuit
// scanning. Most agent steps emit non-trivial bodies, but the
// guard saves cycles on empty result.json and protects against
// pathological "scan an empty string" calls in deep call paths.
func TestScan_ShortBodySkipped(t *testing.T) {
	d := newDefaultDetector(t)
	assert.Empty(t, d.Scan(nil))
	assert.Empty(t, d.Scan([]byte("")))
	assert.Empty(t, d.Scan([]byte("ok")))
}

// TestScan_NilDetectorSkipped — call sites can pass a nil detector
// when secrets are disabled in config. Scan must no-op rather than
// panic so the caller doesn't need a defensive nil check on every
// invocation.
func TestScan_NilDetectorSkipped(t *testing.T) {
	var d *MultiDetector
	assert.Empty(t, d.Scan([]byte("AKIAIOSFODNN7EXAMPLE in body")))
}

// TestScan_DeduplicatesExactOverlap — two patterns matching the
// same bytes (entropy + openai_key on the same token, say) must
// produce a single finding, not two. Operators see "1 leak" not
// "2 leaks" for what's actually one credential.
func TestScan_DeduplicatesExactOverlap(t *testing.T) {
	d := newDefaultDetector(t)
	// openai_key prefix + long enough to also pass entropy
	body := "key=sk-proj1234567890abcdefghijklmnopqrstuvwxyz0123456789ab"
	findings := d.Scan([]byte(body))
	// Should see one finding for this token, not multiple.
	require.NotEmpty(t, findings)
	starts := make(map[int]int)
	for _, f := range findings {
		starts[f.Start]++
	}
	for off, n := range starts {
		assert.LessOrEqual(t, n, 1, "offset %d had %d findings — overlap dedup is broken", off, n)
	}
}

// TestRedact_ReplacesAllFindings — the canonical redact round-
// trip: input contains 3 secrets, output substitutes all three
// with typed markers, no plaintext secret survives.
func TestRedact_ReplacesAllFindings(t *testing.T) {
	d := newDefaultDetector(t)
	body := []byte("aws=AKIAQWERTYUIOPASDFGH github=ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa openai=sk-proj1234567890abcdefghijklmnopqrstuv done")
	findings := d.Scan(body)
	require.GreaterOrEqual(t, len(findings), 3, "test fixture should produce 3+ findings; got %d", len(findings))

	redacted := Redact(body, findings)
	for _, f := range findings {
		assert.NotContains(t, string(redacted), f.Match,
			"redacted body must not contain plaintext finding %q", f.Match)
	}
	assert.Contains(t, string(redacted), "[REDACTED:aws_access_key]")
	assert.Contains(t, string(redacted), "[REDACTED:github_pat]")
	assert.Contains(t, string(redacted), "[REDACTED:openai_key]")
	assert.Contains(t, string(redacted), "done", "non-secret context must survive redaction")
}

// TestRedact_SecondScanFindsNothing — a redacted body must not
// itself trigger the detector on a re-scan. Catches a regression
// where the substitution marker happens to match a pattern.
func TestRedact_SecondScanFindsNothing(t *testing.T) {
	d := newDefaultDetector(t)
	body := []byte("token=ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	findings := d.Scan(body)
	require.NotEmpty(t, findings)
	redacted := Redact(body, findings)
	again := d.Scan(redacted)
	for _, f := range again {
		// Allow no findings, but if any fire they must be on
		// the marker itself, not on the original secret.
		assert.NotContains(t, f.Match, "ghp_",
			"re-scan must not re-detect the original secret in a redacted body: %+v", f)
	}
}

// TestRedact_EmptyFindings — passthrough when there's nothing to
// redact. Returns the original bytes unmodified so the caller
// doesn't pay for an allocation on the common no-secret path.
func TestRedact_EmptyFindings(t *testing.T) {
	body := []byte("clean body with no secrets")
	got := Redact(body, nil)
	assert.Equal(t, string(body), string(got))
}

// TestRedact_PreservesNonOverlappingPositions — when findings
// are in offset order, the surrounding text positions must be
// preserved exactly. Catches a bug where a redaction marker
// of length != original would shift later substitutions.
func TestRedact_PreservesNonOverlappingPositions(t *testing.T) {
	body := []byte("A AKIAQWERTYUIOPASDFGH B ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa C")
	d := newDefaultDetector(t)
	findings := d.Scan(body)
	redacted := Redact(body, findings)
	s := string(redacted)
	assert.True(t, strings.Contains(s, "A "), "leading context preserved")
	assert.True(t, strings.Contains(s, " B "), "middle context preserved")
	assert.True(t, strings.Contains(s, " C"), "trailing context preserved")
}

// TestCountByType — operator-facing summary used by the dashboard
// badge. Two openai_key findings + one github_pat → {openai_key:
// 2, github_pat: 1}.
func TestCountByType(t *testing.T) {
	findings := []Finding{
		{Type: "openai_key", Match: "sk-1"},
		{Type: "openai_key", Match: "sk-2"},
		{Type: "github_pat", Match: "ghp_x"},
	}
	got := CountByType(findings)
	assert.Equal(t, 2, got["openai_key"])
	assert.Equal(t, 1, got["github_pat"])
}

// TestNewMultiDetector_RejectsBadPattern — operator config typo
// must surface at startup, not on the first scan. A bad regex
// returns an error from New so the daemon refuses to come up
// with a misconfigured detector.
func TestNewMultiDetector_RejectsBadPattern(t *testing.T) {
	_, err := NewMultiDetector(Config{
		Patterns: []Pattern{
			{Name: "broken", Regex: `(unclosed`},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken")
}

// TestNewMultiDetector_RejectsBadAllowlist — same shape on the
// allowlist side. A bad regex must fail-fast at startup.
func TestNewMultiDetector_RejectsBadAllowlist(t *testing.T) {
	_, err := NewMultiDetector(Config{
		Allowlist: []string{`[invalid`},
	})
	require.Error(t, err)
}

// TestAction_IsValid — config validation guard. Three valid values;
// anything else (typo, empty) must coerce at the loader level
// rather than silently disable enforcement.
func TestAction_IsValid(t *testing.T) {
	assert.True(t, ActionDetect.IsValid())
	assert.True(t, ActionRedact.IsValid())
	assert.True(t, ActionBlock.IsValid())
	assert.False(t, Action("").IsValid())
	assert.False(t, Action("yolo").IsValid())
}

// TestShannonEntropy — sanity check the entropy fn isolates from
// the detector. "aaaa" has 0 entropy; uniform random looks ~5+.
func TestShannonEntropy(t *testing.T) {
	assert.InDelta(t, 0.0, shannonEntropy([]byte("aaaa")), 0.001)
	assert.Greater(t, shannonEntropy([]byte("ABcdef0123456789!@#$%^&*")), 4.0)
}

// TestScan_PrivateKeyHeaderInProse — the private-key matcher must
// fire on a multi-line PEM block embedded in markdown / a
// chat message, even though the body has only the header line.
// This is the highest-risk leak vector (entire keys leak in agent
// outputs all the time) and worth a dedicated test.
func TestScan_PrivateKeyHeaderInProse(t *testing.T) {
	d := newDefaultDetector(t)
	body := "Here's the deploy key:\n\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAA...\n-----END OPENSSH PRIVATE KEY-----\n"
	findings := d.Scan([]byte(body))
	saw := false
	for _, f := range findings {
		if f.Type == "private_key_block" {
			saw = true
		}
	}
	assert.True(t, saw, "PEM private key header in prose must fire — got %+v", findings)
}

// TestResolveAction_MemoryScanNonDisableable — the memory checkpoint clamps a
// `detect` override up to `redact` (secret scanning there is non-disableable),
// unless the explicit VORNIK_ALLOW_UNSCANNED_MEMORY escape hatch is set. Other
// checkpoints honor `detect` as-is.
func TestResolveAction_MemoryScanNonDisableable(t *testing.T) {
	override := map[string]Action{
		CheckpointMemory:    ActionDetect,
		CheckpointToolAudit: ActionDetect,
	}

	// Memory: detect is clamped to redact by default.
	if got := ResolveAction(CheckpointMemory, override); got != ActionRedact {
		t.Errorf("memory detect override = %q, want clamped to redact", got)
	}
	// Other checkpoints are NOT clamped.
	if got := ResolveAction(CheckpointToolAudit, override); got != ActionDetect {
		t.Errorf("tool_audit detect override = %q, want detect (not clamped)", got)
	}
	// Memory default (no override) is redact.
	if got := ResolveAction(CheckpointMemory, nil); got != ActionRedact {
		t.Errorf("memory default = %q, want redact", got)
	}
	// Block override on memory is honored (stricter than redact).
	if got := ResolveAction(CheckpointMemory, map[string]Action{CheckpointMemory: ActionBlock}); got != ActionBlock {
		t.Errorf("memory block override = %q, want block", got)
	}

	// Explicit escape hatch lets detect through.
	t.Setenv("VORNIK_ALLOW_UNSCANNED_MEMORY", "1")
	if got := ResolveAction(CheckpointMemory, override); got != ActionDetect {
		t.Errorf("memory detect with escape hatch = %q, want detect", got)
	}
}
