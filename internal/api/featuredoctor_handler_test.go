package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
)

// stubFeaturedoctorDeps returns a minimal Deps with a stub ConfigReader
// that reports all gates as off (so features stay in ready/blocked state).
func stubFeaturedoctorDeps() featuredoctor.Deps {
	return featuredoctor.Deps{
		Config:     stubAllGatesOff{},
		SecretsDir: "", // auth prereq will be "absent" → blocked
	}
}

// stubAllGatesOff is a ConfigReader that returns false for every bool key,
// empty string for string keys, and false for "not found".
type stubAllGatesOff struct{}

func (stubAllGatesOff) GateValue(key string) (any, bool) { return false, true }

func TestFeatureStatusEndpoint_ListsSeedFeatures(t *testing.T) {
	h := &DoctorHandlers{
		featureDepsFunc: stubFeaturedoctorDeps,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor/features", nil)
	rec := httptest.NewRecorder()
	h.ListFeatures(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListFeatures status %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var out []featureStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("want 5 features, got %d", len(out))
	}
	// Each entry must have an ID and a status.
	ids := map[string]bool{}
	for _, f := range out {
		if f.ID == "" {
			t.Error("feature entry has empty ID")
		}
		if f.Status == "" {
			t.Errorf("feature %q has empty status", f.ID)
		}
		ids[f.ID] = true
	}
	for _, want := range []string{"instinct", "auth", "memory-rag", "cluster", "trading-series"} {
		if !ids[want] {
			t.Errorf("seed feature %q missing from response", want)
		}
	}
}

func TestFeatureStatusEndpoint_GetSingleFeature(t *testing.T) {
	h := &DoctorHandlers{
		featureDepsFunc: stubFeaturedoctorDeps,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor/features/auth", nil)
	rec := httptest.NewRecorder()
	h.GetFeature(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetFeature status %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var out featureStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out.ID != "auth" {
		t.Errorf("want id=auth, got %q", out.ID)
	}
	if len(out.Prereqs) == 0 {
		t.Error("auth feature should have at least one prereq")
	}
}

func TestFeatureStatusEndpoint_GetNotFound(t *testing.T) {
	h := &DoctorHandlers{
		featureDepsFunc: stubFeaturedoctorDeps,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor/features/nonexistent", nil)
	rec := httptest.NewRecorder()
	h.GetFeature(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestFeatureStatusEndpoint_MethodNotAllowed(t *testing.T) {
	h := &DoctorHandlers{
		featureDepsFunc: stubFeaturedoctorDeps,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features", nil)
	rec := httptest.NewRecorder()
	h.ListFeatures(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestFeatureStatusEndpoint_PrereqDetailPresent(t *testing.T) {
	// Use a deps where instinct.model is set but models are not reachable,
	// so the prereq detail is populated.
	stubDeps := func() featuredoctor.Deps {
		return featuredoctor.Deps{
			Config:     stubInstinctModelConfig{},
			SecretsDir: "",
			Models:     stubUnreachableModels{},
		}
	}
	h := &DoctorHandlers{featureDepsFunc: stubDeps}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor/features/instinct", nil)
	rec := httptest.NewRecorder()
	h.GetFeature(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out featureStatusDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// "distill model reachable" prereq should be present and unmet.
	var found bool
	for _, p := range out.Prereqs {
		if p.Name == "distill model reachable" {
			found = true
			if p.OK {
				t.Error("expected prereq not-OK for unreachable model")
			}
			if p.Remediation == "" {
				t.Error("expected non-empty remediation for unreachable model prereq")
			}
		}
	}
	if !found {
		t.Error("'distill model reachable' prereq not present in response")
	}
}

// stubInstinctModelConfig reports instinct.model = "test-model", all other keys false/zero.
type stubInstinctModelConfig struct{}

func (s stubInstinctModelConfig) GateValue(key string) (any, bool) {
	if key == "instinct.model" {
		return "test-model", true
	}
	return false, true
}

// stubUnreachableModels always returns false for Reachable.
type stubUnreachableModels struct{}

func (stubUnreachableModels) Reachable(_ context.Context, _ string) bool { return false }
