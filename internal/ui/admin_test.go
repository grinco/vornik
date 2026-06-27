package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

// stubAdminAuditRepo is a tiny in-memory implementation of
// persistence.AdminAuditRepository. Insert / List operate on a
// growing slice; callers can seed it before exercising the
// handler.
type stubAdminAuditRepo struct {
	rows []*persistence.AdminAuditEntry
}

func (s *stubAdminAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	if e == nil {
		return nil
	}
	if e.ID == "" {
		e.ID = "admaud-" + e.Action
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	cp := *e
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubAdminAuditRepo) List(_ context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	if filter.PageSize <= 0 {
		filter.PageSize = 50
	}
	out := make([]*persistence.AdminAuditEntry, 0, len(s.rows))
	for i := len(s.rows) - 1; i >= 0; i-- {
		e := s.rows[i]
		if filter.Action != "" && e.Action != filter.Action {
			continue
		}
		if filter.Principal != "" && e.Principal != filter.Principal {
			continue
		}
		if filter.TargetPrefix != "" && !strings.HasPrefix(e.Target, filter.TargetPrefix) {
			continue
		}
		out = append(out, e)
		if len(out) == filter.PageSize {
			break
		}
	}
	return out, nil
}

// stubMCPRefresher counts invocations so the audit-write test can
// verify the gate path actually calls it.
type stubMCPRefresher struct{ called int }

func (s *stubMCPRefresher) RefreshAll(_ context.Context) error {
	s.called++
	return nil
}

// stubReadiness returns a deterministic snapshot. Used by the
// landing-page render fixture.
type stubReadiness struct{}

func (stubReadiness) ReadinessChecks(_ context.Context) []AdminReadinessCheck {
	return []AdminReadinessCheck{
		{Name: "database", Status: "ok"},
		{Name: "chat_provider", Status: "error", Error: "check failed"},
	}
}

// TestAdminLanding_Renders covers the happy-path render with
// readiness + audit data wired.
func TestAdminLanding_Renders(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	_ = audit.Insert(context.Background(), &persistence.AdminAuditEntry{
		Principal: "sk-admin", Source: "ui", Action: "mcp.refresh", Target: "p-alpha",
	})
	s := NewServer(
		WithAdminAuditRepository(audit),
		WithAdminReadinessProvider(stubReadiness{}),
	)

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	s.AdminLanding(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Daemon health") {
		t.Error("missing Daemon health tile")
	}
	if !strings.Contains(body, "database") {
		t.Error("readiness tile should render check names")
	}
	if !strings.Contains(body, "mcp.refresh") {
		t.Error("recent audit row should render action verb")
	}
}

// TestAdminAudit_Renders confirms the audit page renders with
// filter values reflected in the rendered form.
func TestAdminAudit_Renders(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	for _, action := range []string{"mcp.refresh", "config.reload", "key.revoke"} {
		_ = audit.Insert(context.Background(), &persistence.AdminAuditEntry{
			Principal: "sk-admin", Source: "ui", Action: action, Target: "proj-a",
		})
	}
	s := NewServer(WithAdminAuditRepository(audit))

	req := httptest.NewRequest(http.MethodGet, "/admin/audit?action=mcp.refresh", nil)
	rec := httptest.NewRecorder()
	s.AdminAudit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "mcp.refresh") {
		t.Error("filtered row should render")
	}
	// config.reload was filtered out — table shouldn't contain it.
	if strings.Contains(body, ">config.reload<") {
		t.Error("filtered-out action should not render")
	}
}

// TestAdminAudit_PageSizeSelectorPreservesFilters confirms the
// shared pageSizeSelector renders on /ui/admin/audit and threads
// the current filter axes through as hidden form fields. Pre-
// migration the limit was inside the same form as the filter
// inputs, so changing Show forced a submit (and the operator
// rebuilding the filters); post-migration the selector is its own
// auto-submit form and preserves the four filter params via the
// Hidden dict.
func TestAdminAudit_PageSizeSelectorPreservesFilters(t *testing.T) {
	s := NewServer(WithAdminAuditRepository(&stubAdminAuditRepo{}))
	req := httptest.NewRequest(http.MethodGet,
		"/admin/audit?action=mcp.refresh&principal=sk-admin&target=proj-a&since=2026-05-01&limit=50",
		nil)
	rec := httptest.NewRecorder()
	s.AdminAudit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	// Selector form must point at the audit page and use the
	// default param name ("limit"); shared partial renders one
	// select element with that name.
	for _, want := range []string{
		`action="/ui/admin/audit"`,
		`name="limit"`,
		`name="action" value="mcp.refresh"`,
		`name="principal" value="sk-admin"`,
		`name="target" value="proj-a"`,
		`name="since" value="2026-05-01"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestAdminHealthMCP_POSTAudit confirms the MCP refresh POST writes
// an audit row even when the refresher succeeds — the audit write
// is the slice-1 contract for that one mutating endpoint.
func TestAdminHealthMCP_POSTAudit(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	refresher := &stubMCPRefresher{}
	s := NewServer(
		WithAdminAuditRepository(audit),
		WithAdminMCPRefresher(refresher),
	)

	req := httptest.NewRequest(http.MethodPost, "/admin/health/mcp", nil)
	req.Header.Set("User-Agent", "test/1.0")
	rec := httptest.NewRecorder()
	s.AdminHealthMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if refresher.called != 1 {
		t.Errorf("RefreshAll: want 1 call, got %d", refresher.called)
	}
	if len(audit.rows) != 1 {
		t.Fatalf("audit rows: want 1, got %d", len(audit.rows))
	}
	row := audit.rows[0]
	if row.Action != "mcp.refresh" {
		t.Errorf("audit Action: got %q", row.Action)
	}
	if row.UserAgent != "test/1.0" {
		t.Errorf("audit UserAgent: got %q", row.UserAgent)
	}
}

// TestAdminHealthIndex_Renders is a fixture render check.
func TestAdminHealthIndex_Renders(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/health/", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthIndex(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lease audit") {
		t.Error("health index should link to lease audit")
	}
}

// TestAdminHealthLeases_NoSource — degraded mode renders an empty
// state rather than 500.
func TestAdminHealthLeases_NoSource(t *testing.T) {
	s := NewServer() // no WithAdminLeaseAuditSource
	req := httptest.NewRequest(http.MethodGet, "/admin/health/leases", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthLeases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("should render 'not wired' empty state")
	}
}

// TestAdminHealthWatchdog_NoSource — empty-state shape.
func TestAdminHealthWatchdog_NoSource(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/health/watchdog", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthWatchdog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("should render 'not wired' empty state")
	}
}

// TestAdminIntegrationsMCP_NoSource — empty-state shape.
func TestAdminIntegrationsMCP_NoSource(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/integrations/mcp", nil)
	rec := httptest.NewRecorder()
	s.AdminIntegrationsMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Registry not wired") {
		t.Error("should render 'Registry not wired' empty state")
	}
}

// TestAdminRouter_Dispatch confirms the /admin/* router dispatches
// to the right handler for each path.
func TestAdminRouter_Dispatch(t *testing.T) {
	s := NewServer()
	cases := []struct {
		path     string
		wantBody string
	}{
		{"/admin/", "Daemon health"},
		{"/admin/audit", "Admin Audit"},
		{"/admin/health/", "Admin Health"},
		{"/admin/health/leases", "Lease audit"},
		{"/admin/health/watchdog", "Watchdog failures"},
		{"/admin/health/mcp", "MCP health"},
		{"/admin/integrations/mcp", "MCP integrations"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			// D3: adminRouter now re-checks admin scope; stamp admin.
			req := withAdminUI(httptest.NewRequest(http.MethodGet, tc.path, nil))
			rec := httptest.NewRecorder()
			s.adminRouter(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: want 200, got %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body should contain %q", tc.wantBody)
			}
		})
	}
}

// TestAdminRouter_Unknown returns 404.
func TestAdminRouter_Unknown(t *testing.T) {
	s := NewServer()
	req := withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/does-not-exist", nil))
	rec := httptest.NewRecorder()
	s.adminRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
}

// TestAdminRouter_D3_NonAdminDenied is the regression test for audit
// finding D3 (2026-06-10): the mutating /ui/admin/* handlers relied
// SOLELY on the admin.Middleware wrap order (which disengaged once,
// incident b777ef4a). adminRouter now re-checks admin scope so a future
// wrap-order regression fails closed. A non-admin context (auth ON, no
// admin stamp) reaching adminRouter directly must 403. Fails pre-fix
// (the router dispatched to the handler), passes post-fix.
func TestAdminRouter_D3_NonAdminDenied(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/audit", nil)
	req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "project-a"))
	rec := httptest.NewRecorder()
	s.adminRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin reaching adminRouter: want 403, got %d", rec.Code)
	}
}

// TestAdminRouter_D3_NonAdminDeniedOnMutatingPOSTs pins the defense for
// the auth LLD review batch-2 item "per-handler admin re-check on
// /ui/admin/* POST". Rather than copy an admin check into every mutating
// handler (scattered duplication), all admin routes funnel through the
// single fail-closed guard in adminRouter. This asserts that guarantee
// holds for the destructive POST surface: a non-admin (auth ON, no admin
// stamp) hitting any of these mutation endpoints must 403 before the
// handler runs — so a future refactor that moved dispatch out from under
// the guard would be caught here.
func TestAdminRouter_D3_NonAdminDeniedOnMutatingPOSTs(t *testing.T) {
	mutatingPaths := []string{
		"/admin/blackbox/overrides/save",
		"/admin/blackbox/overrides/delete",
		"/admin/blackbox/triggers/bulk-dismiss",
		"/admin/instincts/x/retire",
		"/admin/workflow-proposals/x/decide",
	}
	for _, p := range mutatingPaths {
		t.Run(p, func(t *testing.T) {
			s := NewServer()
			req := httptest.NewRequest(http.MethodPost, p, nil)
			req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "project-a"))
			rec := httptest.NewRecorder()
			s.adminRouter(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("non-admin POST %s: want 403, got %d", p, rec.Code)
			}
		})
	}
}

// TestAdminRouter_D3_AdminProceeds — an admin context dispatches normally.
func TestAdminRouter_D3_AdminProceeds(t *testing.T) {
	s := NewServer()
	req := withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/", nil))
	rec := httptest.NewRecorder()
	s.adminRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin context: want 200, got %d", rec.Code)
	}
}

// TestAdminRouter_D3_AuthOffProceeds — single-tenant (auth off) is
// trusted; the router dispatches without an admin stamp.
func TestAdminRouter_D3_AuthOffProceeds(t *testing.T) {
	s := NewServer()
	req := authOffUIRequest(http.MethodGet, "/admin/")
	rec := httptest.NewRecorder()
	s.adminRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth-off: want 200, got %d", rec.Code)
	}
}

// stubLeaseAudit drives the leases page test fixture.
type stubLeaseAudit struct {
	counts    map[string]int64
	rows      []AdminLeaseAuditRow
	countErr  error
	recentErr error
}

func (s *stubLeaseAudit) CountByStatus(_ context.Context) (map[string]int64, error) {
	return s.counts, s.countErr
}

func (s *stubLeaseAudit) Recent(_ context.Context, _ int) ([]AdminLeaseAuditRow, error) {
	return s.rows, s.recentErr
}

func TestAdminHealthLeases_RendersWithData(t *testing.T) {
	src := &stubLeaseAudit{
		counts: map[string]int64{"LEASED": 5, "FAILED": 2},
		rows: []AdminLeaseAuditRow{
			{
				ID: 1, TaskID: "task_2026_abcdef0123456789", ChangedAt: time.Now(),
				OldStatus: "QUEUED", NewStatus: "LEASED",
				NewLeaseID: "lease-abc", SQLSnippet: "UPDATE tasks",
			},
		},
	}
	s := NewServer(WithAdminLeaseAuditSource(src))
	req := httptest.NewRequest(http.MethodGet, "/admin/health/leases", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthLeases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "LEASED") {
		t.Error("body should contain status counts")
	}
	if !strings.Contains(body, "UPDATE tasks") {
		t.Error("body should contain SQL snippet")
	}
}

// stubStuckExecs feeds the watchdog page.
type stubStuckExecs struct {
	rows []AdminStuckExecution
	err  error
}

func (s *stubStuckExecs) RecentWatchdogFailures(_ context.Context, _ int) ([]AdminStuckExecution, error) {
	return s.rows, s.err
}

func TestAdminHealthWatchdog_RendersWithData(t *testing.T) {
	src := &stubStuckExecs{
		rows: []AdminStuckExecution{
			{
				ExecutionID: "exec_xyz", TaskID: "task_abc",
				ProjectID: "proj-a", WorkflowID: "wf-1",
				StartedAt: time.Now().Add(-time.Hour),
				UpdatedAt: time.Now(),
				ErrorCode: "watchdog/stuck",
				ErrorMsg:  "no checkpoint in 30m",
			},
		},
	}
	s := NewServer(WithAdminStuckExecutionSource(src))
	req := httptest.NewRequest(http.MethodGet, "/admin/health/watchdog", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthWatchdog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "watchdog/stuck") {
		t.Error("body should contain failure code")
	}
}

// stubMCPInventory feeds the MCP health page.
type stubMCPInventory struct{ snap AdminMCPSnapshot }

func (s stubMCPInventory) Snapshot() AdminMCPSnapshot { return s.snap }

func TestAdminHealthMCP_GETWithInventory(t *testing.T) {
	s := NewServer(WithAdminMCPInventory(stubMCPInventory{
		snap: AdminMCPSnapshot{ProjectCount: 2, ServerCount: 5},
	}))
	req := httptest.NewRequest(http.MethodGet, "/admin/health/mcp", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "servers: 5") {
		t.Error("snapshot counts should render")
	}
}

func TestAdminHealthMCP_POSTNoRefresher(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/health/mcp", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthMCP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", rec.Code)
	}
}

// stubMCPConfig feeds the integrations page.
type stubMCPConfig struct{ rows []AdminMCPProjectRow }

func (s stubMCPConfig) ConfiguredMCPServers() []AdminMCPProjectRow { return s.rows }

func TestAdminIntegrationsMCP_WithData(t *testing.T) {
	src := stubMCPConfig{rows: []AdminMCPProjectRow{
		{ProjectID: "proj-a", Servers: []AdminMCPServerRow{{Name: "broker"}}},
	}}
	s := NewServer(WithAdminMCPConfigSource(src))
	req := httptest.NewRequest(http.MethodGet, "/admin/integrations/mcp", nil)
	rec := httptest.NewRecorder()
	s.AdminIntegrationsMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "proj-a") {
		t.Error("project id should render")
	}
}

func TestAdminPrincipal_MissingReturnsUnknown(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	if got := adminPrincipal(req); got != "unknown" {
		t.Errorf("missing principal: got %q, want unknown", got)
	}
}

func TestAdminPrincipal_FromContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req = req.WithContext(admin.ContextWithAdmin(req.Context(), "api_key_sha256:test"))
	if got := adminPrincipal(req); got != "api_key_sha256:test" {
		t.Errorf("got %q", got)
	}
}

// TestClientIP_UsesContextValue — the admin audit IP now comes from the
// centrally-resolved realip context value, not from any request header.
func TestClientIP_UsesContextValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(realip.WithClientIP(req.Context(), "203.0.113.7"))
	if got := clientIP(req); got != "203.0.113.7" {
		t.Errorf("got %q, want context value 203.0.113.7", got)
	}
}

// TestClientIP_IgnoresForgedHeader is the audit-side regression for the
// Cloudflare tunnel real-IP spoof: leftmost-XFF was attacker-controllable.
// With no realip context value set (untrusted path), a forged
// X-Forwarded-For MUST NOT influence the audited IP — we key on RemoteAddr.
func TestClientIP_IgnoresForgedHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:5000"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	if got := clientIP(req); got != "198.51.100.9" {
		t.Errorf("got %q, want RemoteAddr host 198.51.100.9 (forged header ignored)", got)
	}
}

func TestClientIP_RemoteAddrFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.10:54321"
	if got := clientIP(req); got != "192.168.1.10" {
		t.Errorf("got %q", got)
	}
}

// TestAdminAudit_NoRepo renders the empty-state branch.
func TestAdminAudit_NoRepo(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/audit", nil)
	rec := httptest.NewRecorder()
	s.AdminAudit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("body should render 'not wired' empty state")
	}
}

// TestAdminAudit_SinceFilters — both RFC3339 and YYYY-MM-DD forms.
func TestAdminAudit_SinceFilters(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	s := NewServer(WithAdminAuditRepository(audit))

	cases := []string{
		"2026-05-01",
		"2026-05-01T00:00:00Z",
	}
	for _, since := range cases {
		req := httptest.NewRequest(http.MethodGet, "/admin/audit?since="+since, nil)
		rec := httptest.NewRecorder()
		s.AdminAudit(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d for since=%s", rec.Code, since)
		}
	}
}

// TestAdminHealthLeases_ErrorSurface — count/recent errors surface
// in the page without 500-ing.
func TestAdminHealthLeases_ErrorSurface(t *testing.T) {
	src := &stubLeaseAudit{countErr: errStub("count failed")}
	s := NewServer(WithAdminLeaseAuditSource(src))
	req := httptest.NewRequest(http.MethodGet, "/admin/health/leases", nil)
	rec := httptest.NewRecorder()
	s.AdminHealthLeases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "count failed") {
		t.Error("error should render on the page")
	}
}

// errStub is a tiny error implementation used as a stub error
// source in handler tests.
type errStub string

func (e errStub) Error() string { return string(e) }

func TestBuildAdminChatAuditURL(t *testing.T) {
	cases := []struct {
		name          string
		chat, project string
		want          string
	}{
		{"no filters", "", "", "/ui/admin/chat-audit"},
		{"chat only", "559741208", "", "/ui/admin/chat-audit?chat=559741208"},
		{"project only", "", "janka", "/ui/admin/chat-audit?project=janka"},
		{"both filters", "559741208", "janka", "/ui/admin/chat-audit?chat=559741208&project=janka"},
		{"escapes special chars", "a b&c", "p?q", "/ui/admin/chat-audit?chat=a+b%26c&project=p%3Fq"},
	}
	for _, tc := range cases {
		got := buildAdminChatAuditURL(tc.chat, tc.project)
		if got != tc.want {
			t.Errorf("%s: buildAdminChatAuditURL(%q,%q) = %q, want %q",
				tc.name, tc.chat, tc.project, got, tc.want)
		}
	}
}

// TestAdminClampLimit checks the clamp helper.
func TestAdminClampLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 50},
		{"abc", 50},
		{"-5", 50},
		{"100", 100},
		{"50", 50},
		{"7", 7},
		{"99999", 200},
	}
	for _, tc := range cases {
		if got := adminClampLimit(tc.raw); got != tc.want {
			t.Errorf("adminClampLimit(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
