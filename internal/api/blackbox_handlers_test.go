package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

// stubReplaySafetyClassifier implements contracts.ReplaySafetyClassifier
// for tests without importing internal/blackbox. It also satisfies the
// local snapshotable interface checked in AdminBlackBoxSideEffects.
type stubReplaySafetyClassifier struct {
	tools []string
}

func (s *stubReplaySafetyClassifier) IsReplaySafe(toolName string) bool {
	for _, t := range s.tools {
		if t == toolName {
			return true
		}
	}
	return false
}

// Snapshot satisfies the snapshotable duck-type in AdminBlackBoxSideEffects.
func (s *stubReplaySafetyClassifier) Snapshot() []string {
	return s.tools
}

// TestAdminBlackBoxSideEffects_HonestText asserts the handler reports
// enforcement as "enforced" under the deny-by-default policy and
// surfaces the now-real `blackbox.replay_safe_tools` config key
// (Phase C inversion, 2026-06-17 — the key exists, so pointing
// operators at `daemon reload` is honest, not the Pattern-C lie it
// was when the deny-list had no config surface).
func TestAdminBlackBoxSideEffects_HonestText(t *testing.T) {
	classifier := &stubReplaySafetyClassifier{tools: []string{"broker_get_orders"}}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-x"}}),
		WithBlackBoxReplaySafety(classifier),
	)

	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/blackbox/sideeffects", nil), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxSideEffects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["enforcement"] != "enforced" {
		t.Errorf("enforcement = %v, want enforced", body["enforcement"])
	}
	if body["policy"] != "deny_by_default" {
		t.Errorf("policy = %v, want deny_by_default", body["policy"])
	}
	if body["config_tunable"] != true {
		t.Errorf("config_tunable = %v, want true", body["config_tunable"])
	}
	note, _ := body["note"].(string)
	if !strings.Contains(note, "replay_safe_tools") {
		t.Errorf("note should name the real config key replay_safe_tools: %q", note)
	}
	tools, _ := body["replay_safe_tools"].([]any)
	if len(tools) != 1 || tools[0] != "broker_get_orders" {
		t.Errorf("replay_safe_tools = %v, want [broker_get_orders]", tools)
	}
}

// TestAdminBlackBoxSideEffects_Disabled — no classifier wired (but
// admin enabled) → 503.
func TestAdminBlackBoxSideEffects_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-x"}}))
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/blackbox/sideeffects", nil), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxSideEffects(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}
