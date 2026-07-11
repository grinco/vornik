package projectwizard

import (
	"strings"
	"testing"
)

func TestBuildComposerGrounding_RendersRoleLibraryAndStepVocabulary(t *testing.T) {
	out := BuildComposerGrounding(testArchetypes(), []string{"rag.extract", "forge.fetch_diff"})

	for _, want := range []string{
		"`researcher`", "`writer`",
		"file_read, file_write, memory_search", // researcher's tool allowlist, verbatim
		"Gathers information.",
		"`agent`", "`gate`", "`approval`", "`system`",
		"`rag.extract`", "`forge.fetch_diff`",
		"tier\":3",
		"call_project",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected composer grounding to contain %q, got:\n%s", want, out)
		}
	}
}

func TestBuildComposerGrounding_EmptyLibraryAndHandlers_StillRendersStepKinds(t *testing.T) {
	out := BuildComposerGrounding(nil, nil)
	if !strings.Contains(out, "(none available)") {
		t.Errorf("expected an explicit empty-library marker, got:\n%s", out)
	}
	if !strings.Contains(out, "(no system handlers registered on this daemon)") {
		t.Errorf("expected an explicit empty-handlers marker, got:\n%s", out)
	}
	// The three built-in step kinds still document themselves even with
	// no concrete system-handler list.
	for _, want := range []string{"`agent`", "`gate`", "`approval`", "`system`"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected step kind %q even with no handlers, got:\n%s", want, out)
		}
	}
}

func TestBuildComposerGrounding_ArchetypeOrderIsDeterministic(t *testing.T) {
	lib := testArchetypes() // keys "researcher", "writer" — map order is random
	first := BuildComposerGrounding(lib, nil)
	for i := 0; i < 5; i++ {
		if got := BuildComposerGrounding(lib, nil); got != first {
			t.Fatalf("composer grounding is not deterministic across calls:\n--- run 0 ---\n%s\n--- run %d ---\n%s", first, i+1, got)
		}
	}
	// researcher sorts before writer.
	if strings.Index(first, "`researcher`") > strings.Index(first, "`writer`") {
		t.Errorf("expected archetypes in ArchetypeID order (researcher before writer), got:\n%s", first)
	}
}
