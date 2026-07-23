package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
		switch status {
		case persistence.ProposalStatusApproved, persistence.ProposalStatusApplied, persistence.ProposalStatusRolledBack:
			_ = repo.SetStatus(context.Background(), id, persistence.ProposalStatusApproved, "seed-approver")
		case persistence.ProposalStatusRejected:
			_ = repo.SetStatus(context.Background(), id, persistence.ProposalStatusRejected, "seed-approver")
		}
		if status == persistence.ProposalStatusApplied || status == persistence.ProposalStatusRolledBack {
			_ = repo.MarkApplied(context.Background(), id, "seed-op", "OLD")
		}
		if status == persistence.ProposalStatusRolledBack {
			_ = repo.MarkRolledBack(context.Background(), id)
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

// TestNormalizeCPFilter_ReturnsConstantsOnly pins the go/unvalidated-url-
// redirection fix: the decide redirect's `back` URL is assembled from the
// status filter, so normalizeCPFilter must yield a fixed status CONSTANT
// (trimmed) or "" — never the raw request value that tainted the Location.
func TestNormalizeCPFilter_ReturnsConstantsOnly(t *testing.T) {
	// Whitespace-padded valid filters normalise to the exact constant.
	if got := normalizeCPFilter("  " + persistence.ProposalStatusApproved + "\t"); got != persistence.ProposalStatusApproved {
		t.Errorf("padded APPROVED = %q, want %q", got, persistence.ProposalStatusApproved)
	}
	// Attacker-controlled / non-whitelisted input collapses to "".
	for _, in := range []string{"https://evil.com", "//evil.com", "javascript:alert(1)", "bogus", ""} {
		if got := normalizeCPFilter(in); got != "" {
			t.Errorf("normalizeCPFilter(%q) = %q, want \"\"", in, got)
		}
	}
}

// TestAdminControlPlane_DecideRedirectSameOrigin drives adminCPDecide with a
// crafted `status` field and asserts the resulting redirect stays a
// same-origin relative path — no attacker host leaks into Location
// (go/unvalidated-url-redirection).
func TestAdminControlPlane_DecideRedirectSameOrigin(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "d1", "t", persistence.ProposalStatusDraft, false, "agent")
	s := NewServer(WithProposalStore(repo))
	form := url.Values{"id": {"d1"}, "action": {"approve"}, "status": {"https://evil.com/x"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/ui/admin/control-plane") {
		t.Fatalf("redirect not same-origin relative: %q", loc)
	}
	if strings.Contains(loc, "evil.com") {
		t.Fatalf("attacker host leaked into redirect: %q", loc)
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

func TestCPApplyOutcome_MapsEngineErrors(t *testing.T) {
	cases := map[error]string{
		nil:                               "applied",
		controlplane.ErrBusy:              "busy",
		controlplane.ErrDaemonAckRequired: "ack-required",
		controlplane.ErrReviewOnly:        "review-only",
		controlplane.ErrStaleBase:         "stale-base", // must NOT fall to the misleading "apply-failed"
	}
	for err, want := range cases {
		if got := cpApplyOutcome(err); got != want {
			t.Errorf("cpApplyOutcome(%v) = %q, want %q", err, got, want)
		}
	}
	// The stale-base token has a distinct, non-misleading flash message.
	if _, ok := cpFlashMessages["stale-base"]; !ok {
		t.Error("stale-base flash message must be defined")
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

// TestBuildCPOverview_FoldsBlackBoxTriggers — the hub Overview lists open
// Black Box triggers when the healing-trigger repo is wired, so an operator
// can action them without leaving for /ui/admin/blackbox. Backlog item 5
// part 3. CE (no repo) leaves the panel empty.
func TestBuildCPOverview_FoldsBlackBoxTriggers(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-1"))
	_ = repo.Insert(context.Background(), openTrigger("ht-2"))
	s := NewServer(WithHealingTriggerRepository(repo))

	var data AdminControlPlaneData
	s.buildCPOverview(context.Background(), &data, nil)

	if data.OpenTriggerCount != 2 || len(data.OpenTriggers) != 2 {
		t.Fatalf("want 2 folded triggers, got count=%d rows=%d", data.OpenTriggerCount, len(data.OpenTriggers))
	}

	// CE: no repo → no triggers panel.
	var ce AdminControlPlaneData
	NewServer().buildCPOverview(context.Background(), &ce, nil)
	if ce.OpenTriggerCount != 0 || len(ce.OpenTriggers) != 0 {
		t.Errorf("CE (no repo) must not populate triggers; got %d", ce.OpenTriggerCount)
	}
}

func TestAdminControlPlane_DefaultHidesClosed(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "d1", "pending one", persistence.ProposalStatusDraft, false, "tune-detector")
	seedProposal(t, repo, "r1", "rejected one", persistence.ProposalStatusRejected, false, "tune-detector")
	seedProposal(t, repo, "rb1", "rolled back one", persistence.ProposalStatusRolledBack, true, "tune-detector")

	s := NewServer(WithProposalStore(repo))
	// Default proposals view (no status filter) hides REJECTED / ROLLED_BACK.
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals", nil))
	body := rec.Body.String()
	if strings.Contains(body, "rejected one") || strings.Contains(body, "rolled back one") {
		t.Fatal("default proposals view must hide REJECTED/ROLLED_BACK rows")
	}
	if !strings.Contains(body, "pending one") {
		t.Fatal("default view must still show open proposals")
	}
	// The "Open" tab count must reflect only the visible (open) rows — 1 here —
	// not the full set incl. the hidden REJECTED/ROLLED_BACK rows (regression
	// guard: the relabel from "All" to "Open" must not advertise hidden rows).
	if !strings.Contains(body, `Open <span class="opacity-70">1</span>`) {
		t.Errorf("Open tab count must equal the visible open rows (1), not the full set (3)")
	}
	// The Rejected tab reveals them.
	rec = httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals&status=REJECTED", nil))
	if !strings.Contains(rec.Body.String(), "rejected one") {
		t.Fatal("Rejected tab must show rejected proposals")
	}
}

func TestAdminControlPlane_Paginates(t *testing.T) {
	repo := newProposalRepoUI(t)
	for i := 0; i < cpProposalsPageSize+5; i++ {
		seedProposal(t, repo, fmt.Sprintf("d%d", i), fmt.Sprintf("pending %d", i), persistence.ProposalStatusDraft, false, "tune-detector")
	}
	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals", nil))
	body := rec.Body.String()
	// Page 1 exposes a next-page link and no prev link.
	if !strings.Contains(body, "page=2") {
		t.Fatal("page 1 must expose a next-page link when rows exceed the page size")
	}
}

// TestAdminControlPlane_SupersededHidesRollback covers design 2026-07-23 §D.2:
// an APPLIED proposal that a later overlapping-target APPLIED proposal
// overwrote is no longer the live top-of-stack — its rollback would be
// refused by the D1 engine guard, so the hub hides the button and shows the
// "superseded" badge instead.
func TestAdminControlPlane_SupersededHidesRollback(t *testing.T) {
	repo := newProposalRepoUI(t)
	base := time.Unix(1700000000, 0)
	older := base
	newer := base.Add(time.Minute)
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "p1", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "older apply",
		Status: persistence.ProposalStatusApplied, ProposedBy: "agent",
		Approver: "human", AppliedBy: "human", ApplyTarget: "config.yaml",
		AppliedAt: &older,
	})
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "p2", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "newer apply",
		Status: persistence.ProposalStatusApplied, ProposedBy: "agent",
		Approver: "human", AppliedBy: "human", ApplyTarget: "config.yaml",
		AppliedAt: &newer,
	})

	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals&status=APPLIED", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(body, "superseded — rollback unavailable") {
		t.Error("expected the superseded badge on the overwritten row")
	}
	if got := strings.Count(body, "↩ Rollback"); got != 1 {
		t.Errorf("expected exactly 1 rollback button (superseded row's hidden), got %d", got)
	}
}

// TestAdminControlPlane_DisjointTargetsKeepBothRollbacks is the negative case:
// two APPLIED proposals with DIFFERENT targets never overlap, so neither is
// superseded and both keep their rollback button.
func TestAdminControlPlane_DisjointTargetsKeepBothRollbacks(t *testing.T) {
	repo := newProposalRepoUI(t)
	base := time.Unix(1700000000, 0)
	older := base
	newer := base.Add(time.Minute)
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "p1", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "config apply",
		Status: persistence.ProposalStatusApplied, ProposedBy: "agent",
		Approver: "human", AppliedBy: "human", ApplyTarget: "config.yaml",
		AppliedAt: &older,
	})
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "p2", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "other apply",
		Status: persistence.ProposalStatusApplied, ProposedBy: "agent",
		Approver: "human", AppliedBy: "human", ApplyTarget: "other.yaml",
		AppliedAt: &newer,
	})

	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals&status=APPLIED", nil))
	body := rec.Body.String()
	if strings.Contains(body, "superseded") {
		t.Error("disjoint targets must not trigger the superseded badge")
	}
	if got := strings.Count(body, "↩ Rollback"); got != 2 {
		t.Errorf("expected both rollback buttons (disjoint targets), got %d", got)
	}
}

// TestAdminControlPlane_EqualTimestampTieBreak mirrors the engine's strict
// tie-break at the UI layer: two overlapping APPLIED proposals with the SAME
// AppliedAt must render exactly one rollback button (the higher-ID row),
// with the lower-ID row showing the superseded badge — never both refused,
// never both allowed.
func TestAdminControlPlane_EqualTimestampTieBreak(t *testing.T) {
	repo := newProposalRepoUI(t)
	at := time.Unix(1700000000, 0)
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "cpp_aaa", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "tie low",
		Status: persistence.ProposalStatusApplied, ProposedBy: "agent",
		Approver: "human", AppliedBy: "human", ApplyTarget: "config.yaml",
		AppliedAt: &at,
	})
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "cpp_bbb", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "tie high",
		Status: persistence.ProposalStatusApplied, ProposedBy: "agent",
		Approver: "human", AppliedBy: "human", ApplyTarget: "config.yaml",
		AppliedAt: &at,
	})

	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals&status=APPLIED", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(body, "superseded — rollback unavailable") {
		t.Error("expected the superseded badge on the equal-timestamp lower-ID row")
	}
	if got := strings.Count(body, "↩ Rollback"); got != 1 {
		t.Errorf("expected exactly 1 rollback button (tie-break top only), got %d", got)
	}
	if !strings.Contains(body, "tie high") {
		t.Error("expected the higher-ID (tie-break top) row to render")
	}
}

// TestAdminControlPlane_AutoRetiredLabel covers the system-vs-human rejection
// distinction: a row retired by the system (Approver system:*) renders a
// distinct "auto-retired (stale)" label instead of a plain rejection.
func TestAdminControlPlane_AutoRetiredLabel(t *testing.T) {
	repo := newProposalRepoUI(t)
	mustCreate(t, repo, &persistence.ControlPlaneProposal{
		ID: "p1", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "stale draft",
		Status: persistence.ProposalStatusRejected, ProposedBy: "agent",
		Approver: "system:auto-retire-stale",
	})

	s := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/admin/control-plane?section=proposals&status=REJECTED", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "auto-retired (stale)") {
		t.Error("expected the auto-retired label for a system:-approver rejection")
	}
}

// mustCreate seeds a proposal directly via repo.Create (bypassing SetStatus
// transitions) so tests can construct explicit APPLIED/REJECTED rows with
// controlled AppliedAt timestamps.
func mustCreate(t *testing.T, repo persistence.ProposalRepository, p *persistence.ControlPlaneProposal) {
	t.Helper()
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create %s: %v", p.ID, err)
	}
}
