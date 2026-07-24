package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// OpenChangeRequestHandler implements the "forge.open_change_request" system
// step: push the task's committed branch and open a pull/merge request. End-to-
// end idempotent — the branch name is deterministic, PushBranch no-ops when the
// remote already matches, and OpenChangeRequest returns the existing CR — so a
// retry after a crash or a duplicate webhook delivery never opens a second CR.
type OpenChangeRequestHandler struct {
	resolver ProviderResolver
	source   PublishSource
	// store attaches the mail-in patch when a push is rejected. Optional:
	// nil disables the artifact (the task still parks with the reason).
	store ArtifactStore
	// taint is the untrusted-content review gate consulted BEFORE the branch is
	// pushed (taint-lineage-tracking §4.5). Optional: nil disables the check
	// (feature off / not wired), and the write proceeds unchanged.
	taint TaintGate
}

// NewOpenChangeRequestHandler wires the handler. Nil-safe: a missing required
// dependency (resolver/source) surfaces a clear error at Execute rather than
// panicking. store is optional (the push-rejected fallback still parks without
// it, minus the downloadable patch). taint is optional (nil disables the
// untrusted-content review gate).
func NewOpenChangeRequestHandler(resolver ProviderResolver, source PublishSource, store ArtifactStore, taint TaintGate) *OpenChangeRequestHandler {
	return &OpenChangeRequestHandler{resolver: resolver, source: source, store: store, taint: taint}
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

	// Untrusted-content review gate (taint-lineage-tracking §4.5): under a
	// project's enforce mode, an autonomous forge write derived from untrusted
	// lineage PARKS for operator review BEFORE the push happens. Checked here
	// (past the no_change short-circuit, so we never park a write that would
	// open nothing) and before PushBranch, so the operator reviews before any
	// remote side effect. off/advisory return (nil,nil) and proceed.
	if h.taint != nil {
		sig, terr := h.taint.ReviewForgeWrite(ctx, in.Task.ProjectID, in.Task.ID)
		switch {
		case terr != nil:
			// Fail CLOSED (M5): a reviewer error must never let an autonomous
			// write through. ReviewForgeWrite folds resolution errors into a
			// mode-aware decision internally (enforce → park, off/advisory →
			// proceed) and does not surface them here, so a non-nil error is an
			// unexpected contract break — take the conservative path and park for
			// operator review rather than pushing unreviewed.
			park := sig
			if park == nil {
				park = &executor.TaintReviewSignal{State: executor.TaintReviewState, SourceCount: 0, ShownCount: 0}
			}
			out, _ := json.Marshal(park)
			return executor.SystemStepResult{Result: out}, nil
		case sig != nil:
			out, _ := json.Marshal(sig)
			return executor.SystemStepResult{Result: out}, nil
		}
	}

	branch := branchForJob(*job)
	if err := provider.PushBranch(ctx, gitDir, job.Repo, branch, sha); err != nil {
		// A remote REJECTION (missing permission, protected branch, …) is not
		// retry-fixable as-is. Rather than fail (→ retry loop / silent skip),
		// capture the committed change as a mail-in patch and signal the
		// executor to PARK the task awaiting operator action.
		if pre, ok := forgeapi.AsPushRejected(err); ok {
			return h.blockedResult(ctx, in, base, sha, gitDir, branch, pre), nil
		}
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

// blockedResult builds the awaiting-operator PARK signal for an un-pushable
// change: a one-line reason + a kind-specific remediation, plus (best-effort) a
// git-am-able patch of the committed change attached as a downloadable OUTPUT
// artifact so the operator can submit it by hand. The executor turns this signal
// into an AWAITING_INPUT hand-off (no failure, no retry). Never errors — a park
// with a reason is always better than failing.
func (h *OpenChangeRequestHandler) blockedResult(ctx context.Context, in executor.SystemStepInput, base, sha, gitDir, branch string, pre *forgeapi.PushRejectedError) executor.SystemStepResult {
	sig := executor.PublishBlockedSignal{
		State:       executor.SystemStepBlockedState,
		Reason:      fmt.Sprintf("Could not open a change request: branch %q was rejected by the forge (%s). %s", branch, pre.Kind, pre.Output),
		Remediation: pre.Kind.Remediation(),
	}
	if h.store != nil && in.Task != nil && in.Execution != nil {
		if patch, err := patchFromBase(ctx, gitDir, base, sha); err == nil && len(patch) > 0 {
			artName := "unpushable-" + strings.ReplaceAll(branch, "/", "-") + ".patch"
			if art := h.storePatch(ctx, in, artName, patch); art != nil {
				sig.ArtifactID = art.ID
				sig.ArtifactName = art.Name
			}
		}
	}
	out, _ := json.Marshal(sig)
	return executor.SystemStepResult{Result: out}
}

// storePatch writes the patch bytes to a temp file and persists it as a task
// OUTPUT artifact (the store reads from a path). Returns nil on any failure —
// the caller degrades to a park without the downloadable patch.
func (h *OpenChangeRequestHandler) storePatch(ctx context.Context, in executor.SystemStepInput, name string, patch []byte) *persistence.Artifact {
	f, err := os.CreateTemp("", "vornik-patch-*.patch")
	if err != nil {
		return nil
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, werr := f.Write(patch); werr != nil {
		_ = f.Close()
		return nil
	}
	if cerr := f.Close(); cerr != nil {
		return nil
	}
	art, err := h.store.Store(ctx, in.Task.ProjectID, in.Execution.ID, in.Task.ID, name, tmp)
	if err != nil {
		return nil
	}
	return art
}
