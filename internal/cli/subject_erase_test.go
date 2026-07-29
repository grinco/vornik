package cli

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// The --ground help is what an operator reads when they get the flag wrong, and
// it is the only place the discretion rule is stated at the point of decision.
// A ground missing from it is a ground nobody will pick.
func TestErasureGroundHelp_ListsEveryGroundWithItsArticle(t *testing.T) {
	help := erasureGroundHelp()
	for _, g := range []datasubject.ErasureGround{
		datasubject.GroundNoLongerNecessary,
		datasubject.GroundConsentWithdrawn,
		datasubject.GroundObjection,
		datasubject.GroundUnlawfulProcessing,
		datasubject.GroundLegalObligation,
		datasubject.GroundChildServices,
	} {
		if !strings.Contains(help, string(g)) {
			t.Errorf("help omits the ground value %q, so an operator cannot pass it", g)
		}
		if !strings.Contains(help, g.Article()) {
			t.Errorf("help omits %s for %q", g.Article(), g)
		}
	}
}

// The two discretion-removing grounds must be visibly flagged: choosing one
// destroys another person's data, and that consequence has to be on screen at the
// moment of choosing rather than discovered afterwards.
func TestErasureGroundHelp_FlagsTheDestructiveGrounds(t *testing.T) {
	for _, line := range strings.Split(erasureGroundHelp(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		destructive := strings.Contains(line, string(datasubject.GroundUnlawfulProcessing)) ||
			strings.Contains(line, string(datasubject.GroundLegalObligation))
		flagged := strings.Contains(line, "deleted in full")
		if destructive && !flagged {
			t.Errorf("a discretion-removing ground must be flagged as destructive: %q", line)
		}
		if !destructive && flagged {
			t.Errorf("a discretionary ground must NOT be flagged as deleting in full: %q", line)
		}
	}
}

// The long help documents the ground→treatment mapping an operator relies on.
// If the letters here drift from the code, the operator picks the wrong limb.
func TestSubjectEraseHelp_MatchesTheCodeLettering(t *testing.T) {
	long := subjectEraseCmd.Long
	for _, g := range []datasubject.ErasureGround{
		datasubject.GroundUnlawfulProcessing,
		datasubject.GroundLegalObligation,
	} {
		if !strings.Contains(long, g.Article()) {
			t.Errorf("help text omits %s", g.Article())
		}
	}
	// And it must say plainly that redaction is not yet implemented, so nobody
	// reads a deferred row as an erased one.
	if !strings.Contains(strings.ToLower(long), "deferred") {
		t.Error("help must state that shared records are deferred, not erased, in this slice")
	}
}
