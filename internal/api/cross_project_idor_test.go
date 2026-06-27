package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// multiKeyLookup is a hash-keyed APIKeyLookup fake for the IDOR
// E2E. Unlike stubAPIKeyLookup (single fixed row) it resolves each
// bearer to its own api_keys row, so two per-project keys can be
// authenticated in the same test through the real DBKeysBackend.
type multiKeyLookup struct {
	byHash map[string]*persistence.APIKey
}

func (m *multiKeyLookup) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	if row, ok := m.byHash[h]; ok {
		return row, nil
	}
	return nil, persistence.ErrAPIKeyNotFound
}

// wiredExecChain builds the EXACT production middleware stack the
// router wires for /api/v1/executions/{id} — AuthMiddleware (DB-key
// resolution + auth-enabled stamp) → ProjectAuthMiddleware (URL-scope
// gate) → the real apiV1ExecutionsHandler. The executions URL carries
// no `projects` segment, so ProjectAuthMiddleware short-circuits and
// the handler's own requestAllowsProject(exec.ProjectID) is the sole
// cross-project gate. This is what the test exercises end to end.
func wiredExecChain(lookup APIKeyLookup, exec *persistence.Execution) http.Handler {
	srv := NewServer(WithExecutionRepository(&stubExecRepoForFork{exec: exec}))
	var handler http.Handler = http.HandlerFunc(srv.apiV1ExecutionsHandler)
	handler = ProjectAuthMiddleware()(handler)
	handler = AuthMiddleware(AuthConfig{
		Enabled:      true,
		APIKeyLookup: lookup,
	})(handler)
	return handler
}

// TestCrossProjectIDOR_ExecutionReadScopedByKey is the Tier-1 IDOR
// characterization (https://docs.vornik.io): a per-project API key scoped to
// project A must be REJECTED when it reads an execution owned by
// project B, and ALLOWED on an execution owned by project A. Both the
// deny (cross) and allow (same) paths are asserted so the test proves
// the scoping holds, not just that some blanket 403 fires.
//
// A failure of the DENY assertion (key A CAN read project B's
// execution) would be a real cross-project data leak.
func TestCrossProjectIDOR_ExecutionReadScopedByKey(t *testing.T) {
	const (
		projectA = "alpha"
		projectB = "bravo"
	)

	keyA, err := apikey.Generate(projectA)
	require.NoError(t, err)
	keyB, err := apikey.Generate(projectB)
	require.NoError(t, err)

	rowA := &persistence.APIKey{
		ID: "akey-a", ProjectID: projectA,
		KeyHash: apikey.Hash(keyA), CreatedAt: time.Now(),
	}
	rowB := &persistence.APIKey{
		ID: "akey-b", ProjectID: projectB,
		KeyHash: apikey.Hash(keyB), CreatedAt: time.Now(),
	}
	lookup := &multiKeyLookup{byHash: map[string]*persistence.APIKey{
		apikey.Hash(keyA): rowA,
		apikey.Hash(keyB): rowB,
	}}

	// The target resource belongs to project B.
	execB := &persistence.Execution{ID: "exec_b", ProjectID: projectB}
	chain := wiredExecChain(lookup, execB)

	get := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec_b", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}

	t.Run("deny cross-project: key A cannot read project B's execution", func(t *testing.T) {
		rec := get(keyA)
		// 403 (scope denial) or 404 (existence-hiding) are both
		// acceptable IDOR responses; 200 would be a leak.
		require.Containsf(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code,
			"key scoped to %q must NOT read %q's execution (cross-project leak); body: %s",
			projectA, projectB, rec.Body.String())
		require.NotEqual(t, http.StatusOK, rec.Code,
			"SECURITY: key scoped to %q read %q's execution", projectA, projectB)
		require.NotContains(t, rec.Body.String(), "exec_b",
			"SECURITY: cross-project response leaked the execution id")
	})

	t.Run("allow same-project: key B can read its own execution", func(t *testing.T) {
		rec := get(keyB)
		require.Equalf(t, http.StatusOK, rec.Code,
			"key scoped to %q must read its own execution; body: %s",
			projectB, rec.Body.String())
		require.Contains(t, rec.Body.String(), "exec_b",
			"same-project read should return the execution payload")
	})
}
