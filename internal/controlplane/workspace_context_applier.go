package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

// WorkspaceContextApplier applies the config assistant's virtual
// projects/<id>/PROJECT_CONTEXT.md proposals to the project's canonical
// workspace .autonomy/PROJECT_CONTEXT.md file.
type WorkspaceContextApplier struct {
	WorkspaceRoot string
	// ConfigDir roots the non-workspace paths in a proposal's read set, so
	// every dependency the proposal was grounded on is re-checked before the
	// write — not just the target file (audit 2026-09-15 CA-06). Empty
	// disables the extra checks, which is the pre-audit behaviour and is
	// only correct when the proposal has no other dependencies.
	ConfigDir string
	// PreImage makes the rollback record DURABLE before anything is
	// mutated. It receives the proposal id and the snapshot JSON, and any
	// error aborts the apply untouched.
	//
	// Ordering is the entire contract. The engine persists the snapshot
	// Apply RETURNS, in MarkApplied, which happens after the file has
	// already changed: a crash or a ledger failure in that window left the
	// live context rewritten, the proposal still APPROVED, and no durable
	// pre-image — after which retry failed the stale-base check and
	// rollback had nothing to restore from. Nil keeps the old ordering and
	// is only for callers that have no ledger.
	PreImage func(ctx context.Context, proposalID, snapshot string) error
	// LeaderGate is the config-writer lease, consulted before the write.
	// Nil proceeds (single-process deployments).
	LeaderGate any
}

// Kind implements KindApplier.
func (a *WorkspaceContextApplier) Kind() string { return persistence.ProposalKindWorkspaceContext }

// Apply implements KindApplier: it rewrites the project's canonical
// workspace PROJECT_CONTEXT.md after a read-set hash check, and returns the
// prior content as the pre-apply snapshot.
func (a *WorkspaceContextApplier) Apply(ctx context.Context, p *persistence.ControlPlaneProposal) (string, error) {
	op, err := workspaceContextOp(p)
	if err != nil {
		return "", err
	}
	target, err := a.targetPath(p.ProjectID, op.Path)
	if err != nil {
		return "", err
	}
	pre, existed, err := readOptional(target)
	if err != nil {
		return "", err
	}
	wantHash, ok, err := workspaceContextReadHash(p, op.Path)
	if err != nil {
		return "", err
	}
	if !ok || wantHash != contentHashState(pre, existed) {
		return "", ErrStaleBase
	}
	switch op.Op {
	case applyOpCreate:
		if existed {
			return "", ErrScaffoldConflict
		}
	case applyOpReplace:
		if !existed {
			return "", ErrScaffoldConflict
		}
	default:
		return "", fmt.Errorf("workspace_context: unsupported op %q", op.Op)
	}
	// Every OTHER dependency the proposal was grounded on is re-checked
	// too. Checking only the target meant a proposal computed against a
	// project config that had since changed still applied cleanly (audit
	// 2026-09-15 CA-06).
	if err := a.checkReadSetDependencies(p, op.Path); err != nil {
		return "", err
	}
	// One fenced writer (journal LLD §1.2): a process-local mutex does not
	// cover a second daemon or a manual writer.
	if ok, reason := leaderelection.DangerousWriteAllowed(ctx, a.LeaderGate); !ok {
		return "", fmt.Errorf("workspace_context: refusing to write: %s", reason)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	snap, _ := json.Marshal(workspaceContextSnapshot{
		Path: op.Path, Existed: existed, Content: string(pre), AppliedHash: hashString(op.Content),
	})
	// DURABLE PRE-IMAGE FIRST, then the mutation. Nothing below this line
	// may run until the record that undoes it is committed.
	if a.PreImage != nil {
		if err := a.PreImage(ctx, p.ID, string(snap)); err != nil {
			return "", fmt.Errorf("workspace_context: refusing to write without a durable pre-image: %w", err)
		}
	}
	// atomicWrite is temp-write → fsync → rename → parent-dir fsync. The
	// bare os.WriteFile it replaces was not crash-durable and could leave
	// the context file truncated by a partial write.
	if err := atomicWrite(target, []byte(op.Content)); err != nil {
		return "", err
	}
	return string(snap), nil
}

// checkReadSetDependencies re-verifies every non-target entry of the
// proposal's read set against the deployed tree. Returns ErrStaleBase when
// any of them has changed since the proposal was built.
func (a *WorkspaceContextApplier) checkReadSetDependencies(p *persistence.ControlPlaneProposal, targetPath string) error {
	if a.ConfigDir == "" {
		return nil
	}
	var ev struct {
		ReadSet map[string]string `json:"read_set"`
	}
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil {
		return fmt.Errorf("workspace_context: bad evidence: %w", err)
	}
	paths := make([]string, 0, len(ev.ReadSet))
	for rel := range ev.ReadSet {
		if rel != targetPath {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	for _, rel := range paths {
		// Resolve by ROOT PREFIX. A read set can name paths in either tree —
		// the target itself is workspace-rooted — and joining every
		// non-target entry under ConfigDir would resolve a workspace path to
		// the wrong file, hash the wrong bytes and refuse a legitimate apply
		// as stale (companion review-20260915-10cd F5). Fail-closed, but
		// wrong, and wrong-but-safe still blocks the operator.
		full, err := a.resolveReadSetPath(rel)
		if err != nil {
			return fmt.Errorf("workspace_context: read-set path %q: %w", rel, err)
		}
		data, existed, rerr := readOptional(full)
		if rerr != nil {
			return rerr
		}
		if contentHashState(data, existed) != ev.ReadSet[rel] {
			return fmt.Errorf("%w: %s changed since the proposal was built", ErrStaleBase, rel)
		}
	}
	return nil
}

// Rollback implements KindApplier: it restores the pre-apply content, and
// refuses when the file has drifted since the apply.
func (a *WorkspaceContextApplier) Rollback(ctx context.Context, p *persistence.ControlPlaneProposal) error {
	op, err := workspaceContextOp(p)
	if err != nil {
		return err
	}
	var snap workspaceContextSnapshot
	if err := json.Unmarshal([]byte(p.PreApplySnapshot), &snap); err != nil || snap.Path == "" {
		return fmt.Errorf("workspace_context rollback: bad pre_apply_snapshot")
	}
	if snap.Path != op.Path {
		return fmt.Errorf("workspace_context rollback refused: snapshot path %q does not match proposal path %q", snap.Path, op.Path)
	}
	target, err := a.targetPath(p.ProjectID, op.Path)
	if err != nil {
		return err
	}
	cur, existed, err := readOptional(target)
	if err != nil {
		return err
	}
	if !existed || workspaceContextHashBytes(cur) != snap.AppliedHash {
		return ErrRollbackTargetDrifted
	}
	// A rollback WRITES, so it takes the writer fence exactly as Apply does.
	// It was exempt, which meant a superseded process could reverse content
	// the current writer now owns — the more dangerous direction, because it
	// restores bytes that are old by definition (re-audit 2026-09-15, CA-06
	// and CA-22).
	if ok, reason := leaderelection.DangerousWriteAllowed(ctx, a.LeaderGate); !ok {
		return fmt.Errorf("workspace_context: refusing to roll back: %s", reason)
	}
	if !snap.Existed {
		return os.Remove(target)
	}
	return atomicWrite(target, []byte(snap.Content))
}

func (a *WorkspaceContextApplier) targetPath(projectID, opPath string) (string, error) {
	if a == nil || a.WorkspaceRoot == "" {
		return "", fmt.Errorf("workspace_context: workspace root is empty")
	}
	want := "workspace/" + projectID + "/.autonomy/PROJECT_CONTEXT.md"
	if opPath != want {
		return "", fmt.Errorf("workspace_context: path %q is not the canonical context path for project %q", opPath, projectID)
	}
	return safepath.JoinUnder(a.WorkspaceRoot, projectID, ".autonomy", "PROJECT_CONTEXT.md")
}

func workspaceContextOp(p *persistence.ControlPlaneProposal) (applyFileOp, error) {
	if p == nil {
		return applyFileOp{}, fmt.Errorf("workspace_context: nil proposal")
	}
	var ops []applyFileOp
	if err := json.Unmarshal([]byte(p.ApplyOps), &ops); err != nil || len(ops) != 1 {
		return applyFileOp{}, fmt.Errorf("workspace_context: expected exactly one apply op")
	}
	return ops[0], nil
}

func workspaceContextReadHash(p *persistence.ControlPlaneProposal, path string) (string, bool, error) {
	var ev struct {
		ReadSet map[string]string `json:"read_set"`
	}
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil {
		return "", false, fmt.Errorf("workspace_context: bad evidence: %w", err)
	}
	h, ok := ev.ReadSet[path]
	return h, ok, nil
}

type workspaceContextSnapshot struct {
	Path        string `json:"path"`
	Existed     bool   `json:"existed"`
	Content     string `json:"content,omitempty"`
	AppliedHash string `json:"applied_hash"`
}

func readOptional(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func contentHashState(data []byte, existed bool) string {
	if !existed {
		return "ABSENT"
	}
	return hashBytes(data)
}

func hashString(s string) string { return workspaceContextHashBytes([]byte(s)) }

func workspaceContextHashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// StagedState implements StagedRecoverable: it compares the live context file
// against the staged snapshot so restart recovery can tell "the write landed
// and the ledger did not" from "nothing happened" and from a hand edit
// (re-audit 2026-09-15, CA-06).
func (a *WorkspaceContextApplier) StagedState(_ context.Context, p *persistence.ControlPlaneProposal) (string, error) {
	op, err := workspaceContextOp(p)
	if err != nil {
		return "", err
	}
	var snap workspaceContextSnapshot
	if err := json.Unmarshal([]byte(p.PreApplySnapshot), &snap); err != nil || snap.Path == "" {
		return "", fmt.Errorf("workspace_context: bad pre_apply_snapshot")
	}
	target, err := a.targetPath(p.ProjectID, op.Path)
	if err != nil {
		return "", err
	}
	cur, existed, err := readOptional(target)
	if err != nil {
		return "", err
	}
	switch {
	case existed && workspaceContextHashBytes(cur) == snap.AppliedHash:
		return StagedStateApplied, nil
	case contentHashState(cur, existed) == contentHashState([]byte(snap.Content), snap.Existed):
		return StagedStatePending, nil
	default:
		return StagedStateDrift, nil
	}
}

// resolveReadSetPath joins a read-set entry under the root its prefix names:
// `workspace/...` belongs to WorkspaceRoot, everything else to ConfigDir.
//
// The two trees are separate roots and a read set may reference both, so the
// prefix is the only thing that says which one an entry means.
func (a *WorkspaceContextApplier) resolveReadSetPath(rel string) (string, error) {
	const wsPrefix = "workspace/"
	if strings.HasPrefix(rel, wsPrefix) {
		if a.WorkspaceRoot == "" {
			return "", fmt.Errorf("workspace_context: no workspace root for %q", rel)
		}
		return safepath.JoinUnder(a.WorkspaceRoot, strings.TrimPrefix(rel, wsPrefix))
	}
	return safepath.JoinUnder(a.ConfigDir, rel)
}
