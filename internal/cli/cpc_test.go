package cli

// CLI tests for `vornikctl cpc {list,show,cancel}` (coverage-gap sweep
// 2026-06-18, Tier 3). Same harness as admin_test.go / blackbox_
// triggers_test.go: stub the daemon with httptest, point VORNIK_API_URL
// at it, capture stdout, assert the rendered output + that filters and
// the cancel reason reach the wire. captureStdoutFunc is shared from
// blackbox_triggers_test.go (same package).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetCPCFlags restores the package-level cobra flag vars to defaults
// so tests don't leak filter/JSON state into each other.
func resetCPCFlags() {
	cpcListStatus, cpcListCaller, cpcListCallee, cpcListSince = "", "", "", ""
	cpcListLimit, cpcListJSON = 50, false
	cpcShowJSON = false
	cpcCancelReason, cpcCancelJSON = "", false
}

func TestRunCPCList_TableForwardsFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/cpc" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("status") != "running" || q.Get("caller") != "marketing" ||
			q.Get("callee") != "architect" || q.Get("since") != "2026-05-20" || q.Get("limit") != "7" {
			t.Errorf("filters not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(cpcListResponse{Entries: []cpcEntry{
			{ID: "cpc-abc", Status: "running", CallerProject: "marketing", CalleeProject: "architect", CalleeWorkflow: "dev-pipeline", CreatedAt: "2026-05-21T10:00:00Z"},
		}})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()
	cpcListStatus, cpcListCaller, cpcListCallee, cpcListSince, cpcListLimit = "running", "marketing", "architect", "2026-05-20", 7

	out, err := captureStdoutFunc(t, func() error { return runCPCList(cpcListCmd, nil) })
	if err != nil {
		t.Fatalf("runCPCList: %v", err)
	}
	for _, want := range []string{"cpc-abc", "running", "marketing", "architect", "dev-pipeline"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q in:\n%s", want, out)
		}
	}
}

func TestRunCPCList_EmptyMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(cpcListResponse{Entries: nil})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()

	out, err := captureStdoutFunc(t, func() error { return runCPCList(cpcListCmd, nil) })
	if err != nil {
		t.Fatalf("runCPCList: %v", err)
	}
	if !strings.Contains(out, "No cross-project calls match") {
		t.Errorf("expected empty-result message, got:\n%s", out)
	}
}

func TestRunCPCList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(cpcListResponse{Entries: []cpcEntry{{ID: "cpc-json", Status: "completed"}}})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()
	cpcListJSON = true

	out, err := captureStdoutFunc(t, func() error { return runCPCList(cpcListCmd, nil) })
	if err != nil {
		t.Fatalf("runCPCList: %v", err)
	}
	var decoded cpcListResponse
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", jerr, out)
	}
	if len(decoded.Entries) != 1 || decoded.Entries[0].ID != "cpc-json" {
		t.Errorf("unexpected JSON payload: %+v", decoded)
	}
}

func TestRunCPCList_Non200IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin scope required"})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-not-admin")
	resetCPCFlags()

	_, err := captureStdoutFunc(t, func() error { return runCPCList(cpcListCmd, nil) })
	if err == nil {
		t.Fatal("expected an error on 403, got nil")
	}
}

func TestRunCPCList_RequestFailure(t *testing.T) {
	// Point at a closed port so the HTTP round-trip fails before any response.
	t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1")
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()

	_, err := captureStdoutFunc(t, func() error { return runCPCList(cpcListCmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("expected request-failed error, got %v", err)
	}
}

func TestRunCPCShow_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/cpc/cpc-show-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(cpcCancelResponse{Entry: cpcEntry{
			ID: "cpc-show-1", Status: "timed_out", CallerProject: "marketing",
			CallerTaskID: "task-1", CallerStepID: "step-2", CalleeProject: "architect",
			CalleeWorkflow: "dev-pipeline", CalleeTaskID: "task-9", ExpectedSchema: "result.v1",
			CreatedAt: "2026-05-21T10:00:00Z", TimeoutAt: "2026-05-21T11:00:00Z",
			ResolvedAt: "2026-05-21T11:05:00Z", ErrorMessage: "deadline exceeded",
		}})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()

	out, err := captureStdoutFunc(t, func() error { return runCPCShow(cpcShowCmd, []string{"cpc-show-1"}) })
	if err != nil {
		t.Fatalf("runCPCShow: %v", err)
	}
	// printCPCRow renders every populated field, including the optional ones.
	for _, want := range []string{"cpc-show-1", "timed_out", "architect", "dev-pipeline", "task-9", "result.v1", "deadline exceeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunCPCShow_Non200IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such cpc"})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()

	_, err := captureStdoutFunc(t, func() error { return runCPCShow(cpcShowCmd, []string{"missing"}) })
	if err == nil {
		t.Fatal("expected an error on 404, got nil")
	}
}

func TestRunCPCCancel_ForwardsReasonAndPrints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/admin/cpc/cpc-c1/cancel" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["reason"] != "stuck for 3h" {
			t.Errorf("reason not forwarded: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(cpcCancelResponse{Entry: cpcEntry{
			ID: "cpc-c1", Status: "rejected", ErrorMessage: "stuck for 3h",
		}})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetCPCFlags()
	cpcCancelReason = "stuck for 3h"

	out, err := captureStdoutFunc(t, func() error { return runCPCCancel(cpcCancelCmd, []string{"cpc-c1"}) })
	if err != nil {
		t.Fatalf("runCPCCancel: %v", err)
	}
	if !strings.Contains(out, "Cancelled cpc-c1") || !strings.Contains(out, "rejected") {
		t.Errorf("cancel output unexpected:\n%s", out)
	}
	if !strings.Contains(out, "stuck for 3h") {
		t.Errorf("cancel output missing reason echo:\n%s", out)
	}
}

func TestRunCPCCancel_Non200IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin scope required"})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-not-admin")
	resetCPCFlags()

	_, err := captureStdoutFunc(t, func() error { return runCPCCancel(cpcCancelCmd, []string{"cpc-c1"}) })
	if err == nil {
		t.Fatal("expected an error on 403, got nil")
	}
}
