package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
)

// OpenChangeRequestHandler implements the "forge.open_change_request" system
// step: push the task's committed branch and open a pull/merge request. End-to-
// end idempotent — the branch name is deterministic, PushBranch no-ops when the
// remote already matches, and OpenChangeRequest returns the existing CR — so a
// retry after a crash or a duplicate webhook delivery never opens a second CR.
type OpenChangeRequestHandler struct {
	resolver ProviderResolver
	source   PublishSource
}

// NewOpenChangeRequestHandler wires the handler. Nil-safe: a missing dependency
// surfaces a clear error at Execute rather than panicking.
func NewOpenChangeRequestHandler(resolver ProviderResolver, source PublishSource) *OpenChangeRequestHandler {
	return &OpenChangeRequestHandler{resolver: resolver, source: source}
}

// Name implements executor.SystemHandler.
func (h *OpenChangeRequestHandler) Name() string { return "forge.open_change_request" }

// openResult is the handler's result envelope (also the resume short-circuit:
// on re-run the same CR URL comes back, so downstream steps are stable).
type openResult struct {
	CRURL  string `json:"cr_url"`
	Branch string `json:"branch"`
	State  string `json:"state"` // "opened" — OpenChangeRequest is idempotent, returning the existing URL when present
	// Diagnostics (2026-06-13): surfaced in the result envelope (logged by the
	// executor's system-step success path) so a no_change skip is debuggable —
	// it shows which base/sha were compared and the commit count that drove the
	// decision, without threading a logger through the handler.
	Base    string `json:"base,omitempty"`
	SHA     string `json:"sha,omitempty"`
	GitDir  string `json:"git_dir,omitempty"`
	Commits int    `json:"commits_beyond_base,omitempty"`
	CountOK bool   `json:"count_ok,omitempty"`
}

// Execute implements executor.SystemHandler.
func (h *OpenChangeRequestHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	const name = "forge.open_change_request"
	if h == nil || h.resolver == nil || h.source == nil {
		return executor.SystemStepResult{}, errors.New(name + ": handler is missing required dependencies (resolver/source)")
	}
	job, err := forgeJobFromTask(in.Task, name)
	if err != nil {
		return executor.SystemStepResult{}, err
	}
	provider, err := h.resolver.ForgeProvider(ctx, in.Task.ProjectID)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: resolve provider: %w", name, err)
	}
	gitDir, sha, err := h.source.PublishSource(ctx, in.Task)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: locate publish source: %w", name, err)
	}

	base := job.DefaultBranch
	if base == "" {
		base = "main"
	}

	// Nothing to publish: if the head has no commits beyond the base, the child
	// produced no merged change (e.g. it checkpointed/failed). Opening a change
	// request with no diff would error; instead skip cleanly so the task
	// COMPLETEs as a no-op rather than FAILing (incident 2026-06-13).
	n, countOK := commitsBeyondBase(ctx, gitDir, base, sha)
	if countOK && n == 0 {
		out, _ := json.Marshal(openResult{
			Branch: branchForJob(*job), State: "no_change",
			Base: base, SHA: sha, GitDir: gitDir, Commits: n, CountOK: countOK,
		})
		return executor.SystemStepResult{Result: out}, nil
	}

	branch := branchForJob(*job)
	if err := provider.PushBranch(ctx, gitDir, job.Repo, branch, sha); err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: push branch %s: %w", name, branch, err)
	}

	// Always open as a DRAFT: these are LLM-authored changes, so a human must
	// review and mark ready (or discard) before merge — never an auto-mergeable
	// PR. (isFeature now only affects the branch name + title verb.)
	url, err := provider.OpenChangeRequest(ctx, forgeapi.ChangeRequestSpec{
		Repo:   job.Repo,
		Head:   branch,
		Base:   base,
		Title:  titleForJob(*job),
		Body:   bodyForJob(*job),
		Labels: job.Labels,
		Draft:  true,
	})
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: open change request: %w", name, err)
	}

	out, err := json.Marshal(openResult{
		CRURL: url, Branch: branch, State: "opened",
		Base: base, SHA: sha, GitDir: gitDir, Commits: n, CountOK: countOK,
	})
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: marshal result: %w", name, err)
	}
	return executor.SystemStepResult{Result: out}, nil
}
