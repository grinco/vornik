package memory

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

// newTestIndexerWithSecrets builds an Indexer wired with a real
// MultiDetector and the caller's per-checkpoint action override.
// Repo/embedder are left nil — scanContentForSecrets only touches
// the detector + logger.
func newTestIndexerWithSecrets(t *testing.T, actions map[string]secrets.Action) *Indexer {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return &Indexer{
		logger:          zerolog.Nop(),
		secretsDetector: det,
		secretsActions:  actions,
	}
}

// TestScanContentForSecrets_RedactsByDefault — the headline contract
// for the memory checkpoint: an artifact carrying an OpenAI key gets
// redacted before chunking, so the resulting chunks never carry the
// raw secret. Default action is Redact (set by DefaultCheckpoints),
// so an empty override map still redacts.
func TestScanContentForSecrets_RedactsByDefault(t *testing.T) {
	idx := newTestIndexerWithSecrets(t, nil)
	body := `# notes\nkey=sk-proj1234567890abcdefghijklmnopqrstuv\nsome safe text`
	out := idx.scanContentForSecrets("p1", "t1", "notes.md", body)
	assert.NotContains(t, out, "sk-proj1234567890",
		"redact-mode default must remove the OpenAI key from memory content")
	assert.Contains(t, out, "[REDACTED:openai_key]")
	// Surrounding prose preserved so chunks still make semantic sense.
	assert.Contains(t, out, "some safe text")
}

// TestScanContentForSecrets_DetectModeLeavesIntact — operator override
// to detect-only logs the finding but doesn't modify the content. Used
// for staging the detector against a noisy corpus before promoting to
// redact.
// TestScanContentForSecrets_MemoryDetectClampedToRedact — memory secret
// scanning is non-disableable: a `detect` override on the memory checkpoint is
// clamped up to `redact` (ResolveAction), so plaintext credentials can't be
// admitted to durable, searchable memory by config. The explicit
// VORNIK_ALLOW_UNSCANNED_MEMORY escape hatch restores detect-leaves-intact.
func TestScanContentForSecrets_MemoryDetectClampedToRedact(t *testing.T) {
	body := `key=sk-proj1234567890abcdefghijklmnopqrstuv`

	idx := newTestIndexerWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionDetect,
	})
	out := idx.scanContentForSecrets("p1", "t1", "notes.md", body)
	assert.NotContains(t, out, "sk-proj1234567890",
		"memory detect must be clamped to redact (non-disableable)")
	assert.Contains(t, out, "[REDACTED:", "clamped detect should redact")

	// Escape hatch restores the legacy detect-leaves-intact behavior.
	t.Setenv("VORNIK_ALLOW_UNSCANNED_MEMORY", "1")
	idx2 := newTestIndexerWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionDetect,
	})
	out2 := idx2.scanContentForSecrets("p1", "t1", "notes.md", body)
	assert.Equal(t, body, out2, "with the escape hatch, detect leaves content untouched")
}

// TestScanContentForSecrets_BlockDegradesToRedact — Phase 2 leaves the
// memory checkpoint as a Redact-on-Block site. Block at the writer
// (artifact/result.json) is where SECRET_LEAK enforcement lands; refusing
// to ingest into memory would lose signal the operator can still safely
// search post-redaction.
func TestScanContentForSecrets_BlockDegradesToRedact(t *testing.T) {
	idx := newTestIndexerWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointMemory: secrets.ActionBlock,
	})
	body := `ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234`
	out := idx.scanContentForSecrets("p1", "t1", "env.md", body)
	assert.NotContains(t, out, "sk-ant-api03",
		"block-mode degradation must still scrub the secret")
	assert.True(t, strings.Contains(out, "[REDACTED:anthropic_key]"),
		"block degradation should redact, got: %q", out)
}

// TestScanContentForSecrets_NilDetectorIsNoop — disabled-config path.
// When the service container can't build a detector (or is configured
// off), the indexer must pass content through unmodified.
func TestScanContentForSecrets_NilDetectorIsNoop(t *testing.T) {
	idx := &Indexer{logger: zerolog.Nop()}
	body := `key=sk-proj1234567890abcdefghijklmnopqrstuv`
	out := idx.scanContentForSecrets("p1", "t1", "notes.md", body)
	assert.Equal(t, body, out)
}

// TestScanContentForSecrets_NoFindingsReturnsInputUnchanged — fast-path
// for clean inputs. No findings means no allocation, just pass-through.
func TestScanContentForSecrets_NoFindingsReturnsInputUnchanged(t *testing.T) {
	idx := newTestIndexerWithSecrets(t, nil)
	body := "## design notes\n- swap retry layer to single coordinator\n- cleanup pause taxonomy"
	out := idx.scanContentForSecrets("p1", "t1", "design.md", body)
	assert.Equal(t, body, out)
}

// TestSetSecrets_WiresDetector — covers the public setter the service
// container calls during boot. Mirrors the Bot pattern.
func TestSetSecrets_WiresDetector(t *testing.T) {
	idx := &Indexer{logger: zerolog.Nop()}
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	idx.SetSecrets(det, map[string]secrets.Action{secrets.CheckpointMemory: secrets.ActionDetect})

	assert.Same(t, det, idx.secretsDetector)
	assert.Equal(t, secrets.ActionDetect, idx.secretsActions[secrets.CheckpointMemory])
}
