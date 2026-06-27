package cli

// Coverage sweep for `vornikctl key {create,list,rotate,revoke,update}`.
// Same httptest harness as cpc_test.go: stub the daemon, point
// VORNIK_API_URL at it, capture stdout, assert the rendered output +
// that request bodies / paths reach the wire. captureStdoutFunc is
// shared from blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// keyCov_reset restores the package-level key flags so cases don't
// leak filter/JSON state into each other.
func keyCov_reset() {
	keyProject, keyName, keyExpires = "", "", ""
	keyJSONFlag = false
	keyUpdateAddWorkflows, keyUpdateRemoveWorkflows, keyUpdateSetWorkflows = nil, nil, nil
	keyUpdateAllowPush, keyUpdateDisallowPush = false, false
	// Clear the shared cobra command's per-flag Changed state. keyUpdateCmd
	// is package-global, so without this a StringSlice flag's Set() APPENDS
	// instead of replacing on the second pass of `go test -count>1`
	// (set-workflows accumulating [a,b,a,b] → assertion fails). Clearing
	// Changed makes the next Set treat it as a first assignment again.
	for _, name := range []string{"set-workflows", "add-workflow", "remove-workflow"} {
		if f := keyUpdateCmd.Flags().Lookup(name); f != nil {
			f.Changed = false
		}
	}
}

func TestRunKeyCreate_HumanPrintsSecretOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/proj-a/keys" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "ci-bot" {
			t.Errorf("name not forwarded: %+v", body)
		}
		if _, ok := body["expires_at"]; !ok {
			t.Errorf("expires_at missing: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(keyCreatedResponse{
			ID: "key-1", Name: "ci-bot", ProjectID: "proj-a",
			Secret: "sk-vornik-proj-a.RAWSECRET", KeyPrefix: "sk-vornik-proj-a", CreatedAt: "2026-06-01",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	keyCov_reset()
	keyProject, keyName, keyExpires = "proj-a", "ci-bot", "30d"

	out, err := captureStdoutFunc(t, func() error { return runKeyCreate(keyCreateCmd, nil) })
	if err != nil {
		t.Fatalf("runKeyCreate: %v", err)
	}
	for _, want := range []string{"key-1", "ci-bot", "sk-vornik-proj-a.RAWSECRET", "shown ONCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("create output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunKeyCreate_BadExpiryIsError(t *testing.T) {
	t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1")
	keyCov_reset()
	keyProject, keyName, keyExpires = "p", "n", "30q"
	_, err := captureStdoutFunc(t, func() error { return runKeyCreate(keyCreateCmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "invalid --expires") {
		t.Fatalf("expected invalid expiry error, got %v", err)
	}
}

func TestRunKeyCreate_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(keyCreatedResponse{ID: "key-j", Secret: "s"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject, keyName, keyJSONFlag = "p", "n", true
	out, err := captureStdoutFunc(t, func() error { return runKeyCreate(keyCreateCmd, nil) })
	if err != nil {
		t.Fatalf("runKeyCreate: %v", err)
	}
	var decoded keyCreatedResponse
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil || decoded.ID != "key-j" {
		t.Fatalf("bad JSON output: %v %s", jerr, out)
	}
}

func TestRunKeyCreate_Non201IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "denied"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject, keyName = "p", "n"
	_, err := captureStdoutFunc(t, func() error { return runKeyCreate(keyCreateCmd, nil) })
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestRunKeyList_TableAndStatuses(t *testing.T) {
	revoked := "2026-06-10"
	lastUsed := "2026-06-09"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/proj-a/keys" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(keyListResponse{Keys: []keyListEntry{
			{ID: "k-active", Name: "n1", KeyPrefix: "pfx1", CreatedAt: "2026-06-01", LastUsedAt: &lastUsed},
			{ID: "k-revoked", Name: "n2", KeyPrefix: "pfx2", CreatedAt: "2026-06-02", RevokedAt: &revoked},
		}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "proj-a"
	out, err := captureStdoutFunc(t, func() error { return runKeyList(keyListCmd, nil) })
	if err != nil {
		t.Fatalf("runKeyList: %v", err)
	}
	for _, want := range []string{"k-active", "active", "k-revoked", "revoked", "pfx1"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunKeyList_EmptyMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(keyListResponse{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "empty-proj"
	out, err := captureStdoutFunc(t, func() error { return runKeyList(keyListCmd, nil) })
	if err != nil {
		t.Fatalf("runKeyList: %v", err)
	}
	if !strings.Contains(out, "No keys for project") {
		t.Errorf("expected empty message, got %s", out)
	}
}

func TestRunKeyRotate_PrintsNewSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/p/keys/old-id/rotate" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(keyCreatedResponse{ID: "new-id", ProjectID: "p", Secret: "sk-new"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runKeyRotate(keyRotateCmd, []string{"old-id"}) })
	if err != nil {
		t.Fatalf("runKeyRotate: %v", err)
	}
	if !strings.Contains(out, "new-id") || !strings.Contains(out, "sk-new") {
		t.Errorf("rotate output: %s", out)
	}
}

func TestRunKeyRevoke_NoContentSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runKeyRevoke(keyRevokeCmd, []string{"k-9"}) })
	if err != nil {
		t.Fatalf("runKeyRevoke: %v", err)
	}
	if !strings.Contains(out, "Revoked key k-9") {
		t.Errorf("revoke output: %s", out)
	}
}

func TestRunKeyRevoke_Non204IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no key"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	_, err := captureStdoutFunc(t, func() error { return runKeyRevoke(keyRevokeCmd, []string{"missing"}) })
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestRunKeyUpdate_NoModeIsError(t *testing.T) {
	keyCov_reset()
	keyProject = "p"
	// No flags changed → error before any HTTP.
	err := runKeyUpdate(keyUpdateCmd, []string{"k-1"})
	if err == nil || !strings.Contains(err.Error(), "specify one of") {
		t.Fatalf("expected no-mode error, got %v", err)
	}
}

func TestRunKeyUpdate_SetMode_ForwardsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT got %s", r.Method)
		}
		var body map[string][]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body["allowed_workflows"]) != 2 {
			t.Errorf("allowed_workflows not forwarded: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(keyWithWorkflowsResponse{ID: "k-1", AllowedWorkflows: []string{"a", "b"}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	keyUpdateSetWorkflows = []string{"a", "b"}
	// Mark Changed directly rather than via Flags().Set: pflag's StringSlice
	// Set APPENDS once its internal changed bit is set, so on `-count>1` the
	// shared keyUpdateCmd would accumulate [a,b,a,b]. The var is assigned
	// above; keyCov_reset clears Changed between tests.
	keyUpdateCmd.Flags().Lookup("set-workflows").Changed = true

	out, err := captureStdoutFunc(t, func() error { return runKeyUpdate(keyUpdateCmd, []string{"k-1"}) })
	if err != nil {
		t.Fatalf("runKeyUpdate: %v", err)
	}
	if !strings.Contains(out, "- a") || !strings.Contains(out, "- b") {
		t.Errorf("update output: %s", out)
	}
}

func TestFetchKeyAllowedWorkflows_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(keyListResponse{Keys: []keyListEntry{{ID: "other"}}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	_, err := fetchKeyAllowedWorkflows("p", "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

// TestRunKeyUpdate_AllowPushAndDisallowPushMutuallyExclusive — specifying
// both --allow-push and --disallow-push must return an error before any
// HTTP traffic goes out. Sets the package-level variables directly
// (runKeyUpdate reads these, not cobra Changed()) so there's no cobra
// flag state leak into sibling tests.
func TestRunKeyUpdate_AllowPushAndDisallowPushMutuallyExclusive(t *testing.T) {
	t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1") // no traffic expected
	keyCov_reset()
	keyProject = "p"
	keyUpdateAllowPush = true
	keyUpdateDisallowPush = true
	err := runKeyUpdate(keyUpdateCmd, []string{"k-1"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

// TestRunKeyUpdate_PushModeWithWorkflowFlagsIsError — combining a push-mode
// flag (--allow-push/--disallow-push) with any workflow flag
// (--add-workflow/--remove-workflow/--set-workflows) must return a clear error
// BEFORE any HTTP fires (the push branch would otherwise silently drop the
// workflow edit). Final-review FIX 2.
func TestRunKeyUpdate_PushModeWithWorkflowFlagsIsError(t *testing.T) {
	cases := []struct {
		name string
		set  func()
	}{
		{"allow-push + add-workflow", func() {
			keyUpdateAllowPush = true
			keyUpdateAddWorkflows = []string{"wf-x"}
			keyUpdateCmd.Flags().Lookup("add-workflow").Changed = true
		}},
		{"disallow-push + remove-workflow", func() {
			keyUpdateDisallowPush = true
			keyUpdateRemoveWorkflows = []string{"wf-y"}
			keyUpdateCmd.Flags().Lookup("remove-workflow").Changed = true
		}},
		{"allow-push + set-workflows", func() {
			keyUpdateAllowPush = true
			keyUpdateSetWorkflows = []string{"wf-z"}
			keyUpdateCmd.Flags().Lookup("set-workflows").Changed = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1") // no traffic expected
			keyCov_reset()
			keyProject = "p"
			tc.set()
			defer func() {
				keyCov_reset()
				// Clear cobra Changed() state so subtests/sibling tests don't leak.
				for _, n := range []string{"add-workflow", "remove-workflow", "set-workflows"} {
					if f := keyUpdateCmd.Flags().Lookup(n); f != nil {
						f.Changed = false
					}
				}
			}()
			err := runKeyUpdate(keyUpdateCmd, []string{"k-1"})
			if err == nil || !strings.Contains(err.Error(), "cannot be combined with workflow flags") {
				t.Fatalf("expected push/workflow exclusivity error, got %v", err)
			}
		})
	}
}

// TestRunKeyUpdate_AllowPush_IssuesCorrectPUT — setting keyUpdateAllowPush=true
// sends PUT /api/v1/projects/{p}/keys/{kid}/allow-push with {"allow_push":true}.
func TestRunKeyUpdate_AllowPush_IssuesCorrectPUT(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(keyWithAllowPushResponse{
			ID: "k-1", Name: "push-key", KeyPrefix: "pfx", CreatedAt: "2026-06-01",
			AllowPush: true,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "proj-a"
	keyUpdateAllowPush = true // simulate --allow-push

	out, err := captureStdoutFunc(t, func() error { return runKeyUpdate(keyUpdateCmd, []string{"k-1"}) })
	if err != nil {
		t.Fatalf("runKeyUpdate: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/v1/projects/proj-a/keys/k-1/allow-push" {
		t.Errorf("path = %q, want /api/v1/projects/proj-a/keys/k-1/allow-push", gotPath)
	}
	if v, ok := gotBody["allow_push"].(bool); !ok || !v {
		t.Errorf("body allow_push = %v, want true", gotBody["allow_push"])
	}
	if !strings.Contains(out, "enabled") {
		t.Errorf("output missing 'enabled': %s", out)
	}
}

// TestRunKeyUpdate_DisallowPush_IssuesCorrectPUT — keyUpdateDisallowPush=true
// sends PUT with {"allow_push":false} and prints "disabled".
func TestRunKeyUpdate_DisallowPush_IssuesCorrectPUT(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(keyWithAllowPushResponse{
			ID: "k-2", Name: "no-push", KeyPrefix: "pfx", CreatedAt: "2026-06-01",
			AllowPush: false,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	keyUpdateDisallowPush = true // simulate --disallow-push

	out, err := captureStdoutFunc(t, func() error { return runKeyUpdate(keyUpdateCmd, []string{"k-2"}) })
	if err != nil {
		t.Fatalf("runKeyUpdate: %v", err)
	}
	if v, ok := gotBody["allow_push"].(bool); !ok || v {
		t.Errorf("body allow_push = %v, want false", gotBody["allow_push"])
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("output missing 'disabled': %s", out)
	}
}

// TestRunKeyUpdate_AllowPush_JSON — keyUpdateAllowPush=true + keyJSONFlag=true
// emits JSON output.
func TestRunKeyUpdate_AllowPush_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(keyWithAllowPushResponse{
			ID: "k-j", AllowPush: true,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject, keyJSONFlag = "p", true
	keyUpdateAllowPush = true // simulate --allow-push

	out, err := captureStdoutFunc(t, func() error { return runKeyUpdate(keyUpdateCmd, []string{"k-j"}) })
	if err != nil {
		t.Fatalf("runKeyUpdate: %v", err)
	}
	var decoded keyWithAllowPushResponse
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil {
		t.Fatalf("bad JSON output: %v %s", jerr, out)
	}
	if !decoded.AllowPush {
		t.Errorf("decoded allow_push = false, want true")
	}
}

// TestRunKeyUpdate_AllowPush_Non200IsAPIError — non-200 response from the
// allow-push endpoint surfaces as a ParseAPIError (same pattern as revoke).
func TestRunKeyUpdate_AllowPush_Non200IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no key"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	keyUpdateAllowPush = true

	_, err := captureStdoutFunc(t, func() error { return runKeyUpdate(keyUpdateCmd, []string{"k-missing"}) })
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

// TestRunKeyList_ShowsAllowPushColumn — list output has the PUSH column;
// a key with allow_push=true shows "yes", one without shows "no".
func TestRunKeyList_ShowsAllowPushColumn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(keyListResponse{Keys: []keyListEntry{
			{ID: "k-push", Name: "pusher", KeyPrefix: "pfx1", CreatedAt: "2026-06-01", AllowPush: true},
			{ID: "k-nopush", Name: "reader", KeyPrefix: "pfx2", CreatedAt: "2026-06-02"},
		}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	keyCov_reset()
	keyProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runKeyList(keyListCmd, nil) })
	if err != nil {
		t.Fatalf("runKeyList: %v", err)
	}
	if !strings.Contains(out, "PUSH") {
		t.Errorf("list header missing PUSH column: %s", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("push-enabled key should show 'yes': %s", out)
	}
	if !strings.Contains(out, "no") {
		t.Errorf("push-disabled key should show 'no': %s", out)
	}
}

func TestParseExpiry_Forms(t *testing.T) {
	// RFC3339 passes through.
	ts, err := parseExpiry("2026-12-31T00:00:00Z")
	if err != nil || ts.Year() != 2026 {
		t.Fatalf("rfc3339: %v %v", ts, err)
	}
	now := time.Now().UTC()
	d, err := parseExpiry("30d")
	if err != nil || d.Before(now.AddDate(0, 0, 29)) {
		t.Fatalf("30d: %v %v", d, err)
	}
	if _, err := parseExpiry("6m"); err != nil {
		t.Fatalf("6m: %v", err)
	}
	if _, err := parseExpiry("1y"); err != nil {
		t.Fatalf("1y: %v", err)
	}
	for _, bad := range []string{"", "x", "0d", "-5d", "30q", "abc"} {
		if _, err := parseExpiry(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
