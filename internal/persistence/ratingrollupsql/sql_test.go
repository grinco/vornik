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
	got := ToQuestionMarks(SkillArms, 2)
	if strings.Contains(got, "$") {
		t.Error("a $N placeholder survived the rewrite")
	}
	if !strings.Contains(got, "execution_injected_skills") {
		t.Error("the rewrite damaged the query body")
	}
}
