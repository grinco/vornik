package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func newProposalTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Server{proposalStore: sqlite.NewProposalRepository(db.DB)}
}

func decodeProposal(t *testing.T, body string) proposalJSON {
	t.Helper()
	var p proposalJSON
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode proposal: %v (%s)", err, body)
	}
	return p
}

func TestOperatorPropose_WritesDraft(t *testing.T) {
	s := newProposalTestServer(t)
	rec := httptest.NewRecorder()
	body := `{"kind":"config","blastRadius":"project","projectId":"janka","title":"bump scraper timeout","diff":"-30\n+90","proposedBy":"tune-detector"}`
	s.OperatorProposals(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	p := decodeProposal(t, rec.Body.String())
	if p.Status != "DRAFT" || p.ID == "" || p.ProjectID != "janka" {
		t.Fatalf("unexpected proposal: %+v", p)
	}
}

func TestOperatorPropose_ValidatesKindAndScope(t *testing.T) {
	s := newProposalTestServer(t)
	rec := httptest.NewRecorder()
	s.OperatorProposals(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals", `{"kind":"bogus","blastRadius":"project","title":"x"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind must 400, got %d", rec.Code)
	}
}

func TestOperatorProposals_DeniesProjectTenant(t *testing.T) {
	s := newProposalTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operator/proposals", nil)
	req = req.WithContext(context.WithValue(
		ContextWithProjectScope(req.Context(), "proj-a"), authEnabledKey, true))
	s.OperatorProposals(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("project-scoped tenant must be denied (404), got %d", rec.Code)
	}
}

func TestOperatorDecide_ApproveAndSelfApproveGuard(t *testing.T) {
	s := newProposalTestServer(t)
	// Propose (proposed_by = agent).
	rec := httptest.NewRecorder()
	s.OperatorProposals(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals",
		`{"kind":"model","blastRadius":"project","title":"swap model","proposedBy":"agent-x"}`))
	id := decodeProposal(t, rec.Body.String()).ID

	// Self-approval (actor == proposer) is rejected.
	rec = httptest.NewRecorder()
	s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/decide", `{"decision":"approve","actor":"agent-x"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-approval must be 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// A different human approves — OK.
	rec = httptest.NewRecorder()
	s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/decide", `{"decision":"approve","actor":"vadim"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve by a different actor: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if decodeProposal(t, rec.Body.String()).Status != "APPROVED" {
		t.Fatal("status should be APPROVED")
	}

	// Re-deciding a decided proposal is rejected.
	rec = httptest.NewRecorder()
	s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/decide", `{"decision":"reject","actor":"vadim"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-decide must be 409, got %d", rec.Code)
	}
}

func TestOperatorProposals_ListAndGet(t *testing.T) {
	s := newProposalTestServer(t)
	for _, tt := range []string{"a", "b"} {
		rec := httptest.NewRecorder()
		s.OperatorProposals(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals",
			`{"kind":"config","blastRadius":"project","projectId":"janka","title":"t-`+tt+`"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", tt, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.OperatorProposals(rec, operatorReq(http.MethodGet, "/api/v1/operator/proposals?project=janka", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"count\":2") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
}

type fakeApplier struct {
	applyErr, rollbackErr error
	lastID                string
	lastAck               bool
}

func (f *fakeApplier) Apply(_ context.Context, id, _ string, ack bool) error {
	f.lastID, f.lastAck = id, ack
	return f.applyErr
}
func (f *fakeApplier) Rollback(_ context.Context, id string) error {
	f.lastID = id
	return f.rollbackErr
}

// seedApproved proposes + approves a proposal, returning its id.
func seedApproved(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.OperatorProposals(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals",
		`{"kind":"config","blastRadius":"project","projectId":"janka","title":"t","proposedBy":"agent"}`))
	id := decodeProposal(t, rec.Body.String()).ID
	if err := s.proposalStore.SetStatus(context.Background(), id, "APPROVED", "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return id
}

func TestOperatorApply_Success(t *testing.T) {
	s := newProposalTestServer(t)
	fa := &fakeApplier{}
	s.proposalApplier = fa
	id := seedApproved(t, s)
	rec := httptest.NewRecorder()
	s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/apply", `{"actor":"vadim","ackDaemon":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fa.lastID != id || !fa.lastAck {
		t.Errorf("engine not called with id+ack: %+v", fa)
	}
}

func TestOperatorApply_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"review-only", controlplane.ErrReviewOnly, http.StatusUnprocessableEntity},
		{"busy", controlplane.ErrBusy, http.StatusConflict},
		{"daemon-ack", controlplane.ErrDaemonAckRequired, http.StatusConflict},
		{"not-approved", persistence.ErrProposalNotApproved, http.StatusConflict},
		{"traversal", controlplane.ErrPathTraversal, http.StatusUnprocessableEntity},
		{"reload-failed", errors.New("reload rejected"), http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newProposalTestServer(t)
			s.proposalApplier = &fakeApplier{applyErr: c.err}
			id := seedApproved(t, s)
			rec := httptest.NewRecorder()
			s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/apply", `{}`))
			if rec.Code != c.want {
				t.Fatalf("%s: want %d, got %d: %s", c.name, c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOperatorApply_NotWired(t *testing.T) {
	s := newProposalTestServer(t) // no applier
	rec := httptest.NewRecorder()
	s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/x/apply", `{}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired applier must 503, got %d", rec.Code)
	}
}

func TestOperatorRollback_Success(t *testing.T) {
	s := newProposalTestServer(t)
	fa := &fakeApplier{}
	s.proposalApplier = fa
	id := seedApproved(t, s)
	rec := httptest.NewRecorder()
	s.OperatorProposalItem(rec, operatorReq(http.MethodPost, "/api/v1/operator/proposals/"+id+"/rollback", ``))
	if rec.Code != http.StatusOK || fa.lastID != id {
		t.Fatalf("rollback: want 200 + engine called, got %d (%s)", rec.Code, fa.lastID)
	}
}

// TestOperatorProposalsRoute_NotUnderAdminPrefix pins the CE-availability
// decision: the operator proposal routes must NOT be under /api/v1/admin/.
func TestOperatorProposalsRoute_NotUnderAdminPrefix(t *testing.T) {
	files := parsePackageFiles(t)
	admin := adminRouteHandlers(files)
	if admin["OperatorProposals"] || admin["OperatorProposalItem"] {
		t.Fatal("operator proposal handlers must NOT be on an /api/v1/admin/ route (CE feature)")
	}
}
