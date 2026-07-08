package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func newProposalRepoUI(t *testing.T) persistence.ProposalRepository {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewProposalRepository(db.DB)
}

type fakeUIApplier struct {
	applyErr, rollbackErr error
	appliedID             string
	ack                   bool
	rolledID              string
}

func (f *fakeUIApplier) Apply(_ context.Context, id, _ string, ack bool) error {
	f.appliedID, f.ack = id, ack
	return f.applyErr
}
func (f *fakeUIApplier) Rollback(_ context.Context, id string) error {
	f.rolledID = id
	return f.rollbackErr
}

func seedProposal(t *testing.T, repo persistence.ProposalRepository, id, title, status string, applyable bool, proposedBy string) {
	t.Helper()
	p := &persistence.ControlPlaneProposal{
		ID: id, ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: title,
		Status: persistence.ProposalStatusDraft, ProposedBy: proposedBy,
	}
	if applyable {
		p.ApplyTarget = "config.yaml"
		p.ApplyContent = "x: 1\n"
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if status != persistence.ProposalStatusDraft {
		// Drive to the target status via the repo transitions.
		if status == persistence.ProposalStatusApproved || status == persistence.ProposalStatusApplied {
			_ = repo.SetStatus(context.Background(), id, persistence.ProposalStatusApproved, "seed-approver")
		}
		if status == persistence.ProposalStatusApplied {
			_ = repo.MarkApplied(context.Background(), id, "seed-op", "OLD")
		}
	}
}

func TestAdminControlPlane_RendersAndEscapes(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "d1", "draft <script>alert(1)</script>", persistence.ProposalStatusDraft, false, "agent")
	seedProposal(t, repo, "a1", "approved applyable", persistence.ProposalStatusApproved, true, "agent")
	seedProposal(t, repo, "ap1", "applied one", persistence.ProposalStatusApplied, true, "agent")

	s := NewServer(WithProposalStore(repo))
	// The Proposals section carries the ledger rows + status tabs (default is
	// the Overview section now — the hub IA).
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// XSS: the script title must be escaped, not raw.
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("proposal title was NOT escaped — XSS hole")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("expected escaped title in output")
	}
	// State-machine buttons.
	if !strings.Contains(body, "Approve") || !strings.Contains(body, "Apply") || !strings.Contains(body, "Rollback") {
		t.Error("expected approve/apply/rollback buttons for the seeded statuses")
	}
	// Tabs render with counts.
	if !strings.Contains(body, "Pending") || !strings.Contains(body, "Approved") {
		t.Error("expected status tabs")
	}
	// Hub section tabs present on every section.
	if !strings.Contains(body, "Overview") || !strings.Contains(body, "MCP servers") {
		t.Error("expected hub section tabs")
	}
}

func TestAdminControlPlane_OverviewDefault(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "d1", "high failed rate on janka", persistence.ProposalStatusDraft, false, "tune-detector")
	seedProposal(t, repo, "sh1", "Diagnose: web_fetch timeout", persistence.ProposalStatusDraft, false, "self-heal")

	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	// No ?section → defaults to Overview.
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(body, "open proposals awaiting review") {
		t.Error("expected the Overview summary")
	}
	// Source counts + self-heal incident surfaced.
	if !strings.Contains(body, "Tune detector") || !strings.Contains(body, "Self-heal") {
		t.Error("expected per-source open counts on the Overview")
	}
	if !strings.Contains(body, "Open incidents") {
		t.Error("expected the open-incidents section")
	}
	// Nav highlight fix: CurrentPage=admin-control-plane marks the Control-plane
	// nav dest active (panel-item-active + aria-current), not the generic Admin
	// console item — the reported "selected item not highlighted" bug.
	if !strings.Contains(body, "panel-item-active") || !strings.Contains(body, `aria-current="page"`) {
		t.Error("expected the active nav marker rendered for the control-plane page")
	}
}

func TestAdminControlPlane_ProposalsSourceFilter(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "t1", "tune failed-rate", persistence.ProposalStatusDraft, false, "tune-detector")
	seedProposal(t, repo, "i1", "instinct tool-timeout", persistence.ProposalStatusDraft, false, "instinct")

	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals&source=instinct", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(body, "instinct tool-timeout") {
		t.Error("source=instinct should show the instinct proposal")
	}
	if strings.Contains(body, "tune failed-rate") {
		t.Error("source=instinct must hide the tune-detector proposal")
	}
	// Source tabs rendered (>1 source present).
	if !strings.Contains(body, "source:") || !strings.Contains(body, "Tune detector") {
		t.Error("expected source filter tabs")
	}
}

func TestAdminControlPlane_ApprovePost(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "d1", "t", persistence.ProposalStatusDraft, false, "agent")
	s := NewServer(WithProposalStore(repo))
	form := url.Values{"id": {"d1"}, "action": {"approve"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	got, _ := repo.GetByID(context.Background(), "d1")
	if got.Status != persistence.ProposalStatusApproved {
		t.Fatalf("approve did not flip status: %s", got.Status)
	}
}

func TestAdminControlPlane_SelfApprovalFlash(t *testing.T) {
	repo := newProposalRepoUI(t)
	// Proposed by web-admin (the console's fallback actor) → self-approval.
	seedProposal(t, repo, "d1", "t", persistence.ProposalStatusDraft, false, "web-admin")
	s := NewServer(WithProposalStore(repo))
	form := url.Values{"id": {"d1"}, "action": {"approve"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "done=self-approval") {
		t.Fatalf("expected self-approval flash, got redirect %q", rec.Header().Get("Location"))
	}
}

func TestAdminControlPlane_ApplyAndRollbackDelegate(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "a1", "t", persistence.ProposalStatusApproved, true, "agent")
	fa := &fakeUIApplier{}
	s := NewServer(WithProposalStore(repo), WithProposalApplier(fa))

	// Apply with the daemon-ack checkbox.
	form := url.Values{"id": {"a1"}, "action": {"apply"}, "ackDaemon": {"on"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, req)
	if fa.appliedID != "a1" || !fa.ack {
		t.Fatalf("apply not delegated with ack: %+v", fa)
	}
	if !strings.Contains(rec.Header().Get("Location"), "done=applied") {
		t.Errorf("expected applied flash, got %q", rec.Header().Get("Location"))
	}

	// Rollback delegates.
	form = url.Values{"id": {"a1"}, "action": {"rollback"}}
	req = httptest.NewRequest(http.MethodPost, "/admin/control-plane", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.AdminControlPlane(rec, req)
	if fa.rolledID != "a1" {
		t.Fatalf("rollback not delegated: %+v", fa)
	}
}

func TestAdminControlPlane_RepoUnwired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired should still render 200, got %d", rec.Code)
	}
}

// fakeUIDiagnoser satisfies proposalDiagnoser for the Diagnose tab test.
type fakeUIDiagnoser struct {
	verdict    *controlplane.DiagnoseVerdict
	proposalID string
	err        error
	lastFocus  string
	lastProp   bool
}

func (f *fakeUIDiagnoser) Diagnose(_ context.Context, focus string, propose bool) (*controlplane.DiagnoseVerdict, string, error) {
	f.lastFocus, f.lastProp = focus, propose
	return f.verdict, f.proposalID, f.err
}

func TestAdminControlPlaneDiagnose_RendersVerdict(t *testing.T) {
	repo := newProposalRepoUI(t)
	fd := &fakeUIDiagnoser{
		verdict:    &controlplane.DiagnoseVerdict{RootCause: "web_fetch timeout", Confidence: "high", Evidence: []string{"log line"}},
		proposalID: "cpp_x",
	}
	s := NewServer(WithProposalStore(repo), WithDiagnoser(fd))
	form := url.Values{"focus": {"janka"}, "propose": {"on"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane/diagnose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminControlPlaneDiagnose(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if fd.lastFocus != "janka" || !fd.lastProp {
		t.Errorf("diagnoser not called with focus+propose: %+v", fd)
	}
	if !strings.Contains(body, "web_fetch timeout") || !strings.Contains(body, "cpp_x") {
		t.Error("expected the verdict + filed-proposal link rendered")
	}
}

func TestAdminControlPlaneDiagnose_NotWired(t *testing.T) {
	repo := newProposalRepoUI(t)
	s := NewServer(WithProposalStore(repo)) // no diagnoser
	form := url.Values{"focus": {"janka"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane/diagnose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminControlPlaneDiagnose(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not wired") {
		t.Fatalf("expected graceful not-wired message, got %d", rec.Code)
	}
}
