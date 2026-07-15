package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// newTestExecutorWithSecrets returns an Executor wired with a real
// MultiDetector + caller-supplied per-checkpoint action override.
// Enough for the secrets_scan_test cases — they don't need a
// runtime, repos, or workflow resolver because the helpers operate
// on bytes only.
func newTestExecutorWithSecrets(t *testing.T, actions map[string]secrets.Action) *Executor {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return &Executor{
		secretsDetector: det,
		secretsActions:  actions,
		logger:          zerolog.Nop(),
	}
}

// TestScanResultForSecrets_RedactsByDefault — the headline
// contract: a result.json containing an OpenAI key gets redacted
// before any downstream consumer reads the bytes. Default action
// is Redact (set by secrets.DefaultCheckpoints), so an empty
// override map still redacts.
func TestScanResultForSecrets_RedactsByDefault(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	body := []byte(`{"message":"key=sk-proj1234567890abcdefghijklmnopqrstuv","status":"COMPLETED"}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err, "redact-mode default must NOT return ErrSecretLeakBlocked")
	assert.NotContains(t, string(out), "sk-proj1234567890",
		"redact-mode default must remove the OpenAI key from the body")
	assert.Contains(t, string(out), "[REDACTED:openai_key]")
	// Surrounding context preserved so downstream parsers still work.
	assert.Contains(t, string(out), `"status":"COMPLETED"`)
}

// TestScanResultForSecrets_DetectModeLeavesIntact — an operator
// override to detect-only logs but doesn't modify the body.
// Useful for staging the detector against a noisy corpus before
// promoting to redact.
func TestScanResultForSecrets_DetectModeLeavesIntact(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointResultJSON: secrets.ActionDetect,
	})
	body := []byte(`{"message":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(out),
		"detect-mode must NOT modify the body — only logging fires")
}

// TestScanResultForSecrets_NoFindingsReturnsInputBytes — fast-path
// guard: clean bodies pass through without a redact pass + without
// allocating a new slice. Catches a regression where the helper
// would always synthesize a redacted copy.
func TestScanResultForSecrets_NoFindingsReturnsInputBytes(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	body := []byte(`{"message":"all clear","status":"COMPLETED","modified_files":["a.go"]}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(out))
}

// TestScanResultForSecrets_NilDetectorIsNoop — disabled-config or
// fail-to-construct path. Helper must return the input unchanged
// rather than crash on a nil detector.
func TestScanResultForSecrets_NilDetectorIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	body := []byte(`{"message":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(out),
		"nil detector means secrets layer is off; helper must pass body through")
}

// TestScanResultForSecrets_BlockReturnsSentinelError — Phase 2:
// configuring result_json: block must surface ErrSecretLeakBlocked
// alongside the redacted body so the caller can fail the step with
// SECRET_LEAK while still persisting clean audit data.
func TestScanResultForSecrets_BlockReturnsSentinelError(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointResultJSON: secrets.ActionBlock,
	})
	body := []byte(`{"message":"key=sk-proj1234567890abcdefghijklmnopqrstuv","status":"COMPLETED"}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.Error(t, err, "block-mode must return an error")
	assert.ErrorIs(t, err, ErrSecretLeakBlocked,
		"block-mode error must wrap ErrSecretLeakBlocked so the classifier picks up SECRET_LEAK")
	assert.Contains(t, err.Error(), "secret_leak",
		"error message must contain 'secret_leak' so the classifier matches it")
	// Body still flows through redacted — downstream audit/post-mortem
	// must see clean bytes, not raw credentials.
	assert.NotContains(t, string(out), "sk-proj1234567890")
	assert.Contains(t, string(out), "[REDACTED:openai_key]")
}

// TestScanContainerLogsForSecrets_RedactsKnownPatterns — the
// failed-task UI's last-50-lines tail. A `printenv` line in the
// container log that includes an Anthropic key must come back
// redacted before the dashboard renders it.
func TestScanContainerLogsForSecrets_RedactsKnownPatterns(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	body := []byte("ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234\nstep failed")
	out := e.scanContainerLogsForSecrets(context.Background(), &persistence.Execution{ID: "e1"}, "step", body)
	assert.NotContains(t, string(out), "sk-ant-api03",
		"container log redaction must remove the Anthropic key from the displayed bytes")
	assert.Contains(t, string(out), "[REDACTED:anthropic_key]")
	assert.Contains(t, string(out), "step failed", "context preserved")
}

// TestScanToolAuditForSecrets_RedactsByDefault — the audit log's default
// checkpoint action is now Redact (2026-06-16 security hardening): tool_input/
// tool_output are persisted durably in tool_audit_log, so Detect (the prior
// default) left live credentials at rest. Redaction substitutes a typed
// marker, keeping audit context + the finding type while removing the secret.
func TestScanToolAuditForSecrets_RedactsByDefault(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	rawInput := `curl -H "Authorization: Bearer ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" https://api.github.com`
	rawOutput := "200 OK"
	gotInput, gotOutput := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "step", "run_shell", rawInput, rawOutput)
	assert.NotContains(t, gotInput, "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"default must redact the live credential from the persisted audit input")
	assert.Contains(t, gotInput, "[REDACTED:github_pat]")
	assert.Contains(t, gotInput, "https://api.github.com", "surrounding context preserved")
	assert.Equal(t, rawOutput, gotOutput, "no secret in output → unchanged")
}

// TestScanToolAuditForSecrets_RedactModeOverride — when an
// operator opts into redact for the audit log, both input and
// output get sanitised. Useful for compliance environments where
// audit data flows out of vornik into broader log systems.
func TestScanToolAuditForSecrets_RedactModeOverride(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointToolAudit: secrets.ActionRedact,
	})
	rawInput := `curl -H "Authorization: Bearer ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" /api`
	gotInput, _ := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "step", "run_shell", rawInput, "")
	assert.NotContains(t, gotInput, "ghp_aaaaaaaa",
		"redact-mode override must scrub the GitHub PAT from tool audit input")
	assert.True(t, strings.Contains(gotInput, "[REDACTED:github_pat]"),
		"input bytes must include the redaction marker — got %q", gotInput)
}

// TestScanToolAuditForSecrets_NilDetectorPassesThrough — disabled
// secrets layer leaves audit untouched.
func TestScanToolAuditForSecrets_NilDetectorPassesThrough(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	in, out := e.scanToolAuditForSecrets(&persistence.Execution{}, "s", "t", "in", "out")
	assert.Equal(t, "in", in)
	assert.Equal(t, "out", out)
}

// TestScanContainerLogsForSecrets_NilDetectorPassesThrough — when
// no detector is wired, container logs flow through untouched.
func TestScanContainerLogsForSecrets_NilDetectorPassesThrough(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	body := []byte("anything sk-ant-api03-abcdef")
	out := e.scanContainerLogsForSecrets(context.Background(), &persistence.Execution{ID: "e1"}, "s", body)
	assert.Equal(t, body, out, "no detector wired → unchanged")
}

// TestScanContainerLogsForSecrets_EmptyBodyShortCircuits — empty
// input returns empty without invoking the detector.
func TestScanContainerLogsForSecrets_EmptyBodyShortCircuits(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	out := e.scanContainerLogsForSecrets(context.Background(), &persistence.Execution{ID: "e1"}, "s", nil)
	assert.Empty(t, out)
}

// TestScanContainerLogsForSecrets_NoFindingsLeavesBytesUnchanged —
// detector fires but reports no findings → original bytes return.
func TestScanContainerLogsForSecrets_NoFindingsLeavesBytesUnchanged(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	body := []byte("nothing secret here, just regular log output\n")
	out := e.scanContainerLogsForSecrets(context.Background(), &persistence.Execution{ID: "e1"}, "s", body)
	assert.Equal(t, body, out)
}

// TestScanContainerLogsForSecrets_BlockDegradesToRedact — block mode
// for container logs degrades to redact (documented behaviour).
func TestScanContainerLogsForSecrets_BlockDegradesToRedact(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointContainerLogs: secrets.ActionBlock,
	})
	body := []byte("ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234\nfail")
	out := e.scanContainerLogsForSecrets(context.Background(), &persistence.Execution{ID: "e1"}, "s", body)
	// Same shape as redact action — secret gone, marker present.
	assert.NotContains(t, string(out), "sk-ant-api03")
	assert.Contains(t, string(out), "[REDACTED:anthropic_key]")
}

// TestScanContainerLogsForSecrets_DetectModeUnchanged — detect-only
// mode preserves the body while still recording the metric.
func TestScanContainerLogsForSecrets_DetectModeUnchanged(t *testing.T) {
	e := newTestExecutorWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointContainerLogs: secrets.ActionDetect,
	})
	body := []byte("ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234\nfail")
	out := e.scanContainerLogsForSecrets(context.Background(), &persistence.Execution{ID: "e1"}, "s", body)
	assert.Equal(t, body, out, "detect-only must preserve the body verbatim")
}

// TestScanResultForSecrets_PreservesFilenamesInProducedFiles pins
// the 2026-05-18 fix for the janka CV-delivery cascade. The entropy
// detector hit on a writer-emitted filename ("biocentrum-cv-…long
// hash…-en.pdf") and the redactor turned it into "[REDACTED:entropy].pdf",
// which then made verifyClaimedFiles look for /tmp/.../[REDACTED:
// entropy].pdf on disk and hard-fail the step. Path-bearing JSON
// fields must be excluded from redaction so the verifier can still
// match the literal filename.
//
// A real OpenAI key in the same result.json still gets redacted —
// the exclusion is keyed on JSON field name, not on the matched
// string's shape.
func TestScanResultForSecrets_PreservesFilenamesInProducedFiles(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	// produced_files entry is long + entropic enough to trip the
	// detector; the OpenAI key is the canary that the rest of the
	// body still gets scanned.
	body := []byte(`{
		"message":"wrote 3 files; key=sk-proj1234567890abcdefghijklmnopqrstuv",
		"writing":{"written":true},
		"produced_files":[
			"project/artifacts/out/biocentrum-cv-for-msd-senior-lead-aitl-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6.pdf",
			"project/artifacts/out/biocentrum-cv-for-msd-senior-lead-aitl-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6.html"
		]
	}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err)
	s := string(out)

	// File paths must survive verbatim.
	assert.Contains(t, s, "biocentrum-cv-for-msd-senior-lead-aitl-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6.pdf",
		"produced_files PDF path must NOT be redacted")
	assert.Contains(t, s, "biocentrum-cv-for-msd-senior-lead-aitl-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6.html",
		"produced_files HTML path must NOT be redacted")

	// The OpenAI key must still be redacted — the exclusion only
	// covers path fields, not the whole body.
	assert.NotContains(t, s, "sk-proj1234567890abcdefghijklmnopqrstuv",
		"OpenAI key in message field must still get redacted")
	assert.Contains(t, s, "[REDACTED:openai_key]")
}

// TestScanResultForSecrets_PreservesFilenamesInOutputArtifacts mirrors
// the produced_files case but for the outputArtifacts shape (objects
// with a `path` key). Same redaction-vs-verification failure mode.
func TestScanResultForSecrets_PreservesFilenamesInOutputArtifacts(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	body := []byte(`{
		"outputArtifacts":[
			{"name":"deliverable","path":"project/artifacts/out/deliverable-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0.pdf"}
		],
		"message":"ok"
	}`)
	out, err := e.scanResultForSecrets(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err)
	assert.Contains(t, string(out),
		"deliverable-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0.pdf",
		"outputArtifacts[].path must NOT be redacted")
}

// TestExtractPathFieldSpans_BoundariesAreInclusiveOfStringBody
// pins the offset math: each Span must point at the string CONTENT
// (between the quotes), not the surrounding "" or JSON punctuation.
// filterFindingsOutsidePathSpans uses these to decide overlap.
func TestExtractPathFieldSpans_BoundariesAreInclusiveOfStringBody(t *testing.T) {
	body := []byte(`{"produced_files":["foo.pdf","bar.html"],"path":"baz.md"}`)
	spans := extractPathFieldSpans(body)
	require.Len(t, spans, 3)

	got := make([]string, 0, len(spans))
	for _, s := range spans {
		got = append(got, string(body[s.Start:s.End]))
	}
	assert.ElementsMatch(t, []string{"foo.pdf", "bar.html", "baz.md"}, got,
		"spans must point at the literal string content, not the surrounding JSON")
}

// --- Provenance-scoped tool-output exemption (feat/pagedrop-password) ----
// A trusted tool's OUTPUT is the daemon-proxied tool response (the agent
// cannot forge it), so its heuristic generic_kv/entropy hits — e.g. a
// PageDrop viewing password — are operator-facing values, not smuggled
// secrets, and must survive redaction. STRONG credential patterns still
// redact even there; the INPUT (agent-supplied) is always fully scanned.

func newTestExecutorWithTrustedOutputs(t *testing.T, actions map[string]secrets.Action, trusted []string) *Executor {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	return &Executor{
		secretsDetector:           det,
		secretsActions:            actions,
		secretsTrustedOutputTools: trusted,
		logger:                    zerolog.Nop(),
	}
}

func TestScanToolAudit_TrustedToolPasswordSurvives(t *testing.T) {
	e := newTestExecutorWithTrustedOutputs(t,
		map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionRedact},
		[]string{"mcp__pagedrop__pagedrop_publish"})
	out := "Published page.\nView: https://v.example.cc/p/abc-123\nPassword: salt8fog6marrytrip — share separately."
	_, gotOut := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "publish", "mcp__pagedrop__pagedrop_publish_page", "", out)
	assert.Contains(t, gotOut, "salt8fog6marrytrip",
		"trusted publish tool OUTPUT must keep its viewing password (generic_kv exempt by provenance)")
}

func TestScanToolAudit_TrustedToolPrefixRequiresDelimiter(t *testing.T) {
	e := newTestExecutorWithTrustedOutputs(t,
		map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionRedact},
		[]string{"mcp__pagedrop__pagedrop_publish"})
	out := "Password: salt8fog6marrytrip — adjacent tool name must not inherit trust."
	_, gotOut := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "publish", "mcp__pagedrop__pagedrop_publisher_evil", "", out)
	assert.NotContains(t, gotOut, "salt8fog6marrytrip",
		"trusted-output prefix must be exact or underscore-delimited")
	assert.Contains(t, gotOut, "[REDACTED:generic_kv]")
}

func TestScanToolAudit_UntrustedToolPasswordRedacted(t *testing.T) {
	e := newTestExecutorWithTrustedOutputs(t,
		map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionRedact},
		[]string{"mcp__pagedrop__pagedrop_publish"})
	out := "Password: salt8fog6marrytrip — leaked via untrusted tool."
	_, gotOut := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "step", "mcp__scraper__web_fetch", "", out)
	assert.NotContains(t, gotOut, "salt8fog6marrytrip",
		"non-trusted tool output must STILL redact a generic_kv password")
	assert.Contains(t, gotOut, "[REDACTED:generic_kv]")
}

func TestScanToolAudit_TrustedToolStrongCredentialStillRedacted(t *testing.T) {
	e := newTestExecutorWithTrustedOutputs(t,
		map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionRedact},
		[]string{"mcp__pagedrop__pagedrop_publish"})
	out := "here is a real key sk-proj1234567890abcdefghijklmnopqrstuv in trusted output"
	_, gotOut := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "publish", "mcp__pagedrop__pagedrop_publish_page", "", out)
	assert.NotContains(t, gotOut, "sk-proj1234567890",
		"EXFIL GUARD: trusted-output exemption must NOT let a strong credential (OpenAI key) through")
	assert.Contains(t, gotOut, "[REDACTED:openai_key]")
}

func TestScanToolAudit_TrustedToolInputStillRedacted(t *testing.T) {
	e := newTestExecutorWithTrustedOutputs(t,
		map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionRedact},
		[]string{"mcp__pagedrop__pagedrop_publish"})
	in := "Password: injectedSecretValue123 supplied by the agent"
	gotIn, _ := e.scanToolAuditForSecrets(&persistence.Execution{ID: "e1"}, "publish", "mcp__pagedrop__pagedrop_publish_page", in, "")
	assert.NotContains(t, gotIn, "injectedSecretValue123",
		"trusted tool INPUT (agent-supplied) must still be redacted — only OUTPUT is provenance-exempt")
}

// fakeRedactionAudit captures Record calls for the badge-source tests.
type fakeRedactionAudit struct {
	events []persistence.SecretRedactionEvent
	err    error
}

func (f *fakeRedactionAudit) Record(_ context.Context, events []persistence.SecretRedactionEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, events...)
	return nil
}
func (f *fakeRedactionAudit) CountByTask(context.Context, string) (map[string]int, int, error) {
	return nil, 0, nil
}

// TestScanResultForSecrets_RecordsRedactionAudit — the Phase 3 badge
// data path: a redacting scan persists one event per finding type with
// the right task/checkpoint/count. Backlog item 2.
func TestScanResultForSecrets_RecordsRedactionAudit(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	audit := &fakeRedactionAudit{}
	e.secretRedactionAudit = audit

	body := []byte(`{"message":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}`)
	_, err := e.scanResultForSecrets(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err)

	require.NotEmpty(t, audit.events, "a redaction must persist an audit event")
	ev := audit.events[0]
	assert.Equal(t, "t1", ev.TaskID)
	assert.Equal(t, "p1", ev.ProjectID)
	assert.Equal(t, secrets.CheckpointResultJSON, ev.Checkpoint)
	assert.Equal(t, "openai_key", ev.FindingType)
	assert.Equal(t, 1, ev.Count)
	assert.Equal(t, "live", ev.Source)
}

// TestScanResultForSecrets_AuditErrorDoesNotFailStep — best-effort
// contract (design M2): a Record failure logs but the redaction still
// succeeds and the step is not failed.
func TestScanResultForSecrets_AuditErrorDoesNotFailStep(t *testing.T) {
	e := newTestExecutorWithSecrets(t, nil)
	e.secretRedactionAudit = &fakeRedactionAudit{err: assertAnErr}

	body := []byte(`{"message":"key=sk-proj1234567890abcdefghijklmnopqrstuv"}`)
	out, err := e.scanResultForSecrets(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "e1"}, "step", body)
	require.NoError(t, err, "an audit-record failure must not fail the redact-mode step")
	assert.Contains(t, string(out), "[REDACTED:openai_key]", "redaction still happens")
}

var assertAnErr = errStub("boom")

type errStub string

func (e errStub) Error() string { return string(e) }
