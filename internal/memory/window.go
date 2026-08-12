package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Slice 0b of https://docs.vornik.io
// §4.2 — turning a temporal expression in a query into a date window.
//
// Deliberately a pure function with `now` as a parameter: relative expressions
// ("last spring") are meaningless without an anchor, and taking it as an
// argument rather than calling time.Now() internally is what makes the whole
// surface table-testable.
//
// No LLM. A model call here would cost a round-trip on the interactive recall
// path to parse a handful of shapes that regexes cover, and would make the
// result non-deterministic run to run.

// Year bounds for treating a bare four-digit number as a year. A query
// mentioning "migration 157", "port 8080" or "issue 1200" must not become a
// date filter — a false positive silently narrows an ordinary search to a
// window the user never asked for, results vanish, and nothing explains why.
// That failure is worse than not parsing at all, so the range is tight and
// bare numbers outside it are left alone.
const (
	minPlausibleYear = 1990
	maxPlausibleYear = 2100
)

// seasonMonths maps a season name to its (start, end) month, northern
// hemisphere. A southern-hemisphere operator would want these inverted; the
// corpus this serves is northern, and guessing from a query is not possible, so
// the convention is fixed and documented rather than configurable.
var seasonMonths = map[string][2]time.Month{
	"winter": {time.December, time.February},
	"spring": {time.March, time.May},
	"summer": {time.June, time.August},
	"autumn": {time.September, time.November},
	"fall":   {time.September, time.November},
}

var monthNames = map[string]time.Month{
	"january": time.January, "jan": time.January,
	"february": time.February, "feb": time.February,
	"march": time.March, "mar": time.March,
	"april": time.April, "apr": time.April,
	"may":  time.May,
	"june": time.June, "jun": time.June,
	"july": time.July, "jul": time.July,
	"august": time.August, "aug": time.August,
	"september": time.September, "sep": time.September, "sept": time.September,
	"october": time.October, "oct": time.October,
	"november": time.November, "nov": time.November,
	"december": time.December, "dec": time.December,
}

var (
	yearRE     = regexp.MustCompile(`\b(\d{4})\b`)
	quarterRE  = regexp.MustCompile(`\bq([1-4])\s+(\d{4})\b`)
	monthYRE   = regexp.MustCompile(`\b([a-z]+)\s+(\d{4})\b`)
	agoRE      = regexp.MustCompile(`\b(\d+|one|two|three|four|five|six|seven|eight|nine|ten)\s+(day|week|month|year)s?\s+ago\b`)
	numberWord = map[string]int{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
		"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	}
)

// ParseWindow extracts an inclusive [from, to] window from a natural-language
// query, anchored on now. ok is false when the query carries no temporal
// expression, which is the common case and must stay cheap.
//
// Recognised shapes, most specific first:
//
//	Q<n> <year>              quarter
//	<month> <year>           calendar month
//	<year>                   calendar year
//	last|this <season>       northern-hemisphere season
//	last|this year|month|week
//	<n> day|week|month|years ago
//	today | yesterday
//
// Bounds are whole days: from is 00:00:00 of the first day, to is the last
// nanosecond of the last day, so a chunk stamped at any time on a boundary day
// is inside the window. Callers pass these straight into
// SearchOptions.FromDate/ToDate, which bound COALESCE(event_time, created_at).
func ParseWindow(query string, now time.Time) (from, to time.Time, ok bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return time.Time{}, time.Time{}, false
	}
	now = now.UTC()

	// Quarter before month-year, because "q2 2024" also matches the bare-year
	// rule and the quarter is the more specific reading.
	if m := quarterRE.FindStringSubmatch(q); m != nil {
		qn, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		if plausibleYear(y) {
			startMonth := time.Month((qn-1)*3 + 1)
			return monthRange(y, startMonth, y, startMonth+2)
		}
	}

	// "<month> <year>" before bare year, same reasoning.
	if m := monthYRE.FindStringSubmatch(q); m != nil {
		if mon, found := monthNames[m[1]]; found {
			if y, err := strconv.Atoi(m[2]); err == nil && plausibleYear(y) {
				return monthRange(y, mon, y, mon)
			}
		}
	}

	// Seasons. "last winter" spans a year boundary, which is why the range
	// helper takes an explicit year for each end rather than assuming one.
	for name, months := range seasonMonths {
		if !strings.Contains(q, name) {
			continue
		}
		startY, endY := seasonYears(months, q, now)
		return monthRange(startY, months[0], endY, months[1])
	}

	// Relative calendar units.
	switch {
	case strings.Contains(q, "last year"):
		return yearRange(now.Year() - 1)
	case strings.Contains(q, "this year"):
		return yearRange(now.Year())
	case strings.Contains(q, "last month"):
		t := now.AddDate(0, -1, 0)
		return monthRange(t.Year(), t.Month(), t.Year(), t.Month())
	case strings.Contains(q, "this month"):
		return monthRange(now.Year(), now.Month(), now.Year(), now.Month())
	case strings.Contains(q, "last week"):
		// The seven days ending yesterday. Calendar-week semantics (Mon-Sun)
		// would be defensible too, but "last week" in a question about work is
		// nearly always "the recent past", and a rolling window never produces
		// the surprise of an empty result on a Monday morning.
		end := startOfDay(now.AddDate(0, 0, -1))
		start := startOfDay(now.AddDate(0, 0, -7))
		return start, endOfDay(end), true
	case strings.Contains(q, "yesterday"):
		d := startOfDay(now.AddDate(0, 0, -1))
		return d, endOfDay(d), true
	case strings.Contains(q, "today"):
		d := startOfDay(now)
		return d, endOfDay(d), true
	}

	// "<n> <unit> ago".
	if m := agoRE.FindStringSubmatch(q); m != nil {
		n := parseCount(m[1])
		if n > 0 {
			switch m[2] {
			case "day":
				d := startOfDay(now.AddDate(0, 0, -n))
				return d, endOfDay(d), true
			case "week":
				end := startOfDay(now.AddDate(0, 0, -7*n+6))
				start := startOfDay(now.AddDate(0, 0, -7*n))
				return start, endOfDay(end), true
			case "month":
				t := now.AddDate(0, -n, 0)
				return monthRange(t.Year(), t.Month(), t.Year(), t.Month())
			case "year":
				return yearRange(now.Year() - n)
			}
		}
	}

	// Bare year, last because it is the weakest signal.
	if m := yearRE.FindStringSubmatch(q); m != nil {
		if y, err := strconv.Atoi(m[1]); err == nil && plausibleYear(y) {
			return yearRange(y)
		}
	}

	return time.Time{}, time.Time{}, false
}

// seasonYears resolves which year(s) a season expression refers to. "last
// spring" means the most recent spring that has already ENDED — if we are in
// August 2026, that is spring 2026; if we are in February 2026, spring 2026
// hasn't happened, so it means spring 2025.
func seasonYears(months [2]time.Month, q string, now time.Time) (startY, endY int) {
	y := now.Year()
	wrapsYear := months[0] > months[1] // winter: Dec → Feb

	if strings.Contains(q, "last") {
		// Step back a year when this year's season has not finished yet.
		if wrapsYear {
			// Winter Dec(y-1)..Feb(y) has ended once we're past February.
			if now.Month() <= months[1] {
				y--
			}
		} else if now.Month() <= months[1] {
			y--
		}
	}
	if wrapsYear {
		return y - 1, y
	}
	return y, y
}

func plausibleYear(y int) bool { return y >= minPlausibleYear && y <= maxPlausibleYear }

func parseCount(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return numberWord[s]
}

// monthRange builds an inclusive window from the first day of (startY,
// startMonth) to the last day of (endY, endMonth). Taking a year for each end
// is what lets winter span a year boundary without a special case.
func monthRange(startY int, startMonth time.Month, endY int, endMonth time.Month) (time.Time, time.Time, bool) {
	from := time.Date(startY, startMonth, 1, 0, 0, 0, 0, time.UTC)
	// Day 0 of the following month is the last day of this one, which handles
	// February in leap years without a table.
	to := time.Date(endY, endMonth+1, 0, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
	if from.After(to) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func yearRange(y int) (time.Time, time.Time, bool) {
	return monthRange(y, time.January, y, time.December)
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func endOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
}

// resolveQueryWindow applies the ParseQueryWindow opt-in, returning a copy of
// opts with derived bounds when — and only when — the caller asked for it and
// supplied no bounds of its own.
//
// Split out from SearchWithOptions so the precedence rule is testable without a
// database, an embedder, or a searcher: the interesting behaviour here is
// entirely about which of two sources of truth wins.
func resolveQueryWindow(query string, opts SearchOptions, now time.Time) SearchOptions {
	if !opts.ParseQueryWindow {
		return opts
	}
	// Either bound being set means the caller expressed an intent. Filling in
	// the other half from prose would produce a window they never asked for.
	if !opts.FromDate.IsZero() || !opts.ToDate.IsZero() {
		return opts
	}
	from, to, ok := ParseWindow(query, now)
	if !ok {
		return opts
	}
	opts.FromDate = from
	opts.ToDate = to
	return opts
}
