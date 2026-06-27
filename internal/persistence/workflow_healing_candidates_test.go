package persistence

import "testing"

// TestHealingCandidateStatus_IsTerminal — only promoted and rejected
// are settled. The trial states (draft / trial_running / trial_passed
// / trial_failed) all permit further action (re-trial, promote,
// reject) so they are non-terminal.
func TestHealingCandidateStatus_IsTerminal(t *testing.T) {
	cases := []struct {
		s    HealingCandidateStatus
		want bool
	}{
		{HealingCandidateDraft, false},
		{HealingCandidateTrialRunning, false},
		{HealingCandidateTrialPassed, false},
		{HealingCandidateTrialFailed, false},
		{HealingCandidateRejected, true},
		{HealingCandidatePromoted, true},
		{HealingCandidateStatus("bogus"), false},
	}
	for _, c := range cases {
		if got := c.s.IsTerminal(); got != c.want {
			t.Errorf("%q.IsTerminal() = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestHealingTrialVerdict_IsTerminal — pending is the only
// non-terminal verdict; passed/failed/inconclusive/errored are all
// settled outcomes the trial runner stamps via Finish.
func TestHealingTrialVerdict_IsTerminal(t *testing.T) {
	cases := []struct {
		v    HealingTrialVerdict
		want bool
	}{
		{HealingTrialPending, false},
		{HealingTrialPassed, true},
		{HealingTrialFailed, true},
		{HealingTrialInconclusive, true},
		{HealingTrialErrored, true},
		{HealingTrialVerdict("bogus"), false},
	}
	for _, c := range cases {
		if got := c.v.IsTerminal(); got != c.want {
			t.Errorf("%q.IsTerminal() = %v, want %v", c.v, got, c.want)
		}
	}
}
