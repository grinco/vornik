package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectwizard"
)

// scaffoldProposer routes a project-wizard commit through the control-plane
// proposal ledger instead of writing files directly. The wizard's composed
// file set becomes a DRAFT `scaffold` proposal carrying a create-ops bundle;
// an operator reviews the diff, approves, and the shipped Phase-2b apply
// engine creates the files atomically (with rollback). This makes a
// wizard-generated project reviewable-as-a-diff and rollbackable rather than
// direct-committed. See https://docs.vornik.io
// wizard-scaffold-reroute-design.md.
type scaffoldProposer struct {
	proposals persistence.ProposalRepository
	// configDir is the apply engine's root — the directory holding
	// config.yaml (filepath.Dir(ConfigPath)). Apply-op paths are resolved
	// under it.
	configDir string
	// configsDir is the registry root the wizard's file keys are relative to
	// (typically <configDir>/configs). The offset between the two is
	// prepended to each key so an op path resolves correctly under configDir.
	configsDir string
}

// scaffoldOp mirrors controlplane.applyFileOp's JSON shape (that type is
// unexported); the apply engine unmarshals ApplyOps into its own struct.
type scaffoldOp struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// newScaffoldProposer returns a proposer, or nil when the proposal ledger or
// config roots aren't wired (the wizard then falls back to direct-write).
func newScaffoldProposer(proposals persistence.ProposalRepository, configPath, configsDir string) *scaffoldProposer {
	if proposals == nil || configPath == "" || configsDir == "" {
		return nil
	}
	return &scaffoldProposer{
		proposals:  proposals,
		configDir:  filepath.Dir(configPath),
		configsDir: configsDir,
	}
}

// ProposeScaffold implements projectwizard.ScaffoldProposer.
func (p *scaffoldProposer) ProposeScaffold(ctx context.Context, projectID string, files map[string]string) (string, string, error) {
	if len(files) == 0 {
		return "", "", fmt.Errorf("scaffold: no files to propose")
	}
	// Offset from the apply engine's configDir to the registry configsDir the
	// file keys are relative to (usually "configs", or "." when they coincide).
	rel, err := filepath.Rel(p.configDir, p.configsDir)
	if err != nil {
		return "", "", fmt.Errorf("scaffold: resolve config offset: %w", err)
	}

	// Deterministic op order (also what the operator sees in the diff).
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	base := filepath.Clean(p.configDir)
	ops := make([]scaffoldOp, 0, len(files))
	var diff strings.Builder
	for _, key := range keys {
		opPath := filepath.ToSlash(filepath.Join(rel, key))
		target := filepath.Clean(filepath.Join(base, filepath.FromSlash(opPath)))
		// Defense-in-depth path containment (review finding #7): a composed
		// file key must resolve strictly under the config dir. The apply
		// engine's resolveTarget guards this too, but rejecting a traversal
		// key here means a bad key never becomes a filed proposal. Keys are
		// internally rendered (not raw user input), so this should never fire
		// — it's a belt-and-suspenders guard.
		if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
			return "", "", fmt.Errorf("scaffold: file key %q escapes the config dir", key)
		}
		// Pre-flight collision check so a duplicate project id fails at propose
		// time with the same "already exists" signal the direct-write path
		// gives. NOTE: this is a propose-time snapshot — the file could still
		// be created by another actor between here and apply (a benign TOCTOU
		// window across the human review delay). That's why it is a
		// double-check: the apply engine independently re-checks and refuses
		// with ErrScaffoldConflict, so the worst case is a stale DRAFT the
		// operator rejects — never a corrupt/overwritten file.
		if _, statErr := os.Stat(target); statErr == nil {
			return "", "", fmt.Errorf("scaffold: target %s already exists (project id in use)", opPath)
		}
		ops = append(ops, scaffoldOp{Op: "create", Path: opPath, Content: files[key]})
		fmt.Fprintf(&diff, "+++ %s (new file, %d bytes)\n", opPath, len(files[key]))
	}

	applyOps, err := json.Marshal(ops)
	if err != nil {
		return "", "", fmt.Errorf("scaffold: marshal apply_ops: %w", err)
	}

	proposal := &persistence.ControlPlaneProposal{
		ID:          persistence.GenerateID("cpp"),
		Kind:        persistence.ProposalKindScaffold,
		BlastRadius: persistence.ProposalScopeProject,
		ProjectID:   projectID,
		Title:       fmt.Sprintf("Create project %q (wizard scaffold)", projectID),
		Diff:        diff.String(),
		Rationale: fmt.Sprintf(
			"Project %q composed by the creation wizard. Review the %d new file(s), then approve + apply to create the project (gated, atomic, rollbackable).",
			projectID, len(ops)),
		Status: persistence.ProposalStatusDraft,
		// Reserved system principal — server-stamped here, never taken from
		// request input; the propose API rejects reserved tokens from bodies.
		// The acting human admin approves (approver != proposer), so the
		// self-approval gate holds.
		ProposedBy: "operator-ui",
		ApplyOps:   string(applyOps),
	}
	if err := p.proposals.Create(ctx, proposal); err != nil {
		return "", "", fmt.Errorf("scaffold: file proposal: %w", err)
	}
	// Deep-link to the DRAFT proposals list (the freshly-filed one is newest).
	url := controlPlaneProposalsURLForReview
	return proposal.ID, url, nil
}

// controlPlaneProposalsURLForReview is where the commit redirect lands so the
// operator can review + apply the just-filed scaffold proposal.
const controlPlaneProposalsURLForReview = "/ui/admin/control-plane?section=proposals&status=DRAFT&source=operator-ui"

// compile-time assertion the proposer satisfies the wizard seam.
var _ projectwizard.ScaffoldProposer = (*scaffoldProposer)(nil)
