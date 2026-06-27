package memory

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

func newSecretsDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return d
}

func validCandidate() *IngestCandidate {
	return &IngestCandidate{
		ProjectID:        "p1",
		SourceArtifactID: "a1",
		SourceName:       "doc.md",
		ProducerRole:     "researcher",
		Content:          strings.Repeat("The quick brown fox jumps over the lazy dog. ", 5),
	}
}

func TestDefaultGateConfig(t *testing.T) {
	c := DefaultGateConfig()
	if c.MinContentChars != 64 || c.MinContentWords != 10 || c.TruncationToleranceFraction != 0.05 {
		t.Fatalf("default cfg drifted: %+v", c)
	}
}

func TestEnsureContentHash(t *testing.T) {
	c := &IngestCandidate{Content: "hello"}
	EnsureContentHash(c)
	if c.ContentHash == "" {
		t.Fatal("hash not set")
	}
	pre := c.ContentHash
	// Idempotent: second call keeps the existing hash.
	c.Content = "different"
	EnsureContentHash(c)
	if c.ContentHash != pre {
		t.Fatal("hash should be idempotent when already set")
	}
	// nil-safe.
	EnsureContentHash(nil)
}

func TestSchemaMatchGate(t *testing.T) {
	if got := SchemaMatchGate(nil); got.Action != GateReject {
		t.Fatalf("nil: %+v", got)
	}
	cases := []struct {
		name   string
		mutate func(*IngestCandidate)
	}{
		{"no project", func(c *IngestCandidate) { c.ProjectID = "" }},
		{"no artifact", func(c *IngestCandidate) { c.SourceArtifactID = "" }},
		{"no role", func(c *IngestCandidate) { c.ProducerRole = "" }},
		{"no content", func(c *IngestCandidate) { c.Content = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCandidate()
			tc.mutate(c)
			out := SchemaMatchGate(c)
			if out.Action != GateReject {
				t.Fatalf("want reject, got %+v", out)
			}
		})
	}
	if out := SchemaMatchGate(validCandidate()); out.Action != GateAllow {
		t.Fatalf("happy path: %+v", out)
	}
}

func TestProvenanceCompleteGate(t *testing.T) {
	c := validCandidate()
	if out := ProvenanceCompleteGate(c); out.Action != GateAllow {
		t.Fatalf("happy: %+v", out)
	}
	c.SourceArtifactID = ""
	if out := ProvenanceCompleteGate(c); out.Action != GateReject {
		t.Fatalf("missing artifact: %+v", out)
	}
	c = validCandidate()
	c.ProducerRole = ""
	if out := ProvenanceCompleteGate(c); out.Action != GateReject {
		t.Fatalf("missing role: %+v", out)
	}
}

// TestProvenanceCompleteGate_CompanionCarveOut — LLD 22 lets
// companion-origin candidates skip the artifact-ID requirement
// because their lineage lives in producer_role + source_name. A
// non-companion role still requires the full triple.
func TestProvenanceCompleteGate_CompanionCarveOut(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		wantAct GateAction
	}{
		{"claude-code", "companion:claude-code", GateAllow},
		{"codex", "companion:codex", GateAllow},
		{"hyphenated", "companion:gemini-cli", GateAllow},
		{"empty-suffix", "companion:", GateReject},
		{"uppercase", "companion:Claude", GateReject},
		{"non-companion", "researcher", GateReject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCandidate()
			c.SourceArtifactID = "" // no artifact for companion deposits
			c.ProducerRole = tc.role
			if got := ProvenanceCompleteGate(c).Action; got != tc.wantAct {
				t.Errorf("ProvenanceCompleteGate action = %v, want %v", got, tc.wantAct)
			}
		})
	}
}

func TestClassKnownGate(t *testing.T) {
	// Empty class is fine — pipeline assigns.
	c := validCandidate()
	if out := ClassKnownGate(c); out.Action != GateAllow || out.Detail != "" {
		t.Fatalf("empty: %+v", out)
	}
	// Known class.
	c.ProposedClass = ClassResearch
	if out := ClassKnownGate(c); out.Action != GateAllow || out.Detail != "" {
		t.Fatalf("known: %+v", out)
	}
	// Unknown class downgrades with a detail.
	c.ProposedClass = "made-up"
	out := ClassKnownGate(c)
	if out.Action != GateAllow || !strings.Contains(out.Detail, "unknown class") {
		t.Fatalf("unknown: %+v", out)
	}
}

func TestSecretScanGate_AllowPaths(t *testing.T) {
	// Nil detector → Allow.
	out := SecretScanGate(&IngestCandidate{Content: "hi"}, GateConfig{})
	if out.Action != GateAllow {
		t.Fatalf("nil detector: %+v", out)
	}
	// Empty content → Allow.
	cfg := GateConfig{SecretsDetector: newSecretsDetector(t)}
	out = SecretScanGate(&IngestCandidate{}, cfg)
	if out.Action != GateAllow {
		t.Fatalf("empty content: %+v", out)
	}
	// Clean content → Allow.
	c := &IngestCandidate{Content: "just plain notes, nothing secret here"}
	out = SecretScanGate(c, cfg)
	if out.Action != GateAllow {
		t.Fatalf("clean content: %+v", out)
	}
}

func TestSecretScanGate_RedactDetectBlock(t *testing.T) {
	det := newSecretsDetector(t)
	secret := "sk-proj1234567890abcdefghijklmnopqrstuv"
	c := &IngestCandidate{Content: "leak: " + secret}

	// Default → Redact.
	cfgRedact := GateConfig{SecretsDetector: det, SecretsActions: map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionRedact,
	}}
	out := SecretScanGate(c, cfgRedact)
	if out.Action != GateRedact || !strings.Contains(out.NewContent, "[REDACTED:") {
		t.Fatalf("redact: %+v", out)
	}

	// Block → Quarantine.
	cfgBlock := GateConfig{SecretsDetector: det, SecretsActions: map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionBlock,
	}}
	out = SecretScanGate(c, cfgBlock)
	if out.Action != GateQuarantine || !strings.Contains(out.Detail, "block") {
		t.Fatalf("block: %+v", out)
	}

	// Detect on the memory checkpoint is NON-DISABLEABLE: ResolveAction clamps
	// it up to Redact (secrets can't be admitted to durable memory by config).
	cfgDetect := GateConfig{SecretsDetector: det, SecretsActions: map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionDetect,
	}}
	out = SecretScanGate(c, cfgDetect)
	if out.Action != GateRedact || !strings.Contains(out.NewContent, "[REDACTED:") {
		t.Fatalf("detect (should clamp to redact): %+v", out)
	}

	// The explicit escape hatch restores detect-only (Allow, no NewContent).
	t.Setenv("VORNIK_ALLOW_UNSCANNED_MEMORY", "1")
	out = SecretScanGate(c, cfgDetect)
	if out.Action != GateAllow || !strings.Contains(out.Detail, "detect-only") || out.NewContent != "" {
		t.Fatalf("detect with escape hatch: %+v", out)
	}
}

// TestSecretScanGate_RejectOnHeavyRedaction pins the LLD 22 § Risks
// contract: a deposit that is mostly credentials, once redacted, is
// rejected as a secret-dump rather than admitted as a near-empty
// husk. The reject branch was a name-only promise before the
// 2026-05-29 LLD-drift fix (audit §8.1).
func TestSecretScanGate_RejectOnHeavyRedaction(t *testing.T) {
	det := newSecretsDetector(t)
	// A content that is almost entirely high-entropy credentials:
	// several long secret tokens with minimal surrounding prose. The
	// redaction markers are far shorter than the secrets they
	// replace, so the stripped fraction exceeds 50%.
	var b strings.Builder
	b.WriteString("keys:\n")
	for i := 0; i < 8; i++ {
		b.WriteString("sk-proj")
		b.WriteString(strings.Repeat("abcdefghij0123456789", 3))
		b.WriteString("\n")
	}
	c := &IngestCandidate{Content: b.String()}
	cfg := GateConfig{SecretsDetector: det, SecretsActions: map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionRedact,
	}}
	out := SecretScanGate(c, cfg)
	if out.Action != GateReject {
		t.Fatalf("heavy-redaction deposit should reject, got %+v", out)
	}
	if !strings.Contains(out.Detail, "secret-dump") {
		t.Fatalf("reject detail should name secret-dump, got %q", out.Detail)
	}
	// A small secret inside a large body of prose still redacts
	// (well under the 50% strip threshold) — proving the gate
	// doesn't over-reject ordinary notes that happen to paste one key.
	prose := strings.Repeat("This is an ordinary engineering note about the system. ", 20)
	c2 := &IngestCandidate{Content: prose + "token sk-proj1234567890abcdefghijklmnopqrstuv"}
	out2 := SecretScanGate(c2, cfg)
	if out2.Action != GateRedact {
		t.Fatalf("small secret in large prose should redact, got %+v", out2)
	}
}

// TestRedactionStripFraction exercises the helper directly: the
// boundary cases (empty original, growth, partial strip) the gate
// relies on.
func TestRedactionStripFraction(t *testing.T) {
	cases := []struct {
		name           string
		orig, redacted string
		want           float64
	}{
		{"empty original", "", "anything", 0},
		{"no strip", "abcd", "abcd", 0},
		{"grew", "abcd", "abcdefgh", 0}, // marker longer than secret → 0, not negative
		{"half stripped", "abcdefgh", "abcd", 0.5},
		{"all stripped", "abcdefgh", "", 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactionStripFraction(tc.orig, tc.redacted)
			if got != tc.want {
				t.Fatalf("redactionStripFraction(%q,%q) = %v, want %v", tc.orig, tc.redacted, got, tc.want)
			}
		})
	}
}

func TestPolicyMatchGate(t *testing.T) {
	c := &IngestCandidate{Content: "hello world"}
	// No patterns → allow.
	if out := PolicyMatchGate(c, nil); out.Action != GateAllow {
		t.Fatalf("empty patterns: %+v", out)
	}
	// Empty string in slice skipped.
	if out := PolicyMatchGate(c, []string{"", "nope"}); out.Action != GateAllow {
		t.Fatalf("non-matching: %+v", out)
	}
	// Match → Quarantine.
	out := PolicyMatchGate(c, []string{"world"})
	if out.Action != GateQuarantine || !strings.Contains(out.Detail, "matched deny pattern") {
		t.Fatalf("match: %+v", out)
	}
}

func TestMinContentGate(t *testing.T) {
	cfg := GateConfig{} // zero values → defaults inside the gate
	// Reject when below char floor.
	out := MinContentGate(&IngestCandidate{Content: "too short"}, cfg)
	if out.Action != GateReject {
		t.Fatalf("short chars: %+v", out)
	}
	// Quarantine on too-few words.
	mediumNoWords := strings.Repeat("a", 80) // 80 chars, 1 "word"
	out = MinContentGate(&IngestCandidate{Content: mediumNoWords}, cfg)
	if out.Action != GateQuarantine {
		t.Fatalf("few words: %+v", out)
	}
	// Allow when both thresholds met.
	good := strings.Repeat("word ", 30)
	out = MinContentGate(&IngestCandidate{Content: good}, cfg)
	if out.Action != GateAllow {
		t.Fatalf("good: %+v", out)
	}
}

func TestTruncationCheckGate(t *testing.T) {
	// All cases here opt the candidate's class in so the gate
	// actually runs. The "default-off" branch is covered separately
	// by TestTruncationCheckGate_DisabledByDefault.
	cfg := GateConfig{
		TruncationToleranceFraction: 0.05,
		TruncationCheckClasses:      []ContentClass{ClassExternalFetch},
	}
	c := &IngestCandidate{
		Content:       strings.Repeat("x", 1000),
		ProposedClass: ClassExternalFetch,
	}
	// sourceSize zero → allow.
	if out := TruncationCheckGate(c, cfg, 0); out.Action != GateAllow {
		t.Fatalf("zero source: %+v", out)
	}
	// Within tolerance.
	if out := TruncationCheckGate(c, cfg, 1000); out.Action != GateAllow {
		t.Fatalf("exact: %+v", out)
	}
	if out := TruncationCheckGate(c, cfg, 1040); out.Action != GateAllow {
		t.Fatalf("within tol: %+v", out)
	}
	// Drift too negative.
	out := TruncationCheckGate(c, cfg, 2000)
	if out.Action != GateQuarantine {
		t.Fatalf("negative drift: %+v", out)
	}
	// Drift too positive.
	out = TruncationCheckGate(c, cfg, 500)
	if out.Action != GateQuarantine {
		t.Fatalf("positive drift: %+v", out)
	}
}

// TestTruncationCheckGate_DisabledByDefault confirms the new
// off-by-default behaviour. An empty TruncationCheckClasses list
// allows every candidate through regardless of drift — fixes the
// 2026-05-19→20 false-positive spike where 57 research-class
// summaries were quarantined as "truncated" because the writer
// produced output much smaller (summary) or larger (enrichment)
// than the source artifact.
func TestTruncationCheckGate_DisabledByDefault(t *testing.T) {
	cfg := DefaultGateConfig() // empty TruncationCheckClasses
	// A research-class summary at 30% of source size — the exact
	// shape that pre-fix quarantined 44 chunks on the assistant
	// project in two days.
	c := &IngestCandidate{
		Content:       strings.Repeat("x", 1500),
		ProposedClass: ClassResearch,
	}
	if out := TruncationCheckGate(c, cfg, 5000); out.Action != GateAllow {
		t.Fatalf("default-off must allow research summaries: %+v", out)
	}
}

// TestTruncationCheckGate_OnlyAllowlistedClassesGated confirms a
// non-listed class falls through even when drift would otherwise
// quarantine, while a listed class still trips.
func TestTruncationCheckGate_OnlyAllowlistedClassesGated(t *testing.T) {
	cfg := GateConfig{
		TruncationToleranceFraction: 0.05,
		TruncationCheckClasses:      []ContentClass{ClassExternalFetch},
	}
	research := &IngestCandidate{
		Content:       strings.Repeat("x", 1000),
		ProposedClass: ClassResearch,
	}
	if out := TruncationCheckGate(research, cfg, 5000); out.Action != GateAllow {
		t.Errorf("research not in allowlist must allow: %+v", out)
	}
	external := &IngestCandidate{
		Content:       strings.Repeat("x", 1000),
		ProposedClass: ClassExternalFetch,
	}
	if out := TruncationCheckGate(external, cfg, 5000); out.Action != GateQuarantine {
		t.Errorf("external in allowlist must quarantine on drift: %+v", out)
	}
}

// TestTruncationCheckGate_UsesConfiguredTolerance confirms the
// previously-dead TruncationToleranceFraction field is now honoured.
// Pre-fix, the gate hardcoded 0.05 and ignored cfg.
func TestTruncationCheckGate_UsesConfiguredTolerance(t *testing.T) {
	loose := GateConfig{
		TruncationToleranceFraction: 0.50,
		TruncationCheckClasses:      []ContentClass{ClassExternalFetch},
	}
	c := &IngestCandidate{
		Content:       strings.Repeat("x", 1000),
		ProposedClass: ClassExternalFetch,
	}
	// drift = (1000 - 1400) / 1400 = -28.6%. Inside ±50% → allow.
	// Would have tripped the old hardcoded 5%.
	if out := TruncationCheckGate(c, loose, 1400); out.Action != GateAllow {
		t.Fatalf("loose tolerance must allow ~29%% drift: %+v", out)
	}
	// drift = (1000 - 2500) / 2500 = -60%. Outside ±50% → quarantine.
	if out := TruncationCheckGate(c, loose, 2500); out.Action != GateQuarantine {
		t.Fatalf("60%% drift must quarantine: %+v", out)
	}
}

func TestDedupHashGate(t *testing.T) {
	c := &IngestCandidate{ProjectID: "p", ContentHash: "abc"}
	// nil existsFn → allow.
	if out := DedupHashGate(c, nil); out.Action != GateAllow {
		t.Fatalf("nil fn: %+v", out)
	}
	// existsFn returns true → reject.
	out := DedupHashGate(c, func(string, string) (bool, error) { return true, nil })
	if out.Action != GateReject {
		t.Fatalf("dup: %+v", out)
	}
	// existsFn returns false → allow.
	out = DedupHashGate(c, func(string, string) (bool, error) { return false, nil })
	if out.Action != GateAllow {
		t.Fatalf("no dup: %+v", out)
	}
	// existsFn errors → allow with detail.
	out = DedupHashGate(c, func(string, string) (bool, error) { return false, errors.New("boom") })
	if out.Action != GateAllow || !strings.Contains(out.Detail, "dedup lookup failed") {
		t.Fatalf("err: %+v", out)
	}
}

func TestWordCount(t *testing.T) {
	if wordCount("  one two\nthree\tfour ") != 4 {
		t.Fatalf("expected 4")
	}
	if wordCount("") != 0 {
		t.Fatalf("empty")
	}
}

func TestRunStandardGates_NilCandidate(t *testing.T) {
	out, trail := RunStandardGates(nil, GateConfig{}, nil, 0, nil)
	if out.Action != GateReject || trail != nil {
		t.Fatalf("nil: %+v, trail=%v", out, trail)
	}
}

func TestRunStandardGates_SchemaRejectStopsEarly(t *testing.T) {
	c := validCandidate()
	c.Content = ""
	final, trail := RunStandardGates(c, GateConfig{}, nil, 0, nil)
	if final.Action != GateReject {
		t.Fatalf("want reject, got %+v", final)
	}
	if len(trail) != 1 || trail[0].Gate != GateSchemaMatch {
		t.Fatalf("expected single schema entry, got %v", trail)
	}
}

func TestRunStandardGates_SecretRedactInPlace(t *testing.T) {
	det := newSecretsDetector(t)
	c := validCandidate()
	c.Content = c.Content + " sk-proj1234567890abcdefghijklmnopqrstuv"
	cfg := DefaultGateConfig()
	cfg.SecretsDetector = det
	cfg.SecretsActions = map[string]secrets.Action{secrets.CheckpointMemory: secrets.ActionRedact}

	final, trail := RunStandardGates(c, cfg, nil, 0, nil)
	if final.Action != GateAllow {
		t.Fatalf("want allow, got %+v", final)
	}
	// In-place redaction: content has been scrubbed; later gates saw the cleaned content.
	if strings.Contains(c.Content, "sk-proj1234567890") {
		t.Fatalf("expected in-place scrubbing")
	}
	if c.ContentHash == "" {
		t.Fatalf("hash should have been re-stamped")
	}
	// Trail records the redact decision.
	saw := false
	for _, o := range trail {
		if o.Gate == GateSecretScan && o.Action == GateRedact {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected GateRedact in trail")
	}
}

func TestRunStandardGates_DedupCalledAfterHash(t *testing.T) {
	c := validCandidate()
	c.ContentHash = ""
	called := false
	dedup := func(_, hash string) (bool, error) {
		called = true
		if hash == "" {
			t.Fatalf("dedup called with empty hash")
		}
		return false, nil
	}
	_, _ = RunStandardGates(c, DefaultGateConfig(), nil, 0, dedup)
	if !called {
		t.Fatalf("dedup never called")
	}
}

func TestRunStandardGates_QuarantineShortCircuits(t *testing.T) {
	// PolicyMatchGate triggers before MinContent.
	c := validCandidate()
	c.Content = "forbidden " + c.Content
	final, trail := RunStandardGates(c, DefaultGateConfig(), []string{"forbidden"}, 0, nil)
	if final.Action != GateQuarantine || final.Gate != GatePolicyMatch {
		t.Fatalf("want policy quarantine, got %+v", final)
	}
	// Trail must NOT contain MinContent — we short-circuited.
	for _, o := range trail {
		if o.Gate == GateMinContent {
			t.Fatalf("expected short-circuit before MinContent")
		}
	}
}

func TestErrCandidateRejected(t *testing.T) {
	if ErrCandidateRejected.Error() != "candidate rejected by gate" {
		t.Fatalf("sentinel message drifted: %v", ErrCandidateRejected)
	}
}
