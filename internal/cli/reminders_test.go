package cli

// CLI tests for `vornikctl reminders {list,show,cancel,delete,schedule}`
// (coverage-gap sweep 2026-06-18, Tier 3). httptest-stubbed daemon +
// captured stdout, same harness as cpc_test.go / admin_test.go.
// captureStdoutFunc is shared from blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func resetReminderFlags() {
	remindersListStatus, remindersListOperator, remindersListProject = "", "", ""
	remindersListLimit, remindersListJSON = 50, false
	remindersShowJSON = false
	remindersCancelJSON = false
	remindersDeleteYes, remindersDeleteJSON = false, false
	remindersScheduleOperator, remindersScheduleChannel, remindersScheduleChannelRef = "", "", ""
	remindersScheduleProject, remindersScheduleTimezone = "", ""
	remindersScheduleYes, remindersScheduleJSON = false, false
}

// withStdin redirects os.Stdin to a pipe pre-loaded with `input` for the
// duration of fn, so handlers that fmt.Scanln a y/N answer are testable.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = w.Close()
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	fn()
}

func TestRunRemindersList_TableForwardsFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/reminders" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("status") != "pending" || q.Get("operator") != "telegram:42" ||
			q.Get("project") != "snake" || q.Get("limit") != "5" {
			t.Errorf("filters not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(reminderListResponse{Entries: []reminderEntry{
			{ID: "rem-1", Status: "pending", FireAt: "2026-06-20T09:00:00Z", OperatorID: "telegram:42", ProjectID: "snake", Content: "stand-up"},
		}})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()
	remindersListStatus, remindersListOperator, remindersListProject, remindersListLimit = "pending", "telegram:42", "snake", 5

	out, err := captureStdoutFunc(t, func() error { return runRemindersList(remindersListCmd, nil) })
	if err != nil {
		t.Fatalf("runRemindersList: %v", err)
	}
	for _, want := range []string{"rem-1", "pending", "telegram:42", "snake", "stand-up"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q in:\n%s", want, out)
		}
	}
}

func TestRunRemindersList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(reminderListResponse{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()

	out, err := captureStdoutFunc(t, func() error { return runRemindersList(remindersListCmd, nil) })
	if err != nil {
		t.Fatalf("runRemindersList: %v", err)
	}
	if !strings.Contains(out, "No reminders match the filter.") {
		t.Errorf("expected empty message, got:\n%s", out)
	}
}

func TestRunRemindersList_Non200IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()

	if _, err := captureStdoutFunc(t, func() error { return runRemindersList(remindersListCmd, nil) }); err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestRunRemindersShow_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(reminderEntry{
			ID: "rem-show", Status: "pending", OperatorID: "telegram:42",
			Channel: "telegram", ChannelRef: "42", FireAt: "2026-06-20T09:00:00Z",
			Content: "water the plants", CreatedVia: "chat",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()

	out, err := captureStdoutFunc(t, func() error { return runRemindersShow(remindersShowCmd, []string{"rem-show"}) })
	if err != nil {
		t.Fatalf("runRemindersShow: %v", err)
	}
	for _, want := range []string{"rem-show", "pending", "water the plants"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunRemindersCancel_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/cancel") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(reminderEntry{ID: "rem-c", Status: "cancelled"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()

	out, err := captureStdoutFunc(t, func() error { return runRemindersCancel(remindersCancelCmd, []string{"rem-c"}) })
	if err != nil {
		t.Fatalf("runRemindersCancel: %v", err)
	}
	if !strings.Contains(out, "Cancelled rem-c") || !strings.Contains(out, "cancelled") {
		t.Errorf("cancel output unexpected:\n%s", out)
	}
}

func TestRunRemindersDelete_YesFlagSkipsPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(reminderEntry{ID: "rem-d", Status: "pending", Content: "x"})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()
	remindersDeleteYes = true

	out, err := captureStdoutFunc(t, func() error { return runRemindersDelete(remindersDeleteCmd, []string{"rem-d"}) })
	if err != nil {
		t.Fatalf("runRemindersDelete: %v", err)
	}
	if !strings.Contains(out, "deleted rem-d") {
		t.Errorf("expected deleted confirmation, got:\n%s", out)
	}
}

func TestRunRemindersDelete_NotFoundIsIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()

	out, err := captureStdoutFunc(t, func() error { return runRemindersDelete(remindersDeleteCmd, []string{"gone"}) })
	if err != nil {
		t.Fatalf("delete of missing row must not error: %v", err)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected not-found message, got:\n%s", out)
	}
}

func TestRunRemindersDelete_ConfirmAbort(t *testing.T) {
	var deleteCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(reminderEntry{ID: "rem-keep", Status: "pending", Content: "keep me"})
		case http.MethodDelete:
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags() // remindersDeleteYes = false → prompts

	var out string
	var runErr error
	withStdin(t, "n\n", func() {
		out, runErr = captureStdoutFunc(t, func() error { return runRemindersDelete(remindersDeleteCmd, []string{"rem-keep"}) })
	})
	if runErr != nil {
		t.Fatalf("runRemindersDelete: %v", runErr)
	}
	if !strings.Contains(out, "aborted") {
		t.Errorf("expected abort message, got:\n%s", out)
	}
	if deleteCalled {
		t.Error("DELETE must NOT be issued after an aborted confirmation")
	}
}

func TestRunRemindersSchedule_RequiresFlags(t *testing.T) {
	resetReminderFlags() // operator/channel/channel-ref empty
	err := runRemindersSchedule(remindersScheduleCmd, []string{"remind me tomorrow"})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-flags error, got %v", err)
	}
}

func TestRunRemindersSchedule_RequiresText(t *testing.T) {
	resetReminderFlags()
	remindersScheduleOperator, remindersScheduleChannel, remindersScheduleChannelRef = "telegram:42", "telegram", "42"
	err := runRemindersSchedule(remindersScheduleCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "text is required") {
		t.Fatalf("expected text-required error, got %v", err)
	}
}

func TestRunRemindersSchedule_YesCommitsAndPrints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/reminders/from-text" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req fromTextRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.DryRun {
			t.Error("--yes must send a non-dry-run commit")
		}
		_ = json.NewEncoder(w).Encode(fromTextResponse{
			Intent:   fromTextIntent{Kind: "one_shot", FireAt: "2026-06-20T09:00:00Z", Content: "stand-up", Confidence: 0.9},
			Reminder: &reminderEntry{ID: "rem-new", FireAt: "2026-06-20T09:00:00Z"},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetReminderFlags()
	remindersScheduleOperator, remindersScheduleChannel, remindersScheduleChannelRef = "telegram:42", "telegram", "42"
	remindersScheduleYes = true

	out, err := captureStdoutFunc(t, func() error { return runRemindersSchedule(remindersScheduleCmd, []string{"stand-up at 9"}) })
	if err != nil {
		t.Fatalf("runRemindersSchedule: %v", err)
	}
	if !strings.Contains(out, "Created reminder rem-new") {
		t.Errorf("expected creation confirmation, got:\n%s", out)
	}
}
