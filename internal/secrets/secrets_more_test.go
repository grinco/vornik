package secrets

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScan_AWSSecretKey — the aws_secret_key pattern is label-anchored
// (an aws_secret/sk label followed by a 40-char base64-shaped value).
// It's absent from the headline matrix because that matrix uses
// label-free prefixes; this dedicated case proves the labelled form
// fires and that a 40-char value WITHOUT the aws_secret label does not.
func TestScan_AWSSecretKey(t *testing.T) {
	d := newDefaultDetector(t)
	pos := `aws_secret_access_key = "abcdefABCDEF1234567890abcdefABCDEF123456"`
	findings := d.Scan([]byte(pos))
	saw := false
	for _, f := range findings {
		if f.Type == "aws_secret_key" {
			saw = true
		}
	}
	assert.True(t, saw, "labelled aws secret must fire aws_secret_key — got %+v", findings)

	// A bare 40-char base64-shaped value with no aws_secret label must
	// NOT trip aws_secret_key (it may legitimately fire entropy, which
	// is a different, expected detector).
	neg := "value: abcdefABCDEF1234567890abcdefABCDEF123456 done"
	for _, f := range d.Scan([]byte(neg)) {
		assert.NotEqual(t, "aws_secret_key", f.Type,
			"unlabelled 40-char value must not fire aws_secret_key — got %+v", f)
	}
}

// TestScan_ConnectionStringSchemes — the connection_string pattern
// claims postgres/postgresql/mysql/mongodb/redis/amqp. The headline
// matrix only exercises postgres; this proves the other schemes fire
// AND that an http(s) URL with embedded userinfo does NOT (it's not a
// credential-bearing DB/queue URI the pattern targets).
func TestScan_ConnectionStringSchemes(t *testing.T) {
	d := newDefaultDetector(t)
	for _, uri := range []string{
		"mysql://admin:hunter2@db.internal:3306/prod",
		"mongodb://admin:hunter2@db.internal:27017/prod",
		"redis://admin:hunter2@cache.internal:6379",
		"amqp://admin:hunter2@mq.internal:5672",
		"postgresql://admin:hunter2@db.internal:5432/prod",
	} {
		findings := d.Scan([]byte("DATABASE_URL=" + uri + " trailing"))
		saw := false
		for _, f := range findings {
			if f.Type == "connection_string" {
				saw = true
			}
		}
		assert.True(t, saw, "%q must fire connection_string — got %+v", uri, findings)
	}

	// https with userinfo is not a targeted DB/queue scheme.
	for _, f := range d.Scan([]byte("see https://user:pass@example.com/path for the doc page")) {
		assert.NotEqual(t, "connection_string", f.Type,
			"http(s) URL must not fire connection_string — got %+v", f)
	}
}

// TestScan_MatchHoldsRawSecretButRedactDoesNot — the security
// invariant of the whole package: Finding.Match carries the raw
// matched bytes (it must, for offset bookkeeping), but the *redacted
// output* — the thing that gets persisted/displayed — must never
// contain that raw secret. This is the leak-prevention contract.
func TestScan_MatchHoldsRawSecretButRedactDoesNot(t *testing.T) {
	d := newDefaultDetector(t)
	secret := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	body := []byte("token=" + secret)
	findings := d.Scan(body)
	require.NotEmpty(t, findings)

	// Match holds the raw secret (offset bookkeeping needs it).
	foundRaw := false
	for _, f := range findings {
		if f.Type == "github_pat" {
			assert.Equal(t, secret, f.Match, "Finding.Match must carry the exact matched bytes")
			foundRaw = true
		}
	}
	require.True(t, foundRaw)

	// The redacted output — the persisted/displayed artifact — must
	// not leak it.
	redacted := string(Redact(body, findings))
	assert.NotContains(t, redacted, secret, "redacted output must not contain the raw secret")
	assert.NotContains(t, redacted, "ghp_", "not even the secret prefix should survive")
	assert.Contains(t, redacted, "[REDACTED:github_pat]")
}

// TestScan_EntropyDisabled — operators with high entropy false-positive
// rates disable the entropy pass and rely on regexes alone. A custom
// high-entropy token that matches no curated regex must then produce
// NO finding.
func TestScan_EntropyDisabled(t *testing.T) {
	d, err := NewMultiDetector(Config{EntropyDisabled: true})
	require.NoError(t, err)
	// 60-char high-entropy token, no curated prefix; sits in bare
	// context (no api_key/token label) so generic_kv won't catch it.
	custom := "f7Hq2KpWnVx9rTzL3mYsBdGcJaQuXoCePlAjMfNbDvIeRtSuKgVwHnFiZyOhUx"
	findings := d.Scan([]byte("blob " + custom + " blob"))
	assert.Empty(t, findings, "entropy disabled: bare high-entropy token must not fire — got %+v", findings)

	// Sanity: a curated regex still fires with entropy disabled.
	reg := d.Scan([]byte("creds AKIAQWERTYUIOPASDFGH here"))
	saw := false
	for _, f := range reg {
		if f.Type == "aws_access_key" {
			saw = true
		}
	}
	assert.True(t, reg != nil && saw, "regex pass must still work with entropy disabled — got %+v", reg)
}

// TestScan_EntropyMinBitsThreshold — a long but LOW-entropy token
// (single repeated char) sits above the length floor yet below the
// per-char bit floor, so it must not fire. Guards against the entropy
// detector degenerating into "any long base64-ish run".
func TestScan_EntropyMinBitsThreshold(t *testing.T) {
	d := newDefaultDetector(t)
	low := strings.Repeat("a", 50) // 50 chars, entropy 0 bits
	for _, f := range d.Scan([]byte("pad " + low + " pad")) {
		assert.NotEqual(t, "entropy", f.Type,
			"low-entropy long run must not fire entropy — got %+v", f)
	}
}

// TestScan_EntropyMinLenThreshold — a high-entropy token SHORTER than
// the length floor must not fire entropy. Below the floor the entropy
// token regex never even constructs a candidate, so short random-looking
// fragments in prose stay quiet.
func TestScan_EntropyMinLenThreshold(t *testing.T) {
	d := newDefaultDetector(t)
	// 20-char high-entropy token — well under the 40-char default floor.
	short := "Xq7Lm2Pz9Rt4Vw1Ks6B"
	for _, f := range d.Scan([]byte("frag " + short + " frag here padding")) {
		assert.NotEqual(t, "entropy", f.Type,
			"sub-floor token must not fire entropy — got %+v", f)
	}
}

// TestScan_CustomEntropyThresholds — lowering EntropyMinLen lets a
// shorter high-entropy token fire that the default floor would miss.
// Proves the knobs are actually wired into the token regex, not just
// stored.
func TestScan_CustomEntropyThresholds(t *testing.T) {
	short := "Xq7Lm2Pz9Rt4Vw1Ks6B" // 19 chars, high entropy
	def := newDefaultDetector(t)
	defFindings := def.Scan([]byte("frag " + short + " frag here padding"))
	for _, f := range defFindings {
		require.NotEqual(t, "entropy", f.Type, "default floor should miss this short token")
	}

	tuned, err := NewMultiDetector(Config{EntropyMinLen: 16, EntropyMinBits: 3.5})
	require.NoError(t, err)
	findings := tuned.Scan([]byte("frag " + short + " frag here padding"))
	saw := false
	for _, f := range findings {
		if f.Type == "entropy" {
			saw = true
		}
	}
	assert.True(t, saw, "lowered EntropyMinLen must let short token fire — got %+v", findings)
}

// TestNewMultiDetector_NonPositiveThresholdsFallBackToDefaults — a
// negative/zero EntropyMinLen or EntropyMinBits must NOT produce a
// degenerate detector (e.g. a 0-length token regex matching every
// byte). Both clamp to the documented defaults (40 / 4.5).
func TestNewMultiDetector_NonPositiveThresholdsFallBackToDefaults(t *testing.T) {
	d, err := NewMultiDetector(Config{EntropyMinLen: -5, EntropyMinBits: -1})
	require.NoError(t, err)
	assert.Equal(t, 40, d.entropyMinLen)
	assert.InDelta(t, 4.5, d.entropyMinBits, 0.0001)

	// And behaviorally: a short token still doesn't fire (no degenerate
	// match-everything regex).
	for _, f := range d.Scan([]byte("frag Xq7Lm2Pz9Rt4Vw1Ks6B frag padding here")) {
		assert.NotEqual(t, "entropy", f.Type, "fallback floor must still gate short tokens")
	}
}

// TestScan_EntropyOverlapGuard — when a curated regex already covers a
// token, the entropy pass must NOT also fire on the same byte range.
// Operators should see one finding per secret, not a regex+entropy
// double-count. (Distinct from TestScan_DeduplicatesExactOverlap, which
// covers the post-sort dedup; this covers the pre-emptive overlap skip.)
func TestScan_EntropyOverlapGuard(t *testing.T) {
	d := newDefaultDetector(t)
	// openai_key prefix + long high-entropy tail: would independently
	// satisfy the entropy detector if not guarded.
	body := "sk-Xq7Lm2Pz9Rt4Vw1Ks6BdGcJaQuXoCePlAjMfNbDvIeRtSuKgVwHn"
	findings := d.Scan([]byte(body))
	require.NotEmpty(t, findings)
	entropyCount, openaiCount := 0, 0
	for _, f := range findings {
		switch f.Type {
		case "entropy":
			entropyCount++
		case "openai_key":
			openaiCount++
		}
	}
	assert.GreaterOrEqual(t, openaiCount, 1, "openai_key must fire — got %+v", findings)
	assert.Equal(t, 0, entropyCount, "entropy must not double-fire on a regex-covered token — got %+v", findings)
}

// TestScan_AllowlistSuppressesRegexFinding — allowlist suppression is
// applied AFTER both regex and entropy passes, so a narrowly-allowlisted
// value suppresses even a deterministic regex hit. Proves an operator
// can whitelist one specific known-safe key shape without disabling the
// whole pattern.
func TestScan_AllowlistSuppressesRegexFinding(t *testing.T) {
	d, err := NewMultiDetector(Config{Allowlist: []string{`AKIAQWERTYUIOPASDFGH`}})
	require.NoError(t, err)
	findings := d.Scan([]byte("creds AKIAQWERTYUIOPASDFGH here in env config"))
	for _, f := range findings {
		assert.NotEqual(t, "aws_access_key", f.Type,
			"allowlisted AWS key must be suppressed even though regex matches — got %+v", f)
	}
}

// TestScan_DataImageAllowlisted — base64-embedded markdown images
// (data:image/...;base64,...) are common in agent output and would
// otherwise light up the entropy detector. The default allowlist
// suppresses them.
func TestScan_DataImageAllowlisted(t *testing.T) {
	d := newDefaultDetector(t)
	body := "![logo](data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==)"
	for _, f := range d.Scan([]byte(body)) {
		assert.NotEqual(t, "entropy", f.Type, "data:image base64 must be allowlisted — got %+v", f)
	}
}

// TestScan_CustomPatternAppended — operator config can add custom
// patterns; the detector must compile and fire them with the supplied
// symbolic Name as Finding.Type (so the redaction marker and metrics
// carry the operator's label).
func TestScan_CustomPatternAppended(t *testing.T) {
	d, err := NewMultiDetector(Config{
		Patterns: []Pattern{{Name: "acme_token", Regex: `\bXSEC-[0-9]{8}\b`}},
	})
	require.NoError(t, err)
	findings := d.Scan([]byte("here is XSEC-12345678 token value"))
	require.Len(t, findings, 1)
	assert.Equal(t, "acme_token", findings[0].Type)
	assert.Equal(t, "XSEC-12345678", findings[0].Match)

	redacted := string(Redact([]byte("here is XSEC-12345678 token value"), findings))
	assert.Contains(t, redacted, "[REDACTED:acme_token]")
	assert.NotContains(t, redacted, "XSEC-12345678")
}

// TestRedact_SkipsMalformedRanges — Redact is defensive against
// hand-constructed Findings: a range whose End exceeds len(text), an
// inverted range (End<Start), or an out-of-order finding (Start before
// the cursor) is skipped rather than panicking or corrupting output.
// Scan-emitted findings never take these branches, but callers may pass
// findings through filters that reorder/trim them.
func TestRedact_SkipsMalformedRanges(t *testing.T) {
	body := []byte("0123456789")

	// End beyond len: skipped, body returned unchanged.
	assert.Equal(t, "0123456789",
		string(Redact(body, []Finding{{Type: "x", Start: 2, End: 99}})))

	// Inverted range (End < Start): skipped.
	assert.Equal(t, "0123456789",
		string(Redact(body, []Finding{{Type: "x", Start: 5, End: 2}})))

	// Out-of-order: the second finding starts before the cursor left by
	// the first, so it's skipped; only the first is applied.
	got := string(Redact(body, []Finding{
		{Type: "a", Start: 4, End: 6},
		{Type: "b", Start: 1, End: 3},
	}))
	assert.Equal(t, "0123[REDACTED:a]6789", got)
}

// TestRedact_MultipleValidRangesInOrder — the happy path with two
// well-formed, ordered, non-overlapping findings: both are replaced and
// the inter-finding context survives intact. Complements the
// Scan-driven redact tests with a hand-built deterministic fixture.
func TestRedact_MultipleValidRangesInOrder(t *testing.T) {
	body := []byte("0123456789")
	got := string(Redact(body, []Finding{
		{Type: "a", Start: 1, End: 3},
		{Type: "b", Start: 5, End: 7},
	}))
	assert.Equal(t, "0[REDACTED:a]34[REDACTED:b]789", got)
}

// TestSpanList_OverlapsHalfOpen — the entropy-pass overlap guard treats
// spans as half-open [Start, End). A candidate that merely *touches* a
// regex span at the boundary does not overlap (so adjacent distinct
// tokens are each scanned); any interior intersection does.
func TestSpanList_OverlapsHalfOpen(t *testing.T) {
	sl := spanIndex([]Finding{{Start: 5, End: 10}})
	assert.False(t, sl.overlaps(0, 5), "touching at left boundary is not overlap")
	assert.True(t, sl.overlaps(0, 6), "crossing into span overlaps")
	assert.True(t, sl.overlaps(7, 8), "fully inside overlaps")
	assert.True(t, sl.overlaps(9, 20), "tail overlap counts")
	assert.False(t, sl.overlaps(10, 12), "touching at right boundary is not overlap")
}

// TestResolveAction_UnknownCheckpointAndInvalidOverride — checkpoints
// not present in the default map fall through to ActionDetect (the
// safe, audit-visible default), and an INVALID override action string
// (a config typo) is ignored in favor of the default rather than
// silently disabling enforcement.
func TestResolveAction_UnknownCheckpointAndInvalidOverride(t *testing.T) {
	// Unknown checkpoint, no override -> Detect.
	assert.Equal(t, ActionDetect, ResolveAction("not_a_real_checkpoint", nil))

	// Invalid override action on a known checkpoint -> ignored, falls
	// back to the compiled default (webhook defaults to Block).
	override := map[string]Action{CheckpointWebhook: Action("garbage")}
	assert.Equal(t, ActionBlock, ResolveAction(CheckpointWebhook, override),
		"invalid override must not override the default")

	// Valid override on a known checkpoint is honored.
	override2 := map[string]Action{CheckpointResultJSON: ActionBlock}
	assert.Equal(t, ActionBlock, ResolveAction(CheckpointResultJSON, override2))
}

// TestDefaultCheckpoints_WebhookBlocksMemoryRedacts — pins the two
// security-load-bearing default choices: inbound webhook payloads are
// BLOCKED (a secret there is a misconfiguration to surface, not rewrite)
// and memory is REDACTED (chunks live forever and surface via search).
// A regression flipping either default to Detect would silently admit
// plaintext at the worst boundaries.
func TestDefaultCheckpoints_WebhookBlocksMemoryRedacts(t *testing.T) {
	defs := DefaultCheckpoints()
	assert.Equal(t, ActionBlock, defs[CheckpointWebhook], "webhook must default to block")
	assert.Equal(t, ActionRedact, defs[CheckpointMemory], "memory must default to redact")
	// tool_audit moved from detect to redact (no raw creds at rest).
	assert.Equal(t, ActionRedact, defs[CheckpointToolAudit], "tool_audit must default to redact")
	// Every default action is itself valid.
	for cp, a := range defs {
		assert.True(t, a.IsValid(), "default action for %s (%q) must be valid", cp, a)
	}
}

// TestResolveAction_MemoryEscapeHatchValues — the VORNIK_ALLOW_UNSCANNED_MEMORY
// escape hatch only opens for affirmative truthy values; anything else
// (empty, "0", "false", arbitrary junk, whitespace-padded "off") keeps the
// non-disableable redact clamp on the memory checkpoint. A loose parse
// here would be a credential-exposure footgun.
func TestResolveAction_MemoryEscapeHatchValues(t *testing.T) {
	override := map[string]Action{CheckpointMemory: ActionDetect}

	// Truthy values open the hatch (detect honored).
	for _, v := range []string{"1", "true", "yes", "on", "  TRUE  ", "On"} {
		t.Setenv("VORNIK_ALLOW_UNSCANNED_MEMORY", v)
		assert.Equal(t, ActionDetect, ResolveAction(CheckpointMemory, override),
			"value %q must open the escape hatch", v)
	}

	// Non-truthy values keep the clamp (detect -> redact).
	for _, v := range []string{"", "0", "false", "no", "off", "enabled", "2", "tru"} {
		t.Setenv("VORNIK_ALLOW_UNSCANNED_MEMORY", v)
		assert.Equal(t, ActionRedact, ResolveAction(CheckpointMemory, override),
			"value %q must keep the redact clamp", v)
	}
}
