package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/registry"
)

// NOTE (deliberately uncovered, container.go): executeAgentStep,
// executeWarmAgentStep, startContainer, runVerifiers, and
// runHallucinationDetector drive real podman / git / network and
// have no fake seam reachable from a unit test, so they are not
// exercised here. Only the PURE helpers below are covered.

// TestContainerCov_InjectBudgetEnv_MonthlyOverspentClamps covers the
// monthly remaining<0 clamp branch (the daily clamp is covered
// elsewhere; this pins the monthly arm).
func TestContainerCov_InjectBudgetEnv_MonthlyOverspentClamps(t *testing.T) {
	proj := &registry.Project{
		ID:     "p1",
		Budget: registry.ProjectBudget{MonthlyHardUSD: 50},
	}
	repo := &stubBudgetRepo{monthly: 80} // over the monthly cap
	env := map[string]string{}

	_, err := injectBudgetEnv(context.Background(), env, repo, proj, midMonth)
	require.NoError(t, err)
	assert.Equal(t, "0.0000", env["VORNIK_BUDGET_MONTHLY_REMAINING_USD"],
		"monthly remaining must clamp to 0 when overspent")
	_, hasD := env["VORNIK_BUDGET_DAILY_REMAINING_USD"]
	assert.False(t, hasD, "no daily cap → no daily env var")
}

// containerCov_emptyKeyMinter implements warmProjectKeyMinter but
// returns ("", nil) — a mint that "succeeds" with no key. The helper
// must treat that as a failure (return false) without logging an
// error (err is nil).
type containerCov_emptyKeyMinter struct{}

func (containerCov_emptyKeyMinter) MintTaskKey(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (containerCov_emptyKeyMinter) RevokeTaskKey(_ context.Context, _ string) error { return nil }
func (containerCov_emptyKeyMinter) MintProjectScopedKey(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

// TestContainerCov_InjectWarmProjectKey_EmptyKeyIsFailure covers the
// "minted empty key, no error" branch — env left untouched, false.
func TestContainerCov_InjectWarmProjectKey_EmptyKeyIsFailure(t *testing.T) {
	e := &Executor{apiKeyMinter: containerCov_emptyKeyMinter{}, logger: zerolog.Nop()}
	roleEnv := map[string]string{"VORNIK_API_KEY": "static"}
	ok := e.injectWarmProjectKey(context.Background(), "proj", "coder", roleEnv)
	assert.False(t, ok, "an empty minted key must be treated as a failure")
	assert.Equal(t, "static", roleEnv["VORNIK_API_KEY"], "env must be untouched when the key is empty")
}

// TestContainerCov_InjectWarmProjectKey_EmptyProjectIDGuard covers
// the empty-projectID guard (returns false before any mint).
func TestContainerCov_InjectWarmProjectKey_EmptyProjectIDGuard(t *testing.T) {
	minter := &stubAPIKeyMinter{projectKey: "sk-x"}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}
	roleEnv := map[string]string{}
	if ok := e.injectWarmProjectKey(context.Background(), "", "coder", roleEnv); ok {
		t.Error("empty projectID must short-circuit to false")
	}
	if len(minter.projectMintCalls) != 0 {
		t.Error("no mint should be attempted with an empty projectID")
	}
}

// TestContainerCov_InjectWarmProjectKey_NilMinterGuard covers the
// nil-minter guard.
func TestContainerCov_InjectWarmProjectKey_NilMinterGuard(t *testing.T) {
	e := &Executor{apiKeyMinter: nil, logger: zerolog.Nop()}
	if ok := e.injectWarmProjectKey(context.Background(), "proj", "coder", map[string]string{}); ok {
		t.Error("nil minter must short-circuit to false")
	}
}

// containerCov_errMinter logs an error on mint so the warn branch in
// injectWarmProjectKey is exercised explicitly here as well.
type containerCov_errMinter struct{}

func (containerCov_errMinter) MintTaskKey(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (containerCov_errMinter) RevokeTaskKey(_ context.Context, _ string) error { return nil }
func (containerCov_errMinter) MintProjectScopedKey(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("mint boom")
}

func TestContainerCov_InjectWarmProjectKey_MintErrorLogs(t *testing.T) {
	e := &Executor{apiKeyMinter: containerCov_errMinter{}, logger: zerolog.Nop()}
	roleEnv := map[string]string{"VORNIK_API_KEY": "static"}
	if ok := e.injectWarmProjectKey(context.Background(), "proj", "coder", roleEnv); ok {
		t.Error("mint error must yield false")
	}
}

// TestContainerCov_ResolveStagingSrc_EvalSymlinksFallbackAccept
// covers the EvalSymlinks-fails fallback that still ACCEPTS: a
// not-yet-existing file beneath an allowed root. EvalSymlinks errors
// (no such file) so the code clean-falls-back to the abs path, which
// still passes pathUnderAny.
func TestContainerCov_ResolveStagingSrc_EvalSymlinksFallbackAccept(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	// A path under the root that does not exist on disk yet.
	candidate := filepath.Join(resolvedRoot, "subdir", "not-created-yet.txt")
	got, ok := resolveStagingSrc(candidate, []string{resolvedRoot})
	if !ok {
		t.Fatalf("non-existent path under an allowed root should accept via the clean-fallback, got reject")
	}
	if got != filepath.Clean(candidate) {
		t.Errorf("expected clean(abs) fallback path %q, got %q", filepath.Clean(candidate), got)
	}
}

// TestContainerCov_ResolveStagingSrc_RelativeUnderCWDRejected covers
// the relative-path-made-absolute branch: a bare relative path is
// resolved against CWD, then rejected because CWD isn't an allowed
// root. (Pre-audit this walked straight through.)
func TestContainerCov_ResolveStagingSrc_RelativeRejected(t *testing.T) {
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	if _, ok := resolveStagingSrc("relative/path.txt", []string{resolved}); ok {
		t.Error("a relative path resolved against CWD must be rejected (CWD is not an allowed root)")
	}
}
