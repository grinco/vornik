package trading

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func etTime(t *testing.T, year int, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	tz, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	return time.Date(year, month, day, hour, minute, 0, 0, tz)
}

// Ported verbatim from internal/autonomy when the calendar moved here
// (2026-09-10) so the autonomy pre-check and the analysis-evidence gate
// share one holiday list. NYSE 2026-2027 calendar.
func TestIsUSMarketHoliday_Coverage(t *testing.T) {
	cases := []struct {
		name string
		date time.Time
		want bool
	}{
		{"new years 2026", etTime(t, 2026, 1, 1, 12, 0), true},
		{"mlk day 2026", etTime(t, 2026, 1, 19, 12, 0), true},
		{"presidents day 2026", etTime(t, 2026, 2, 16, 12, 0), true},
		{"good friday 2026", etTime(t, 2026, 4, 3, 12, 0), true},
		{"memorial day 2026", etTime(t, 2026, 5, 25, 12, 0), true},
		{"juneteenth 2026 fri", etTime(t, 2026, 6, 19, 12, 0), true},
		{"july 4 2026 sat observed jul 3", etTime(t, 2026, 7, 3, 12, 0), true},
		{"labor day 2026", etTime(t, 2026, 9, 7, 12, 0), true},
		{"thanksgiving 2026", etTime(t, 2026, 11, 26, 12, 0), true},
		{"christmas 2026 fri", etTime(t, 2026, 12, 25, 12, 0), true},
		{"normal monday april 2026", etTime(t, 2026, 4, 13, 12, 0), false},
		{"day before mlk 2026", etTime(t, 2026, 1, 16, 12, 0), false},
		{"day after thanksgiving 2026 half day open", etTime(t, 2026, 11, 27, 12, 0), false},
		{"easter monday 2026", etTime(t, 2026, 4, 6, 12, 0), false},
		{"good friday 2027", etTime(t, 2027, 3, 26, 12, 0), true},
		{"new years 2027 fri", etTime(t, 2027, 1, 1, 12, 0), true},
		{"christmas 2027 sat observed fri dec 24", etTime(t, 2027, 12, 24, 12, 0), true},
		{"dec 31 2027 fri stays open", etTime(t, 2027, 12, 31, 12, 0), false},
		// NYSE exception: a Saturday Jan 1 is not observed on the prior Friday.
		{"dec 31 2032 fri before saturday jan 1 stays open", etTime(t, 2032, 12, 31, 12, 0), false},
		{"jan 1 2033 saturday", etTime(t, 2033, 1, 1, 12, 0), false},
		{"jan 3 2033 monday is a normal session", etTime(t, 2033, 1, 3, 12, 0), false},
		// A Sunday Jan 1 is observed on Monday Jan 2.
		{"jan 2 2034 monday observed", etTime(t, 2034, 1, 2, 12, 0), true},
		{"july 4 2027 sunday observed mon jul 5", etTime(t, 2027, 7, 5, 12, 0), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsUSMarketHoliday(tc.date), tc.date.Format("2006-01-02 Mon"))
		})
	}
}

func TestEasterFriday_KnownDates(t *testing.T) {
	tz, _ := time.LoadLocation("America/New_York")
	for _, tc := range []struct {
		year  int
		month time.Month
		day   int
	}{{2026, 4, 3}, {2025, 4, 18}, {2024, 3, 29}, {2027, 3, 26}} {
		assert.Equal(t, time.Date(tc.year, tc.month, tc.day, 0, 0, 0, 0, tz), easterFriday(tc.year, tz), "year %d", tc.year)
	}
}

// The analysis-evidence gate only examines ticks the trading-rth pre-check
// would have let through, so both must agree on the same instants.
func TestUSRegularSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     time.Time
		open   bool
		reason string
	}{
		{"mid session", etTime(t, 2026, 9, 10, 10, 12), true, ""},
		{"at the open", etTime(t, 2026, 9, 10, 9, 30), true, ""},
		{"at the close", etTime(t, 2026, 9, 10, 16, 0), false, "post-market"},
		{"pre-market", etTime(t, 2026, 9, 10, 9, 29), false, "pre-market"},
		{"saturday", etTime(t, 2026, 9, 12, 12, 0), false, "weekend"},
		{"labor day", etTime(t, 2026, 9, 7, 12, 0), false, "holiday"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open, reason := USRegularSession(tc.at)
			assert.Equal(t, tc.open, open)
			if tc.reason != "" {
				assert.Contains(t, reason, tc.reason)
			}
		})
	}
	// A UTC instant is converted, not trusted as-is.
	open, _ := USRegularSession(time.Date(2026, 9, 10, 14, 12, 0, 0, time.UTC))
	assert.True(t, open)
}
