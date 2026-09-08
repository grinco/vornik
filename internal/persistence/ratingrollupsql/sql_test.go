package ratingrollupsql

import "strings"

import "testing"

// $10 must not be mangled by the rewrite of $1. The query has two placeholders
// today; the loop is written highest-first so adding an eleventh does not
// silently corrupt the text.
func TestToQuestionMarks_RewritesHighestFirst(t *testing.T) {
	got := ToQuestionMarks("a=$1 b=$10 c=$2", 10)
	if got != "a=? b=? c=?" {
		t.Fatalf("got %q", got)
	}
}

func TestToQuestionMarks_LeavesTheQueryOtherwiseIntact(t *testing.T) {
	got := ToNumberedPlaceholders(SkillArms, 3)
	if strings.Contains(got, "$") {
		t.Error("a $N placeholder survived the rewrite")
	}
	if !strings.Contains(got, "execution_injected_skills") {
		t.Error("the rewrite damaged the query body")
	}
}

// SkillArms references its sha filter twice, so it must be rewritten with
// NUMBERED placeholders: the positional form would turn one argument into two
// and shift every later parameter, which is how this first failed.
func TestSkillArmsNeedsNumberedPlaceholders(t *testing.T) {
	if strings.Count(SkillArms, "$3") != 2 {
		t.Fatal("SkillArms no longer references $3 twice; re-check whether the " +
			"numbered rewrite is still required")
	}
	got := ToNumberedPlaceholders(SkillArms, 3)
	if strings.Count(got, "?3") != 2 {
		t.Error("both references to the sha filter must survive as ?3, so one " +
			"argument serves both")
	}
	if strings.Contains(got, "$") {
		t.Error("a $N placeholder survived the rewrite")
	}
}
