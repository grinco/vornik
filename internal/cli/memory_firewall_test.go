package cli

// CLI tests for `vornikctl memory firewall {mode,evaluations,set-policy}`
// (coverage-gap sweep 2026-06-18, Tier 3). httptest-stubbed daemon +
// captured stdout. captureStdoutFunc is shared from
// blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func resetMemoryFirewallFlags() {
	mfModeJSON = false
	mfEvalsProject, mfEvalsDecision, mfEvalsSince = "", "", ""
	mfEvalsLimit, mfEvalsJSON, mfEvalsCSV = 0, false, false
	mfSetPolicySensitivity, mfSetPolicyTenantID, mfSetPolicyExpiresAt = "", "", ""
	mfSetPolicyPermittedRoles, mfSetPolicyAllowedPurposes, mfSetPolicyJSON = "", "", false
	mfSetPolicySensitivitySet = false
	mfSetPolicyTenantIDSet = false
	mfSetPolicyExpiresAtSet = false
	mfSetPolicyPermittedRolesSet = false
	mfSetPolicyAllowedPurposesSet = false
}

func TestRunMemoryFirewallMode_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/memory/policy/mode" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(firewallModeResp{
			Mode:              "enforce",
			DescriptionByMode: map[string]string{"enforce": "drops disallowed chunks"},
			Note:              "default-on since 2026-05-29",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetMemoryFirewallFlags()

	out, err := captureStdoutFunc(t, func() error { return runMemoryFirewallMode(memoryFirewallModeCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryFirewallMode: %v", err)
	}
	for _, want := range []string{"enforce", "drops disallowed chunks", "default-on since 2026-05-29"} {
		if !strings.Contains(out, want) {
			t.Errorf("mode output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunMemoryFirewallMode_Non200IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin scope required"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-no")
	resetMemoryFirewallFlags()

	if _, err := captureStdoutFunc(t, func() error { return runMemoryFirewallMode(memoryFirewallModeCmd, nil) }); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestRunMemoryFirewallEvals_ForwardsFiltersAndRenders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/memory/policy/evaluations" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("project_id") != "snake" || q.Get("decision") != "drop" || q.Get("limit") != "3" {
			t.Errorf("filters not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(firewallEvalsResp{
			Count: 1,
			Evaluations: []firewallEvalRow{
				{ChunkID: "chunk-9", Decision: "drop", EvaluatedAt: "2026-06-01T00:00:00Z", ReasonDetail: "expired policy", RequestRole: "analyst", RequestPurpose: "operational"},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetMemoryFirewallFlags()
	mfEvalsProject, mfEvalsDecision, mfEvalsLimit = "snake", "drop", 3

	out, err := captureStdoutFunc(t, func() error { return runMemoryFirewallEvals(memoryFirewallEvalsCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryFirewallEvals: %v", err)
	}
	for _, want := range []string{"chunk-9", "drop", "expired policy", "analyst"} {
		if !strings.Contains(out, want) {
			t.Errorf("evals output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunMemoryFirewallEvals_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(firewallEvalsResp{Count: 0})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetMemoryFirewallFlags()
	mfEvalsProject = "snake"

	out, err := captureStdoutFunc(t, func() error { return runMemoryFirewallEvals(memoryFirewallEvalsCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryFirewallEvals: %v", err)
	}
	if !strings.Contains(out, "No evaluations found") {
		t.Errorf("expected empty message, got:\n%s", out)
	}
}

func TestRunMemoryFirewallSetPolicy_ForwardsBodyAndRenders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/memory/policy/chunks/chunk-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req chunkPolicyUpdateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Only the flags marked "set" should appear in the body.
		if req.SensitivityTier == nil || *req.SensitivityTier != "restricted" {
			t.Errorf("sensitivity not forwarded: %+v", req.SensitivityTier)
		}
		if req.PermittedRoles == nil || len(*req.PermittedRoles) != 2 {
			t.Errorf("permitted roles not forwarded: %+v", req.PermittedRoles)
		}
		if req.TenantID != nil {
			t.Errorf("tenant should be untouched (not set), got %v", *req.TenantID)
		}
		resp := chunkPolicyUpdateResponse{ChunkID: "chunk-1", PolicyDigest: "sha256:new", AuditEntry: "aud-1"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	resetMemoryFirewallFlags()
	mfSetPolicySensitivity, mfSetPolicySensitivitySet = "restricted", true
	mfSetPolicyPermittedRoles, mfSetPolicyPermittedRolesSet = "lead,analyst", true

	out, err := captureStdoutFunc(t, func() error { return runMemoryFirewallSetPolicy(memoryFirewallSetPolicyCmd, []string{"chunk-1"}) })
	if err != nil {
		t.Fatalf("runMemoryFirewallSetPolicy: %v", err)
	}
	for _, want := range []string{"chunk-1", "sha256:new", "aud-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("set-policy output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunMemoryFirewallSetPolicy_InvalidExpiresAt(t *testing.T) {
	// Bad --expires-at must fail before any HTTP round-trip.
	resetMemoryFirewallFlags()
	mfSetPolicyExpiresAt, mfSetPolicyExpiresAtSet = "not-a-timestamp", true

	err := runMemoryFirewallSetPolicy(memoryFirewallSetPolicyCmd, []string{"chunk-1"})
	if err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("expected RFC3339 parse error, got %v", err)
	}
}
