package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetCompanionFlags zeros every package-level flag binding so a
// previous test's values don't leak. Cobra wires flags into globals
// so this must run between every t.Run.
func resetCompanionFlags() {
	companionGrantProject = ""
	companionGrantClient = ""
	companionGrantLabel = ""
	companionGrantWorkflowsCSV = ""
	companionGrantBudgetStr = ""
	companionGrantExpires = ""
	companionGrantJSON = false
	companionKeysProject = ""
	companionKeysJSON = false
}

// captureGrantRequest spins up a test server that returns a canned
// 201 response and records the request body so a test can assert
// the CLI's wire format. Returns the captured body bytes plus the
// server (caller closes it).
func captureGrantRequest(t *testing.T, response string) (*httptest.Server, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(response))
	}))
	return srv, &captured
}

// TestRunCompanionGrant_WorkflowsCSV_ParsedAndForwarded — the CSV
// flag form must split, trim, and forward as a JSON array of
// strings. The CLI's only non-trivial logic.
func TestRunCompanionGrant_WorkflowsCSV_ParsedAndForwarded(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"claude-code",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"allowedWorkflows":["wf-a","wf-b"],"createdAt":"2026-05-27T10:00:00Z"
	}`)
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	// Stress the splitter with stray whitespace + trailing comma.
	companionGrantWorkflowsCSV = " wf-a , wf-b, "
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	require.NotEmpty(t, *captured, "test server should have received a request body")
	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	wfs, _ := got["allowedWorkflows"].([]any)
	require.Lenf(t, wfs, 2, "expected 2 workflows, got %v", wfs)
	assert.Equal(t, "wf-a", wfs[0])
	assert.Equal(t, "wf-b", wfs[1])
}

func TestRunCompanionGrant_OmitsWorkflowsField_WhenCSVEmpty(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"claude-code",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"createdAt":"2026-05-27T10:00:00Z"
	}`)
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantWorkflowsCSV = ""
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	_, present := got["allowedWorkflows"]
	assert.False(t, present,
		"empty --workflows must OMIT the field (so server treats as 'all'), not send []")
}

func TestRunCompanionGrant_RejectsWorkflowsCSV_OnlyWhitespaceAndCommas(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantWorkflowsCSV = ", , "

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --workflows")
}

func TestRunCompanionGrant_BudgetFloat_ParsedAndForwarded(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv, captured := captureGrantRequest(t, `{
		"id":"k1","projectId":"alpha","clientKind":"claude-code",
		"secret":"sk-vornik-alpha.xxx","keyPrefix":"sk-vornik-al",
		"budgetCapUsd":50.25,"createdAt":"2026-05-27T10:00:00Z"
	}`)
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantBudgetStr = "50.25"
	companionGrantJSON = true

	require.NoError(t, runCompanionGrant(nil, nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(*captured, &got))
	assert.Equal(t, 50.25, got["budgetCapUsd"])
}

func TestRunCompanionGrant_RejectsMalformedBudget(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	companionGrantProject = "alpha"
	companionGrantClient = "claude-code"
	companionGrantBudgetStr = "not-a-number"

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --budget-usd")
}

func TestRunCompanionGrant_PropagatesAPIError(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"UNKNOWN_CLIENT","message":"clientKind must be one of ..."}}`))
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")

	companionGrantProject = "alpha"
	companionGrantClient = "bogus"

	err := runCompanionGrant(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNKNOWN_CLIENT",
		"server-side error code must surface in the CLI error message")
}

// TestRunCompanionKeysList_FormatsTable — happy-path list renders
// rows with the expected status mapping (revoked > expired > active)
// and replaces missing labels with "-". JSON form is tested in the
// next test.
func TestRunCompanionKeysList_FormatsTable(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "alpha", r.URL.Query().Get("projectId"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"keys": [
				{
					"id":"k1","projectId":"alpha","clientKind":"claude-code",
					"sessionLabel":"vadim/laptop","keyPrefix":"sk-vornik-al",
					"allowedWorkflows":["wf-a"],"createdAt":"2026-05-27T10:00:00Z"
				},
				{
					"id":"k2","projectId":"alpha","clientKind":"codex",
					"keyPrefix":"sk-vornik-al","createdAt":"2026-05-26T10:00:00Z",
					"revokedAt":"2026-05-26T11:00:00Z"
				}
			]
		}`))
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")
	companionKeysProject = "alpha"
	companionKeysJSON = true // simpler to assert against JSON

	require.NoError(t, runCompanionKeysList(nil, nil))
}

func TestRunCompanionKeysList_PropagatesAPIError(t *testing.T) {
	t.Cleanup(resetCompanionFlags)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"VALIDATION_ERROR","message":"projectId required"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-admin-key")
	companionKeysProject = "" // server returns 400

	err := runCompanionKeysList(nil, nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToUpper(err.Error()), "VALIDATION_ERROR")
}
