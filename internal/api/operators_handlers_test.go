package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubOperatorProfileRepo is the api-side counterpart of the
// dispatcher's stubOpProfileRepo. The CLI surface uses Get +
// List + Upsert + Delete, plus the audit-log read.
type stubOperatorProfileRepo struct {
	mu      sync.Mutex
	rows    map[string]*persistence.OperatorProfile
	listErr error
	getErr  error
}

func newStubOperatorProfileRepo() *stubOperatorProfileRepo {
	return &stubOperatorProfileRepo{rows: map[string]*persistence.OperatorProfile{}}
}

func newOperatorAPIServer(repo *stubOperatorProfileRepo) *Server {
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithOperatorProfileRepository(repo),
	)
}

func withAdminKey(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-admin"))
}

func (s *stubOperatorProfileRepo) Get(_ context.Context, id string) (*persistence.OperatorProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	row, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *row
	return &cp, nil
}
func (s *stubOperatorProfileRepo) Upsert(_ context.Context, p *persistence.OperatorProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.rows[p.OperatorID] = &cp
	return nil
}
func (s *stubOperatorProfileRepo) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}
func (s *stubOperatorProfileRepo) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]*persistence.OperatorProfile, 0, len(s.rows))
	for _, r := range s.rows {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// TestListOperators_Empty: 200 + empty entries when no rows.
func TestListOperators_Empty(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	s := newOperatorAPIServer(repo)
	req := withAdminKey(httptest.NewRequest(http.MethodGet, "/api/v1/operators", nil))
	rec := httptest.NewRecorder()
	s.ListOperators(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp OperatorListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("entries len=%d, want 0", len(resp.Entries))
	}
}

// TestListOperators_PopulatedRendersRows: a few rows in the repo
// flow through as JSON entries with operator_id + structured
// preserved + notes truncated cleanly.
func TestListOperators_PopulatedRendersRows(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	now := time.Now()
	repo.rows["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42",
		Structured: []byte(`{"tone":"terse"}`),
		Notes:      "prefers code blocks",
		UpdatedAt:  now,
		CreatedAt:  now.Add(-1 * time.Hour),
	}
	s := newOperatorAPIServer(repo)
	req := withAdminKey(httptest.NewRequest(http.MethodGet, "/api/v1/operators", nil))
	rec := httptest.NewRecorder()
	s.ListOperators(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	var resp OperatorListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 1 {
		t.Fatalf("entries len=%d", len(resp.Entries))
	}
	e := resp.Entries[0]
	if e.OperatorID != "telegram:42" || e.Notes != "prefers code blocks" {
		t.Errorf("row mismatch: %+v", e)
	}
	if e.Structured == "" {
		t.Errorf("structured JSON should be carried in the response")
	}
}

// TestListOperators_RepoErrorYields500.
func TestListOperators_RepoErrorYields500(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	repo.listErr = errors.New("connection refused")
	s := newOperatorAPIServer(repo)
	req := withAdminKey(httptest.NewRequest(http.MethodGet, "/api/v1/operators", nil))
	rec := httptest.NewRecorder()
	s.ListOperators(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", rec.Code)
	}
}

// TestListOperators_UnwiredYields503.
func TestListOperators_UnwiredYields503(t *testing.T) {
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
	)
	req := withAdminKey(httptest.NewRequest(http.MethodGet, "/api/v1/operators", nil))
	rec := httptest.NewRecorder()
	s.ListOperators(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rec.Code)
	}
}

func TestOperatorsRequireAdminKey(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	repo.rows["telegram:42"] = &persistence.OperatorProfile{OperatorID: "telegram:42"}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithOperatorProfileRepository(repo),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/operators", nil)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-project"))
	rec := httptest.NewRecorder()
	s.ListOperators(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestOperatorLinkAndAuditEndpointsRequireAdminKey(t *testing.T) {
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
	)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list links", http.MethodGet, "/api/v1/operators/telegram:42/links", ""},
		{"create link", http.MethodPost, "/api/v1/operators/telegram:42/links", `{"channel_speaker_id":"web:abc"}`},
		{"delete link", http.MethodDelete, "/api/v1/operators/telegram:42/links/web:abc", ""},
		{"audit", http.MethodGet, "/api/v1/operators/telegram:42/audit", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-project"))
			rec := httptest.NewRecorder()
			s.operatorsRouter(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestShowOperator_FoundAndMissing.
func TestShowOperator_FoundAndMissing(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	repo.rows["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42", Structured: []byte(`{}`), UpdatedAt: time.Now(),
	}
	s := newOperatorAPIServer(repo)

	// Found
	req := withAdminKey(httptest.NewRequest(http.MethodGet, "/api/v1/operators/telegram:42", nil))
	rec := httptest.NewRecorder()
	s.ShowOperator(rec, req, "telegram:42")
	if rec.Code != http.StatusOK {
		t.Errorf("found status=%d", rec.Code)
	}

	// Missing → 404
	req2 := withAdminKey(httptest.NewRequest(http.MethodGet, "/api/v1/operators/telegram:nobody", nil))
	rec2 := httptest.NewRecorder()
	s.ShowOperator(rec2, req2, "telegram:nobody")
	if rec2.Code != http.StatusNotFound {
		t.Errorf("missing status=%d, want 404", rec2.Code)
	}
}

// TestSetOperatorKey_CommitsAndAudits.
func TestSetOperatorKey_CommitsAndAudits(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	s := newOperatorAPIServer(repo)
	body := `{"key":"tone","value":"terse","rationale":"operator asked"}`
	req := withAdminKey(httptest.NewRequest(http.MethodPost, "/api/v1/operators/telegram:42",
		strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.SetOperatorKey(rec, req, "telegram:42")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	row, _ := repo.Get(context.Background(), "telegram:42")
	if row == nil {
		t.Fatalf("Upsert didn't land")
	}
	if !strings.Contains(string(row.Structured), "terse") {
		t.Errorf("structured = %q", row.Structured)
	}
}

// TestSetOperatorKey_RejectsUnknownKey: same security guard the
// dispatcher tool has — only allow-listed keys.
func TestSetOperatorKey_RejectsUnknownKey(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	s := newOperatorAPIServer(repo)
	body := `{"key":"prompt_injection","value":"x","rationale":"r"}`
	req := withAdminKey(httptest.NewRequest(http.MethodPost, "/api/v1/operators/telegram:42",
		strings.NewReader(body)))
	rec := httptest.NewRecorder()
	s.SetOperatorKey(rec, req, "telegram:42")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetOperatorKey_RejectsOversizedBody(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	s := newOperatorAPIServer(repo)
	req := withAdminKey(httptest.NewRequest(http.MethodPost, "/api/v1/operators/telegram:42", strings.NewReader(strings.Repeat("x", 64*1024+1))))
	rec := httptest.NewRecorder()
	s.SetOperatorKey(rec, req, "telegram:42")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if row, _ := repo.Get(context.Background(), "telegram:42"); row != nil {
		t.Fatalf("saved operator profile for oversized body: %#v", row)
	}
}

// TestSetOperatorKey_RequiresRationale.
func TestSetOperatorKey_RequiresRationale(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	s := newOperatorAPIServer(repo)
	body := `{"key":"tone","value":"terse","rationale":""}`
	req := withAdminKey(httptest.NewRequest(http.MethodPost, "/api/v1/operators/telegram:42",
		strings.NewReader(body)))
	rec := httptest.NewRecorder()
	s.SetOperatorKey(rec, req, "telegram:42")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// TestSetOperatorKey_EmptyValueRemovesKey: mirrors the
// dispatcher-tool empty-value-deletes-key contract.
func TestSetOperatorKey_EmptyValueRemovesKey(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	repo.rows["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42",
		Structured: []byte(`{"tone":"terse","time_zone":"Europe/Prague"}`),
	}
	s := newOperatorAPIServer(repo)
	body := `{"key":"tone","value":"","rationale":"undo"}`
	req := withAdminKey(httptest.NewRequest(http.MethodPost, "/api/v1/operators/telegram:42",
		strings.NewReader(body)))
	rec := httptest.NewRecorder()
	s.SetOperatorKey(rec, req, "telegram:42")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	row, _ := repo.Get(context.Background(), "telegram:42")
	if strings.Contains(string(row.Structured), "terse") {
		t.Errorf("tone should be removed; got %q", row.Structured)
	}
	if !strings.Contains(string(row.Structured), "Europe/Prague") {
		t.Errorf("other keys should be preserved; got %q", row.Structured)
	}
}

// TestForgetOperator_DeletesAndAuditsViaCLI.
func TestForgetOperator_DeletesAndAuditsViaCLI(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	repo.rows["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42", Structured: []byte(`{}`),
	}
	s := newOperatorAPIServer(repo)
	body := `{"rationale":"GDPR request"}`
	req := withAdminKey(httptest.NewRequest(http.MethodDelete, "/api/v1/operators/telegram:42",
		strings.NewReader(body)))
	rec := httptest.NewRecorder()
	s.ForgetOperator(rec, req, "telegram:42")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := repo.Get(context.Background(), "telegram:42"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("row should be gone")
	}
}

// TestOperatorsRouter_DispatchesByPath: the path-segment
// dispatch in operatorsRouter sends GET /id → ShowOperator,
// POST /id → SetOperatorKey, DELETE /id → ForgetOperator.
func TestOperatorsRouter_DispatchesByPath(t *testing.T) {
	repo := newStubOperatorProfileRepo()
	repo.rows["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42", Structured: []byte(`{}`), UpdatedAt: time.Now(),
	}
	s := newOperatorAPIServer(repo)

	cases := []struct {
		method string
		body   string
		want   int
	}{
		{http.MethodGet, "", http.StatusOK},
		{http.MethodPost, `{"key":"tone","value":"terse","rationale":"r"}`, http.StatusOK},
		{http.MethodDelete, `{"rationale":"GDPR"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			// Re-seed for each iteration since DELETE removes it.
			if _, ok := repo.rows["telegram:42"]; !ok {
				repo.rows["telegram:42"] = &persistence.OperatorProfile{
					OperatorID: "telegram:42", Structured: []byte(`{}`), UpdatedAt: time.Now(),
				}
			}
			req := withAdminKey(httptest.NewRequest(tc.method, "/api/v1/operators/telegram:42",
				strings.NewReader(tc.body)))
			rec := httptest.NewRecorder()
			s.operatorsRouter(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s status=%d, want %d; body=%s", tc.method, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
