package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/forge"
)

// testProvider builds a Provider pointed at base with a fixed minted token, so
// REST methods exercise real HTTP against an httptest stub without needing a key.
func testProvider(base string) *Provider {
	return &Provider{
		appID:          1,
		installationID: 2,
		apiBaseURL:     strings.TrimRight(base, "/"),
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		mintFn: func(context.Context, *http.Client, string, int64, int64, *rsa.PrivateKey) (string, time.Time, error) {
			return "ghs_test", time.Now().Add(time.Hour), nil
		},
		now: time.Now,
	}
}

func TestNew_RequiresCredentials(t *testing.T) {
	if _, err := New(forge.GitHubConfig{}); err == nil {
		t.Fatal("want error for empty github config")
	}
	if _, err := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: ""}); err == nil {
		t.Fatal("want error for missing key path")
	}
}

func TestName(t *testing.T) {
	if (&Provider{}).Name() != forge.ProviderGitHub {
		t.Errorf("Name() should be %q", forge.ProviderGitHub)
	}
}

func TestClassifyEvent(t *testing.T) {
	cases := []struct {
		name     string
		event    string
		body     string
		wantOK   bool
		wantNum  int
		wantCR   bool
		wantHead string
		labels   []string
	}{
		{
			name:    "issue labeled",
			event:   "issues",
			body:    `{"action":"labeled","repository":{"full_name":"o/r","default_branch":"main"},"issue":{"number":7,"labels":[{"name":"bug"}]},"label":{"name":"bug"}}`,
			wantOK:  true,
			wantNum: 7,
			wantCR:  false,
			// An issue job has no change-request head to check out.
			wantHead: "",
			labels:   []string{"bug"},
		},
		{
			name:     "pull request opened",
			event:    "pull_request",
			body:     `{"action":"opened","repository":{"full_name":"o/r","default_branch":"main"},"pull_request":{"number":12,"labels":[]}}`,
			wantOK:   true,
			wantNum:  12,
			wantCR:   true,
			wantHead: "refs/pull/12/head",
		},
		{name: "issue closed ignored", event: "issues", body: `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":7}}`, wantOK: false},
		{name: "issue opened ignored (label required)", event: "issues", body: `{"action":"opened","repository":{"full_name":"o/r","default_branch":"main"},"issue":{"number":7}}`, wantOK: false},
		{name: "pr synchronize ignored", event: "pull_request", body: `{"action":"synchronize","repository":{"full_name":"o/r"},"pull_request":{"number":3}}`, wantOK: false},
		{name: "push ignored", event: "push", body: `{"repository":{"full_name":"o/r"}}`, wantOK: false},
		{name: "no repo ignored", event: "issues", body: `{"action":"opened","issue":{"number":1}}`, wantOK: false},
		{name: "malformed ignored", event: "issues", body: `not json`, wantOK: false},
	}
	p := &Provider{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-GitHub-Event", tc.event)
			job, ok := p.ClassifyEvent(h, []byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if job.Number != tc.wantNum || job.IsChangeRequest != tc.wantCR || job.Provider != forge.ProviderGitHub {
				t.Errorf("job=%+v", job)
			}
			if job.Repo != "o/r" || job.DefaultBranch != "main" {
				t.Errorf("repo/default-branch wrong: %+v", job)
			}
			if job.HeadRef != tc.wantHead {
				t.Errorf("HeadRef=%q want %q (job=%+v)", job.HeadRef, tc.wantHead, job)
			}
			if len(tc.labels) > 0 && (len(job.Labels) != 1 || job.Labels[0] != tc.labels[0]) {
				t.Errorf("labels=%v", job.Labels)
			}
		})
	}
}

func TestFetchDiff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/12" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/vnd.github.v3.diff" {
			t.Errorf("accept=%s", r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "token ghs_test" {
			t.Errorf("auth=%s", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "diff --git a b\n")
	}))
	defer srv.Close()

	diff, err := testProvider(srv.URL).FetchDiff(context.Background(), "o/r", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(diff), "diff --git") {
		t.Errorf("diff=%q", diff)
	}
}

func TestOpenChangeRequest_CreatesAndAppliesLabels(t *testing.T) {
	var created, labeled bool
	var gotDraft bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			if got := r.URL.Query().Get("head"); got != "o:fix/issue-7" {
				t.Errorf("head query=%q", got)
			}
			_, _ = io.WriteString(w, `[]`) // none exist
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls":
			created = true
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			gotDraft, _ = b["draft"].(bool)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":15,"html_url":"https://github.com/o/r/pull/15"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/15/labels":
			labeled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	url, err := testProvider(srv.URL).OpenChangeRequest(context.Background(), forge.ChangeRequestSpec{
		Repo: "o/r", Head: "fix/issue-7", Base: "main", Title: "t", Body: "Closes #7",
		Labels: []string{"automated"}, Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/o/r/pull/15" {
		t.Errorf("url=%s", url)
	}
	if !created || !labeled || !gotDraft {
		t.Errorf("created=%v labeled=%v draft=%v", created, labeled, gotDraft)
	}
}

func TestOpenChangeRequest_IdempotentWhenExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Fatalf("must not POST when an OPEN PR already exists (got %s)", r.URL.Path)
		}
		// state=open query → an open PR; the handler short-circuits to it.
		_, _ = io.WriteString(w, `[{"number":9,"html_url":"https://github.com/o/r/pull/9","state":"open"}]`)
	}))
	defer srv.Close()

	url, err := testProvider(srv.URL).OpenChangeRequest(context.Background(), forge.ChangeRequestSpec{
		Repo: "o/r", Head: "fix/issue-7", Base: "main", Title: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/o/r/pull/9" {
		t.Errorf("want existing PR url, got %s", url)
	}
}

// TestOpenChangeRequest_ClosedPROpensFresh — regression for incident 2026-06-13.
// The head branch is deterministic per issue (fix/issue-<n>), so re-running an
// issue whose prior PR was CLOSED/MERGED must open a FRESH PR, not resurrect the
// dead one. Pre-fix the handler queried state=all and returned existing[0]
// regardless of state, so a re-run of issue #17 kept returning the user-closed
// PR #18 and the pipeline COMPLETED pointing at a closed PR (no usable PR). The
// query must be state=open AND a non-open match must be ignored.
func TestOpenChangeRequest_ClosedPROpensFresh(t *testing.T) {
	var sawStateOpen, created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			if r.URL.Query().Get("state") == "open" {
				sawStateOpen = true
			}
			// state=open returns no open PRs (the only one for this head is closed).
			// Defensive: even if a closed PR leaked into the list, the handler must
			// skip it — include one to prove the state check.
			_, _ = io.WriteString(w, `[{"number":18,"html_url":"https://github.com/o/r/pull/18","state":"closed"}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls":
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":19,"html_url":"https://github.com/o/r/pull/19"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	url, err := testProvider(srv.URL).OpenChangeRequest(context.Background(), forge.ChangeRequestSpec{
		Repo: "o/r", Head: "fix/issue-17", Base: "main", Title: "t", Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawStateOpen {
		t.Error("idempotency lookup must query state=open, not state=all")
	}
	if !created {
		t.Error("a closed PR for the head must NOT short-circuit — a fresh PR must be created")
	}
	if url != "https://github.com/o/r/pull/19" {
		t.Errorf("want fresh PR #19, got %s", url)
	}
}

func TestPostReview_MapsEvent(t *testing.T) {
	for _, tc := range []struct {
		ev   forge.ReviewEvent
		want string
	}{
		{forge.ReviewComment, "COMMENT"},
		{forge.ReviewApprove, "APPROVE"},
		{forge.ReviewRequestChanges, "REQUEST_CHANGES"},
		{forge.ReviewEvent("garbage"), "COMMENT"},
	} {
		var gotEvent string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/o/r/pulls/3/reviews" {
				t.Errorf("path=%s", r.URL.Path)
			}
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			gotEvent = b["event"]
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		}))
		err := testProvider(srv.URL).PostReview(context.Background(), "o/r", 3, forge.ReviewSpec{Body: "lgtm", Event: tc.ev})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		if gotEvent != tc.want {
			t.Errorf("event=%q want %q", gotEvent, tc.want)
		}
	}
}

func TestPushBranch_DelegatesToPushFn(t *testing.T) {
	var gotDir, gotBranch, gotSha, gotTok string
	p := testProvider("http://unused")
	p.pushFn = func(_ context.Context, dir, branch, sha, tok string) error {
		gotDir, gotBranch, gotSha, gotTok = dir, branch, sha, tok
		return nil
	}
	if err := p.PushBranch(context.Background(), "/work/project", "o/r", "fix/issue-7", "abc123"); err != nil {
		t.Fatal(err)
	}
	if gotDir != "/work/project" || gotBranch != "fix/issue-7" || gotSha != "abc123" || gotTok != "ghs_test" {
		t.Errorf("push args: dir=%s branch=%s sha=%s tok=%s", gotDir, gotBranch, gotSha, gotTok)
	}
}

func TestFetchDiff_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "no such pull")
	}))
	defer srv.Close()
	if _, err := testProvider(srv.URL).FetchDiff(context.Background(), "o/r", 99); err == nil {
		t.Fatal("want error on HTTP 404")
	}
}

func TestToken_Caches(t *testing.T) {
	calls := 0
	p := testProvider("http://unused")
	p.mintFn = func(context.Context, *http.Client, string, int64, int64, *rsa.PrivateKey) (string, time.Time, error) {
		calls++
		return "ghs_cached", time.Now().Add(time.Hour), nil
	}
	for i := 0; i < 3; i++ {
		if tok, err := p.token(context.Background()); err != nil || tok != "ghs_cached" {
			t.Fatalf("token=%q err=%v", tok, err)
		}
	}
	if calls != 1 {
		t.Errorf("mint called %d times, want 1 (cached)", calls)
	}
}

// TestClassifyEvent_HeaderlessInference: with no X-GitHub-Event header (the
// relay-forwarded path), the event type is inferred from the payload shape.
func TestClassifyEvent_HeaderlessInference(t *testing.T) {
	p := &Provider{}
	issue := `{"action":"labeled","repository":{"full_name":"o/r","default_branch":"main"},"issue":{"number":7,"labels":[{"name":"bug"}]}}`
	job, ok := p.ClassifyEvent(http.Header{}, []byte(issue))
	if !ok || job.Number != 7 || job.IsChangeRequest {
		t.Errorf("headerless issue: ok=%v job=%+v", ok, job)
	}
	pr := `{"action":"opened","repository":{"full_name":"o/r","default_branch":"main"},"pull_request":{"number":12}}`
	job2, ok2 := p.ClassifyEvent(http.Header{}, []byte(pr))
	if !ok2 || job2.Number != 12 || !job2.IsChangeRequest {
		t.Errorf("headerless PR: ok=%v job=%+v", ok2, job2)
	}
}

func TestClassifyEvent_PopulatesTitleBody(t *testing.T) {
	p := &Provider{}
	h := http.Header{}
	h.Set("X-GitHub-Event", "issues")
	job, ok := p.ClassifyEvent(h, []byte(`{"action":"labeled","repository":{"full_name":"o/r","default_branch":"main"},"issue":{"number":7,"title":"My title","body":"My body","labels":[{"name":"bug"}]}}`))
	if !ok || job.Title != "My title" || job.Body != "My body" {
		t.Errorf("issue title/body not populated: %+v ok=%v", job, ok)
	}
	hp := http.Header{}
	hp.Set("X-GitHub-Event", "pull_request")
	pr, ok := p.ClassifyEvent(hp, []byte(`{"action":"opened","repository":{"full_name":"o/r"},"pull_request":{"number":3,"title":"PR title","body":"PR body"}}`))
	if !ok || pr.Title != "PR title" || pr.Body != "PR body" {
		t.Errorf("PR title/body not populated: %+v ok=%v", pr, ok)
	}
}
