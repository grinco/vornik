package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

// The artifact store used to rewrite stored bytes with the FULL detector,
// heuristic rules included. Design
// https://docs.vornik.io
// narrows the write-back to the strong, prefix-anchored rules — the same
// boundary the executor's tool-output path, outputguard's read-back and
// `secrets scan-history --apply` already draw.
//
// Evidence for the narrowing: ~7,000 `entropy` findings across two independent
// production surfaces with ZERO true positives — Go test identifiers in review
// artifacts, hashed filenames in shell output.

// entropyBait is an identifier that ACTUALLY trips the `entropy` rule, verified
// against the live detector rather than assumed.
//
// The filed item described the eaten token as "a long CamelCase name". Probing
// the detector corrected that: plain CamelCase does NOT fire — the rule needs
// 40+ chars AND 4.5 bits/char, and `TestOutputGuardScanWithProvenance…` (62
// chars, letters only) sits below the bar. What fires is an identifier carrying
// digits and case variety, which is what a versioned or hash-suffixed test name
// looks like. Using a string that does not fire would have made every test here
// pass before the fix and prove nothing — which is exactly what happened on the
// first attempt.
const entropyBait = "TestOutputGuard_Scan_v2_EmptyWindow_9f3aB7cD2eF1gH4iJ6kL8mN0pQ"

// entropyBaitFilename is the OTHER measured false-positive population: a
// `run_shell` directory listing's hashed filenames, len 57-58, 6,992 of the
// ~7,050 production findings. Same class, different surface.
const entropyBaitFilename = "./.aG7kQ2mZx9Lp4Rt8Vw1Ny6Bd3Fh5Jc0Se2Xu7Yi4Ko9Pa1Qb6Tr3.json"

// TestStore_EntropyFindingDoesNotRewriteArtifact — C1.
//
// An artifact whose only findings are heuristic is stored BYTE-IDENTICAL, and
// the recorded hash is the source's. Fails before the fix: `entropy` matched
// the identifier and `Redact` replaced it with `[REDACTED:entropy]`.
func TestStore_EntropyFindingDoesNotRewriteArtifact(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil) // default = redact

	body := "# Review\n\n**Test coverage**: `" + entropyBait + "` validates the empty-window case\n"
	src := writeTempSource(t, "review.md", body)

	art, err := store.Store(context.Background(), "p1", "exec1", "task1", "review.md", src)
	require.NoError(t, err)
	require.NotNil(t, art)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)

	assert.Equal(t, body, string(stored),
		"an artifact with only heuristic findings must be stored unmodified — "+
			"redacting an operator-facing review artifact destroys the evidence it exists to carry")
	assert.NotContains(t, string(stored), "[REDACTED:",
		"no redaction marker may appear when nothing strong was found")

	sum := sha256.Sum256([]byte(body))
	require.NotNil(t, art.ContentHashSHA256, "a scanned text artifact records its hash")
	assert.Equal(t, hex.EncodeToString(sum[:]), *art.ContentHashSHA256,
		"the recorded hash must be the source's when nothing was rewritten")
}

// TestStore_StrongFindingStillRedactsBesideAnIdentifier — C2, and the test that
// fails if DropHeuristic is ever applied too widely.
//
// The narrowing must not cost a single credential class. A strong finding in the
// SAME body as a heuristic one still redacts, and the identifier beside it
// survives untouched.
func TestStore_StrongFindingStillRedactsBesideAnIdentifier(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)

	body := "# Review\n\ncovered by `" + entropyBait + "`\nkey=sk-proj1234567890abcdefghijklmnopqrstuv\ntrailing text\n"
	src := writeTempSource(t, "review.md", body)

	art, err := store.Store(context.Background(), "p1", "exec1", "task1", "review.md", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	got := string(stored)

	assert.NotContains(t, got, "sk-proj1234567890",
		"a strong prefix-anchored credential must still be redacted — C2")
	assert.Contains(t, got, "[REDACTED:openai_key]")
	assert.Contains(t, got, entropyBait,
		"the identifier beside the credential must survive: only the strong span is rewritten")
	assert.Contains(t, got, "trailing text",
		"content after the redacted span must be intact — Redact needs findings in offset order, "+
			"and DropHeuristic filters in source order to preserve it")
	assert.True(t, utf8Valid(got), "the rewritten body must still be valid UTF-8")
}

// TestStore_DetectModeUnchangedForBothRuleClasses — §4's scope boundary.
//
// The action vocabulary is untouched: detect-only stores verbatim whichever
// class fired. Without this, a future change could narrow detection itself,
// which C3 forbids.
func TestStore_DetectModeUnchangedForBothRuleClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"heuristic only", "notes: `" + entropyBait + "`\n"},
		{"strong", "key=sk-proj1234567890abcdefghijklmnopqrstuv\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStoreWithSecrets(t, map[string]secrets.Action{
				secrets.CheckpointArtifacts: secrets.ActionDetect,
			})
			src := writeTempSource(t, "n.md", tc.body)
			art, err := store.Store(context.Background(), "p1", "e", "tk", "n.md", src)
			require.NoError(t, err)

			stored, err := os.ReadFile(art.StoragePath)
			require.NoError(t, err)
			assert.Equal(t, tc.body, string(stored),
				"detect-only stores content unchanged regardless of rule class")
		})
	}
}

// TestStore_BlockDegradedRedactAlsoNarrows — the ActionBlock branch degrades to
// redact and must narrow identically. Two call sites redact in scanForBackend;
// a fix applied to only one would leave this arm eating identifiers.
func TestStore_BlockDegradedRedactAlsoNarrows(t *testing.T) {
	store := newTestStoreWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointArtifacts: secrets.ActionBlock,
	})

	body := "coverage: `" + entropyBait + "`\n"
	src := writeTempSource(t, "n.md", body)
	art, err := store.Store(context.Background(), "p1", "e", "tk", "n.md", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Equal(t, body, string(stored),
		"block-degraded-to-redact must narrow the same way as ActionRedact — "+
			"both branches call Redact and both must pass the strong-only set")
}

// TestStore_ReviewArtifactRoundTripsUnmodified — regression test for the
// 2026-08-27 finding.
//
// review-20260826-acfd.md, a stored companion architectural review, came back
// reading "**Test coverage**: `[REDACTED:entropy]` validates the empty-window
// case". The redacted token was a Go test function identifier. The finding it
// supported became uncitable and the source bytes are gone — Redact is one-way.
//
// This pins the exact shape: markdown prose citing an identifier in backticks.
func TestStore_ReviewArtifactRoundTripsUnmodified(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)

	body := strings.Join([]string{
		"# Review: heuristic rules stop rewriting stored artifacts",
		"",
		"### F1 — order preservation",
		"",
		"**Test coverage**: `" + entropyBait + "` validates the empty-window case",
		"",
		"Evidence: artifact_20260827000720_e722dc9c8da32f1d",
		"",
		"Shell output quoted verbatim:",
		"    " + entropyBaitFilename,
		"",
	}, "\n")
	src := writeTempSource(t, "review-20260826-acfd.md", body)

	art, err := store.Store(context.Background(), "p1", "e", "tk", "review-20260826-acfd.md", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Equal(t, body, string(stored),
		"a companion review artifact must survive storage unmodified — this is the 2026-08-27 incident")
	assert.NotContains(t, string(stored), "[REDACTED:entropy]",
		"the marker that made the original artifact misinform its reader")
}

// utf8Valid is a local helper so the assertion above reads as an assertion
// rather than an import.
func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
