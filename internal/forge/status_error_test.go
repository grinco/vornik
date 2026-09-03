package forge

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// A forge returned every non-200 as one flat fmt.Errorf, so a 404 (the PR does
// not exist) was indistinguishable from a 502 (GitHub is having a moment). The
// task-level retry then spent its whole budget on failures that could not
// succeed — three attempts in five seconds against
// grinco/headmatch#999999 on 2026-09-01.
//
// Design https://docs.vornik.io

func TestStatusPermanence_ClassifiesByStatus(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
		why       string
	}{
		{http.StatusNotFound, true, "the target does not exist; a second look cannot change that"},
		{http.StatusGone, true, "explicitly and permanently removed"},
		{http.StatusUnauthorized, true, "bad app credentials — token() refreshes proactively, so this is not an expired cache"},
		{http.StatusForbidden, true, "access revoked (absent rate-limit headers — see the split test)"},
		{http.StatusUnprocessableEntity, true, "a malformed request is deterministic"},
		{http.StatusTooManyRequests, false, "the rate limiter is telling us to come back"},
		{http.StatusInternalServerError, false, "GitHub's problem, usually brief"},
		{http.StatusBadGateway, false, "transient"},
		{http.StatusServiceUnavailable, false, "transient"},
		{http.StatusGatewayTimeout, false, "transient"},
		// The safe default: anything unreasoned-about retries exactly as today,
		// so this change can only ever REMOVE futile retries, never add a new
		// way to lose work.
		{http.StatusTeapot, false, "unknown status defaults to transient"},
		{451, false, "unknown status defaults to transient"},
		{http.StatusOK, false, "not a failure at all"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			got := StatusIsPermanent(tc.status, nil)
			if got != tc.permanent {
				t.Errorf("StatusIsPermanent(%d) = %v, want %v — %s",
					tc.status, got, tc.permanent, tc.why)
			}
		})
	}
}

// TestStatusPermanence_403SplitsOnRateLimitHeaders — the one judgement call in
// the table.
//
// GitHub uses 403 for both "you may not do this" and, historically, for
// SECONDARY rate limits. The modern secondary-rate-limit response carries
// Retry-After or x-ratelimit-remaining: 0. Both directions are asserted: a
// one-sided test would pass with the logic exactly backwards.
func TestStatusPermanence_403SplitsOnRateLimitHeaders(t *testing.T) {
	cases := []struct {
		name      string
		header    http.Header
		permanent bool
	}{
		{"bare 403 is permanent", http.Header{}, true},
		{"nil headers are permanent", nil, true},
		{"Retry-After means rate limited", http.Header{"Retry-After": []string{"60"}}, false},
		{"exhausted rate limit", http.Header{"X-Ratelimit-Remaining": []string{"0"}}, false},
		{"remaining budget is NOT a rate limit", http.Header{"X-Ratelimit-Remaining": []string{"4999"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusIsPermanent(http.StatusForbidden, tc.header); got != tc.permanent {
				t.Errorf("403 with %v: permanent = %v, want %v", tc.header, got, tc.permanent)
			}
		})
	}
}

// TestPermanentError_UnwrapsAndReports — mirrors PushRejectedError's shape so
// the package has one way of doing this, not two.
func TestPermanentError_UnwrapsAndReports(t *testing.T) {
	inner := errors.New("underlying")
	err := &PermanentError{Status: http.StatusNotFound, Op: "fetch diff", Detail: "Not Found", Err: inner}

	if !errors.Is(err, inner) {
		t.Error("PermanentError must unwrap to its cause")
	}
	pe, ok := AsPermanent(fmt.Errorf("wrapped: %w", err))
	if !ok {
		t.Fatal("AsPermanent must see through a wrapping error")
	}
	if pe.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", pe.Status)
	}
	if got := err.Error(); got == "" {
		t.Error("Error() must not be empty")
	}
}

// TestAsPermanent_IsFalseForOrdinaryErrors — the guard that keeps the terminal
// path narrow. A plain error must never read as permanent, or an unrelated
// failure would stop retrying.
func TestAsPermanent_IsFalseForOrdinaryErrors(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("boom"),
		fmt.Errorf("forge/github: fetch diff HTTP 404: not found"), // the OLD flat shape
		&PushRejectedError{Branch: "x", Kind: PushRejectionPermission},
	} {
		if _, ok := AsPermanent(err); ok {
			t.Errorf("AsPermanent(%v) = true; only a typed *PermanentError may report permanent — "+
				"text-matching a flattened message is exactly what this replaces", err)
		}
	}
}
