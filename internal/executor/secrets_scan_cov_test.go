package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

const secretsScanCov_pat = `curl -H "Authorization: Bearer ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" /api`

// TestSecretsScanCov_ToolAudit_NoFindingsEarlyReturn covers the branch where
// neither input nor output contains a secret — the helper returns the
// originals before resolving an action.
func TestSecretsScanCov_ToolAudit_NoFindingsEarlyReturn(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	in, out := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "s", "run_shell",
		"echo hello", "hello\n")
	assert.Equal(t, "echo hello", in)
	assert.Equal(t, "hello\n", out)
}

// TestSecretsScanCov_ToolAudit_DetectModeRetainsRaw covers the Detect arm:
// findings are logged but the raw input/output are returned unmodified.
func TestSecretsScanCov_ToolAudit_DetectModeRetainsRaw(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointToolAudit: secrets.ActionDetect,
	})
	in, out := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "s", "run_shell",
		secretsScanCov_pat, "200 OK")
	// Detect mode does not rewrite — the secret stays in the returned bytes.
	assert.Contains(t, in, "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.Equal(t, "200 OK", out)
}

// TestSecretsScanCov_ToolAudit_BlockDegradesToRedact covers the Block arm,
// which for the tool-audit checkpoint degrades to Redact (dropping the row
// would hurt observability more than redaction does).
func TestSecretsScanCov_ToolAudit_BlockDegradesToRedact(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointToolAudit: secrets.ActionBlock,
	})
	in, _ := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "s", "run_shell",
		secretsScanCov_pat, "")
	assert.NotContains(t, in, "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.Contains(t, in, "[REDACTED:github_pat]")
}
