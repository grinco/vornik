package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyClaimedModifications_AbsentArraySkips(t *testing.T) {
	e := &Executor{}
	// No modified_files field → no verification, no error.
	err := e.verifyClaimedModifications(
		[]byte(`{"status":"COMPLETED","message":"wrote a thing"}`),
		"/tmp/ws", "/tmp/proj", time.Now(),
	)
	require.NoError(t, err)

	// Empty array → same.
	err = e.verifyClaimedModifications(
		[]byte(`{"modified_files":[]}`), "/tmp/ws", "/tmp/proj", time.Now(),
	)
	require.NoError(t, err)
}

func TestVerifyClaimedModifications_EmptyBytesSkip(t *testing.T) {
	e := &Executor{}
	require.NoError(t, e.verifyClaimedModifications(nil, "/tmp/ws", "/tmp/proj", time.Now()))
	require.NoError(t, e.verifyClaimedModifications([]byte{}, "/tmp/ws", "/tmp/proj", time.Now()))
}

func TestVerifyClaimedModifications_MalformedJSONSkips(t *testing.T) {
	e := &Executor{}
	// Not valid JSON — treated as "no claim", not as a failure. Structural
	// claim is an opt-in affordance, not a mandatory contract.
	require.NoError(t, e.verifyClaimedModifications(
		[]byte(`not json at all`), "/tmp/ws", "/tmp/proj", time.Now(),
	))
}

func TestVerifyClaimedModifications_ClaimMissingFile(t *testing.T) {
	proj := t.TempDir()
	e := &Executor{}

	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["project/PROJECT_CONTEXT.md"]}`),
		t.TempDir(), proj, time.Now(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestVerifyClaimedModifications_ClaimStaleFile(t *testing.T) {
	proj := t.TempDir()
	// Create a file with an mtime 2 minutes in the past.
	target := filepath.Join(proj, "PROJECT_CONTEXT.md")
	require.NoError(t, os.WriteFile(target, []byte("old content"), 0o644))
	past := time.Now().Add(-2 * time.Minute)
	require.NoError(t, os.Chtimes(target, past, past))

	e := &Executor{}
	stepStart := time.Now().Add(-30 * time.Second) // step started 30s ago
	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["project/PROJECT_CONTEXT.md"]}`),
		t.TempDir(), proj, stepStart,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates step start")
	assert.Contains(t, err.Error(), "PROJECT_CONTEXT.md")
}

func TestVerifyClaimedModifications_ClaimFreshFile(t *testing.T) {
	proj := t.TempDir()
	target := filepath.Join(proj, "PROJECT_CONTEXT.md")
	stepStart := time.Now().Add(-5 * time.Second)
	// Write AFTER stepStart (default mtime = now).
	require.NoError(t, os.WriteFile(target, []byte("fresh content"), 0o644))

	e := &Executor{}
	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["project/PROJECT_CONTEXT.md"]}`),
		t.TempDir(), proj, stepStart,
	)
	require.NoError(t, err)
}

func TestVerifyClaimedModifications_AcceptsContainerAbsolutePath(t *testing.T) {
	proj := t.TempDir()
	target := filepath.Join(proj, "PROJECT_CONTEXT.md")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	e := &Executor{}
	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["/app/workspace/project/PROJECT_CONTEXT.md"]}`),
		t.TempDir(), proj, time.Now().Add(-1*time.Hour),
	)
	require.NoError(t, err)
}

func TestVerifyClaimedModifications_AcceptsWorkspaceRelativePath(t *testing.T) {
	ws := t.TempDir()
	outDir := filepath.Join(ws, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	target := filepath.Join(outDir, "summary.md")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	e := &Executor{}
	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["artifacts/out/summary.md"]}`),
		ws, t.TempDir(), time.Now().Add(-1*time.Hour),
	)
	require.NoError(t, err)
}

func TestVerifyClaimedModifications_RejectsPathTraversal(t *testing.T) {
	proj := t.TempDir()
	// Even if the file exists somewhere, a ../ escape must never resolve.
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644))

	e := &Executor{}
	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["project/../secret"]}`),
		t.TempDir(), proj, time.Now(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unresolvable path")
}

func TestVerifyClaimedModifications_MultipleProblemsReported(t *testing.T) {
	proj := t.TempDir()
	// One stale, one missing.
	stale := filepath.Join(proj, "stale.md")
	require.NoError(t, os.WriteFile(stale, []byte("old"), 0o644))
	past := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(stale, past, past))

	e := &Executor{}
	err := e.verifyClaimedModifications(
		[]byte(`{"modified_files":["project/stale.md","project/missing.md"]}`),
		t.TempDir(), proj, time.Now().Add(-1*time.Minute),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale.md")
	assert.Contains(t, err.Error(), "missing.md")
	assert.Contains(t, err.Error(), "2 file(s)")
}

// TestVerifyClaimedFiles_ProducedFilesChecked — agents that emit the
// produced_files convention (vision OCR dump, scout PROJECT_CONTEXT.md)
// must have each named file actually present and freshly written.
func TestVerifyClaimedFiles_ProducedFilesChecked(t *testing.T) {
	ws := t.TempDir()
	stepStart := time.Now().Add(-5 * time.Second)

	// Real file written after stepStart — must pass.
	target := filepath.Join(ws, "artifacts", "out", "ocr.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("scanned text"), 0o644))

	e := &Executor{}
	require.NoError(t, e.verifyClaimedFiles(
		[]byte(`{"produced_files":["artifacts/out/ocr.txt"]}`),
		ws, t.TempDir(), stepStart,
	))

	// Same field, missing file — must fail and name the source.
	err := e.verifyClaimedFiles(
		[]byte(`{"produced_files":["artifacts/out/missing.txt"]}`),
		ws, t.TempDir(), stepStart,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "produced_files")
	assert.Contains(t, err.Error(), "missing.txt")
}

// TestVerifyClaimedFiles_OutputArtifactsChecked — outputArtifacts[].path
// is now verified the same way as modified_files. The entrypoint
// auto-injects <step>-response.md, so legitimate runs always have at
// least one entry — the verifier must accept it when the file is real.
func TestVerifyClaimedFiles_OutputArtifactsChecked(t *testing.T) {
	ws := t.TempDir()
	stepStart := time.Now().Add(-5 * time.Second)

	// Real synthetic response artifact.
	target := filepath.Join(ws, "artifacts", "out", "plan-response.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("plan output"), 0o644))

	e := &Executor{}
	require.NoError(t, e.verifyClaimedFiles(
		[]byte(`{"outputArtifacts":[{"name":"plan-response.md","path":"/app/workspace/artifacts/out/plan-response.md"}]}`),
		ws, t.TempDir(), stepStart,
	))

	// Fabricated entry — agent claimed an artifact that doesn't exist.
	err := e.verifyClaimedFiles(
		[]byte(`{"outputArtifacts":[{"name":"phantom.md","path":"/app/workspace/artifacts/out/phantom.md"}]}`),
		ws, t.TempDir(), stepStart,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outputArtifacts")
	assert.Contains(t, err.Error(), "phantom.md")
}

// TestVerifyClaimedFiles_AllSourcesAggregated — when multiple claim
// sources fail in the same step, the error message must name each
// source so the operator can tell which contract was violated.
func TestVerifyClaimedFiles_AllSourcesAggregated(t *testing.T) {
	ws := t.TempDir()
	proj := t.TempDir()
	stepStart := time.Now().Add(-1 * time.Minute)

	e := &Executor{}
	err := e.verifyClaimedFiles(
		[]byte(`{
			"modified_files":["project/missing-mod.md"],
			"produced_files":["artifacts/out/missing-prod.md"],
			"outputArtifacts":[{"name":"x","path":"/app/workspace/missing-out.md"}]
		}`),
		ws, proj, stepStart,
	)
	require.Error(t, err)
	for _, want := range []string{
		"modified_files", "missing-mod.md",
		"produced_files", "missing-prod.md",
		"outputArtifacts", "missing-out.md",
		"3 file(s)",
	} {
		assert.Contains(t, err.Error(), want, "missing fragment %q", want)
	}
}

// TestVerifyClaimedFiles_AllAbsentSkipsCleanly — with none of the
// three claim sources present, the verifier must succeed without
// touching the filesystem.
func TestVerifyClaimedFiles_AllAbsentSkipsCleanly(t *testing.T) {
	e := &Executor{}
	require.NoError(t, e.verifyClaimedFiles(
		[]byte(`{"status":"COMPLETED","message":"done"}`),
		"/nonexistent-ws", "/nonexistent-proj", time.Now(),
	))
}

// TestVerifyClaimedFiles_ProducedFilesInProjectDir_FalseNegative is the
// regression guard for task be7e (janka jobspin-cz scan, 2026-06-20):
//
//   - The researcher wrote scan-jobspin-cz-2026-06-20.md and research.md via
//     `file_write project/artifacts/out/<name>` (persisting into the project
//     workspace mounted at /app/workspace/project inside the container).
//   - It claimed those paths in produced_files WITHOUT the "project/" prefix
//     (just "artifacts/out/scan-jobspin-cz-2026-06-20.md"), which is the
//     convention the agent uses for files in the standard output directory.
//   - persistArtifacts found both files via source #3 (effectiveProjectDir
//     walk) and successfully persisted them to the durable store.
//   - verifyClaimedFiles resolved "artifacts/out/X" to
//     workspaceDir/artifacts/out/X (the default relative-path case in
//     resolveClaimedPath) — wrong, because the files were in projectDir.
//   - The step was falsely marked failed: "file does not exist at
//     /tmp/vornik-exec-.../workspace/artifacts/out/research.md".
//
// Fix contract: when a file resolved to workspaceDir does not exist, fall
// back to the same relative path under effectiveProjectDir before declaring
// "does not exist". A genuine hallucination (file exists in neither) still
// returns an error — only files that exist on disk are passed through.
func TestVerifyClaimedFiles_ProducedFilesInProjectDir_FalseNegative(t *testing.T) {
	ws := t.TempDir()
	proj := t.TempDir()
	stepStart := time.Now().Add(-10 * time.Second)

	// Agent wrote to the PROJECT dir, NOT the ephemeral workspace.
	// workspaceDir/artifacts/out/ is intentionally left empty.
	projOut := filepath.Join(proj, "artifacts", "out")
	require.NoError(t, os.MkdirAll(projOut, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(projOut, "scan-jobspin-cz-2026-06-20.md"),
		[]byte("scan content"), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(projOut, "research.md"),
		[]byte("research notes"), 0o644,
	))

	// Also write the step-response artifact to workspaceDir/artifacts/out/
	// so that the outputArtifacts entry passes (entrypoint always writes it
	// there; its successful resolution must not be disturbed by the fix).
	wsOut := filepath.Join(ws, "artifacts", "out")
	require.NoError(t, os.MkdirAll(wsOut, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(wsOut, "research-response.md"),
		[]byte("response artifact"), 0o644,
	))

	// Exact claim shape the janka researcher produces:
	//   outputArtifacts path uses /app/workspace/artifacts/out/ prefix
	//   produced_files uses bare relative paths without "project/" prefix
	result := `{
		"outputArtifacts": [
			{"name":"research-response.md","path":"/app/workspace/artifacts/out/research-response.md"}
		],
		"produced_files": [
			"artifacts/out/research.md",
			"artifacts/out/scan-jobspin-cz-2026-06-20.md"
		]
	}`

	e := &Executor{}
	// Before fix: this returned an error "file does not exist at
	// /tmp/.../workspace/artifacts/out/research.md" even though the file
	// existed at proj/artifacts/out/research.md (the project workspace).
	// After fix: the fallback to projectDir resolves both files → no error.
	require.NoError(t, e.verifyClaimedFiles([]byte(result), ws, proj, stepStart),
		"files in effectiveProjectDir must not be falsely reported missing "+
			"when claimed with a bare relative path (no 'project/' prefix) — "+
			"regression for task be7e (janka jobspin-cz 2026-06-20)")
}

// TestVerifyClaimedFiles_GenuinelyMissingFileStillFails ensures the fix's
// project-dir fallback does NOT suppress real hallucinations where the
// file exists in neither workspaceDir nor projectDir.
func TestVerifyClaimedFiles_GenuinelyMissingFileStillFails(t *testing.T) {
	ws := t.TempDir()
	proj := t.TempDir()
	stepStart := time.Now().Add(-10 * time.Second)

	// Only the step-response artifact exists; the agent claims an extra
	// file that was never actually written anywhere.
	wsOut := filepath.Join(ws, "artifacts", "out")
	require.NoError(t, os.MkdirAll(wsOut, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(wsOut, "research-response.md"),
		[]byte("response artifact"), 0o644,
	))

	// "ghost.md" does not exist in workspaceDir OR projectDir.
	result := `{
		"produced_files": ["artifacts/out/ghost.md"]
	}`

	e := &Executor{}
	err := e.verifyClaimedFiles([]byte(result), ws, proj, stepStart)
	require.Error(t, err, "a file that does not exist in EITHER dir must still fail")
	assert.Contains(t, err.Error(), "ghost.md")
}

// TestVerifyClaimedFiles_TraversalClaimRejected documents the security
// property the be7e projectDir fallback must never weaken: a claim that
// escapes the workspace via ".." is rejected as unresolvable by
// resolveClaimedPath/safeJoinUnder BEFORE the fallback is reached, so the
// fallback can never be a path-traversal vector. A real /etc/passwd exists,
// so a naive os.Stat would otherwise "find" it.
func TestVerifyClaimedFiles_TraversalClaimRejected(t *testing.T) {
	ws := t.TempDir()
	proj := t.TempDir()
	stepStart := time.Now().Add(-10 * time.Second)

	result := `{
		"produced_files": ["../../../../../../etc/passwd"]
	}`

	e := &Executor{}
	err := e.verifyClaimedFiles([]byte(result), ws, proj, stepStart)
	require.Error(t, err, "a path-traversal claim must be rejected, not silently accepted via fallback")
	assert.Contains(t, err.Error(), "unresolvable path",
		"traversal claim must fail at resolveClaimedPath, never reach the projectDir fallback")
}

func TestResolveClaimedPath_ProjectAbsolute(t *testing.T) {
	got := resolveClaimedPath("/app/workspace/project/x/y.md", "/ws", "/proj")
	assert.Equal(t, filepath.Clean("/proj/x/y.md"), got)
}

func TestResolveClaimedPath_WorkspaceAbsolute(t *testing.T) {
	got := resolveClaimedPath("/app/workspace/artifacts/out/z.md", "/ws", "/proj")
	assert.Equal(t, filepath.Clean("/ws/artifacts/out/z.md"), got)
}

func TestResolveClaimedPath_ProjectRelative(t *testing.T) {
	got := resolveClaimedPath("project/PROJECT_CONTEXT.md", "/ws", "/proj")
	assert.Equal(t, filepath.Clean("/proj/PROJECT_CONTEXT.md"), got)
}

func TestResolveClaimedPath_BareName(t *testing.T) {
	got := resolveClaimedPath("result.json", "/ws", "/proj")
	assert.Equal(t, filepath.Clean("/ws/result.json"), got)
}

func TestResolveClaimedPath_MountItself(t *testing.T) {
	assert.Equal(t, "", resolveClaimedPath("/app/workspace/project", "/ws", "/proj"))
	assert.Equal(t, "", resolveClaimedPath("/app/workspace", "/ws", "/proj"))
}

func TestResolveClaimedPath_Traversal(t *testing.T) {
	// Traversal must produce empty (unresolvable) regardless of how
	// the base path is expressed.
	for _, claim := range []string{
		"project/../../etc/passwd",
		"/app/workspace/project/../../../etc/passwd",
		"../outside",
	} {
		assert.Equal(t, "", resolveClaimedPath(claim, "/ws", "/proj"),
			"claim %q should be rejected", claim)
	}
}
