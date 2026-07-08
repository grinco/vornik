package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/controlplane"
)

type fakeDiagnoser struct {
	verdict    *controlplane.DiagnoseVerdict
	proposalID string
	err        error

	lastFocus   string
	lastPropose bool
}

func (f *fakeDiagnoser) Diagnose(_ context.Context, focus string, propose bool) (*controlplane.DiagnoseVerdict, string, error) {
	f.lastFocus, f.lastPropose = focus, propose
	return f.verdict, f.proposalID, f.err
}

func TestOperatorDiagnose_ReturnsVerdict(t *testing.T) {
	s := newProposalTestServer(t)
	fd := &fakeDiagnoser{
		verdict:    &controlplane.DiagnoseVerdict{RootCause: "web_fetch timeout", Confidence: "high", Evidence: []string{"log"}},
		proposalID: "cp_abc",
	}
	s.diagnoser = fd
	rec := httptest.NewRecorder()
	s.OperatorDiagnose(rec, operatorReq(http.MethodPost, "/api/v1/operator/diagnose", `{"focus":"janka","propose":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnose: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fd.lastFocus != "janka" || !fd.lastPropose {
		t.Errorf("engine not called with focus+propose: %+v", fd)
	}
	var out struct {
		Verdict    controlplane.DiagnoseVerdict `json:"verdict"`
		ProposalID string                       `json:"proposalId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Verdict.RootCause != "web_fetch timeout" || out.ProposalID != "cp_abc" {
		t.Errorf("unexpected response: %+v", out)
	}
}

func TestOperatorDiagnose_RequiresFocus(t *testing.T) {
	s := newProposalTestServer(t)
	s.diagnoser = &fakeDiagnoser{}
	rec := httptest.NewRecorder()
	s.OperatorDiagnose(rec, operatorReq(http.MethodPost, "/api/v1/operator/diagnose", `{"focus":"  "}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty focus must 400, got %d", rec.Code)
	}
}

func TestOperatorDiagnose_NotWired(t *testing.T) {
	s := newProposalTestServer(t) // no diagnoser
	rec := httptest.NewRecorder()
	s.OperatorDiagnose(rec, operatorReq(http.MethodPost, "/api/v1/operator/diagnose", `{"focus":"janka"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired diagnoser must 503, got %d", rec.Code)
	}
}

func TestOperatorDiagnose_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ambiguous", controlplane.ErrDiagnoseAmbiguousFocus, http.StatusConflict},
		{"no-llm", controlplane.ErrDiagnoseNoLLM, http.StatusServiceUnavailable},
		{"other", errors.New("llm timeout"), http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newProposalTestServer(t)
			s.diagnoser = &fakeDiagnoser{err: c.err}
			rec := httptest.NewRecorder()
			s.OperatorDiagnose(rec, operatorReq(http.MethodPost, "/api/v1/operator/diagnose", `{"focus":"x"}`))
			if rec.Code != c.want {
				t.Fatalf("%s: want %d, got %d: %s", c.name, c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOperatorDiagnose_DeniesProjectTenant(t *testing.T) {
	s := newProposalTestServer(t)
	s.diagnoser = &fakeDiagnoser{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/diagnose", nil)
	req = req.WithContext(context.WithValue(
		ContextWithProjectScope(req.Context(), "proj-a"), authEnabledKey, true))
	s.OperatorDiagnose(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("project-scoped tenant must be denied (404), got %d", rec.Code)
	}
}

func TestOperatorDiagnose_MethodNotAllowed(t *testing.T) {
	s := newProposalTestServer(t)
	s.diagnoser = &fakeDiagnoser{}
	rec := httptest.NewRecorder()
	s.OperatorDiagnose(rec, operatorReq(http.MethodGet, "/api/v1/operator/diagnose", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must 405, got %d", rec.Code)
	}
}
