package controlplane

import (
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// CheckKindPathPolicy enforces the per-kind path rule that moving a write off
// the KindApplier seam would otherwise drop (config-apply-journal design
// §9.2c).
//
// WHY THIS EXISTS. `WorkspaceContextApplier.targetPath` refused any op whose
// path was not the canonical context path for that project. That refusal was
// the applier's, and the applier is going away — so without carrying the rule
// across, a `workspace_context` proposal could write ANY file under the
// workspace root. That is a widening, not a refactor, and it is the kind of
// thing a move like this loses silently.
//
// It is a policy for kinds that need one, not a second authorisation layer: a
// kind with no entry is unconstrained here and still passes the file apply
// path's own containment guard and class gates.
func CheckKindPathPolicy(kind, projectID string, paths []string) error {
	if kind != persistence.ProposalKindWorkspaceContext {
		return nil
	}
	if projectID == "" {
		return fmt.Errorf("%s: no project id, so the canonical context path cannot be checked", kind)
	}
	if len(paths) != 1 {
		// The applier decoded exactly one op and refused otherwise. A set
		// would let the canonical path carry another file alongside it.
		return fmt.Errorf("%s: expected exactly one apply op, got %d", kind, len(paths))
	}
	want := WorkspaceContextPath(projectID)
	if paths[0] != want {
		return fmt.Errorf("%s: path %q is not the canonical context path for project %q (%q)",
			kind, paths[0], projectID, want)
	}
	return nil
}

// WorkspaceRootName is the apply root the project workspace tree is registered
// under. It is the first segment of every workspace-rooted op path, which is
// what lets one resolver serve two trees (design §9.2b).
const WorkspaceRootName = "workspace"

// WorkspaceContextPath is the ONE virtual path a workspace_context proposal may
// write, rooted at the `workspace` named root.
//
// Exported and used by both the policy and the proposal builder, so the rule
// has one spelling — the alternative is a builder that constructs a path and a
// checker that reconstructs it, which is two implementations of one constant.
func WorkspaceContextPath(projectID string) string {
	return WorkspaceRootName + "/" + projectID + "/.autonomy/PROJECT_CONTEXT.md"
}
