package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"vornik.io/vornik/internal/forge"
	ghapp "vornik.io/vornik/internal/github"
)

// The Actions half of the GitHub provider — forge.CIReader
// (https://docs.vornik.io §7).
//
// Read-only against Actions. Nothing here re-runs, cancels or dispatches a
// workflow, and the App asks for `actions: read` alone.

// wireCIRun is the subset of GET /repos/{repo}/actions/runs/{id} this reads.
type wireCIRun struct {
	ID           int64  `json:"id"`
	HeadSHA      string `json:"head_sha"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	RunAttempt   int    `json:"run_attempt"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	RunStartedAt string `json:"run_started_at"`
	UpdatedAt    string `json:"updated_at"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

type wireCIJobs struct {
	Jobs []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"jobs"`
}

type wireCIArtifacts struct {
	Artifacts []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_in_bytes"`
		Expired   bool   `json:"expired"`
	} `json:"artifacts"`
}

// FetchCIRun returns the run's conclusion and its per-job breakdown.
//
// Two calls, because GitHub splits them: the run gives the overall conclusion
// and the commit, the jobs endpoint gives the per-job detail a named-job filter
// needs. A jobs failure is NOT fatal — the run's own conclusion is the load
// bearing part, and losing the breakdown degrades the review's detail rather
// than its correctness.
func (p *Provider) FetchCIRun(ctx context.Context, repo string, runID int64) (forge.CIRun, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return forge.CIRun{}, err
	}

	url := fmt.Sprintf("%s/repos/%s/actions/runs/%d", p.apiBaseURL, repo, runID)
	req, err := p.newReq(ctx, http.MethodGet, url, nil, tok)
	if err != nil {
		return forge.CIRun{}, err
	}
	resp, body, err := p.do(req)
	if err != nil {
		return forge.CIRun{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return forge.CIRun{}, forge.NewStatusError("forge/github: fetch ci run",
			resp.StatusCode, resp.Header, excerpt(body))
	}
	var w wireCIRun
	if err := json.Unmarshal(body, &w); err != nil {
		return forge.CIRun{}, fmt.Errorf("forge/github: decode ci run: %w", err)
	}

	out := forge.CIRun{
		RunID:        w.ID,
		HeadSHA:      w.HeadSHA,
		WorkflowName: w.Name,
		WorkflowPath: w.Path,
		RunAttempt:   w.RunAttempt,
		Conclusion:   w.Conclusion,
		StartedAt:    parseGitHubTime(w.RunStartedAt),
		CompletedAt:  parseGitHubTime(w.UpdatedAt),
	}
	// EMPTY FOR A FORK PR, and that is GitHub's documented behaviour rather
	// than something to code around. Number stays 0, the run is still
	// recorded, and only the PR-scoped paths skip it (design §3.3).
	if len(w.PullRequests) > 0 {
		out.Number = w.PullRequests[0].Number
	}

	jobs, jerr := p.fetchCIJobs(ctx, tok, repo, runID)
	if jerr != nil {
		// Deliberately not returned: the run's conclusion is what triggers and
		// what an operator reads first. A missing breakdown is a thinner
		// review, not a wrong one.
		return out, nil
	}
	out.Jobs = jobs
	return out, nil
}

func (p *Provider) fetchCIJobs(ctx context.Context, tok, repo string, runID int64) ([]forge.CIJob, error) {
	url := fmt.Sprintf("%s/repos/%s/actions/runs/%d/jobs?per_page=100", p.apiBaseURL, repo, runID)
	req, err := p.newReq(ctx, http.MethodGet, url, nil, tok)
	if err != nil {
		return nil, err
	}
	resp, body, err := p.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, forge.NewStatusError("forge/github: fetch ci jobs",
			resp.StatusCode, resp.Header, excerpt(body))
	}
	var w wireCIJobs
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("forge/github: decode ci jobs: %w", err)
	}
	out := make([]forge.CIJob, 0, len(w.Jobs))
	for _, j := range w.Jobs {
		out = append(out, forge.CIJob{Name: j.Name, Status: j.Status, Conclusion: j.Conclusion})
	}
	return out, nil
}

// ListCIArtifacts names what the run uploaded.
func (p *Provider) ListCIArtifacts(ctx context.Context, repo string, runID int64) ([]forge.CIArtifact, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/repos/%s/actions/runs/%d/artifacts?per_page=100", p.apiBaseURL, repo, runID)
	req, err := p.newReq(ctx, http.MethodGet, url, nil, tok)
	if err != nil {
		return nil, err
	}
	resp, body, err := p.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, forge.NewStatusError("forge/github: list ci artifacts",
			resp.StatusCode, resp.Header, excerpt(body))
	}
	var w wireCIArtifacts
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("forge/github: decode ci artifacts: %w", err)
	}
	out := make([]forge.CIArtifact, 0, len(w.Artifacts))
	for _, a := range w.Artifacts {
		out = append(out, forge.CIArtifact{
			ID: a.ID, Name: a.Name, SizeBytes: a.SizeBytes, Expired: a.Expired,
		})
	}
	return out, nil
}

// FetchCIArtifact downloads one artifact, refusing anything over maxBytes.
//
// The refusal happens BEFORE the body is read, from Content-Length, and the
// read itself is bounded by a LimitReader at maxBytes+1 so a response that
// lies about its length cannot spend more than one byte over the ceiling. A
// reader that streamed first and checked after would already have paid the
// cost the cap exists to avoid.
//
// The bytes are a ZIP — GitHub always zips artifacts — and are returned raw.
// Unpacking belongs to the caller that knows what it asked for; doing it here
// would put an archive parser inside the provider.
func (p *Provider) FetchCIArtifact(ctx context.Context, repo string, artifactID int64, maxBytes int64) ([]byte, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/repos/%s/actions/artifacts/%d/zip", p.apiBaseURL, repo, artifactID)
	req, err := p.newReq(ctx, http.MethodGet, url, nil, tok)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge/github: GET %s: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, forge.NewStatusError("forge/github: fetch ci artifact",
			resp.StatusCode, resp.Header, excerpt(body))
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes > %d", forge.ErrCIArtifactTooLarge, resp.ContentLength, maxBytes)
	}
	limit := maxBytes
	if limit <= 0 {
		limit = maxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("forge/github: read ci artifact: %w", err)
	}
	if int64(len(data)) > limit {
		// The response under-reported its length. Refuse rather than truncate:
		// a silently shortened ZIP is a corrupt one, and the caller would have
		// no way to tell that from a small artifact.
		return nil, fmt.Errorf("%w: body exceeded %d bytes", forge.ErrCIArtifactTooLarge, limit)
	}
	return data, nil
}

// VerifyCIAccess reports whether the installation grants `actions: read`.
//
// Mirrors VerifyPushAccess: called at boot for every project with CI ingestion
// configured, so a missing permission is a warning an operator can act on
// rather than a 403 hours later against the first completed run — by which
// time the outcome that would have triggered a review is simply absent, and an
// absence looks like "CI has not finished yet".
func (p *Provider) VerifyCIAccess(ctx context.Context) error {
	// The permission set comes from the token exchange, not from probing an
	// Actions endpoint: a probe would need a repository and a run to aim at,
	// and "no runs yet" would be indistinguishable from "not permitted".
	level, err := ghapp.CheckInstallationPermission(ctx, p.httpClient, p.apiBaseURL,
		p.appID, p.installationID, p.key, "actions")
	if err != nil {
		return fmt.Errorf("forge/github: verify ci access: %w", err)
	}
	if level == "" {
		return fmt.Errorf("forge/github: installation lacks actions:read — grant " +
			"Actions: Read on the App and RE-ACCEPT the installation on each " +
			"repository, which GitHub requires for a new permission")
	}
	return nil
}

// parseGitHubTime reads GitHub's RFC3339 timestamps, yielding the zero time
// for an absent or malformed one rather than an error: a missing timestamp
// must not fail an ingestion whose point is the conclusion.
func parseGitHubTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
