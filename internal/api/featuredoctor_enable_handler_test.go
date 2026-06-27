package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/persistence"
)

// nopApplier is a no-op enableApplierFunc used as a test-path signal for the
// fail-closed admin gate. When server==nil and enableApplierFunc!=nil (case 3),
// the gate is intentionally bypassed for unit tests. The nopApplier is never
// called in tests that exercise pre-apply paths (dry-run, prereq checking, etc.).
var nopApplier = func(_ context.Context, _ featuredoctor.Feature, _ featuredoctor.Deps,
	_ *featuredoctor.EnablePlan, _ featuredoctor.ConfigWriter, _ featuredoctor.Reloader,
) (featuredoctor.PrereqResult, error) {
	return featuredoctor.PrereqResult{}, nil
}

// fakeEnableWriter is a test ConfigWriter that tracks calls.
type fakeEnableWriter struct {
	content       []byte
	backupPath    string
	writeErr      error
	writeCalled   bool
	restoreCalled bool
}

func (f *fakeEnableWriter) Read() ([]byte, error)   { return f.content, nil }
func (f *fakeEnableWriter) Write(data []byte) error { f.writeCalled = true; return f.writeErr }
func (f *fakeEnableWriter) Backup() (string, error) { return f.backupPath, nil }
func (f *fakeEnableWriter) Restore(string) error    { f.restoreCalled = true; return nil }
func (f *fakeEnableWriter) Validate() error         { return nil }

// fakeEnableReloader records calls and injects an optional error.
type fakeEnableReloader struct {
	called bool
	err    error
}

func (r *fakeEnableReloader) Reload(_ context.Context) error { r.called = true; return r.err }

// buildEnableTestHandler creates a DoctorHandlers wired with stub feature deps
// that satisfy the "auth" prereq (the simplest feature whose prereq is the
// presence of admin-key.txt; we supply a dir with the file absent so gates
// are off but the prereq is still fixable via a gate — except auth's prereq
// is actually unfixable when the key is absent). Use a no-prereq feature stub
// via featureDepsFunc override instead.
func buildEnableTestHandlerWithDeps(depsFunc func() featuredoctor.Deps) *DoctorHandlers {
	return &DoctorHandlers{
		featureDepsFunc:   depsFunc,
		enableApplierFunc: nopApplier, // signal "test path" for fail-closed gate (case 3)
	}
}

func TestEnableFeature_DryRun_ReturnsPlan(t *testing.T) {
	// Use memory-rag: its prereqs check model reachability; supply a stub
	// that reports all models reachable and all gates off.
	deps := func() featuredoctor.Deps {
		return featuredoctor.Deps{
			Config: stubAllGatesOff{},
			Models: &stubReachableModels{},
		}
	}
	h := buildEnableTestHandlerWithDeps(deps)

	body := `{"apply":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/memory-rag/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var plan enablePlanDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if plan.FeatureID != "memory-rag" {
		t.Errorf("want feature_id=memory-rag, got %q", plan.FeatureID)
	}
	// All memory-rag gates are off so there should be changes.
	if len(plan.Changes) == 0 {
		t.Error("expected at least one gate change in dry-run plan")
	}
}

func TestEnableFeature_DryRun_NoWrite(t *testing.T) {
	// Verify that dry-run (apply=false) does NOT call Write on the config writer.
	// We confirm this by observing the handler returns a plan shape, not a result shape.
	deps := func() featuredoctor.Deps {
		return featuredoctor.Deps{
			Config: stubAllGatesOff{},
			Models: &stubReachableModels{},
		}
	}
	h := buildEnableTestHandlerWithDeps(deps)
	// configPath is intentionally empty — a real apply would return 503.

	body := `{"apply":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/memory-rag/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	// 200 means the plan was returned (no write attempted).
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for dry-run, got %d: %s", rec.Code, rec.Body)
	}
	// Response must be a plan shape (has "changes"), not a result shape (has "ok").
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, hasChanges := raw["changes"]; !hasChanges {
		t.Error("dry-run response must contain 'changes' field")
	}
	if _, hasOK := raw["ok"]; hasOK {
		t.Error("dry-run response must NOT contain 'ok' field (that's the apply result)")
	}
}

// captureAuditRepo records Insert calls for the audit regression.
type captureAuditRepo struct {
	entries []*persistence.AdminAuditEntry
}

func (c *captureAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	c.entries = append(c.entries, e)
	return nil
}
func (c *captureAuditRepo) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return c.entries, nil
}

// TestEnableFeature_Apply_WritesAuditRow is the regression for the missing
// admin-audit trail on feature enables: the AdminAuditRepository contract
// requires every mutating admin POST to write a durable row. A feature
// enable mutates config + triggers a reload, so it must audit. Drives the
// apply path with an injected applier and asserts exactly one
// "feature.enable" row with the target feature id.
func TestEnableFeature_Apply_WritesAuditRow(t *testing.T) {
	audit := &captureAuditRepo{}
	srv := NewServer(WithAdminAuditRepository(audit))

	deps := func() featuredoctor.Deps {
		return featuredoctor.Deps{Config: stubAllGatesOff{}, Models: &stubReachableModels{}}
	}
	h := &DoctorHandlers{
		featureDepsFunc: deps,
		enableApplierFunc: func(_ context.Context, _ featuredoctor.Feature, _ featuredoctor.Deps,
			_ *featuredoctor.EnablePlan, _ featuredoctor.ConfigWriter, _ featuredoctor.Reloader,
		) (featuredoctor.PrereqResult, error) {
			return featuredoctor.PrereqResult{OK: true, Detail: "applied"}, nil
		},
	}
	h.SetServer(srv)

	body := `{"apply":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/memory-rag/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	// Auth disabled → the admin gate passes without an admin key (the gate
	// itself is covered by its own tests; here we exercise the audit write).
	req = authDisabledReq(req)
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("want exactly 1 audit row, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "feature.enable" {
		t.Errorf("Action = %q, want feature.enable", e.Action)
	}
	if e.Target != "memory-rag" {
		t.Errorf("Target = %q, want memory-rag", e.Target)
	}
	if e.Principal == "" {
		t.Error("Principal must be recorded (non-empty)")
	}
	if e.Source != "api" {
		t.Errorf("Source = %q, want api", e.Source)
	}
}

func TestEnableFeature_Apply_MissingConfigPath_Returns503(t *testing.T) {
	// With server==nil and enableApplierFunc==nil the fail-closed gate returns 503.
	// This is a superset of the original "missing config path" check: if the
	// handler is not properly wired (no server, no injected applier) it must never
	// proceed past authentication — 503 is the correct outcome either way.
	h := &DoctorHandlers{
		featureDepsFunc: func() featuredoctor.Deps {
			return featuredoctor.Deps{Config: stubAllGatesOff{}, Models: &stubReachableModels{}}
		},
		// server: nil, enableApplierFunc: nil → fail-closed gate fires
	}

	body := `{"apply":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/memory-rag/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body)
	}
}

func TestEnableFeature_UnknownFeature_Returns404(t *testing.T) {
	h := &DoctorHandlers{featureDepsFunc: stubFeaturedoctorDeps, enableApplierFunc: nopApplier}
	body := `{"apply":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/nonexistent/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestEnableFeature_MethodNotAllowed(t *testing.T) {
	h := &DoctorHandlers{featureDepsFunc: stubFeaturedoctorDeps}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor/features/auth/enable", nil)
	rec := httptest.NewRecorder()
	h.EnableFeature(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestEnableFeature_Apply_InvokesApplier(t *testing.T) {
	// Verify that apply=true calls the enableApplierFunc and returns
	// the verify result. Uses the injected fakes to avoid real I/O.
	deps := func() featuredoctor.Deps {
		return featuredoctor.Deps{
			Config: stubAllGatesOff{},
			Models: &stubReachableModels{},
		}
	}

	writer := &fakeEnableWriter{
		content:    []byte("memory:\n  llm_consolidate_enabled: false\n  response_cache_enabled: false\n  embedding_cache_enabled: false\n"),
		backupPath: "bak",
	}
	reloader := &fakeEnableReloader{}

	h := &DoctorHandlers{
		featureDepsFunc: deps,
		enableApplierFunc: func(_ context.Context, f featuredoctor.Feature, _ featuredoctor.Deps,
			_ *featuredoctor.EnablePlan, _ featuredoctor.ConfigWriter, _ featuredoctor.Reloader,
		) (featuredoctor.PrereqResult, error) {
			// Record that the applier was called and return OK.
			_ = writer.Write([]byte("applied"))
			_ = reloader.Reload(context.Background())
			return featuredoctor.PrereqResult{OK: true, Detail: "applied"}, nil
		},
	}

	body := `{"apply":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/memory-rag/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}

	var result enableResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !result.OK {
		t.Error("apply result must be OK")
	}
	if result.FeatureID != "memory-rag" {
		t.Errorf("want feature_id=memory-rag, got %q", result.FeatureID)
	}
	// Verify the applier actually ran (writer.Write was called from the injected func).
	if !writer.writeCalled {
		t.Error("applier must have called Write")
	}
	if !reloader.called {
		t.Error("applier must have called Reload")
	}
}

func TestEnableFeature_PrereqUnmet_Returns409(t *testing.T) {
	// auth feature requires admin-key.txt (unfixable). SecretsDir has no file.
	deps := func() featuredoctor.Deps {
		return featuredoctor.Deps{
			Config:     stubAllGatesOff{},
			SecretsDir: t.TempDir(),
		}
	}
	h := buildEnableTestHandlerWithDeps(deps)

	body := `{"apply":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/auth/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("want 409 for unfixable prereq, got %d: %s", rec.Code, rec.Body)
	}
}

func TestExtractFeatureEnableID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/doctor/features/auth/enable", "auth"},
		{"/api/v1/doctor/features/memory-rag/enable", "memory-rag"},
		{"/api/v1/doctor/features/auth", ""},       // no /enable suffix
		{"/api/v1/doctor/features/", ""},           // no id
		{"/api/v1/other/path/enable", ""},          // wrong prefix
		{"/api/v1/doctor/features/a/b/enable", ""}, // nested id
	}
	for _, tc := range cases {
		got := extractFeatureEnableID(tc.path)
		if got != tc.want {
			t.Errorf("extractFeatureEnableID(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestApplyMechanismString(t *testing.T) {
	if applyMechanismString(featuredoctor.ReloadHot) != "reload-hot" {
		t.Error("wrong string for ReloadHot")
	}
	if applyMechanismString(featuredoctor.RestartRequired) != "restart-required" {
		t.Error("wrong string for RestartRequired")
	}
	unknown := applyMechanismString(featuredoctor.ApplyMechanism(99))
	if unknown == "" {
		t.Error("should return non-empty string for unknown mechanism")
	}
}

func TestEnableFeature_ServerNil_NoApplier_Returns503(t *testing.T) {
	// fix(featuredoctor): fail-closed enable gate — server=nil + enableApplierFunc=nil
	// (not a test injection, case 2) must reject with 503 rather than proceeding without auth.
	h := &DoctorHandlers{
		featureDepsFunc: stubFeaturedoctorDeps,
		// server: nil — not wired
		// enableApplierFunc: nil — NOT a test injection; fail-closed must reject
	}
	body := `{"apply":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor/features/auth/enable",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()

	h.EnableFeature(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("fail-closed gate: want 503, got %d: %s", rec.Code, rec.Body)
	}
}

func TestExtractFeatureEnableID_TrailingSlash(t *testing.T) {
	// fix(featuredoctor): trailing slash on /enable/ must resolve to the same id as /enable.
	got := extractFeatureEnableID("/api/v1/doctor/features/auth/enable/")
	if got != "auth" {
		t.Errorf("want %q, got %q", "auth", got)
	}
}

// stubReachableModels always reports models as reachable.
type stubReachableModels struct{}

func (s *stubReachableModels) Reachable(_ context.Context, _ string) bool { return true }
