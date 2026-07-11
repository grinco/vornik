package projectdoctor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// fakeComposerRecovery is a scripted ComposerRecovery test double.
type fakeComposerRecovery struct {
	found  bool
	detail string
}

func (f fakeComposerRecovery) LeftoverJournal(_ string) (bool, string) {
	return f.found, f.detail
}

// TestCheckComposerCommit_NilChecker_Neutral: no ComposerRecovery
// wired (minimal/CE deployment, or a container that never resolved a
// live config dir) degrades to neutral — never red, never blocking
// project completeness.
func TestCheckComposerCommit_NilChecker_Neutral(t *testing.T) {
	d := New(Deps{})
	res := d.checkComposerCommit(&registry.Project{ID: "p"})
	if res.Status != StatusNeutral {
		t.Errorf("status = %q, want neutral", res.Status)
	}
	if res.Required {
		t.Error("composer_commit must never be Required")
	}
}

// TestCheckComposerCommit_Found_Yellow: a leftover journal is
// surfaced as a yellow heads-up (never red — it self-heals on the
// next restart) with the checker's detail carried through verbatim.
func TestCheckComposerCommit_Found_Yellow(t *testing.T) {
	d := New(Deps{ComposerRecovery: fakeComposerRecovery{found: true, detail: "stuck commit for p"}})
	res := d.checkComposerCommit(&registry.Project{ID: "p"})
	if res.Status != StatusYellow {
		t.Errorf("status = %q, want yellow", res.Status)
	}
	if res.Detail != "stuck commit for p" {
		t.Errorf("detail = %q, want the checker's detail verbatim", res.Detail)
	}
	if res.Required {
		t.Error("composer_commit must never be Required (a leftover journal self-heals)")
	}
}

// TestCheckComposerCommit_NotFound_Green: a clean tree reports green.
func TestCheckComposerCommit_NotFound_Green(t *testing.T) {
	d := New(Deps{ComposerRecovery: fakeComposerRecovery{found: false}})
	res := d.checkComposerCommit(&registry.Project{ID: "p"})
	if res.Status != StatusGreen {
		t.Errorf("status = %q, want green", res.Status)
	}
}

// TestCheckComposerCommit_RunOne_Wired confirms the "composer_commit"
// key is reachable through RunOne (the per-check re-run endpoint), not
// just the aggregate Run().
func TestCheckComposerCommit_RunOne_Wired(t *testing.T) {
	proj := &registry.Project{ID: "p"}
	d := New(Deps{
		Registry:         fakeResolver{proj: proj},
		ComposerRecovery: fakeComposerRecovery{found: true, detail: "x"},
	})
	res, err := d.RunOne(context.Background(), "p", "composer_commit")
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.Key != "composer_commit" || res.Status != StatusYellow {
		t.Errorf("unexpected result: %+v", res)
	}
}
