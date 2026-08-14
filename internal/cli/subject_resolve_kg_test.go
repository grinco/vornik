package cli

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// The command's contract is mostly its OUTPUT: an operator decides from the
// preview whether an entity is really this person, and the wrong decision on an
// erasure request destroys a third party's data. These pin the parts of that
// output a refactor could quietly drop.

func TestSubjectResolveKGHelp_SaysNothingIsWrittenWithoutAnEntity(t *testing.T) {
	long := subjectResolveKGCmd.Long
	for _, want := range []string{"PREVIEW", "Nothing is written", "--entity"} {
		if !strings.Contains(long, want) {
			t.Errorf("help does not mention %q — an operator could believe the preview binds", want)
		}
	}
	if !strings.Contains(long, "possible") {
		t.Error("help does not state the confidence ceiling; a `possible` link must not read as a fact")
	}
}

// --adopt is a subject-deleting flag. The help has to distinguish the case it
// covers (a placeholder) from the case it must never cover (another identified
// person), or an operator will reach for it on a conflict.
func TestSubjectResolveKGHelp_BoundsAdopt(t *testing.T) {
	long := subjectResolveKGCmd.Long
	if !strings.Contains(long, "kg:<entity-id>") {
		t.Error("help does not name the placeholder shape --adopt applies to")
	}
	if !strings.Contains(long, "does not override") {
		t.Error("help does not say --adopt cannot resolve a conflict between two identified people")
	}
}

func TestDescribeKGState_DistinguishesEveryOutcome(t *testing.T) {
	cases := []struct {
		name string
		cand datasubject.KGCandidate
		want string
	}{
		{"free", datasubject.KGCandidate{State: datasubject.KGUnbound}, "free"},
		{"already here", datasubject.KGCandidate{State: datasubject.KGBoundHere}, "already bound here"},
		{
			"placeholder",
			datasubject.KGCandidate{State: datasubject.KGBoundToPlaceholder, BoundSubjectID: "ds_ph"},
			"--adopt",
		},
		{
			"conflict",
			datasubject.KGCandidate{
				State: datasubject.KGConflict, BoundSubjectID: "ds_x", BoundSubjectName: "Jane D.",
			},
			"resolve by hand",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeKGState(tc.cand)
			if !strings.Contains(got, tc.want) {
				t.Errorf("describeKGState = %q, want it to mention %q", got, tc.want)
			}
		})
	}
	// A conflict must NAME the other subject: "held by someone else" leaves the
	// operator with nothing to act on.
	conflict := describeKGState(datasubject.KGCandidate{
		State: datasubject.KGConflict, BoundSubjectID: "ds_x", BoundSubjectName: "Jane D.",
	})
	if !strings.Contains(conflict, "ds_x") || !strings.Contains(conflict, "Jane D.") {
		t.Errorf("conflict state %q does not identify the other subject", conflict)
	}
}

func TestAnyPlaceholderAndAnyConflict(t *testing.T) {
	none := []datasubject.KGCandidate{{State: datasubject.KGUnbound}, {State: datasubject.KGBoundHere}}
	if anyPlaceholder(none) || anyConflict(none) {
		t.Error("clean candidate set reported a placeholder or conflict")
	}
	mixed := []datasubject.KGCandidate{
		{State: datasubject.KGUnbound},
		{State: datasubject.KGBoundHere},
		{State: datasubject.KGBoundToPlaceholder},
		{State: datasubject.KGConflict},
	}
	if !anyPlaceholder(mixed) || !anyConflict(mixed) {
		t.Error("mixed candidate set failed to report a placeholder or conflict")
	}
}

func TestTruncateName_KeepsShortNamesIntact(t *testing.T) {
	if got := truncateName("Jane Doe"); got != "Jane Doe" {
		t.Errorf("truncateName mangled a short name: %q", got)
	}
	long := strings.Repeat("x", 100)
	got := truncateName(long)
	if len([]rune(got)) > 48 {
		t.Errorf("truncateName returned %d runes, want <= 48", len([]rune(got)))
	}
}
