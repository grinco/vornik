package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubInstinctRepo is a focused mock implementing only the methods the
// REST handlers call (List / Get / Retire / RecomputeConfidence). The
// rest of the InstinctRepository contract stays no-op.
type stubInstinctRepo struct {
	rows           []*persistence.Instinct
	retireCalls    int
	recomputeCalls int
	listErr        error
	retireErr      error
}

func (s *stubInstinctRepo) Upsert(context.Context, *persistence.Instinct) (string, error) {
	return "", nil
}
func (s *stubInstinctRepo) AddEvidence(context.Context, *persistence.InstinctEvidence) (bool, error) {
	return false, nil
}

func (s *stubInstinctRepo) RecordActionVersion(context.Context, *persistence.InstinctActionVersion) error {
	return nil
}

func (s *stubInstinctRepo) ListActionHistory(context.Context, string, int) ([]*persistence.InstinctActionVersion, error) {
	return nil, nil
}
func (s *stubInstinctRepo) RecomputeConfidence(_ context.Context, id string, _ persistence.InstinctScorer) error {
	s.recomputeCalls++
	return nil
}
func (s *stubInstinctRepo) Get(_ context.Context, id string) (*persistence.Instinct, error) {
	for _, r := range s.rows {
		if r.ID == id {
			cp := *r
			return &cp, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (s *stubInstinctRepo) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := []*persistence.Instinct{}
	for _, r := range s.rows {
		if filter.Domain != nil && r.Domain != *filter.Domain {
			continue
		}
		if filter.Scope != nil && r.Scope != *filter.Scope {
			continue
		}
		if filter.Status != nil && r.Status != *filter.Status {
			continue
		}
		if filter.ProjectID != nil && r.ProjectID != *filter.ProjectID {
			continue
		}
		if filter.MinConfidence != nil && r.Confidence < *filter.MinConfidence {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}
func (s *stubInstinctRepo) CountActiveProjects(context.Context, string) (int, error) { return 0, nil }
func (s *stubInstinctRepo) CountByDomainStatus(context.Context) ([]persistence.InstinctDomainStatusCount, error) {
	return nil, nil
}
func (s *stubInstinctRepo) Retire(_ context.Context, id string) error {
	if s.retireErr != nil {
		return s.retireErr
	}
	s.retireCalls++
	for _, r := range s.rows {
		if r.ID == id {
			r.Status = persistence.InstinctStatusRetired
			return nil
		}
	}
	return persistence.ErrNotFound
}
func (s *stubInstinctRepo) RecordApplication(context.Context, *persistence.InstinctApplication) error {
	return nil
}
func (s *stubInstinctRepo) ListApplications(context.Context, string, int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *stubInstinctRepo) ListPendingRecoveryApplications(context.Context, int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *stubInstinctRepo) ResolveApplication(context.Context, string, string) error {
	return nil
}
func (s *stubInstinctRepo) ListApplicationCounts(context.Context, []string) (map[string]*persistence.InstinctApplicationCounts, error) {
	return nil, nil
}

// fixedScorer is a deterministic InstinctScorer for the recompute test.
type fixedScorer struct{}

func (fixedScorer) Score(persistence.InstinctScoreInput) (float64, string) {
	return 0.5, persistence.InstinctStatusActive
}

func sampleInstincts() []*persistence.Instinct {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return []*persistence.Instinct{
		{ID: "ins_1", Scope: "project", ProjectID: "alpha", Domain: "recovery", TriggerKey: "tk_a",
			Trigger: []byte(`{"role":"coder","error_class":"Timeout"}`), Action: "retry resolved it",
			Confidence: 0.82, SupportCount: 5, ContradictCount: 1, Source: "observer", Status: "active",
			CreatedAt: now, UpdatedAt: now, LastSeenAt: now},
		{ID: "ins_2", Scope: "project", ProjectID: "alpha", Domain: "quality", TriggerKey: "tk_b",
			Action: "tighten schema", Confidence: 0.40, SupportCount: 2, Source: "observer", Status: "candidate",
			CreatedAt: now, UpdatedAt: now, LastSeenAt: now},
		{ID: "ins_3", Scope: "global", Domain: "recovery", TriggerKey: "tk_a",
			Action: "fall back to model-x", Confidence: 0.90, SupportCount: 9, Source: "observer", Status: "promoted",
			CreatedAt: now, UpdatedAt: now, LastSeenAt: now},
	}
}

func instinctTestServer(repo persistence.InstinctRepository, scorer persistence.InstinctScorer, audit *stubAdminAuditRepo) *Server {
	opts := []ServerOption{
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-x"}}),
	}
	if repo != nil {
		opts = append(opts, WithInstinctRepository(repo))
	}
	if scorer != nil {
		opts = append(opts, WithInstinctScorer(scorer))
	}
	if audit != nil {
		opts = append(opts, WithAdminAuditRepository(audit))
	}
	return NewServer(opts...)
}

func TestListInstincts_NoRepo(t *testing.T) {
	s := instinctTestServer(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts", nil)
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no repo = %d, want 503", rec.Code)
	}
}

func TestListInstincts_MethodNotAllowed(t *testing.T) {
	s := instinctTestServer(&stubInstinctRepo{}, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/instincts", nil)
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d, want 405", rec.Code)
	}
}

func TestListInstincts_FilterAndShape(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts?domain=recovery", nil)
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp InstinctListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Instincts) != 2 {
		t.Fatalf("domain=recovery returned %d, want 2", len(resp.Instincts))
	}
	// Trigger_json must surface as the raw `trigger` string.
	var sawTrigger bool
	for _, in := range resp.Instincts {
		if in.ID == "ins_1" {
			if in.Trigger == "" {
				t.Errorf("ins_1 trigger empty, want trigger_json carried through")
			}
			sawTrigger = true
		}
	}
	if !sawTrigger {
		t.Error("ins_1 not in result set")
	}
}

func TestListInstincts_MinConfidence(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)
	// min_confidence=0.8 → ins_1 (0.82) + ins_3 (0.90), drops ins_2 (0.40).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts?min_confidence=0.8", nil)
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	var resp InstinctListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Instincts) != 2 {
		t.Fatalf("min_confidence filter returned %d, want 2", len(resp.Instincts))
	}

	// Invalid min_confidence → 400.
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/instincts?min_confidence=2.0", nil)
	brec := httptest.NewRecorder()
	s.ListInstincts(brec, bad)
	if brec.Code != http.StatusBadRequest {
		t.Errorf("min_confidence=2.0 = %d, want 400", brec.Code)
	}
}

func TestShowInstinct_FoundAndNotFound(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_2", nil)
	rec := httptest.NewRecorder()
	s.instinctsRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("show = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp InstinctShowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Instinct.ID != "ins_2" {
		t.Errorf("show id = %q, want ins_2", resp.Instinct.ID)
	}

	miss := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/nope", nil)
	mrec := httptest.NewRecorder()
	s.instinctsRouter(mrec, miss)
	if mrec.Code != http.StatusNotFound {
		t.Errorf("missing show = %d, want 404", mrec.Code)
	}
}

func TestRetireInstinct_RouterAndAudit(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	audit := &stubAdminAuditRepo{}
	s := instinctTestServer(repo, nil, audit)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instincts/ins_1/retire", nil)
	rec := httptest.NewRecorder()
	s.instinctsRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retire = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.retireCalls != 1 {
		t.Errorf("retire calls = %d, want 1", repo.retireCalls)
	}
	// Audit row written.
	if len(audit.rows) != 1 || audit.rows[0].Action != "instinct.retired" {
		t.Errorf("audit rows = %+v, want one instinct.retired", audit.rows)
	}

	// GET on /retire → 405.
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_1/retire", nil)
	brec := httptest.NewRecorder()
	s.instinctsRouter(brec, bad)
	if brec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET retire = %d, want 405", brec.Code)
	}

	// Unknown sub-path → 404.
	unk := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_1/bogus", nil)
	urec := httptest.NewRecorder()
	s.instinctsRouter(urec, unk)
	if urec.Code != http.StatusNotFound {
		t.Errorf("unknown subpath = %d, want 404", urec.Code)
	}
}

func TestRetireInstinct_NotFound(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts(), retireErr: persistence.ErrNotFound}
	s := instinctTestServer(repo, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/instincts/ghost/retire", nil)
	rec := httptest.NewRecorder()
	s.instinctsRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retire missing = %d, want 404", rec.Code)
	}
}

func TestAdminInstinctsRecompute_Disabled(t *testing.T) {
	// Admin disabled → 404 (hidden surface).
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	req := authEnabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/admin/instincts/recompute", nil))
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled = %d, want 404", rec.Code)
	}
}

func TestAdminInstinctsRecompute_NonAdminKey(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, fixedScorer{}, nil)
	req := withAdminKeyContext(authEnabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/admin/instincts/recompute", nil)), "sk-user-x")
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
}

func TestAdminInstinctsRecompute_NoScorer(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil) // scorer not wired
	req := withAdminKeyContext(authEnabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/admin/instincts/recompute", nil)), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no scorer = %d, want 503", rec.Code)
	}
}

func TestAdminInstinctsRecompute_Success(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	audit := &stubAdminAuditRepo{}
	s := instinctTestServer(repo, fixedScorer{}, audit)
	// domain=recovery → recompute ins_1 + ins_3 (2 rows).
	req := withAdminKeyContext(authEnabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/admin/instincts/recompute?domain=recovery", nil)), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("recompute = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp InstinctRecomputeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Recomputed != 2 {
		t.Errorf("recomputed = %d, want 2", resp.Recomputed)
	}
	if repo.recomputeCalls != 2 {
		t.Errorf("recompute calls = %d, want 2", repo.recomputeCalls)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "instinct.recomputed" {
		t.Errorf("audit rows = %+v, want one instinct.recomputed", audit.rows)
	}
	// The raw admin bearer token must never be persisted in cleartext;
	// the audit principal is the redacted fingerprint, matching sibling
	// admin handlers (e.g. AdminCPCCancel).
	if p := audit.rows[0].Principal; p == "sk-admin-x" || !strings.HasPrefix(p, "api_key_") {
		t.Errorf("audit principal = %q, want redacted api_key_* principal, not the raw key", p)
	}
}

func TestInstinctShowRetire_NoRepo(t *testing.T) {
	s := instinctTestServer(nil, nil, nil)
	// Show
	rec := httptest.NewRecorder()
	s.ShowInstinct(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instincts/x", nil), "x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("show no-repo = %d, want 503", rec.Code)
	}
	// Retire
	rec2 := httptest.NewRecorder()
	s.RetireInstinct(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/instincts/x/retire", nil), "x")
	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("retire no-repo = %d, want 503", rec2.Code)
	}
}

func TestListInstincts_ListError(t *testing.T) {
	repo := &stubInstinctRepo{listErr: context.DeadlineExceeded}
	s := instinctTestServer(repo, nil, nil)
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instincts", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list error = %d, want 500", rec.Code)
	}
}

func TestInstinctsRouter_BareDelegatesToList(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)
	// Trailing slash with empty id → list.
	rec := httptest.NewRecorder()
	s.instinctsRouter(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instincts/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bare router = %d, want 200", rec.Code)
	}
}

// recomputeErrRepo fails RecomputeConfidence to cover the error branch.
type recomputeErrRepo struct{ *stubInstinctRepo }

func (recomputeErrRepo) RecomputeConfidence(context.Context, string, persistence.InstinctScorer) error {
	return context.DeadlineExceeded
}

func TestAdminInstinctsRecompute_RecomputeError(t *testing.T) {
	repo := recomputeErrRepo{&stubInstinctRepo{rows: sampleInstincts()}}
	s := instinctTestServer(repo, fixedScorer{}, nil)
	req := withAdminKeyContext(authEnabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/admin/instincts/recompute", nil)), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("recompute error = %d, want 500", rec.Code)
	}
}

func TestAdminInstinctsRecompute_ListError(t *testing.T) {
	repo := &stubInstinctRepo{listErr: context.DeadlineExceeded}
	s := instinctTestServer(repo, fixedScorer{}, nil)
	req := withAdminKeyContext(authEnabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/admin/instincts/recompute", nil)), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("recompute list error = %d, want 500", rec.Code)
	}
}

func TestAdminInstinctsRecompute_MethodNotAllowed(t *testing.T) {
	s := instinctTestServer(&stubInstinctRepo{}, fixedScorer{}, nil)
	rec := httptest.NewRecorder()
	s.AdminInstinctsRecompute(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/instincts/recompute", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET recompute = %d, want 405", rec.Code)
	}
}

func TestShowInstinct_EmptyID(t *testing.T) {
	s := instinctTestServer(&stubInstinctRepo{}, nil, nil)
	rec := httptest.NewRecorder()
	s.ShowInstinct(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instincts/", nil), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty id show = %d, want 400", rec.Code)
	}
}

// authEnabledReq stamps authEnabledKey=true so the admin gate enforces
// the full 401/403 matrix (the auth-disabled bypass is for single-
// operator deployments).
func authEnabledReq(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authEnabledKey, true))
}
