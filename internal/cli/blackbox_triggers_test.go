package cli

// CLI tests for `vornikctl blackbox trigger` + `vornikctl blackbox
// override`. Same pattern as admin_test.go — spin up an httptest
// server, point VORNIK_API_URL at it, capture stdout, assert.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// captureStdoutFunc runs fn with stdout redirected to a pipe and
// returns whatever fn wrote. Restores stdout even when fn errors.
func captureStdoutFunc(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()
	runErr := fn()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b), runErr
}

// TestBlackBoxTriggerList_Table — happy-path tabular render.
func TestBlackBoxTriggerList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/workflow-healing/triggers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("project"); got != "p1" {
			t.Errorf("project filter not forwarded: %q", got)
		}
		_ = json.NewEncoder(w).Encode(healingTriggerListResp{
			Entries: []healingTriggerRow{
				{
					ID: "hb-1", ProjectID: "p1", WorkflowID: "wf-a",
					TriggerClass: "failure_rate_spike", Status: "open",
					BaselineValue: 0.10, ComparisonValue: 0.20,
					CreatedAt: "2026-05-26T12:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	triggerListProject = "p1"
	triggerListWorkflow, triggerListStatus, triggerListClass = "", "", ""
	triggerListLimit, triggerListJSON = 50, false

	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxTriggerList(nil, nil)
	})
	if err != nil {
		t.Fatalf("runBlackBoxTriggerList: %v", err)
	}
	for _, want := range []string{"hb-1", "p1/wf-a", "failure_rate_spike", "0.1000", "0.2000"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q in:\n%s", want, out)
		}
	}
}

// TestBlackBoxTriggerList_Empty — JSON response with no entries
// gets the friendly empty-state message instead of an empty table.
func TestBlackBoxTriggerList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(healingTriggerListResp{Entries: nil})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	triggerListProject, triggerListWorkflow, triggerListStatus, triggerListClass = "", "", "", ""
	triggerListLimit, triggerListJSON = 50, false
	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxTriggerList(nil, nil)
	})
	if err != nil {
		t.Fatalf("runBlackBoxTriggerList: %v", err)
	}
	if !strings.Contains(out, "No triggers match") {
		t.Errorf("expected friendly empty-state, got %q", out)
	}
}

// TestBlackBoxTriggerDismiss_Happy — POST .../{id}/dismiss returns
// 200; CLI prints confirmation.
func TestBlackBoxTriggerDismiss_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/admin/workflow-healing/triggers/hb-9/dismiss"
		if r.URL.Path != want {
			t.Errorf("path: %q, want %q", r.URL.Path, want)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: %s, want POST", r.Method)
		}
		_, _ = w.Write([]byte(`{"id":"hb-9","status":"dismissed"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxTriggerDismiss(nil, []string{"hb-9"})
	})
	if err != nil {
		t.Fatalf("runBlackBoxTriggerDismiss: %v", err)
	}
	if !strings.Contains(out, "hb-9 dismissed") {
		t.Errorf("output missing confirmation: %q", out)
	}
}

// TestBlackBoxTriggerGenerateCandidate_Happy — POST returns a
// trigger with the new proposal_id; CLI prints both IDs.
func TestBlackBoxTriggerGenerateCandidate_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"hb-9","workflow_id":"wf-a","status":"generated_candidate","proposal_id":"wpr-7"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	triggerGenerateJSON = false
	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxTriggerGenerateCandidate(nil, []string{"hb-9"})
	})
	if err != nil {
		t.Fatalf("runBlackBoxTriggerGenerateCandidate: %v", err)
	}
	for _, want := range []string{"hb-9", "wpr-7", "workflow-proposals show wpr-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
}

// TestBlackBoxTriggerBulkDismiss_PartialFailure — server returns
// partial failure shape; CLI exits non-zero AND prints the failed
// IDs.
func TestBlackBoxTriggerBulkDismiss_PartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(healingTriggerBulkDismissResp{
			Dismissed: 1,
			Failures: []healingBulkFailure{
				{ID: "hb-bad", Error: "not found"},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	triggerBulkJSON = false
	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxTriggerBulkDismiss(nil, []string{"hb-ok", "hb-bad"})
	})
	if err == nil {
		t.Error("expected non-nil error on partial failure")
	}
	if !strings.Contains(out, "Dismissed 1 of 2") || !strings.Contains(out, "FAIL hb-bad: not found") {
		t.Errorf("output incomplete: %q", out)
	}
}

// TestBlackBoxOverrideSet_NothingToSave — no action flag → CLI
// fails fast without a server round-trip.
func TestBlackBoxOverrideSet_NothingToSave(t *testing.T) {
	// Reset module-level flags so prior tests don't leak state.
	overrideSetProject, overrideSetWorkflow, overrideSetClass = "p1", "wf-1", "failure_rate_spike"
	overrideSetThreshold, overrideSetMuteHours = 0, 0
	overrideSetClearMute = false
	// Build a Cobra command with no flags Changed.
	cmd := newOverrideSetForTest()
	if err := runBlackBoxOverrideSet(cmd, nil); err == nil {
		t.Error("expected error when no action flag is set")
	}
}

// TestBlackBoxOverrideSet_Happy — exercises the JSON body the CLI
// emits to the server.
func TestBlackBoxOverrideSet_Happy(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"project_id":"p1","workflow_id":"wf-1","trigger_class":"failure_rate_spike","threshold_override":0.50,"muted_until":"2026-05-30T00:00:00Z"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	overrideSetProject, overrideSetWorkflow, overrideSetClass = "p1", "wf-1", "failure_rate_spike"
	overrideSetThreshold = 50.0
	overrideSetMuteHours = 6
	overrideSetClearMute = false
	overrideSetNotes = "tuning"
	overrideSetJSON = false
	cmd := newOverrideSetForTest()
	_ = cmd.Flags().Set("threshold-pct", "50")
	_ = cmd.Flags().Set("mute-hours", "6")
	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxOverrideSet(cmd, nil)
	})
	if err != nil {
		t.Fatalf("runBlackBoxOverrideSet: %v", err)
	}
	// The body must carry the relative-delta form (not percent).
	if v, ok := gotBody["threshold_override"].(float64); !ok || v != 0.50 {
		t.Errorf("body threshold_override = %v, want 0.50", gotBody["threshold_override"])
	}
	if gotBody["mute_duration"] != "6h" {
		t.Errorf("mute_duration in body = %v, want 6h", gotBody["mute_duration"])
	}
	if !strings.Contains(out, "Override saved for p1/wf-1/failure_rate_spike") {
		t.Errorf("confirmation missing: %s", out)
	}
}

// TestBlackBoxOverrideDelete_Happy — body shape + confirmation.
func TestBlackBoxOverrideDelete_Happy(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/workflow-healing/overrides/delete" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"status":"deleted"}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	overrideDelProject, overrideDelWorkflow, overrideDelClass = "p1", "wf-1", "failure_rate_spike"
	out, err := captureStdoutFunc(t, func() error {
		return runBlackBoxOverrideDelete(nil, nil)
	})
	if err != nil {
		t.Fatalf("runBlackBoxOverrideDelete: %v", err)
	}
	if gotBody["project_id"] != "p1" || gotBody["trigger_class"] != "failure_rate_spike" {
		t.Errorf("body shape: %+v", gotBody)
	}
	if !strings.Contains(out, "Override deleted: p1/wf-1/failure_rate_spike") {
		t.Errorf("confirmation missing: %s", out)
	}
}

// newOverrideSetForTest builds a fresh cobra command with the
// override-set flags bound to the module-level vars so tests can
// flip Changed() without relying on init() ordering.
func newOverrideSetForTest() *cobra.Command {
	cmd := &cobra.Command{Use: "set"}
	cmd.Flags().StringVar(&overrideSetProject, "project", overrideSetProject, "")
	cmd.Flags().StringVar(&overrideSetWorkflow, "workflow", overrideSetWorkflow, "")
	cmd.Flags().StringVar(&overrideSetClass, "class", overrideSetClass, "")
	cmd.Flags().Float64Var(&overrideSetThreshold, "threshold-pct", overrideSetThreshold, "")
	cmd.Flags().IntVar(&overrideSetMuteHours, "mute-hours", overrideSetMuteHours, "")
	cmd.Flags().BoolVar(&overrideSetClearMute, "clear-mute", overrideSetClearMute, "")
	cmd.Flags().StringVar(&overrideSetNotes, "notes", overrideSetNotes, "")
	cmd.Flags().BoolVar(&overrideSetJSON, "json", overrideSetJSON, "")
	return cmd
}
