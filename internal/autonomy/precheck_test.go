package autonomy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// timeAt builds a time anchored in America/New_York so test
// expectations don't drift with the host's local timezone.
func timeAt(t *testing.T, year int, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}
	return time.Date(year, month, day, hour, minute, 0, 0, tz)
}

// TestIsUSMarketHoliday_Coverage — pin the curated set so a
// future calendar update doesn't silently flip a holiday into
// a "trading day" classification or vice versa. List sourced
// from NYSE 2026-2027 holiday calendar.
func TestIsUSMarketHoliday_Coverage(t *testing.T) {
	cases := []struct {
		name string
		date time.Time
		want bool
	}{
		// 2026 holidays (full close).
		{"new years 2026", timeAt(t, 2026, 1, 1, 12, 0), true}, // Thursday Jan 1
		{"mlk day 2026", timeAt(t, 2026, 1, 19, 12, 0), true},  // 3rd Mon Jan
		{"presidents day 2026", timeAt(t, 2026, 2, 16, 12, 0), true},
		{"good friday 2026", timeAt(t, 2026, 4, 3, 12, 0), true}, // Easter Sunday 2026 = Apr 5; Good Fri = Apr 3
		{"memorial day 2026", timeAt(t, 2026, 5, 25, 12, 0), true},
		{"juneteenth 2026 fri", timeAt(t, 2026, 6, 19, 12, 0), true},
		{"july 4 2026 sat → observed jul 3 fri", timeAt(t, 2026, 7, 3, 12, 0), true},
		{"labor day 2026", timeAt(t, 2026, 9, 7, 12, 0), true},
		{"thanksgiving 2026", timeAt(t, 2026, 11, 26, 12, 0), true},
		{"christmas 2026 fri", timeAt(t, 2026, 12, 25, 12, 0), true},

		// Non-holidays sanity-check.
		{"normal monday april 2026", timeAt(t, 2026, 4, 13, 12, 0), false},
		{"day before mlk 2026", timeAt(t, 2026, 1, 16, 12, 0), false},
		{"day after thanksgiving 2026 (half day, NOT closed)", timeAt(t, 2026, 11, 27, 12, 0), false},
		{"easter monday 2026 (not a market holiday)", timeAt(t, 2026, 4, 6, 12, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isUSMarketHoliday(tc.date)
			assert.Equal(t, tc.want, got, "date %s", tc.date.Format("2006-01-02 Mon"))
		})
	}
}

// TestEasterFriday_KnownDates — Good Friday is the canary for
// the Anonymous Gregorian algorithm. Pin a few years.
func TestEasterFriday_KnownDates(t *testing.T) {
	tz, _ := time.LoadLocation("America/New_York")
	cases := []struct {
		year  int
		month time.Month
		day   int
	}{
		{2026, 4, 3},  // Easter Sunday Apr 5 → Good Fri Apr 3
		{2025, 4, 18}, // Easter Sunday Apr 20
		{2024, 3, 29}, // Easter Sunday Mar 31
		{2027, 3, 26}, // Easter Sunday Mar 28
	}
	for _, tc := range cases {
		t.Run("good friday", func(t *testing.T) {
			got := easterFriday(tc.year, tz)
			want := time.Date(tc.year, tc.month, tc.day, 0, 0, 0, 0, tz)
			assert.Equal(t, want, got, "year %d", tc.year)
		})
	}
}

// TestBrokerReachable_DetectsErrorFieldsInBody — pins the
// 2026-05-06 audit fix: a /caps response that returns HTTP 200
// but carries a `*_error` field with non-empty content means the
// broker MCP's "front door" is up but the IBKR pipeline behind
// it is broken. Pre-fix the precheck only looked at status code
// and let 6 trading ticks fire against a degraded pipeline; one
// of them produced a fabricated TSM @ $150 trade proposal
// (TSM trades at ~$395) — see the audit notes for full context.
func TestBrokerReachable_DetectsErrorFieldsInBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "clean caps body — reachable",
			body: `{"configured":{"mode":"paper"},"portfolio":{"account":"DUH"}}`,
			want: true,
		},
		{
			name: "portfolio_error present — NOT reachable",
			body: `{"configured":{"mode":"paper"},"portfolio":null,"portfolio_error":"sidecar request failed: dial tcp ...: connect: connection refused"}`,
			want: false,
		},
		{
			name: "future error field (any *_error key) — NOT reachable",
			body: `{"configured":{"mode":"paper"},"feed_error":"streaming feed disconnected"}`,
			want: false,
		},
		{
			name: "empty error string — reachable (lenient on transient nulls)",
			body: `{"portfolio_error":""}`,
			want: true,
		},
		{
			name: "non-string error value — reachable (defensive parse)",
			body: `{"portfolio_error":null}`,
			want: true,
		},
		{
			name: "malformed JSON body — reachable (lenient on shape changes)",
			body: `{not-json`,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got := brokerReachable(context.Background(), srv.URL)
			assert.Equal(t, tc.want, got, "body: %s", tc.body)
		})
	}
}

// TestBrokerReachable_StatusGate — pre-existing 5xx / connection-
// refused behaviour preserved. A non-2xx status overrides the
// body inspection: there's no body to trust.
func TestBrokerReachable_StatusGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"portfolio":{"account":"DUH"}}`))
	}))
	defer srv.Close()
	assert.False(t, brokerReachable(context.Background(), srv.URL))
}
