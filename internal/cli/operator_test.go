package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// captureStdoutFn swaps os.Stdout for a pipe, runs fn, and returns
// what was written. The CLI runners print directly to stdout so
// this is the simplest way to assert their output without
// refactoring the command tree.
func captureStdoutFn(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// TestOperatorList_FormatsRows mounts a fake /api/v1/operators
// endpoint, points VORNIK_API_URL at it, and asserts the table
// prints the operator id + the structured keys.
func TestOperatorList_FormatsRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/operators" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]string{
				{
					"operator_id": "telegram:42",
					"channel":     "telegram",
					"structured":  `{"tone":"terse"}`,
					"notes":       "prefers code",
					"updated_at":  "2026-05-23T08:00:00Z",
					"created_at":  "2026-05-22T08:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	out, err := captureStdoutFn(t, func() error { return runOperatorList(nil, nil) })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "telegram:42") {
		t.Errorf("output missing operator_id; got %q", out)
	}
}

// TestOperatorList_EmptyPrintsHint: zero rows should print a
// human hint rather than a bare header.
func TestOperatorList_EmptyPrintsHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]string{}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	out, err := captureStdoutFn(t, func() error { return runOperatorList(nil, nil) })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no operator") {
		t.Errorf("expected 'no operator' hint; got %q", out)
	}
}

// TestOperatorShow_PrintsFields hits /api/v1/operators/{id} and
// expects the human-readable field block.
func TestOperatorShow_PrintsFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/operators/telegram:42") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entry": map[string]string{
				"operator_id": "telegram:42",
				"channel":     "telegram",
				"structured":  `{"tone":"terse"}`,
				"notes":       "n",
				"updated_at":  "2026-05-23T08:00:00Z",
				"created_at":  "2026-05-22T08:00:00Z",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	out, err := captureStdoutFn(t, func() error { return runOperatorShow(nil, []string{"telegram:42"}) })
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "telegram:42") || !strings.Contains(out, "terse") {
		t.Errorf("output missing expected fields; got %q", out)
	}
}

// TestOperatorSet_SendsPostBody pins the wire shape: POST to
// /api/v1/operators/{id} with {key, value, rationale}.
func TestOperatorSet_SendsPostBody(t *testing.T) {
	var captured struct {
		Key, Value, Rationale string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entry": map[string]string{"operator_id": "telegram:42", "structured": `{}`},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	// Flag-driven; reset between cases since cobra holds them.
	operatorSetKey = "tone"
	operatorSetValue = "terse"
	operatorSetRationale = "operator asked"
	defer func() { operatorSetKey, operatorSetValue, operatorSetRationale = "", "", "" }()

	_, err := captureStdoutFn(t, func() error { return runOperatorSet(nil, []string{"telegram:42"}) })
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if captured.Key != "tone" || captured.Value != "terse" || captured.Rationale != "operator asked" {
		t.Errorf("body mismatch: %+v", captured)
	}
}

// TestOperatorSet_RejectsMissingRationale: client-side guard
// before we even round-trip. Saves a round-trip and makes the
// error message immediate.
func TestOperatorSet_RejectsMissingRationale(t *testing.T) {
	operatorSetKey = "tone"
	operatorSetValue = "terse"
	operatorSetRationale = ""
	defer func() { operatorSetKey, operatorSetValue = "", "" }()

	err := runOperatorSet(nil, []string{"telegram:42"})
	if err == nil {
		t.Fatalf("expected error for missing --rationale")
	}
	if !strings.Contains(err.Error(), "rationale") {
		t.Errorf("error should mention rationale; got %q", err.Error())
	}
}

// TestOperatorForget_SendsDelete: DELETE method + rationale in
// body when --reason is supplied.
func TestOperatorForget_SendsDelete(t *testing.T) {
	var method string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"operator_id": "telegram:42", "status": "forgotten",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	operatorForgetReason = "GDPR"
	operatorForgetYes = true
	defer func() { operatorForgetReason, operatorForgetYes = "", false }()

	_, err := captureStdoutFn(t, func() error { return runOperatorForget(nil, []string{"telegram:42"}) })
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method=%s, want DELETE", method)
	}
	if body["rationale"] != "GDPR" {
		t.Errorf("rationale not propagated: %+v", body)
	}
}

// TestOperatorForget_RequiresConfirm: without --yes, the runner
// refuses to send the DELETE so a stray invocation can't wipe a
// row by accident.
func TestOperatorForget_RequiresConfirm(t *testing.T) {
	operatorForgetReason = "test"
	operatorForgetYes = false
	defer func() { operatorForgetReason = "" }()

	err := runOperatorForget(nil, []string{"telegram:42"})
	if err == nil {
		t.Fatalf("expected confirmation error")
	}
}
