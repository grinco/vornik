package trading

import (
	"fmt"
	"time"
)

// The US equity session calendar. Moved here from internal/autonomy on
// 2026-09-10 so the trading-rth autonomy pre-check and the executor's
// analysis-evidence gate read one clock: a tick the pre-check admits is a
// tick the gate examines, and nothing else is.

// USRegularSession reports whether the NYSE regular session is open at the
// given instant (converted to America/New_York), with the same weekday,
// holiday and 09:30-16:00 rules the pre-check applies. The reason names the
// closed state ("weekend", "holiday", "pre-market", "post-market") or the
// timezone failure; it is empty when open. Half-day early closes are not
// modelled, matching the pre-check.
func USRegularSession(at time.Time) (bool, string) {
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		return false, fmt.Sprintf("cannot load America/New_York timezone: %v", err)
	}
	now := at.In(tz)
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false, fmt.Sprintf("market closed (weekend, %s ET)", wd)
	}
	if IsUSMarketHoliday(now) {
		return false, fmt.Sprintf("market closed (US holiday %s)", now.Format("2006-01-02"))
	}
	open := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, tz)
	closeAt := time.Date(now.Year(), now.Month(), now.Day(), 16, 0, 0, 0, tz)
	if now.Before(open) {
		return false, fmt.Sprintf("pre-market (%s ET, opens at 09:30)", now.Format("15:04:05"))
	}
	if !now.Before(closeAt) {
		return false, fmt.Sprintf("post-market (%s ET, closed at 16:00)", now.Format("15:04:05"))
	}
	return true, ""
}

// IsUSMarketHoliday reports whether the given local date (in
// ET) is a full-close US equity market holiday. Mirrors the
// list the strategist uses in its operating-window prompt so
// the daemon-side gate and the agent-side fallback agree.
//
// The half-day closes (day after Thanksgiving, Christmas Eve)
// are deliberately omitted — markets are open 09:30-13:00, so
// the standard RTH check would refuse half the live session.
// The strategist's mid-execution check at 13:00 catches an
// edge tick scheduled too late.
//
// Easter-relative dates (Good Friday) are computed via the
// Anonymous Gregorian algorithm so the function stays
// table-free across years.
func IsUSMarketHoliday(t time.Time) bool {
	return isNewYearsObserved(t) ||
		isNthWeekday(t, time.January, time.Monday, 3) || // MLK Day
		isNthWeekday(t, time.February, time.Monday, 3) || // Presidents Day
		isGoodFriday(t) ||
		isLastMonday(t, time.May) || // Memorial Day
		isObservedFixed(t, time.June, 19) || // Juneteenth
		isObservedFixed(t, time.July, 4) || // Independence Day
		isNthWeekday(t, time.September, time.Monday, 1) || // Labor Day
		isNthWeekday(t, time.November, time.Thursday, 4) || // Thanksgiving
		isObservedFixed(t, time.December, 25) // Christmas
}

// isNewYearsObserved: Jan 1 on a weekday, or Monday Jan 2 when Jan 1 fell on
// a Sunday. A Saturday Jan 1 is NOT observed on the preceding Friday (NYSE
// rule: the prior year's Dec 31 stays open).
func isNewYearsObserved(t time.Time) bool {
	if t.Month() != time.January {
		return false
	}
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	case time.Monday:
		return t.Day() == 1 || (t.Day() == 2 && time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location()).Weekday() == time.Sunday)
	default:
		return t.Day() == 1
	}
}

// isObservedFixed: a fixed-date holiday on its weekday, on the Monday after a
// Sunday date, or on the Friday before a Saturday date. New Year's Day is
// NOT handled here: NYSE does not observe a Saturday Jan 1 on the prior
// Friday (Dec 31 stays open), so it has its own rule above.
func isObservedFixed(t time.Time, month time.Month, day int) bool {
	if t.Month() != month {
		return false
	}
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	case time.Monday:
		return t.Day() == day || t.Day() == day+1
	case time.Friday:
		return t.Day() == day || t.Day() == day-1
	default:
		return t.Day() == day
	}
}

// isNthWeekday: the n-th given weekday of the month.
func isNthWeekday(t time.Time, month time.Month, wd time.Weekday, n int) bool {
	if t.Month() != month || t.Weekday() != wd {
		return false
	}
	return (t.Day()-1)/7 == n-1
}

// isLastMonday: the last Monday of the month (Memorial Day).
func isLastMonday(t time.Time, month time.Month) bool {
	return t.Month() == month && t.Weekday() == time.Monday && t.Day() >= 25
}

func isGoodFriday(t time.Time) bool {
	y, m, d := easterFriday(t.Year(), t.Location()).Date()
	ty, tm, td := t.Date()
	return y == ty && m == tm && d == td
}

// easterFriday returns the Good Friday date for the given
// year, in the supplied location at midnight. Anonymous
// Gregorian algorithm — accurate for the Gregorian calendar
// (1583+).
func easterFriday(year int, loc *time.Location) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	easter := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
	return easter.AddDate(0, 0, -2) // Good Friday is 2 days before Easter Sunday.
}
