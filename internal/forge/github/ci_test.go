package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/forge"
)

// The Actions reader (LLD 2026-09-08-forge-ci-outcomes-design.md §7).
//
// The size ceiling is the part worth the most care: it is a cost AND a safety
// control, and the ways to get it wrong are all "read first, check later".

func ciTestProvider(t *testing.T, h http.Handler) (*Provider, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	// A pre-warmed token cache rather than a mint stub: these tests are about
	// the Actions endpoints, and making each one also serve the token exchange
	// would put the auth flow into every fixture.
	p := &Provider{
		apiBaseURL:  srv.URL,
		httpClient:  srv.Client(),
		cachedToken: "ghs_test",
		tokenExpiry: time.Now().Add(time.Hour),
	}
	return p, srv.Close
}

func TestFetchCIRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/infra/actions/runs/77", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":77,"head_sha":"deadbeef","name":"Terraform",
			"path":".github/workflows/terraform-plan.yml","run_attempt":2,
			"status":"completed","conclusion":"failure",
			"run_started_at":"2026-09-08T10:00:00Z","updated_at":"2026-09-08T10:05:00Z",
			"pull_requests":[{"number":42}]}`)
	})
	mux.HandleFunc("/repos/acme/infra/actions/runs/77/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"jobs":[
			{"name":"terraform-plan","status":"completed","conclusion":"failure"},
			{"name":"lint","status":"completed","conclusion":"success"}]}`)
	})
	p, done := ciTestProvider(t, mux)
	defer done()

	got, err := p.FetchCIRun(context.Background(), "acme/infra", 77)
	if err != nil {
		t.Fatalf("FetchCIRun: %v", err)
	}
	if got.Conclusion != "failure" || got.HeadSHA != "deadbeef" || got.Number != 42 {
		t.Errorf("run did not decode: %+v", got)
	}
	if got.WorkflowPath != ".github/workflows/terraform-plan.yml" {
		t.Errorf("WorkflowPath = %q — the path is what the workflow filter matches on", got.WorkflowPath)
	}
	if got.RunAttempt != 2 {
		t.Errorf("RunAttempt = %d, want 2", got.RunAttempt)
	}
	if len(got.Jobs) != 2 || got.Jobs[0].Name != "terraform-plan" {
		t.Errorf("jobs did not decode: %+v", got.Jobs)
	}
	if got.StartedAt.IsZero() || got.CompletedAt.IsZero() {
		t.Errorf("timestamps did not decode: %+v", got)
	}
}

// A fork PR's workflow_run carries no pull request. The run must still be
// returned — recorded with Number 0 — rather than treated as unusable
// (design §3.3).
func TestFetchCIRunWithNoPullRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/infra/actions/runs/78", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":78,"head_sha":"cafe","conclusion":"success",
			"path":".github/workflows/deploy.yml","pull_requests":[]}`)
	})
	mux.HandleFunc("/repos/acme/infra/actions/runs/78/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"jobs":[]}`)
	})
	p, done := ciTestProvider(t, mux)
	defer done()

	got, err := p.FetchCIRun(context.Background(), "acme/infra", 78)
	if err != nil {
		t.Fatalf("FetchCIRun: %v", err)
	}
	if got.Number != 0 {
		t.Errorf("Number = %d, want 0", got.Number)
	}
	if got.Conclusion != "success" {
		t.Errorf("the run must still be usable: %+v", got)
	}
}

// A jobs-endpoint failure must not lose the run. The conclusion is what
// triggers and what an operator reads first.
func TestFetchCIRunSurvivesAJobsFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/infra/actions/runs/79", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":79,"head_sha":"beef","conclusion":"failure"}`)
	})
	mux.HandleFunc("/repos/acme/infra/actions/runs/79/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p, done := ciTestProvider(t, mux)
	defer done()

	got, err := p.FetchCIRun(context.Background(), "acme/infra", 79)
	if err != nil {
		t.Fatalf("a jobs failure must not fail the run: %v", err)
	}
	if got.Conclusion != "failure" {
		t.Errorf("Conclusion = %q, want failure", got.Conclusion)
	}
	if len(got.Jobs) != 0 {
		t.Errorf("Jobs = %+v, want empty", got.Jobs)
	}
}

func TestListCIArtifactsCarriesExpiry(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/infra/actions/runs/80/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"artifacts":[
			{"id":1,"name":"plan","size_in_bytes":120,"expired":false},
			{"id":2,"name":"old-plan","size_in_bytes":90,"expired":true}]}`)
	})
	p, done := ciTestProvider(t, mux)
	defer done()

	got, err := p.ListCIArtifacts(context.Background(), "acme/infra", 80)
	if err != nil {
		t.Fatalf("ListCIArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2", len(got))
	}
	// An expired artifact is RETURNED, not omitted, so a caller can say "the
	// plan existed and GitHub deleted it" rather than "no plan".
	if !got[1].Expired {
		t.Error("an expired artifact must be reported as expired, not dropped")
	}
}

// The ceiling refuses from Content-Length, before the body is consumed. The
// error naming the SIZE is the assertion that the length was what refused it:
// the read-side check further down has no size to report.
func TestFetchCIArtifactRefusesFromContentLength(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 5000))
	})
	p, done := ciTestProvider(t, h)
	defer done()

	_, err := p.FetchCIArtifact(context.Background(), "acme/infra", 1, 100)
	if !errors.Is(err, forge.ErrCIArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrCIArtifactTooLarge", err)
	}
	if !strings.Contains(err.Error(), "5000") {
		t.Errorf("the error must name the size so an operator can raise the cap: %v", err)
	}
}

// A response that UNDER-REPORTS its length must still be refused, not
// truncated: a shortened ZIP is a corrupt one, indistinguishable from a small
// artifact.
func TestFetchCIArtifactRefusesABodyThatExceedsTheCap(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: chunked, so the ceiling can only be enforced on read.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 500))
	})
	p, done := ciTestProvider(t, h)
	defer done()

	_, err := p.FetchCIArtifact(context.Background(), "acme/infra", 1, 100)
	if !errors.Is(err, forge.ErrCIArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrCIArtifactTooLarge", err)
	}
}

func TestFetchCIArtifactReturnsBytesUnderTheCap(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("PK\x03\x04plan"))
	})
	p, done := ciTestProvider(t, h)
	defer done()

	got, err := p.FetchCIArtifact(context.Background(), "acme/infra", 1, 1024)
	if err != nil {
		t.Fatalf("FetchCIArtifact: %v", err)
	}
	if string(got) != "PK\x03\x04plan" {
		t.Errorf("got %q", got)
	}
}

// VerifyCIAccess reads the permission set off the token exchange rather than
// probing an Actions endpoint: a probe needs a repository and a run to aim at,
// and "no runs yet" would be indistinguishable from "not permitted".
func TestVerifyCIAccess(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	mk := func(perms string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z","permissions":{`+perms+`}}`)
		}))
	}

	granted := mk(`"actions":"read","contents":"write"`)
	defer granted.Close()
	p := &Provider{appID: 1, installationID: 2, key: key, apiBaseURL: granted.URL, httpClient: granted.Client()}
	if err := p.VerifyCIAccess(context.Background()); err != nil {
		t.Errorf("actions:read should verify: %v", err)
	}

	// Absent, not merely lower — GitHub omits a permission that was never
	// granted, so the empty level is the real-world shape.
	missing := mk(`"contents":"write"`)
	defer missing.Close()
	p2 := &Provider{appID: 1, installationID: 2, key: key, apiBaseURL: missing.URL, httpClient: missing.Client()}
	err := p2.VerifyCIAccess(context.Background())
	if err == nil {
		t.Fatal("a missing actions permission must be an error")
	}
	// The remedy is the non-obvious part: a new permission needs the
	// installation RE-ACCEPTED per repository, which nothing else tells you.
	if !strings.Contains(err.Error(), "RE-ACCEPT") {
		t.Errorf("the error must name the remedy: %v", err)
	}
}
