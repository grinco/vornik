package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubHealingOverrideRepo is an in-memory implementation tailored
// for the override admin handlers. Mirrors the stubProposalsRepo /
// stubHealingTriggerRepo shape.
type stubHealingOverrideRepo struct {
	mu          sync.Mutex
	rows        map[string]*persistence.HealingTriggerOverride
	upsertErr   error
	deleteErr   error
	lastUpsert  *persistence.HealingTriggerOverride
	lastDelete  struct{ project, workflow, class string }
	deleteCalls int
}

func newStubHealingOverrideRepo() *stubHealingOverrideRepo {
	return &stubHealingOverrideRepo{rows: map[string]*persistence.HealingTriggerOverride{}}
}

func overrideKey(p, w, c string) string { return p + "|" + w + "|" + c }

func (s *stubHealingOverrideRepo) Upsert(_ context.Context, o *persistence.HealingTriggerOverride) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *o
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	cp.UpdatedAt = time.Now().UTC()
	s.rows[overrideKey(o.ProjectID, o.WorkflowID, string(o.TriggerClass))] = &cp
	s.lastUpsert = &cp
	return nil
}

func (s *stubHealingOverrideRepo) Get(_ context.Context, project, workflow string, class persistence.HealingTriggerClass) (*persistence.HealingTriggerOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.rows[overrideKey(project, workflow, string(class))]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return o, nil
}

func (s *stubHealingOverrideRepo) List(_ context.Context, _ int) ([]*persistence.HealingTriggerOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.HealingTriggerOverride, 0, len(s.rows))
	for _, o := range s.rows {
		out = append(out, o)
	}
	return out, nil
}

func (s *stubHealingOverrideRepo) Delete(_ context.Context, project, workflow string, class persistence.HealingTriggerClass) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, overrideKey(project, workflow, string(class)))
	s.lastDelete.project, s.lastDelete.workflow, s.lastDelete.class = project, workflow, string(class)
	s.deleteCalls++
	return nil
}

// TestAdminBlackBoxOverrides_NotWired — repo absent, list page
// renders the not-wired state.
func TestAdminBlackBoxOverrides_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrides(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/overrides", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("empty-state message missing")
	}
}

// TestAdminBlackBoxOverrides_ListRenders — populated repo,
// active mute renders with the "muted" pill + active row.
func TestAdminBlackBoxOverrides_ListRenders(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	thresh := 0.50
	until := time.Now().UTC().Add(24 * time.Hour)
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID:         "proj-a",
		WorkflowID:        "wf-noisy",
		TriggerClass:      persistence.HealingTriggerFailureRateSpike,
		ThresholdOverride: &thresh,
		MutedUntil:        &until,
		Notes:             "rolling forward — see SEV-12",
		CreatedBy:         "operator-x",
	})
	s := NewServer(WithHealingOverrideRepository(repo))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrides(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/overrides", nil))
	body := rec.Body.String()
	// Where the row would render — find the table body. If absent
	// at all, render path is borked.
	if !strings.Contains(body, "Active overrides") {
		t.Fatalf("page heading missing; full body: %q", body[:min2(800, len(body))])
	}
	idx := strings.Index(body, "<tbody")
	tbodyDump := ""
	if idx >= 0 {
		tbodyDump = body[idx:min2(idx+2400, len(body))]
	}
	// Go's html/template escapes `+` to `&#43;` in HTML text
	// context. The browser renders it as "+" — but the raw byte
	// stream contains the entity. Assert on the escaped form
	// (which is what landed in production renders too).
	for _, want := range []string{
		"proj-a", "wf-noisy", "failure_rate_spike", "&#43;50.0%",
		"muted", "rolling forward", "operator-x",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; tbody slice: %q", want, tbodyDump)
		}
	}
}

// TestAdminBlackBoxOverrides_PrefilledFromQuery — the trigger
// detail page links here with ?project=…&workflow=…&class=…; the
// form must populate those fields so an operator gets a one-click
// edit instead of having to retype.
func TestAdminBlackBoxOverrides_PrefilledFromQuery(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	s := NewServer(WithHealingOverrideRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/ui/admin/blackbox/overrides?project=p1&workflow=wf-1&class=cost_regression", nil)
	s.AdminBlackBoxOverrides(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `value="p1"`) {
		t.Error("project field not prefilled")
	}
	if !strings.Contains(body, `value="wf-1"`) {
		t.Error("workflow field not prefilled")
	}
	if !strings.Contains(body, `value="cost_regression" selected`) {
		t.Error("class select not prefilled")
	}
}

// TestAdminBlackBoxOverrideSave_HappyPath — threshold + mute both
// set, repo gets a populated row, audit writes one entry, redirect
// to bare list.
func TestAdminBlackBoxOverrideSave_HappyPath(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingOverrideRepository(repo),
		WithAdminAuditRepository(audit),
	)
	form := strings.NewReader("project=p1&workflow=wf-1&class=failure_rate_spike&threshold_pct=50&mute_hours=6&notes=tuning")
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/overrides/save", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrideSave(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/blackbox/overrides" {
		t.Errorf("redirect: %q", loc)
	}
	if repo.lastUpsert == nil {
		t.Fatal("upsert not called")
	}
	if repo.lastUpsert.ThresholdOverride == nil || *repo.lastUpsert.ThresholdOverride != 0.50 {
		t.Errorf("threshold stored as relative delta: got %v", repo.lastUpsert.ThresholdOverride)
	}
	if repo.lastUpsert.MutedUntil == nil {
		t.Error("mute window not stored")
	} else {
		dt := time.Until(*repo.lastUpsert.MutedUntil)
		if dt < 5*time.Hour+30*time.Minute || dt > 6*time.Hour+30*time.Minute {
			t.Errorf("mute_until off: want ~6h from now, got %v", dt)
		}
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-override.saved" {
		t.Errorf("audit: %#v", audit.rows)
	}
}

// TestAdminBlackBoxOverrideSave_MissingFields — required field
// missing → redirect back with action_error, no repo write.
func TestAdminBlackBoxOverrideSave_MissingFields(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	s := NewServer(WithHealingOverrideRepository(repo))
	form := strings.NewReader("project=&workflow=wf-1&class=failure_rate_spike&threshold_pct=50")
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/overrides/save", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrideSave(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "action_error=") {
		t.Errorf("expected action_error redirect, got %q", rec.Header().Get("Location"))
	}
	if repo.lastUpsert != nil {
		t.Error("repo should not be touched on validation failure")
	}
}

// TestAdminBlackBoxOverrideSave_UnknownClass — class not in the
// enum is rejected at the form boundary.
func TestAdminBlackBoxOverrideSave_UnknownClass(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	s := NewServer(WithHealingOverrideRepository(repo))
	form := strings.NewReader("project=p1&workflow=wf-1&class=bogus&threshold_pct=50")
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/overrides/save", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrideSave(rec, req)
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "unknown+trigger+class") {
		t.Errorf("expected unknown-class banner: %q", loc)
	}
}

// TestAdminBlackBoxOverrideSave_NothingToSave — all action fields
// blank/empty → reject, don't write a useless row.
func TestAdminBlackBoxOverrideSave_NothingToSave(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	s := NewServer(WithHealingOverrideRepository(repo))
	form := strings.NewReader("project=p1&workflow=wf-1&class=failure_rate_spike")
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/overrides/save", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrideSave(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "nothing+to+save") {
		t.Errorf("expected nothing-to-save banner: %q", rec.Header().Get("Location"))
	}
	if repo.lastUpsert != nil {
		t.Error("repo should not be touched")
	}
}

// TestAdminBlackBoxOverrideSave_ClearMute — clear_mute=1 + blank
// mute_hours → stored row has MutedUntil=nil (mute is wiped).
func TestAdminBlackBoxOverrideSave_ClearMute(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	s := NewServer(WithHealingOverrideRepository(repo))
	form := strings.NewReader("project=p1&workflow=wf-1&class=failure_rate_spike&threshold_pct=30&clear_mute=1")
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/overrides/save", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrideSave(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d", rec.Code)
	}
	if repo.lastUpsert == nil || repo.lastUpsert.MutedUntil != nil {
		t.Errorf("clear_mute should leave MutedUntil nil; got %+v", repo.lastUpsert)
	}
}

// TestAdminBlackBoxOverrideDelete_HappyPath — POST removes the row
// from the repo + writes one audit entry.
func TestAdminBlackBoxOverrideDelete_HappyPath(t *testing.T) {
	repo := newStubHealingOverrideRepo()
	thresh := 0.25
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID:         "p1",
		WorkflowID:        "wf-1",
		TriggerClass:      persistence.HealingTriggerFailureRateSpike,
		ThresholdOverride: &thresh,
	})
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingOverrideRepository(repo),
		WithAdminAuditRepository(audit),
	)
	form := strings.NewReader("project=p1&workflow=wf-1&class=failure_rate_spike")
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/overrides/delete", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxOverrideDelete(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d", rec.Code)
	}
	if repo.deleteCalls != 1 {
		t.Errorf("delete calls: %d", repo.deleteCalls)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-override.deleted" {
		t.Errorf("audit: %#v", audit.rows)
	}
}
