package controlplane

import "testing"

// Config-apply-journal design §9.2c step 2. WorkspaceContextApplier.targetPath
// refused any op whose path was not the canonical context path FOR THAT
// PROJECT. Moving the write onto the file apply path must carry that rule
// across — without it a workspace_context proposal could write anywhere under
// the workspace root, which is a real widening and not a refactor.

func TestKindPathPolicy_AcceptsTheCanonicalPath(t *testing.T) {
	err := CheckKindPathPolicy("workspace_context", "proj-1",
		[]string{"workspace/proj-1/.autonomy/PROJECT_CONTEXT.md"})
	if err != nil {
		t.Fatalf("the canonical path was refused: %v", err)
	}
}

func TestKindPathPolicy_RefusesAnotherProjectsContext(t *testing.T) {
	err := CheckKindPathPolicy("workspace_context", "proj-1",
		[]string{"workspace/proj-2/.autonomy/PROJECT_CONTEXT.md"})
	if err == nil {
		t.Fatal("a proposal for proj-1 was allowed to write proj-2's context")
	}
}

func TestKindPathPolicy_RefusesAnyOtherFileUnderTheRoot(t *testing.T) {
	for _, p := range []string{
		"workspace/proj-1/.autonomy/OTHER.md",
		"workspace/proj-1/src/main.go",
		"workspace/proj-1/.ssh/authorized_keys",
		"configs/projects/proj-1.yaml",
	} {
		if err := CheckKindPathPolicy("workspace_context", "proj-1", []string{p}); err == nil {
			t.Fatalf("workspace_context was allowed to write %q", p)
		}
	}
}

// Exactly one op. The applier decoded exactly one and refused otherwise; a set
// of ops would let one canonical path carry another file alongside it.
func TestKindPathPolicy_RefusesMoreThanOneOp(t *testing.T) {
	err := CheckKindPathPolicy("workspace_context", "proj-1", []string{
		"workspace/proj-1/.autonomy/PROJECT_CONTEXT.md",
		"workspace/proj-1/.autonomy/PROJECT_CONTEXT.md",
	})
	if err == nil {
		t.Fatal("two ops were accepted for a kind that writes exactly one file")
	}
}

// A kind with no policy is unconstrained here — the file apply path's own
// containment and class gates still apply. This function adds a rule for kinds
// that need one; it is not a second authorisation layer for everything.
func TestKindPathPolicy_UnpolicedKindIsUntouched(t *testing.T) {
	if err := CheckKindPathPolicy("config_change", "proj-1", []string{"configs/swarms/x.md"}); err != nil {
		t.Fatalf("an unpoliced kind was refused: %v", err)
	}
}

// An empty project id cannot be checked against, so it is refused rather than
// silently matching whatever the path says.
func TestKindPathPolicy_EmptyProjectIsRefused(t *testing.T) {
	if err := CheckKindPathPolicy("workspace_context", "", []string{"workspace/p/.autonomy/PROJECT_CONTEXT.md"}); err == nil {
		t.Fatal("a workspace_context proposal with no project id was accepted")
	}
}
