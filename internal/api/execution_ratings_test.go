package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// stubRatingRepo satisfies persistence.ExecutionRatingRepository and records
// what the handler actually did, so "refused" can be told from "refused and
// wrote anyway".
type stubRatingRepo struct {
	rows      map[string]*persistence.ExecutionRating // executionID|raterID
	upserts   int
	deletes   []string
	upsertErr error
	getErr    error
}

func newStubRatingRepo() *stubRatingRepo {
	return &stubRatingRepo{rows: map[string]*persistence.ExecutionRating{}}
}

func ratingKey(executionID, raterID string) string { return executionID + "|" + raterID }

func (s *stubRatingRepo) Upsert(_ context.Context, rating *persistence.ExecutionRating) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts++
	cp := *rating
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	cp.UpdatedAt = time.Now().UTC()
	s.rows[ratingKey(rating.ExecutionID, rating.RaterID)] = &cp
	return nil
}

func (s *stubRatingRepo) Get(_ context.Context, executionID, raterID string) (*persistence.ExecutionRating, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if r, ok := s.rows[ratingKey(executionID, raterID)]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}

func (s *stubRatingRepo) ListByExecution(_ context.Context, executionID string) ([]*persistence.ExecutionRating, error) {
	var out []*persistence.ExecutionRating
	for _, r := range s.rows {
		if r.ExecutionID == executionID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *stubRatingRepo) Delete(_ context.Context, executionID, raterID string) error {
	s.deletes = append(s.deletes, ratingKey(executionID, raterID))
	delete(s.rows, ratingKey(executionID, raterID))
	return nil
}

func (s *stubRatingRepo) DeleteOlderThan(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func newRatingServer(t *testing.T, repo persistence.ExecutionRatingRepository, exec *persistence.Execution) *Server {
	t.Helper()
	opts := []ServerOption{}
	if repo != nil {
		opts = append(opts, WithExecutionRatingRepository(repo))
	}
	if exec != nil {
		opts = append(opts, WithExecutionRepository(&stubExecRepoForFork{exec: exec}))
	}
	return NewServer(opts...)
}

// ratingRequest builds a request in the auth-DISABLED shape, where
// X-Operator-Id is honoured as the caller's identity. An auth-enabled
// deployment resolves the API key's principal instead; requestOperatorID owns
// that distinction and TestRequestOperatorID_* covers it.
func ratingRequest(t *testing.T, method string, body any, operator string) *http.Request {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	} else {
		rdr = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, "/api/v1/executions/exec_1/rating", rdr)
	r.Header.Set("Content-Type", "application/json")
	if operator != "" {
		r.Header.Set("X-Operator-Id", operator)
	}
	return r.WithContext(context.WithValue(r.Context(), authEnabledKey, false))
}

func ratedExecution() *persistence.Execution {
	return &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
}

// The double must agree with production about what absence looks like, or it
// certifies a broken handler path rather than failing. The lint requires this
// assertion in any package holding such a double — the guard exists because a
// stub returning (nil, nil) against a production ErrNotFound is invisible until
// the handler's 404 branch never fires.
func TestStubRatingRepo_HonoursTheMissContract(t *testing.T) {
	repo := newStubRatingRepo()
	repotest.AssertMiss(t, "ExecutionRatingRepository.Get",
		func() (*persistence.ExecutionRating, error) {
			return repo.Get(context.Background(), "exec-absent", "op-nobody")
		})
}

func TestExecutionRatingUpsert_HappyPath(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodPost,
		ExecutionRatingRequest{Verdict: "down", Reason: "thin, and it ignored the format"}, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingUpsert(rec, req, "exec_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := repo.rows[ratingKey("exec_1", "op_1")]
	if got == nil {
		t.Fatal("nothing stored")
	}
	if got.Verdict != persistence.VerdictDown || got.Reason != "thin, and it ignored the format" {
		t.Fatalf("stored the wrong row: %+v", got)
	}
	// The identity comes from the resolver, never from the body — a rating
	// filed under someone else's name is a forged row in a future rollup.
	if got.RaterID != "op_1" {
		t.Fatalf("rater = %q, want the resolved identity", got.RaterID)
	}
}

// An anonymous rating cannot be edited by its author, attributed in a rollup, or
// told apart from a second rater's — three properties the record exists to have.
// So an unresolvable identity is 401 and writes NOTHING.
func TestExecutionRatingUpsert_UnknownRaterIs401AndWritesNothing(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodPost, ExecutionRatingRequest{Verdict: "up"}, "")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingUpsert(rec, req, "exec_1")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.upserts != 0 {
		t.Fatalf("an unidentified caller wrote %d row(s)", repo.upserts)
	}
}

// The verdict set is closed in three places: here, the Go constants, and the
// table's CHECK. This asserts the first, so a bad value never reaches the other
// two.
func TestExecutionRatingUpsert_RefusesAThirdVerdict(t *testing.T) {
	for _, verdict := range []string{"sideways", "UP", "", "5"} {
		t.Run(verdict, func(t *testing.T) {
			repo := newStubRatingRepo()
			srv := newRatingServer(t, repo, ratedExecution())

			req := ratingRequest(t, http.MethodPost, ExecutionRatingRequest{Verdict: verdict}, "op_1")
			rec := httptest.NewRecorder()
			srv.ExecutionRatingUpsert(rec, req, "exec_1")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("verdict %q got %d, want 400", verdict, rec.Code)
			}
			if repo.upserts != 0 {
				t.Fatalf("verdict %q was stored", verdict)
			}
		})
	}
}

// Over the cap is REFUSED, not truncated: a rating that says half of what its
// author wrote is worse than one that made them shorten it.
func TestExecutionRatingUpsert_RefusesAnOversizedReasonRatherThanTruncating(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, ratedExecution())

	long := strings.Repeat("x", persistence.ExecutionRatingReasonMax+1)
	req := ratingRequest(t, http.MethodPost,
		ExecutionRatingRequest{Verdict: "up", Reason: long}, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingUpsert(rec, req, "exec_1")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.upserts != 0 {
		t.Fatal("an oversized reason was stored, possibly truncated")
	}
}

func TestExecutionRatingUpsert_AcceptsAReasonExactlyAtTheCap(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodPost, ExecutionRatingRequest{
		Verdict: "up", Reason: strings.Repeat("x", persistence.ExecutionRatingReasonMax),
	}, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingUpsert(rec, req, "exec_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("a reason at the cap was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// A reason is never required — asking for one before accepting a down-vote is
// how you stop getting down-votes.
func TestExecutionRatingUpsert_ReasonIsOptional(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodPost, ExecutionRatingRequest{Verdict: "down"}, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingUpsert(rec, req, "exec_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("a down-vote with no reason was refused: %d %s", rec.Code, rec.Body.String())
	}
}

func TestExecutionRatingUpsert_UnknownExecutionIs404(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, nil) // no execution repo → nothing to scope against

	req := ratingRequest(t, http.MethodPost, ExecutionRatingRequest{Verdict: "up"}, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingUpsert(rec, req, "exec_1")

	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Fatalf("expected the handler to refuse without an execution, got %d", rec.Code)
	}
	if repo.upserts != 0 {
		t.Fatal("stored a rating for an execution it could not verify")
	}
}

func TestExecutionRatingGet_ReturnsTheCallersOwnRating(t *testing.T) {
	repo := newStubRatingRepo()
	repo.rows[ratingKey("exec_1", "op_1")] = &persistence.ExecutionRating{
		ExecutionID: "exec_1", RaterID: "op_1", Verdict: persistence.VerdictUp,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodGet, nil, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingGet(rec, req, "exec_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got ExecutionRatingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Verdict != "up" || got.RaterID != "op_1" {
		t.Fatalf("wrong rating returned: %+v", got)
	}
}

// Absence is a 404, which is the honest answer to "what is my rating on this
// run" when there isn't one. See design §3.1 for why this is not (nil, nil).
func TestExecutionRatingGet_UnratedIs404(t *testing.T) {
	srv := newRatingServer(t, newStubRatingRepo(), ratedExecution())

	req := ratingRequest(t, http.MethodGet, nil, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingGet(rec, req, "exec_1")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unrated execution, got %d", rec.Code)
	}
}

// DELETE is keyed on the RESOLVED identity, never a request parameter —
// otherwise any caller could delete anyone's rating by naming them.
func TestExecutionRatingDelete_RemovesOnlyTheCallersOwnRow(t *testing.T) {
	repo := newStubRatingRepo()
	for _, rater := range []string{"op_1", "op_2"} {
		repo.rows[ratingKey("exec_1", rater)] = &persistence.ExecutionRating{
			ExecutionID: "exec_1", RaterID: rater, Verdict: persistence.VerdictUp,
		}
	}
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodDelete, nil, "op_1")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingDelete(rec, req, "exec_1")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.deletes) != 1 || repo.deletes[0] != ratingKey("exec_1", "op_1") {
		t.Fatalf("deleted the wrong key(s): %v", repo.deletes)
	}
	if repo.rows[ratingKey("exec_1", "op_2")] == nil {
		t.Fatal("deleted another rater's rating")
	}
}

func TestExecutionRatingDelete_UnknownRaterIs401AndDeletesNothing(t *testing.T) {
	repo := newStubRatingRepo()
	repo.rows[ratingKey("exec_1", "op_1")] = &persistence.ExecutionRating{
		ExecutionID: "exec_1", RaterID: "op_1", Verdict: persistence.VerdictUp,
	}
	srv := newRatingServer(t, repo, ratedExecution())

	req := ratingRequest(t, http.MethodDelete, nil, "")
	rec := httptest.NewRecorder()
	srv.ExecutionRatingDelete(rec, req, "exec_1")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if len(repo.deletes) != 0 {
		t.Fatalf("an unidentified caller deleted %v", repo.deletes)
	}
}

// nil repo keeps the endpoint at 503 rather than 500, matching the hints
// surface: "not wired on this deployment" is a different fact from "broke".
func TestExecutionRating_503WhenUnwired(t *testing.T) {
	srv := NewServer()
	for _, call := range []func(http.ResponseWriter, *http.Request, string){
		srv.ExecutionRatingUpsert, srv.ExecutionRatingGet, srv.ExecutionRatingDelete,
	} {
		rec := httptest.NewRecorder()
		call(rec, ratingRequest(t, http.MethodPost, ExecutionRatingRequest{Verdict: "up"}, "op_1"), "exec_1")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 when unwired, got %d", rec.Code)
		}
	}
}

// The router must reach all three verbs on the same path, and refuse the ones
// that are not there — a handler nothing routes to is a documented behaviour
// nothing implements.
func TestExecutionRatingRoute_DispatchesEachVerb(t *testing.T) {
	repo := newStubRatingRepo()
	srv := newRatingServer(t, repo, ratedExecution())

	cases := []struct {
		method string
		body   any
		want   int
	}{
		{http.MethodPost, ExecutionRatingRequest{Verdict: "up"}, http.StatusOK},
		{http.MethodGet, nil, http.StatusOK},
		{http.MethodDelete, nil, http.StatusNoContent},
		// Not a verb this resource has.
		{http.MethodPatch, ExecutionRatingRequest{Verdict: "up"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			req := ratingRequest(t, tc.method, tc.body, "op_1")
			rec := httptest.NewRecorder()
			srv.apiV1ExecutionsHandler(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s /rating = %d, want %d: %s",
					tc.method, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
