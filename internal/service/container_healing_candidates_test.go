package service

// Unit tests for the Self-Healing Workflow Genome v1 service-layer
// adapters (Unit 5). The load-bearing logic here is the sentinel-error
// translation (workflowhealing → api) the handlers depend on for their
// HTTP-status mapping, plus the nil-constructor guards.

import (
	"errors"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/ui"
	"vornik.io/vornik/internal/workflowhealing"
)

func TestMapHealingError_Translation(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"not-found", workflowhealing.ErrCandidateNotFound, api.ErrHealingCandidateNotFound},
		{"terminal", workflowhealing.ErrCandidateTerminal, api.ErrHealingCandidateTerminal},
		{"not-promotable", workflowhealing.ErrCandidateNotPromotable, api.ErrHealingCandidateNotPromotable},
		{"bad-mode", workflowhealing.ErrUnsupportedMode, api.ErrHealingTrialMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapHealingError(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("errors.Is mismatch: got %v want sentinel %v", got, tc.want)
			}
		})
	}
}

// TestMapHealingError_WrappedSentinel: the runner wraps its sentinels with
// %w, so the adapter must still match through the wrap and preserve the
// original message for the operator.
func TestMapHealingError_WrappedSentinel(t *testing.T) {
	orig := fmt.Errorf("%w: cand-9 (status=promoted)", workflowhealing.ErrCandidateTerminal)
	got := mapHealingError(orig)
	if !errors.Is(got, api.ErrHealingCandidateTerminal) {
		t.Fatalf("expected api.ErrHealingCandidateTerminal, got %v", got)
	}
	if got.Error() != orig.Error() {
		t.Errorf("message not preserved: got %q want %q", got.Error(), orig.Error())
	}
}

// TestMapHealingError_UnknownPassthrough: an unrecognised error passes
// through verbatim (the handler maps it to 500).
func TestMapHealingError_UnknownPassthrough(t *testing.T) {
	boom := errors.New("db exploded")
	got := mapHealingError(boom)
	if got != boom {
		t.Fatalf("expected passthrough, got %v", got)
	}
	for _, sentinel := range []error{
		api.ErrHealingCandidateNotFound,
		api.ErrHealingCandidateTerminal,
		api.ErrHealingCandidateNotPromotable,
		api.ErrHealingTrialMode,
	} {
		if errors.Is(got, sentinel) {
			t.Fatalf("unknown error should not match %v", sentinel)
		}
	}
}

func TestNewHealingAdapters_NilGuards(t *testing.T) {
	if r := newHealingTrialRunnerAdapter(nil); r != nil {
		t.Errorf("nil runner must yield nil adapter, got %v", r)
	}
	if p := newHealingPromoterAdapter(nil); p != nil {
		t.Errorf("nil promoter must yield nil adapter, got %v", p)
	}
	if r := newHealingTrialRunnerUIAdapter(nil); r != nil {
		t.Errorf("nil runner must yield nil ui adapter, got %v", r)
	}
	if p := newHealingPromoterUIAdapter(nil); p != nil {
		t.Errorf("nil promoter must yield nil ui adapter, got %v", p)
	}
}

// TestMapHealingErrorUI_Translation mirrors the api translation test for
// the ui sentinel set (Unit 6 — the /ui/admin/blackbox/candidates seam).
func TestMapHealingErrorUI_Translation(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"not-found", workflowhealing.ErrCandidateNotFound, ui.ErrUICandidateNotFound},
		{"terminal", workflowhealing.ErrCandidateTerminal, ui.ErrUICandidateTerminal},
		{"not-promotable", workflowhealing.ErrCandidateNotPromotable, ui.ErrUICandidateNotPromotable},
		{"bad-mode", workflowhealing.ErrUnsupportedMode, ui.ErrUITrialMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapHealingErrorUI(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("errors.Is mismatch: got %v want sentinel %v", got, tc.want)
			}
		})
	}
}

// TestMapHealingErrorUI_WrappedAndUnknown: matches through a %w wrap and
// passes unknown errors through verbatim.
func TestMapHealingErrorUI_WrappedAndUnknown(t *testing.T) {
	wrapped := fmt.Errorf("%w: cand-1", workflowhealing.ErrCandidateNotPromotable)
	if !errors.Is(mapHealingErrorUI(wrapped), ui.ErrUICandidateNotPromotable) {
		t.Errorf("wrapped sentinel not matched through ui translation")
	}
	boom := errors.New("db exploded")
	if got := mapHealingErrorUI(boom); got != boom {
		t.Errorf("unknown error must pass through, got %v", got)
	}
}

// TestHealingWrappedError_Unwrap: the wrapper exposes the sentinel via
// Unwrap so errors.Is chains work even without the explicit Is method.
func TestHealingWrappedError_Unwrap(t *testing.T) {
	e := wrapHealingSentinel(errors.New("boom"), api.ErrHealingCandidateNotFound)
	if unwrapped := errors.Unwrap(e); unwrapped != api.ErrHealingCandidateNotFound {
		t.Fatalf("Unwrap: got %v", unwrapped)
	}
}
