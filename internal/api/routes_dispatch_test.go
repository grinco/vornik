// Package api: tests for the per-resource path-dispatch routers
// (apiV1ExecutionsHandler, apiV1SwarmsHandler, apiV1WorkflowsHandler,
// apiV1ProjectsHandler).
//
// These tests don't aim to exhaustively cover every backing handler
// — those have their own dedicated tests. The goal here is to pin
// the URL-shape → handler routing contract: the right handler fires
// for the right path/method, and method-mismatch / unknown-path get
// the documented response.
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- apiV1ExecutionsHandler ------------------------------------------

func TestAPIv1Executions_MissingExecutionID(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ExecutionsHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestAPIv1Executions_UnknownPath_NotFound(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1/banana", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ExecutionsHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestAPIv1Executions_MethodMismatch_NotFound(t *testing.T) {
	// /pause requires POST; GET should fall through to 404 (no
	// matching switch arm).
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1/pause", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ExecutionsHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestAPIv1Executions_DispatchesGet(t *testing.T) {
	// No execution repo → handler will respond with the documented
	// repo-not-configured error. We just need a non-NotFound code
	// to confirm dispatch happened.
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ExecutionsHandler(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Errorf("expected dispatch to GetExecution; got 404")
	}
}

func TestAPIv1Executions_DispatchesPauseResumeRetry(t *testing.T) {
	cases := []struct{ path string }{
		{"/api/v1/executions/exec-1/pause"},
		{"/api/v1/executions/exec-1/resume"},
		{"/api/v1/executions/exec-1/retry-from-step"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			srv := &Server{}
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.apiV1ExecutionsHandler(rec, req)
			// Dispatch confirmed by non-404 — backend handlers return
			// 503 / 500 / 400 depending on missing deps.
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s: expected dispatch, got 404", tc.path)
			}
		})
	}
}

// --- apiV1SwarmsHandler / apiV1WorkflowsHandler ----------------------

func TestAPIv1Swarms_DispatchesList(t *testing.T) {
	srv := &Server{}
	// /api/v1/swarms/ with empty ID dispatches to ListSwarms; nil
	// registry → 200 with empty list per ListSwarms semantics.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms/", nil)
	rec := httptest.NewRecorder()
	srv.apiV1SwarmsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from nil-registry list dispatch; got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIv1Swarms_DispatchesGet(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/swarms/sw-1", nil)
	rec := httptest.NewRecorder()
	srv.apiV1SwarmsHandler(rec, req)
	// Nil registry → 503 per GetSwarm contract.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", rec.Code)
	}
}

func TestAPIv1Workflows_DispatchesList(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/", nil)
	rec := httptest.NewRecorder()
	srv.apiV1WorkflowsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}

func TestAPIv1Workflows_DispatchesGet(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/wf-1", nil)
	rec := httptest.NewRecorder()
	srv.apiV1WorkflowsHandler(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", rec.Code)
	}
}

// --- apiV1ProjectsHandler --------------------------------------------

func TestAPIv1Projects_RootRedirectsToList(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ProjectsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (list dispatch)", rec.Code)
	}
}

func TestAPIv1Projects_RootPostInvalid(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ProjectsHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// The projects dispatcher routes a huge surface — table-driven sanity
// check confirming each known suffix gets dispatched. Some backing
// handlers respond with 404 by design when their deps are nil (e.g.
// CallMCPTool: "MCP is not configured"); for those rows we set
// allow404=true. The contract being pinned is "dispatcher routed the
// request" — confirmed when the body contains a JSON error envelope
// rather than the raw http.NotFound "404 page not found\n" string.
func TestAPIv1Projects_DispatchesKnownPaths(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"config-get", http.MethodGet, "/api/v1/projects/p1/config"},
		{"autonomy-eval-list", http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations"},
		{"autonomy-summary", http.MethodGet, "/api/v1/projects/p1/autonomy/summary"},
		{"webhooks-events", http.MethodGet, "/api/v1/projects/p1/webhooks/events"},
		{"memory-search", http.MethodGet, "/api/v1/projects/p1/memory/search"},
		{"memory-feedback", http.MethodGet, "/api/v1/projects/p1/memory/feedback"},
		{"memory-epochs", http.MethodGet, "/api/v1/projects/p1/memory/epochs"},
		{"memory-rollback", http.MethodPost, "/api/v1/projects/p1/memory/rollback"},
		{"memory-rollbacks", http.MethodGet, "/api/v1/projects/p1/memory/rollbacks"},
		{"memory-quarantine-list", http.MethodGet, "/api/v1/projects/p1/memory/quarantine"},
		{"memory-quarantine-action", http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop"},
		{"memory-health", http.MethodGet, "/api/v1/projects/p1/memory/health"},
		{"mcp-tools", http.MethodGet, "/api/v1/projects/p1/mcp/tools"},
		{"mcp-tools-call", http.MethodPost, "/api/v1/projects/p1/mcp/tools/call"},
		{"tasks-list-get", http.MethodGet, "/api/v1/projects/p1/tasks"},
		{"tasks-csv", http.MethodGet, "/api/v1/projects/p1/tasks.csv"},
		{"audit-csv", http.MethodGet, "/api/v1/projects/p1/audit.csv"},
		{"spend-csv", http.MethodGet, "/api/v1/projects/p1/spend.csv"},
		{"executions-list", http.MethodGet, "/api/v1/projects/p1/executions"},
		{"task-messages-list", http.MethodGet, "/api/v1/projects/p1/tasks/t1/messages"},
		{"task-amend", http.MethodPost, "/api/v1/projects/p1/tasks/t1/amend"},
		{"task-pause", http.MethodPost, "/api/v1/projects/p1/tasks/t1/pause"},
		{"task-resume", http.MethodPost, "/api/v1/projects/p1/tasks/t1/resume"},
		{"task-close", http.MethodPost, "/api/v1/projects/p1/tasks/t1/close"},
		{"task-summarize", http.MethodPost, "/api/v1/projects/p1/tasks/t1/summarize"},
		{"task-cancel", http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel"},
		{"task-cancel-trailing-slash", http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel/"},
		{"task-retry", http.MethodPost, "/api/v1/projects/p1/tasks/t1/retry"},
		{"task-retry-trailing-slash", http.MethodPost, "/api/v1/projects/p1/tasks/t1/retry/"},
		{"task-logs", http.MethodGet, "/api/v1/projects/p1/tasks/t1/logs"},
		{"task-explain", http.MethodGet, "/api/v1/projects/p1/tasks/t1/explain"},
		{"task-get-by-id", http.MethodGet, "/api/v1/projects/p1/tasks/t1"},
		{"task-message-answer", http.MethodPost, "/api/v1/projects/p1/tasks/t1/messages/cp1/answer"},
		{"gist", http.MethodGet, "/api/v1/projects/p1/gist"},
		{"keys-list", http.MethodGet, "/api/v1/projects/p1/keys"},
		{"keys-create", http.MethodPost, "/api/v1/projects/p1/keys"},
	}
	srv := &Server{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			srv.apiV1ProjectsHandler(rec, req)
			body := rec.Body.String()
			// http.NotFound writes exactly "404 page not found\n";
			// anything else proves dispatch fired (handlers respond
			// with a JSON envelope on missing deps).
			if rec.Code == http.StatusNotFound && strings.Contains(body, "404 page not found") {
				t.Errorf("path %q method %q: dispatcher returned http.NotFound (no handler matched)", tc.path, tc.method)
			}
		})
	}
}

func TestAPIv1Projects_UnknownSubpath_NotFound(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/banana/peel", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ProjectsHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestAPIv1Projects_KeysRotate(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/keys/k1/rotate", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ProjectsHandler(rec, req)
	// dispatched (not 404)
	if rec.Code == http.StatusNotFound {
		t.Errorf("keys/rotate: dispatcher returned NotFound")
	}
}

func TestAPIv1Projects_KeysDelete(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/p1/keys/k1", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ProjectsHandler(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Errorf("keys delete: dispatcher returned NotFound")
	}
}

func TestAPIv1Projects_KeysInvalidPath(t *testing.T) {
	srv := &Server{}
	// /keys// is an invalid trailing-slash form that splitKeyActionPath rejects.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/p1/keys/", nil)
	rec := httptest.NewRecorder()
	srv.apiV1ProjectsHandler(rec, req)
	// Either 400 (split rejects) or 404 (no match) — pinning that we
	// don't crash + return a 4xx category.
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("expected 4xx for invalid keys path; got %d", rec.Code)
	}
}
