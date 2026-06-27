// Package github implements forge.ForgeProvider for GitHub, on top of the
// existing GitHub App plumbing in internal/github (JWT→installation-token mint,
// REST). It is the first forge provider; GitLab/Gitea are future sibling
// packages implementing the same interface. The provider is constructed already
// bound to its App credentials, so no method takes an auth argument.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/forge"
	ghapp "vornik.io/vornik/internal/github"
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	tokenTTLBuffer    = 5 * time.Minute
	maxResponseBytes  = 1 << 20 // 1 MiB: diffs can be large
	errBodyExcerpt    = 512
)

func init() {
	forge.Register(forge.ProviderGitHub, func(cfg forge.Config) (forge.ForgeProvider, error) {
		return New(cfg.GitHub)
	})
}

// mintFunc matches ghapp.MintInstallationToken; injectable for tests.
type mintFunc func(ctx context.Context, c *http.Client, apiBase string, appID, installID int64, key *rsa.PrivateKey) (string, time.Time, error)

// pushFunc performs an authenticated branch push from a local clone's origin;
// injectable so tests don't shell out to git.
type pushFunc func(ctx context.Context, gitDir, branch, sha, token string) error

// Provider is the GitHub forge provider. Safe for concurrent use; the token
// cache is guarded by mu.
type Provider struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	apiBaseURL     string
	httpClient     *http.Client

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time

	mintFn mintFunc
	pushFn pushFunc
	now    func() time.Time
}

// New builds a GitHub provider from its config, reading the App private key from
// disk (it never leaves this process — only short-lived tokens do). A missing
// credential is a clear error.
func New(cfg forge.GitHubConfig) (*Provider, error) {
	if cfg.AppID == 0 || cfg.InstallationID == 0 || strings.TrimSpace(cfg.PrivateKeyPath) == "" {
		return nil, fmt.Errorf("forge/github: incomplete credentials (need app_id + installation_id + private_key_path)")
	}
	keyBytes, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("forge/github: read private key: %w", err)
	}
	key, err := ghapp.LoadPrivateKeyPEM(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("forge/github: %w", err)
	}
	base := strings.TrimSpace(cfg.APIBaseURL)
	if base == "" {
		base = defaultAPIBaseURL
	}
	return &Provider{
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		key:            key,
		apiBaseURL:     strings.TrimRight(base, "/"),
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		mintFn:         ghapp.MintInstallationToken,
		pushFn:         gitPushToOrigin,
		now:            time.Now,
	}, nil
}

// Name returns the provider discriminator.
func (p *Provider) Name() string { return forge.ProviderGitHub }

// token returns a valid installation token, minting + caching as needed.
func (p *Provider) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedToken != "" && time.Until(p.tokenExpiry) > tokenTTLBuffer {
		return p.cachedToken, nil
	}
	tok, exp, err := p.mintFn(ctx, p.httpClient, p.apiBaseURL, p.appID, p.installationID, p.key)
	if err != nil {
		return "", err
	}
	p.cachedToken, p.tokenExpiry = tok, exp
	return tok, nil
}

// ClassifyEvent turns a verified GitHub webhook into a ForgeJob. It handles
// `issues` (opened/labeled → issue job) and `pull_request` (opened/reopened/
// ready_for_review → change-request job); everything else returns ok=false.
// Deterministic: no LLM, no network.
func (p *Provider) ClassifyEvent(h http.Header, body []byte) (forge.ForgeJob, bool) {
	var pl ghEventPayload
	if err := json.Unmarshal(body, &pl); err != nil {
		return forge.ForgeJob{}, false
	}
	if pl.Repository.FullName == "" {
		return forge.ForgeJob{}, false
	}
	event := strings.TrimSpace(h.Get("X-GitHub-Event"))
	if event == "" {
		// Header-less path (e.g. a relay-forwarded body): infer the event type
		// from the payload shape. A pull_request object means a PR event; an
		// issue object means an issues event.
		switch {
		case pl.PullRequest != nil:
			event = "pull_request"
		case pl.Issue != nil:
			event = "issues"
		}
	}
	job := forge.ForgeJob{
		Provider:      forge.ProviderGitHub,
		Repo:          pl.Repository.FullName,
		Action:        pl.Action,
		DefaultBranch: pl.Repository.DefaultBranch,
	}
	switch event {
	case "issues":
		// Only a labeled issue is an actionable forge job — applying a label is
		// the maintainer's explicit "handle this" signal. A bare issues.opened
		// is intentionally ignored so opening any issue doesn't auto-spawn work.
		if pl.Issue == nil || pl.Action != "labeled" {
			return forge.ForgeJob{}, false
		}
		job.Number = pl.Issue.Number
		job.Labels = labelNames(pl.Issue.Labels)
		job.Title = pl.Issue.Title
		job.Body = pl.Issue.Body
		job.IsChangeRequest = false
		return job, true
	case "pull_request":
		if pl.PullRequest == nil ||
			(pl.Action != "opened" && pl.Action != "reopened" && pl.Action != "ready_for_review") {
			return forge.ForgeJob{}, false
		}
		job.Number = pl.PullRequest.Number
		job.Labels = labelNames(pl.PullRequest.Labels)
		job.Title = pl.PullRequest.Title
		job.Body = pl.PullRequest.Body
		job.IsChangeRequest = true
		// The synthetic pull ref (vs the head branch name) resolves on the base
		// repo even when the PR comes from a fork, and needs only the number.
		job.HeadRef = fmt.Sprintf("refs/pull/%d/head", pl.PullRequest.Number)
		return job, true
	default:
		return forge.ForgeJob{}, false
	}
}

// FetchDiff returns the unified diff for a pull request (Accept: …diff), so the
// reviewer agent never needs forge CLI access.
func (p *Provider) FetchDiff(ctx context.Context, repo string, number int) ([]byte, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/repos/%s/pulls/%d", p.apiBaseURL, repo, number)
	req, err := p.newReq(ctx, http.MethodGet, url, nil, tok)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	resp, body, err := p.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forge/github: fetch diff HTTP %d: %s", resp.StatusCode, excerpt(body))
	}
	return body, nil
}

// PushBranch pushes sha to branch on the forge, from the local clone at gitDir's
// origin. Idempotent + non-force: git rejects a non-fast-forward push, and an
// already-up-to-date push is a no-op success. The token is passed to git via an
// in-process credential path (never argv) — see gitPushToOrigin.
func (p *Provider) PushBranch(ctx context.Context, gitDir, repo, branch, sha string) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	return p.pushFn(ctx, gitDir, branch, sha, tok)
}

// OpenChangeRequest opens a pull request and returns its URL. Idempotent: if a PR
// already exists for the head branch, its URL is returned instead of opening a
// duplicate. Labels (if any) are applied after creation.
func (p *Provider) OpenChangeRequest(ctx context.Context, s forge.ChangeRequestSpec) (string, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return "", err
	}
	owner := repoOwner(s.Repo)
	if owner == "" {
		return "", fmt.Errorf("forge/github: repo %q is not owner/name", s.Repo)
	}

	// Idempotency: an OPEN PR for this head short-circuits (retry-safe: a
	// duplicate webhook or a re-run within the same issue returns the same PR
	// rather than erroring on a duplicate-PR 422).
	//
	// CRITICAL: only an OPEN PR counts. The head branch is a deterministic
	// function of the issue (branchForJob → fix/issue-<n>), so once a prior PR
	// for that branch is CLOSED or MERGED, a re-run of the same issue must open
	// a FRESH PR — not resurrect the dead one. Querying `state=all` and
	// returning existing[0] regardless of state kept handing back the
	// user-closed PR #18 on every re-run of issue #17, so the pipeline COMPLETED
	// pointing at a closed PR and never surfaced the new work (incident
	// 2026-06-13). Query open-only AND re-check state defensively.
	listURL := fmt.Sprintf("%s/repos/%s/pulls?state=open&head=%s:%s", p.apiBaseURL, s.Repo, owner, s.Head)
	listReq, err := p.newReq(ctx, http.MethodGet, listURL, nil, tok)
	if err != nil {
		return "", err
	}
	resp, body, err := p.do(listReq)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("forge/github: list PRs HTTP %d: %s", resp.StatusCode, excerpt(body))
	}
	var existing []prResponse
	if err := json.Unmarshal(body, &existing); err != nil {
		return "", fmt.Errorf("forge/github: parse PR list: %w", err)
	}
	for _, pr := range existing {
		if pr.State == "open" && pr.HTMLURL != "" {
			return pr.HTMLURL, nil
		}
	}

	// Create.
	createBody, _ := json.Marshal(map[string]any{
		"title": s.Title, "head": s.Head, "base": s.Base, "body": s.Body, "draft": s.Draft,
	})
	createReq, err := p.newReq(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/pulls", p.apiBaseURL, s.Repo), createBody, tok)
	if err != nil {
		return "", err
	}
	resp, body, err = p.do(createReq)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("forge/github: open PR HTTP %d: %s", resp.StatusCode, excerpt(body))
	}
	var created prResponse
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("forge/github: parse created PR: %w", err)
	}
	if created.HTMLURL == "" {
		return "", fmt.Errorf("forge/github: created PR missing html_url")
	}
	if len(s.Labels) > 0 && created.Number > 0 {
		p.applyLabels(ctx, tok, s.Repo, created.Number, s.Labels) // best-effort; PR already exists
	}
	return created.HTMLURL, nil
}

// PostReview posts a review against a pull request, mapping the neutral
// ReviewEvent onto GitHub's Reviews API event.
func (p *Provider) PostReview(ctx context.Context, repo string, number int, r forge.ReviewSpec) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	reqBody, _ := json.Marshal(map[string]string{"body": r.Body, "event": githubReviewEvent(r.Event)})
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", p.apiBaseURL, repo, number)
	req, err := p.newReq(ctx, http.MethodPost, url, reqBody, tok)
	if err != nil {
		return err
	}
	resp, body, err := p.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// GitHub forbids a PR's author from APPROVE-ing or REQUEST_CHANGES-ing
		// their own PR (422 "Can not approve your own pull request"). When
		// vornik's posting identity IS the PR author (it opened the PR), an
		// approving review can't land — but a plain COMMENT can. Downgrade and
		// retry once so the review feedback still posts instead of failing the
		// whole task. Incident: task_20260621234753_5a30265e08d1f09a.
		if resp.StatusCode == http.StatusUnprocessableEntity &&
			r.Event != forge.ReviewComment &&
			strings.Contains(strings.ToLower(string(body)), "your own pull request") {
			cReqBody, _ := json.Marshal(map[string]string{"body": r.Body, "event": "COMMENT"})
			cReq, cErr := p.newReq(ctx, http.MethodPost, url, cReqBody, tok)
			if cErr != nil {
				return cErr
			}
			cResp, cBody, cErr := p.do(cReq)
			if cErr != nil {
				return cErr
			}
			if cResp.StatusCode != http.StatusOK && cResp.StatusCode != http.StatusCreated {
				return fmt.Errorf("forge/github: post review (COMMENT fallback after self-approve 422) HTTP %d: %s", cResp.StatusCode, excerpt(cBody))
			}
			return nil
		}
		return fmt.Errorf("forge/github: post review HTTP %d: %s", resp.StatusCode, excerpt(body))
	}
	return nil
}

// VerifyPushAccess checks that the App installation grants contents:write — the
// permission PushBranch needs to open a change request.
func (p *Provider) VerifyPushAccess(ctx context.Context) error {
	ok, level, err := ghapp.CheckContentsWrite(ctx, p.httpClient, p.apiBaseURL, p.appID, p.installationID, p.key)
	if err != nil {
		return fmt.Errorf("forge/github: verify push access: %w", err)
	}
	if !ok {
		return fmt.Errorf("forge/github: installation lacks contents:write (have %q) — grant Contents: Read & write on the App installation", level)
	}
	return nil
}

func (p *Provider) applyLabels(ctx context.Context, tok, repo string, number int, labels []string) {
	reqBody, _ := json.Marshal(map[string][]string{"labels": labels})
	url := fmt.Sprintf("%s/repos/%s/issues/%d/labels", p.apiBaseURL, repo, number)
	req, err := p.newReq(ctx, http.MethodPost, url, reqBody, tok)
	if err != nil {
		return
	}
	if resp, _, err := p.do(req); err == nil {
		_ = resp
	}
}

// githubReviewEvent maps the neutral ReviewEvent onto GitHub's Reviews API event
// string. Unknown values fall back to COMMENT (the safe, non-gating choice).
func githubReviewEvent(e forge.ReviewEvent) string {
	switch e {
	case forge.ReviewApprove:
		return "APPROVE"
	case forge.ReviewRequestChanges:
		return "REQUEST_CHANGES"
	default:
		return "COMMENT"
	}
}

// --- small REST helpers ---

func (p *Provider) newReq(ctx context.Context, method, url string, body []byte, token string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, fmt.Errorf("forge/github: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (p *Provider) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("forge/github: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return resp, body, nil
}

type prResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"` // "open" | "closed" — only an OPEN PR is a valid idempotent match
}

type ghEventPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Issue *struct {
		Number int       `json:"number"`
		Title  string    `json:"title"`
		Body   string    `json:"body"`
		Labels []ghLabel `json:"labels"`
	} `json:"issue,omitempty"`
	PullRequest *struct {
		Number int       `json:"number"`
		Title  string    `json:"title"`
		Body   string    `json:"body"`
		Labels []ghLabel `json:"labels"`
	} `json:"pull_request,omitempty"`
}

type ghLabel struct {
	Name string `json:"name"`
}

func labelNames(in []ghLabel) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, l := range in {
		out = append(out, l.Name)
	}
	return out
}

func repoOwner(fullName string) string {
	if i := strings.Index(fullName, "/"); i > 0 {
		return fullName[:i]
	}
	return ""
}

func excerpt(b []byte) string {
	s := string(b)
	if len(s) > errBodyExcerpt {
		return s[:errBodyExcerpt] + "..."
	}
	return s
}
