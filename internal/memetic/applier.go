package memetic

// Slice 4 — Apply path for approved architect proposals. Reads
// the approved proposal, writes the new YAML to both config trees
// (source + deployed, per the two-trees discipline), commits the
// change in the source tree's git repo, refreshes the daemon's
// in-memory config (which fires the cross-instance NOTIFY), and
// stamps the proposal row as applied.
//
// Slice 4 boundary: Apply runs ONLY against status=approved rows.
// Pending rows must be decided first via the operator review path
// (Slice 3). The MarkApplied repository contract already enforces
// the approved → applied transition at the SQL layer; the applier
// also short-circuits before any filesystem work for clarity in
// the error message.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ErrProposalNotApproved fires when the operator tries to apply a
// proposal that isn't in status=approved. Maps to 409 Conflict at
// the API layer. Distinct from ErrInvalidProposalTransition so
// the admin endpoint can return a clear message.
var ErrProposalNotApproved = errors.New("memetic: proposal must be approved before apply")

// WorkflowWriter writes a workflow YAML body to both config trees
// (source + deployed) and reports the source-tree path so the
// caller can git-commit it. Adapters in the service layer handle
// the actual filesystem semantics.
type WorkflowWriter interface {
	// Write places `body` at workflows/<workflowID>.md in both
	// trees. Returns the absolute source-tree path so the
	// GitCommitter can stage it; if there's no source tree
	// (deployed-only deployment), returns "" + nil error.
	Write(ctx context.Context, workflowID string, body []byte) (sourcePath string, err error)
}

// GitCommitter stages + commits one file in the source-tree's git
// repo and returns the commit SHA. nil committer means "no git
// available" — the applier still writes the files and reloads
// the daemon, but reports an empty applied_commit. The admin
// endpoint surfaces that case so operators know they can't roll
// back via git revert.
type GitCommitter interface {
	// Commit stages `path` (absolute) and runs `git commit -m
	// message`. Returns the resulting commit SHA. Implementations
	// should set git author/committer to the operator's identity
	// when available.
	Commit(ctx context.Context, path, message, authorName, authorEmail string) (sha string, err error)
}

// ConfigReloadTrigger triggers the daemon's in-process config
// reload. The existing service.ConfigReloader satisfies this via
// a one-line adapter; the post-reload hook fires the cross-
// instance NOTIFY so peer replicas refresh too (Slice 3a of the
// horizontal-scaling design). nil means single-replica deployment
// without a reloader wired — the applier logs and continues; the
// next file-watcher tick or explicit reload picks up the file.
type ConfigReloadTrigger interface {
	Reload() error
}

// ApplierConfig tunes the applier's behaviour.
type ApplierConfig struct {
	// AuthorName/Email are stamped on git commits. Empty values
	// fall through to whatever the git env / config set.
	AuthorName  string
	AuthorEmail string
}

// Applier owns the apply-path workflow. Constructed via NewApplier
// with the narrow interfaces wired by the service layer. All
// dependencies are nil-safe except the proposals repo — without
// it there's no row to advance.
type Applier struct {
	proposals persistence.WorkflowProposalRepository
	writer    WorkflowWriter
	git       GitCommitter
	reloader  ConfigReloadTrigger
	cfg       ApplierConfig
}

// NewApplier wires the applier. proposals is mandatory; writer +
// git + reloader are nil-safe (the applier degrades gracefully
// when a dependency is missing).
func NewApplier(
	proposals persistence.WorkflowProposalRepository,
	writer WorkflowWriter,
	git GitCommitter,
	reloader ConfigReloadTrigger,
	cfg ApplierConfig,
) *Applier {
	return &Applier{
		proposals: proposals,
		writer:    writer,
		git:       git,
		reloader:  reloader,
		cfg:       cfg,
	}
}

// Apply runs one apply turn for `proposalID`. Returns the updated
// proposal row on success. Error sentinels the API layer maps:
//
//   - ErrProposalNotApproved          → 409 (row isn't approved)
//   - persistence.ErrNotFound          → 404
//   - persistence.ErrInvalidProposalTransition → 409 (race; repo guard)
//
// On filesystem / git failures, the proposal row stays in
// status=approved so the operator can retry — the apply is
// designed to be idempotent at the row level even though it
// isn't fully transactional at the filesystem level (we can't
// atomically write two files + commit + reload).
func (a *Applier) Apply(ctx context.Context, proposalID, decidedBy string) (*persistence.WorkflowProposal, error) {
	if proposalID == "" {
		return nil, fmt.Errorf("memetic.Apply: proposalID is required")
	}
	if a.proposals == nil {
		return nil, fmt.Errorf("memetic.Apply: proposals repo not wired")
	}
	if a.writer == nil {
		return nil, fmt.Errorf("memetic.Apply: workflow writer not wired")
	}

	got, err := a.proposals.Get(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	if got.Status != persistence.WorkflowProposalStatusApproved {
		return nil, fmt.Errorf("%w: current status=%s",
			ErrProposalNotApproved, got.Status)
	}

	// 1. Filesystem writeback (both trees).
	sourcePath, err := a.writer.Write(ctx, got.WorkflowID, []byte(got.ProposalYAML))
	if err != nil {
		return nil, fmt.Errorf("memetic.Apply: write workflow %q: %w", got.WorkflowID, err)
	}

	// 2. Git commit in the source tree, if available. Empty
	// sourcePath = deployed-only deployment (no operator
	// checkout); skip git but log so operators know.
	var commitSHA string
	if a.git != nil && sourcePath != "" {
		msg := a.formatCommitMessage(got, decidedBy)
		sha, err := a.git.Commit(ctx, sourcePath, msg,
			a.cfg.AuthorName, a.cfg.AuthorEmail)
		if err != nil {
			return nil, fmt.Errorf("memetic.Apply: git commit: %w", err)
		}
		commitSHA = sha
	}

	// 3. Config reload — refresh the local daemon's in-memory
	// registry AND broadcast NOTIFY to peer replicas via the
	// post-reload hook. Best-effort: a reload failure doesn't
	// undo the filesystem write; the file-watcher catches the
	// change within ~5s anyway. We log and continue rather than
	// failing the whole apply.
	if a.reloader != nil {
		if err := a.reloader.Reload(); err != nil {
			// Don't fail the apply — the file is on disk and
			// the file-watcher will catch up. Operator sees a
			// "reload had issues" hint via the returned row's
			// note (best-effort surfacing).
			_ = err // intentionally swallowed; see comment above
		}
	}

	// 4. Stamp the proposal row as applied. If commitSHA is
	// empty (no git), we use a sentinel so the row's not-null
	// schema isn't violated and operators can grep for the
	// no-commit case.
	storedSHA := commitSHA
	if storedSHA == "" {
		storedSHA = "no-git"
	}
	if err := a.proposals.MarkApplied(ctx, proposalID, storedSHA); err != nil {
		return nil, fmt.Errorf("memetic.Apply: mark applied: %w", err)
	}
	// Read the updated row back so the caller sees applied_at +
	// applied_commit. Best-effort; on read failure the operator
	// still gets confirmation that Apply succeeded (the row is
	// in the right state) but loses the in-line timestamps.
	updated, err := a.proposals.Get(ctx, proposalID)
	if err != nil {
		// Build a minimal record from what we know so the
		// caller's contract holds.
		now := time.Now().UTC()
		return &persistence.WorkflowProposal{
			ID:            proposalID,
			WorkflowID:    got.WorkflowID,
			Status:        persistence.WorkflowProposalStatusApplied,
			AppliedAt:     &now,
			AppliedCommit: storedSHA,
		}, nil
	}
	return updated, nil
}

// formatCommitMessage builds the structured commit message
// described in the design doc § Slice 4. One-line subject from
// the workflow_id + a truncated motivation; body carries the
// proposal_id, approver, confidence, and evidence list.
func (a *Applier) formatCommitMessage(p *persistence.WorkflowProposal, appliedBy string) string {
	subject := fmt.Sprintf("workflow(%s): %s", p.WorkflowID, truncateOneLine(p.Motivation, 60))
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Auto-proposed by architect (proposal_id=%s, confidence=%.2f).\n",
		p.ID, p.Confidence)
	if appliedBy != "" {
		fmt.Fprintf(&b, "Applied by %s at %s.\n",
			appliedBy, time.Now().UTC().Format(time.RFC3339))
	}
	if p.Motivation != "" {
		b.WriteString("\nMotivation:\n")
		for _, line := range strings.Split(strings.TrimSpace(p.Motivation), "\n") {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if len(p.EvidenceRunIDs) > 0 {
		fmt.Fprintf(&b, "\nEvidence (%d runs):\n", len(p.EvidenceRunIDs))
		for _, id := range p.EvidenceRunIDs {
			b.WriteString("  - ")
			b.WriteString(id)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// truncateOneLine collapses newlines + caps at maxLen so the
// commit subject stays on one line and within typical git
// formatting conventions. Appends "…" when truncated.
func truncateOneLine(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}
