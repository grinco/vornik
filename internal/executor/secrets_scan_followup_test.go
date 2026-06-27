package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// TestScanContainerLogsForSecrets_NilDetectorPassesThrough —
// without the detector wired, the body is returned unchanged
// (no panic, no scan attempt).
func TestScanContainerLogsForSecrets_NilDetector(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	body := []byte("ANTHROPIC_API_KEY=sk-secret\nlog content")
	got := e.scanContainerLogsForSecrets(context.Background(),
		&persistence.Execution{ID: "x"}, "step", body)
	assert.Equal(t, body, got, "no detector = passthrough")
}

// TestScanContainerLogsForSecrets_EmptyBody — guard against an
// empty body (no log content yet for this step). Helper must
// return the empty input unchanged without scanning.
func TestScanContainerLogsForSecrets_EmptyBody(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	got := e.scanContainerLogsForSecrets(context.Background(),
		&persistence.Execution{ID: "x"}, "step", nil)
	assert.Nil(t, got)
	got2 := e.scanContainerLogsForSecrets(context.Background(),
		&persistence.Execution{ID: "x"}, "step", []byte{})
	assert.Empty(t, got2)
}

// TestScanContainerLogsForSecrets_NoFindings — body that
// doesn't match any pattern flows through unchanged. The
// detector's Scan reports zero findings; the helper
// short-circuits before resolving an action.
func TestScanContainerLogsForSecrets_NoFindings(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	body := []byte("just some innocuous log content\nstep complete\n")
	got := e.scanContainerLogsForSecrets(context.Background(),
		&persistence.Execution{ID: "x"}, "step", body)
	assert.Equal(t, body, got)
}

// TestScanContainerLogsForSecrets_DetectMode — when the action
// for the container-logs checkpoint is Detect, the body flows
// through unmodified even when findings exist. This is the
// "log but don't strip" policy — operators may want to see the
// raw stack trace including the leaked credential because
// fixing the underlying bug needs the full context.
func TestScanContainerLogsForSecrets_DetectMode(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointContainerLogs: secrets.ActionDetect,
	})
	body := []byte("ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234\nlog content")
	got := e.scanContainerLogsForSecrets(context.Background(),
		&persistence.Execution{ID: "x"}, "step", body)
	assert.Equal(t, body, got,
		"detect-mode preserves the raw bytes so operators see the full stack trace")
}

// TestScanContainerLogsForSecrets_BlockDegradesToRedact — when
// the checkpoint is configured to Block, the executor logs
// a warning that block-is-not-enforced and falls through to
// redact (so the dashboard doesn't display the credential).
// This pins the current Phase 1 contract that block is
// observability-only on container logs.
func TestScanContainerLogsForSecrets_BlockDegrades(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointContainerLogs: secrets.ActionBlock,
	})
	body := []byte("ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234\nlog content")
	got := e.scanContainerLogsForSecrets(context.Background(),
		&persistence.Execution{ID: "x"}, "step", body)
	assert.NotEqual(t, body, got, "block-action must degrade to redact, not return raw bytes")
	assert.Contains(t, string(got), "[REDACTED",
		"redaction marker must appear (block degrades to redact)")
}
