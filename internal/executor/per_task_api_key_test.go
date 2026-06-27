package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubAPIKeyMinter is a test double for APIKeyMinter. It optionally
// implements warmProjectKeyMinter (Finding B1(b)) when projectKey is set.
type stubAPIKeyMinter struct {
	mintKey    string
	mintErr    error
	revoked    []string
	revokeErr  error
	projectKey string
	projectErr error
	// projectMintCalls records (projectID, role) tuples passed to
	// MintProjectScopedKey so warm-path tests can assert scoping.
	projectMintCalls [][2]string
}

func (s *stubAPIKeyMinter) MintTaskKey(_ context.Context, _, _ string) (string, error) {
	return s.mintKey, s.mintErr
}
func (s *stubAPIKeyMinter) RevokeTaskKey(_ context.Context, taskID string) error {
	if s.revokeErr != nil {
		return s.revokeErr
	}
	s.revoked = append(s.revoked, taskID)
	return nil
}
func (s *stubAPIKeyMinter) MintProjectScopedKey(_ context.Context, projectID, role string) (string, error) {
	s.projectMintCalls = append(s.projectMintCalls, [2]string{projectID, role})
	return s.projectKey, s.projectErr
}

// Compile-time guard: stubAPIKeyMinter must satisfy APIKeyMinter and the
// optional warm-path interface.
var _ APIKeyMinter = (*stubAPIKeyMinter)(nil)
var _ warmProjectKeyMinter = (*stubAPIKeyMinter)(nil)

// TestInjectWarmProjectKey_ScopedKeyReplacesStatic — Finding B1(b).
// Warm-pool containers previously ran on the UNSCOPED static agent key
// (container.go ~1297 logged a warning). Pools are keyed per
// (project, role, image), so a project-scoped key closes the
// cross-project escalation. injectWarmProjectKey must mint a key scoped
// to the pool's project and overwrite BOTH VORNIK_API_KEY and
// VORNIK_LLM_API_KEY in the warm roleEnv.
func TestInjectWarmProjectKey_ScopedKeyReplacesStatic(t *testing.T) {
	minter := &stubAPIKeyMinter{projectKey: "sk-vornik-projwarm.warmsecret"}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}

	roleEnv := map[string]string{
		"VORNIK_API_KEY":     "73721891-unscoped-all-access",
		"VORNIK_LLM_API_KEY": "73721891-unscoped-all-access",
	}

	ok := e.injectWarmProjectKey(context.Background(), "projwarm", "coder", roleEnv)

	assert.True(t, ok, "warm project key injection must report success when minted")
	assert.Equal(t, "sk-vornik-projwarm.warmsecret", roleEnv["VORNIK_API_KEY"],
		"warm VORNIK_API_KEY must be the project-scoped key, not the static all-access key")
	assert.Equal(t, "sk-vornik-projwarm.warmsecret", roleEnv["VORNIK_LLM_API_KEY"],
		"warm VORNIK_LLM_API_KEY must be the project-scoped key, not the static all-access key")
	require.Len(t, minter.projectMintCalls, 1)
	assert.Equal(t, [2]string{"projwarm", "coder"}, minter.projectMintCalls[0],
		"minted key must be scoped to the pool's (project, role)")
}

// TestInjectWarmProjectKey_MintFailureKeepsStatic — when the project-key
// mint fails the warm container keeps its existing env (availability beats
// key freshness, mirroring the ephemeral path). Returns false.
func TestInjectWarmProjectKey_MintFailureKeepsStatic(t *testing.T) {
	minter := &stubAPIKeyMinter{projectErr: errors.New("db down")}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}
	roleEnv := map[string]string{"VORNIK_API_KEY": "static"}

	ok := e.injectWarmProjectKey(context.Background(), "projwarm", "coder", roleEnv)

	assert.False(t, ok)
	assert.Equal(t, "static", roleEnv["VORNIK_API_KEY"],
		"static key must survive a mint failure on the warm path")
}

// TestInjectWarmProjectKey_NonProjectMinterIsNoop — a minter that does not
// implement warmProjectKeyMinter (e.g. a dev stub) leaves env untouched and
// returns false, preserving warm-pool reuse.
func TestInjectWarmProjectKey_NonProjectMinterIsNoop(t *testing.T) {
	e := &Executor{apiKeyMinter: &taskOnlyMinter{}, logger: zerolog.Nop()}
	roleEnv := map[string]string{"VORNIK_API_KEY": "static"}

	ok := e.injectWarmProjectKey(context.Background(), "projwarm", "coder", roleEnv)

	assert.False(t, ok)
	assert.Equal(t, "static", roleEnv["VORNIK_API_KEY"])
}

// taskOnlyMinter implements APIKeyMinter but NOT warmProjectKeyMinter.
type taskOnlyMinter struct{}

func (taskOnlyMinter) MintTaskKey(_ context.Context, _, _ string) (string, error) { return "x", nil }
func (taskOnlyMinter) RevokeTaskKey(_ context.Context, _ string) error            { return nil }

var _ APIKeyMinter = taskOnlyMinter{}

// TestInjectPerTaskKey_OverridesVORNIK_API_KEY — when the minter is wired
// and the task has a non-empty ProjectID, injectPerTaskKey must overwrite
// VORNIK_API_KEY in the env map with the minted raw key.
func TestInjectPerTaskKey_OverridesVORNIK_API_KEY(t *testing.T) {
	minter := &stubAPIKeyMinter{mintKey: "sk-vornik-proj.rawsecret42"}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}

	env := map[string]string{
		"VORNIK_API_KEY":   "old-static-key",
		"VORNIK_LLM_MODEL": "gpt-oss-20b",
	}

	e.injectPerTaskKey(context.Background(), "proj-1", "task-abc", env)

	assert.Equal(t, "sk-vornik-proj.rawsecret42", env["VORNIK_API_KEY"],
		"VORNIK_API_KEY must be replaced with the minted raw key")
	assert.Equal(t, "gpt-oss-20b", env["VORNIK_LLM_MODEL"],
		"unrelated env vars must be untouched")
}

// TestInjectPerTaskKey_OverridesVORNIK_LLM_API_KEY — Finding B1(a).
// The agent entrypoint calls the daemon's chat-completions proxy with
// VORNIK_LLM_API_KEY. Before the fix this carried the UNSCOPED static
// agent key, so a prompt-injected agent could read $VORNIK_LLM_API_KEY
// and present it as an all-access credential. After the fix the LLM key
// MUST be the same project-scoped per-task key as VORNIK_API_KEY, so the
// container carries no unscoped credential. The chat proxy accepts a
// project-scoped key for the agent's own project (requestAllowsProject).
func TestInjectPerTaskKey_OverridesVORNIK_LLM_API_KEY(t *testing.T) {
	minter := &stubAPIKeyMinter{mintKey: "sk-vornik-proj.rawsecret42"}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}

	env := map[string]string{
		"VORNIK_API_KEY":     "old-static-key",
		"VORNIK_LLM_API_KEY": "73721891-unscoped-all-access",
	}

	e.injectPerTaskKey(context.Background(), "proj-1", "task-abc", env)

	assert.Equal(t, "sk-vornik-proj.rawsecret42", env["VORNIK_LLM_API_KEY"],
		"VORNIK_LLM_API_KEY must be replaced with the scoped per-task key (no unscoped credential in-container)")
	assert.Equal(t, env["VORNIK_API_KEY"], env["VORNIK_LLM_API_KEY"],
		"both daemon-callback creds must be the same minted scoped key")
}

// TestInjectPerTaskKey_MintErrorKeepsStaticKey — when mint fails, the
// static key from AgentLLMEnv must pass through unchanged. The task must
// still be startable (availability beats key freshness).
func TestInjectPerTaskKey_MintErrorKeepsStaticKey(t *testing.T) {
	minter := &stubAPIKeyMinter{mintErr: errors.New("db unavailable")}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}

	env := map[string]string{"VORNIK_API_KEY": "static-fallback-key"}

	// injectPerTaskKey must not return an error; it logs and falls back.
	e.injectPerTaskKey(context.Background(), "proj-1", "task-abc", env)

	assert.Equal(t, "static-fallback-key", env["VORNIK_API_KEY"],
		"static key must survive when mint fails")
}

// TestInjectPerTaskKey_NilMinterLeavesEnvUntouched — nil minter is the
// sqlite/dev posture. The static VORNIK_API_KEY must pass through
// unchanged without any nil-dereference.
func TestInjectPerTaskKey_NilMinterLeavesEnvUntouched(t *testing.T) {
	e := &Executor{apiKeyMinter: nil}
	env := map[string]string{"VORNIK_API_KEY": "static-key"}

	e.injectPerTaskKey(context.Background(), "proj-1", "task-abc", env)

	assert.Equal(t, "static-key", env["VORNIK_API_KEY"])
}

// TestInjectPerTaskKey_EmptyProjectIDSkipsMint — empty project ID is not a
// valid mint target; the env must remain unchanged even if a minter is wired.
func TestInjectPerTaskKey_EmptyProjectIDSkipsMint(t *testing.T) {
	minter := &stubAPIKeyMinter{mintKey: "sk-vornik-proj.should-not-appear"}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}
	env := map[string]string{"VORNIK_API_KEY": "static-key"}

	e.injectPerTaskKey(context.Background(), "" /*projectID*/, "task-abc", env)

	assert.Equal(t, "static-key", env["VORNIK_API_KEY"],
		"empty project ID must skip mint")
}

// TestRevokeTaskKey_DelegatesWithCorrectID — revokeTaskKey must call
// the minter's RevokeTaskKey with exactly the task ID provided.
func TestRevokeTaskKey_DelegatesWithCorrectID(t *testing.T) {
	minter := &stubAPIKeyMinter{}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}

	e.revokeTaskKey("task-revoke-me")

	require.Len(t, minter.revoked, 1)
	assert.Equal(t, "task-revoke-me", minter.revoked[0])
}

// TestRevokeTaskKey_NilMinterIsNoop — nil minter must not panic.
func TestRevokeTaskKey_NilMinterIsNoop(t *testing.T) {
	e := &Executor{apiKeyMinter: nil}
	// Must not panic.
	e.revokeTaskKey("task-xyz")
}

// TestRevokeTaskKey_ErrorIsLoggedNotReturned — revokeTaskKey absorbs the
// error from a failing minter (logs WARN). It is called from a defer and
// must not propagate.
func TestRevokeTaskKey_ErrorIsLoggedNotReturned(t *testing.T) {
	minter := &stubAPIKeyMinter{revokeErr: errors.New("db down")}
	e := &Executor{apiKeyMinter: minter, logger: zerolog.Nop()}
	// Must not panic or bubble the error.
	e.revokeTaskKey("task-abc")
}

// TestMintAndRevoke_StartContainerFailureStillRevokes — pin that the
// mint→defer-revoke ordering introduced in executeAgentStep means a
// startContainer failure does NOT leak the minted key.
//
// The test drives the ordering directly: injectPerTaskKey on extraEnv,
// defer revokeTaskKey (as executeAgentStep does), then startContainer
// via a runtime that returns startErr. The minter must record exactly
// one revoke even though the container never started.
func TestMintAndRevoke_StartContainerFailureStillRevokes(t *testing.T) {
	minter := &stubAPIKeyMinter{mintKey: "sk-vornik-proj.ephemeral99"}
	rt := NewMockRuntime()
	rt.startErr = errors.New("container start failed")

	e := &Executor{
		apiKeyMinter: minter,
		logger:       zerolog.Nop(),
		runtime:      rt,
		config: &Config{
			AgentLLMEnv: map[string]string{"VORNIK_API_KEY": "static-key"},
		},
	}

	task := &persistence.Task{ID: "task-order-test", ProjectID: "proj-order"}
	role := &registry.SwarmRole{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}

	runStep := func() {
		extraEnv := make(map[string]string)
		if minted := e.injectPerTaskKey(context.Background(), task.ProjectID, task.ID, extraEnv); minted {
			defer e.revokeTaskKey(task.ID) // mirrors executeAgentStep ordering
		}
		// startContainer errors — revoke must still fire via the defer above.
		_, _ = e.startContainer(context.Background(), task, "exec-1", role.Runtime.Image, role.Name,
			t.TempDir(), t.TempDir(), t.TempDir(), role, "", 5*time.Second, extraEnv)
	}

	runStep()

	require.Len(t, minter.revoked, 1,
		"revokeTaskKey must be called once even when startContainer fails")
	assert.Equal(t, task.ID, minter.revoked[0])
}

// TestWarmPoolStaticKeyWarnOnce_DeduplicatesPerProjectRole — the warm-pool
// warn dedup fires at most once per (project, role) key within a process.
// Uses shouldWarnWarmStaticKey (the production function backed by the
// package-level warmPoolStaticKeyWarnOnce map) so the test exercises the
// real code path, not a local re-implementation.
//
// Unique project/role strings are used per sub-case to avoid cross-test
// contamination from the persistent package-level map.
func TestWarmPoolStaticKeyWarnOnce_DeduplicatesPerProjectRole(t *testing.T) {
	// The dedup map is package-level and persists for the process lifetime,
	// so clear this test's keys up front — otherwise a second pass under
	// `go test -count>1` sees them already stored and the first-call
	// assertions fail. Key format is projectID + "/" + roleName.
	warmPoolStaticKeyWarnOnce.Delete("proj-dedup-A/role-alpha")
	warmPoolStaticKeyWarnOnce.Delete("proj-dedup-A/role-beta")

	// First call for the project/role-a pair must return true (should warn).
	first := shouldWarnWarmStaticKey("proj-dedup-A", "role-alpha")
	assert.True(t, first, "first call for proj-dedup-A/role-alpha must return true")

	// Duplicate call for the identical pair must return false (already warned).
	second := shouldWarnWarmStaticKey("proj-dedup-A", "role-alpha")
	assert.False(t, second, "second call for proj-dedup-A/role-alpha must return false (dedup)")

	// A different role for the same project must be independent — must warn.
	differentRole := shouldWarnWarmStaticKey("proj-dedup-A", "role-beta")
	assert.True(t, differentRole, "first call for proj-dedup-A/role-beta must return true (different role)")
}
