package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func resetWorkflowProposalsFlags() {
	wfpListStatus = "pending"
	wfpListWorkflow = ""
	wfpListLimit = 50
	wfpListJSON = false
	wfpShowJSON = false
	wfpShowYAML = false
	wfpDecideNotes = ""
}

// LIST -----------------------------------------------------------

func TestWorkflowProposalsList_TableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/workflow-proposals" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("status"); got != "pending" {
			t.Errorf("status filter not forwarded: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wfProposalListJSON{
			Proposals: []wfProposalJSON{
				{ID: "wpr_20260525_abc", WorkflowID: "research", Status: "pending",
					Confidence: 0.81, EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
					CreatedAt: "2026-05-25T10:00:00Z"},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsList(workflowProposalsListCmd, nil)
	})
	if err != nil {
		t.Fatalf("runWorkflowProposalsList: %v", err)
	}
	if !strings.Contains(out, "wpr_20260525_abc") {
		t.Errorf("missing id in table: %s", out)
	}
	if !strings.Contains(out, "research") {
		t.Errorf("missing workflow_id: %s", out)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("missing status: %s", out)
	}
}

// TestWorkflowProposalsList_AllStatus — --status all should omit
// the status query parameter so the server returns every row.
func TestWorkflowProposalsList_AllStatus(t *testing.T) {
	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		_ = json.NewEncoder(w).Encode(wfProposalListJSON{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()
	wfpListStatus = "all"

	_, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsList(workflowProposalsListCmd, nil)
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotStatus != "" {
		t.Errorf("status=all should omit the query param, server saw %q", gotStatus)
	}
}

// TestWorkflowProposalsList_CustomCSVStatus — csv status filter
// passes through unchanged.
func TestWorkflowProposalsList_CustomCSVStatus(t *testing.T) {
	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		_ = json.NewEncoder(w).Encode(wfProposalListJSON{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()
	wfpListStatus = "approved,applied"
	wfpListWorkflow = "research"
	wfpListLimit = 5

	if _, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsList(workflowProposalsListCmd, nil)
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotStatus != "approved,applied" {
		t.Errorf("status: %q", gotStatus)
	}
}

func TestWorkflowProposalsList_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wfProposalListJSON{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsList(workflowProposalsListCmd, nil)
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "No proposals") {
		t.Errorf("empty list should say 'No proposals', got %q", out)
	}
}

func TestWorkflowProposalsList_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wfProposalListJSON{
			Proposals: []wfProposalJSON{{ID: "wpr-1", Status: "pending"}},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()
	wfpListJSON = true

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsList(workflowProposalsListCmd, nil)
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var decoded wfProposalListJSON
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("--json output not parseable JSON: %v: %s", err, out)
	}
	if len(decoded.Proposals) != 1 {
		t.Errorf("decoded proposals: %d", len(decoded.Proposals))
	}
}

func TestWorkflowProposalsList_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"WORKFLOW_PROPOSALS_DISABLED","message":"not wired"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()
	_, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsList(workflowProposalsListCmd, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("server error should propagate, got %v", err)
	}
}

// SHOW ----------------------------------------------------------

func TestWorkflowProposalsShow_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", WorkflowID: "research", Status: "approved",
			Confidence: 0.83, Motivation: "across 9 runs the implement step regressed",
			EvidenceRunIDs: []string{"r-2", "r-1", "r-3"},
			ProposalYAML:   "---\nworkflowId: research\n---\n# body\n",
			DecidedBy:      "operator-x",
			DecidedAt:      "2026-05-25T11:00:00Z",
			CreatedAt:      "2026-05-25T10:00:00Z",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsShow(workflowProposalsShowCmd, []string{"wpr-1"})
	})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{
		"wpr-1", "research", "approved",
		"operator-x", "implement step",
		"=== Proposed WORKFLOW.md ===",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q: %s", want, out)
		}
	}
	// Evidence should be sorted alphabetically so the output is
	// stable.
	idx1 := strings.Index(out, "r-1")
	idx2 := strings.Index(out, "r-2")
	idx3 := strings.Index(out, "r-3")
	if idx1 >= idx2 || idx2 >= idx3 {
		t.Errorf("evidence not sorted: r-1@%d r-2@%d r-3@%d", idx1, idx2, idx3)
	}
}

func TestWorkflowProposalsShow_YAMLOnly(t *testing.T) {
	yaml := "---\nworkflowId: research\nversion: 2.0.0\n---\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", ProposalYAML: yaml,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()
	wfpShowYAML = true

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsShow(workflowProposalsShowCmd, []string{"wpr-1"})
	})
	if err != nil {
		t.Fatalf("show --yaml: %v", err)
	}
	if out != yaml {
		t.Errorf("--yaml output should be raw YAML only, got %q", out)
	}
}

func TestWorkflowProposalsShow_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"proposal not found"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	_, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsShow(workflowProposalsShowCmd, []string{"missing"})
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404 should propagate, got %v", err)
	}
}

// APPROVE / REJECT ----------------------------------------------

func TestDecideProposal_Approve_SendsCorrectBody(t *testing.T) {
	var gotBody bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_, _ = gotBody.ReadFrom(r.Body)
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", Status: "approved", DecidedBy: "sk-admin",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return decideProposal("wpr-1", "approved", "ship it")
	})
	if err != nil {
		t.Fatalf("decide approve: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(gotBody.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "approved" {
		t.Errorf("status: %q", body["status"])
	}
	if body["notes"] != "ship it" {
		t.Errorf("notes: %q", body["notes"])
	}
	if !strings.Contains(out, "approved") || !strings.Contains(out, "sk-admin") {
		t.Errorf("output should mention status + decider: %s", out)
	}
}

func TestDecideProposal_Reject_NoNotes(t *testing.T) {
	var gotBody bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = gotBody.ReadFrom(r.Body)
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", Status: "rejected", DecidedBy: "sk-admin",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	if _, err := captureStdoutFn(t, func() error {
		return decideProposal("wpr-1", "rejected", "")
	}); err != nil {
		t.Fatalf("decide reject: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(gotBody.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, hasNotes := body["notes"]; hasNotes {
		t.Errorf("empty notes should be omitted from body, got %+v", body)
	}
}

func TestDecideProposal_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_TRANSITION","message":"already decided"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	_, err := captureStdoutFn(t, func() error {
		return decideProposal("wpr-1", "approved", "")
	})
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("409 should propagate, got %v", err)
	}
}

// TestDecideProposal_MinimalServerResponse — server returned 200
// with the minimal {id, status} fallback (read-after-write failed
// server-side). The CLI should still report success, not error.
func TestDecideProposal_MinimalServerResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal payload — no decided_by, no other fields. The
		// CLI's fallback path is what we want to cover here.
		_, _ = w.Write([]byte(`{"id":"wpr-1","status":"approved"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return decideProposal("wpr-1", "approved", "")
	})
	if err != nil {
		t.Fatalf("decide should succeed on minimal payload, got %v", err)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("output should still mention status: %s", out)
	}
}

// TestWorkflowProposalsShow_JSONBranch — --json emits a JSON
// object the caller can re-parse.
func TestWorkflowProposalsShow_JSONBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", Status: "pending",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()
	wfpShowJSON = true

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsShow(workflowProposalsShowCmd, []string{"wpr-1"})
	})
	if err != nil {
		t.Fatalf("show --json: %v", err)
	}
	var got wfProposalJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json should emit valid JSON, got %q: %v", out, err)
	}
	if got.ID != "wpr-1" {
		t.Errorf("decoded id: %q", got.ID)
	}
}

// TestWorkflowProposalsShow_AppliedAndRolledBackBranches pins the
// renderer's optional-block branches so the applied_at / rollback
// lines are exercised.
func TestWorkflowProposalsShow_AppliedAndRolledBackBranches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", WorkflowID: "research", Status: "rolled_back",
			AppliedAt: "2026-05-25T11:00:00Z", AppliedCommit: "abc1234",
			RollbackCommit: "def5678",
			Notes:          "regressed",
			CreatedAt:      "2026-05-25T10:00:00Z",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return runWorkflowProposalsShow(workflowProposalsShowCmd, []string{"wpr-1"})
	})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{
		"Applied:", "abc1234", "Rollback:", "def5678", "regressed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q: %s", want, out)
		}
	}
}

// APPLY / ROLLBACK ----------------------------------------------

func TestPostProposalAction_Apply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/apply") {
			t.Errorf("path should end with /apply: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", Status: "applied", AppliedCommit: "abc1234567890",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return postProposalAction("wpr-1", "apply")
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "applied") || !strings.Contains(out, "abc1234") {
		t.Errorf("output should mention status + commit: %s", out)
	}
}

func TestPostProposalAction_Rollback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rollback") {
			t.Errorf("path should end with /rollback: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(wfProposalJSON{
			ID: "wpr-1", Status: "rolled_back", RollbackCommit: "def5678",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	out, err := captureStdoutFn(t, func() error {
		return postProposalAction("wpr-1", "rollback")
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !strings.Contains(out, "rolled_back") || !strings.Contains(out, "def5678") {
		t.Errorf("output should mention status + commit: %s", out)
	}
}

func TestPostProposalAction_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":"PROPOSAL_NOT_APPROVED","message":"not approved"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetWorkflowProposalsFlags()

	_, err := captureStdoutFn(t, func() error {
		return postProposalAction("wpr-1", "apply")
	})
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("409 should propagate, got %v", err)
	}
}

// HELPERS -------------------------------------------------------

func TestIndent_PrefixesEveryLine(t *testing.T) {
	if got := indent("a\nb\nc", "> "); got != "> a\n> b\n> c" {
		t.Errorf("indent: %q", got)
	}
	if got := indent("", "> "); got != "" {
		t.Errorf("empty should stay empty: %q", got)
	}
}
