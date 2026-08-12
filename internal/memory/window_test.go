package memory

import (
	"testing"
	"time"
)

// Slice 0b of 2026-08-10-memory-benchmark-harness-design.md §4.2.
//
// ParseWindow turns a temporal expression in a query into a date window so
// recall can bound on it. Pure function, no LLM, and `now` is a parameter
// rather than time.Now() so every case below is deterministic.

// refNow is the anchor for every relative case: a Wednesday in August 2026.
var refNow = time.Date(2026, 8, 12, 15, 4, 5, 0, time.UTC)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// endOf returns the last instant of the given day, matching ParseWindow's
// inclusive upper bound.
func endOf(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 23, 59, 59, 999999999, time.UTC)
}

func TestParseWindow(t *testing.T) {
	cases := []struct {
		name  string
		query string
		from  time.Time
		to    time.Time
	}{
		// Absolute years.
		{"bare year", "what did we decide in 2023?", day(2023, 1, 1), endOf(2023, 12, 31)},
		{"year with noise", "2019 postmortems", day(2019, 1, 1), endOf(2019, 12, 31)},

		// Absolute months.
		{"month and year", "March 2024 incidents", day(2024, 3, 1), endOf(2024, 3, 31)},
		{"abbreviated month", "sep 2025 release notes", day(2025, 9, 1), endOf(2025, 9, 30)},
		{"february leap year", "february 2024", day(2024, 2, 1), endOf(2024, 2, 29)},
		{"february non-leap", "february 2023", day(2023, 2, 1), endOf(2023, 2, 28)},

		// Quarters.
		{"quarter", "Q2 2024 roadmap", day(2024, 4, 1), endOf(2024, 6, 30)},
		{"q4", "q4 2023", day(2023, 10, 1), endOf(2023, 12, 31)},

		// Seasons — northern hemisphere, documented in ParseWindow.
		{"last spring", "what did we ship last spring?", day(2026, 3, 1), endOf(2026, 5, 31)},
		{"last winter", "last winter outage", day(2025, 12, 1), endOf(2026, 2, 28)},

		// Relative to refNow (2026-08-12).
		{"last year", "last year's decisions", day(2025, 1, 1), endOf(2025, 12, 31)},
		{"last month", "last month's changes", day(2026, 7, 1), endOf(2026, 7, 31)},
		{"months ago", "three months ago", day(2026, 5, 1), endOf(2026, 5, 31)},
		{"this year", "this year so far", day(2026, 1, 1), endOf(2026, 12, 31)},
		{"yesterday", "what broke yesterday", day(2026, 8, 11), endOf(2026, 8, 11)},
		{"today", "today's deploys", day(2026, 8, 12), endOf(2026, 8, 12)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := ParseWindow(tc.query, refNow)
			if !ok {
				t.Fatalf("ParseWindow(%q) found no window; want %v..%v", tc.query, tc.from, tc.to)
			}
			if !from.Equal(tc.from) {
				t.Errorf("from = %v, want %v", from, tc.from)
			}
			if !to.Equal(tc.to) {
				t.Errorf("to = %v, want %v", to, tc.to)
			}
		})
	}
}

// TestParseWindow_NoTemporalExpression is the case that must NOT fire. A false
// positive here silently narrows an ordinary query to a window the user never
// asked for, which is worse than not parsing at all: results vanish and nothing
// explains why.
func TestParseWindow_NoTemporalExpression(t *testing.T) {
	for _, q := range []string{
		"how does the scheduler lease tasks?",
		"why is the reranker disabled",
		"", // empty
		"error 10243 fractional shares",
		// Bare numbers that are not plausible years must not be read as one.
		"chunk 512 overlap 64",
		"migration 157",
		"port 8080 conflict",
	} {
		if from, to, ok := ParseWindow(q, refNow); ok {
			t.Errorf("ParseWindow(%q) wrongly found window %v..%v", q, from, to)
		}
	}
}

// TestParseWindow_ImplausibleYearsRejected — a four-digit number is only a year
// if it could plausibly be one. Without this, "migration 1234" or a port number
// becomes a date filter.
func TestParseWindow_ImplausibleYearsRejected(t *testing.T) {
	for _, q := range []string{"issue 1200", "count 3000", "id 1099"} {
		if _, _, ok := ParseWindow(q, refNow); ok {
			t.Errorf("ParseWindow(%q) treated an implausible number as a year", q)
		}
	}
}

// TestParseWindow_WindowIsOrdered — from must never exceed to, whatever the
// expression. An inverted window matches nothing and would look like an empty
// corpus rather than a parse bug.
func TestParseWindow_WindowIsOrdered(t *testing.T) {
	queries := []string{
		"in 2023", "March 2024", "Q3 2022", "last spring", "last winter",
		"last year", "last month", "three months ago", "yesterday", "today",
		"this year",
	}
	for _, q := range queries {
		from, to, ok := ParseWindow(q, refNow)
		if !ok {
			t.Fatalf("ParseWindow(%q) unexpectedly found nothing", q)
		}
		if from.After(to) {
			t.Errorf("ParseWindow(%q) inverted: from %v after to %v", q, from, to)
		}
	}
}

// The remaining ParseWindow shapes, split out so the main table stays readable.
func TestParseWindow_RelativeUnits(t *testing.T) {
	cases := []struct {
		name  string
		query string
		from  time.Time
		to    time.Time
	}{
		{"this month", "this month's incidents", day(2026, 8, 1), endOf(2026, 8, 31)},
		{"last week", "what landed last week", day(2026, 8, 5), endOf(2026, 8, 11)},
		{"days ago", "5 days ago", day(2026, 8, 7), endOf(2026, 8, 7)},
		{"one week ago", "one week ago", day(2026, 8, 5), endOf(2026, 8, 11)},
		{"two years ago", "two years ago", day(2024, 1, 1), endOf(2024, 12, 31)},
		// 2026 is NOT a leap year — February ends on the 28th.
		{"numeric months ago", "6 months ago", day(2026, 2, 1), endOf(2026, 2, 28)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := ParseWindow(tc.query, refNow)
			if !ok {
				t.Fatalf("ParseWindow(%q) found nothing", tc.query)
			}
			if !from.Equal(tc.from) || !to.Equal(tc.to) {
				t.Errorf("ParseWindow(%q) = %v..%v, want %v..%v",
					tc.query, from, to, tc.from, tc.to)
			}
		})
	}
}

// Seasons other than the two in the main table, plus the "this <season>" form,
// which resolves to the current year rather than stepping back.
func TestParseWindow_Seasons(t *testing.T) {
	cases := []struct {
		name  string
		query string
		from  time.Time
		to    time.Time
	}{
		{"this summer", "this summer's work", day(2026, 6, 1), endOf(2026, 8, 31)},
		// refNow is mid-August, so summer 2026 has NOT ended yet and "last
		// summer" means 2025 — see seasonYears.
		{"last summer", "last summer", day(2025, 6, 1), endOf(2025, 8, 31)},
		{"last autumn", "last autumn", day(2025, 9, 1), endOf(2025, 11, 30)},
		{"last fall", "last fall", day(2025, 9, 1), endOf(2025, 11, 30)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := ParseWindow(tc.query, refNow)
			if !ok {
				t.Fatalf("ParseWindow(%q) found nothing", tc.query)
			}
			if !from.Equal(tc.from) || !to.Equal(tc.to) {
				t.Errorf("ParseWindow(%q) = %v..%v, want %v..%v",
					tc.query, from, to, tc.from, tc.to)
			}
		})
	}
}

// TestParseWindow_LastSeasonBeforeItEnds — "last spring" asked in February means
// LAST year's spring, because this year's hasn't happened yet. Getting this
// wrong returns a window in the future.
func TestParseWindow_LastSeasonBeforeItEnds(t *testing.T) {
	feb := time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)
	from, to, ok := ParseWindow("last spring", feb)
	if !ok {
		t.Fatal("ParseWindow found nothing")
	}
	if !from.Equal(day(2025, 3, 1)) || !to.Equal(endOf(2025, 5, 31)) {
		t.Errorf("last spring in Feb 2026 = %v..%v, want spring 2025", from, to)
	}
	if from.After(feb) {
		t.Error("resolved a window in the future")
	}
}

// TestParseWindow_ImplausibleYearInQualifiedForm — a quarter or month paired with
// an implausible year must not produce a window from the qualified form.
func TestParseWindow_ImplausibleYearInQualifiedForm(t *testing.T) {
	for _, q := range []string{"q2 1200", "march 1099"} {
		if _, _, ok := ParseWindow(q, refNow); ok {
			t.Errorf("ParseWindow(%q) accepted an implausible year", q)
		}
	}
}

// TestParseWindow_UnknownWordYearIsStillAYear — "<not-a-month> <year>" must fall
// through to the bare-year rule rather than being rejected outright.
func TestParseWindow_UnknownWordYearIsStillAYear(t *testing.T) {
	from, to, ok := ParseWindow("release 2023 retrospective", refNow)
	if !ok {
		t.Fatal("ParseWindow found nothing for a non-month word before a year")
	}
	if !from.Equal(day(2023, 1, 1)) || !to.Equal(endOf(2023, 12, 31)) {
		t.Errorf("got %v..%v, want all of 2023", from, to)
	}
}

// TestMonthRange_RejectsInvertedRange — a defensive branch: no caller currently
// produces a range whose start month follows its end within one year, but a
// future season or quarter table could, and silently returning an inverted
// window would match nothing while looking like an empty corpus.
func TestMonthRange_RejectsInvertedRange(t *testing.T) {
	if _, _, ok := monthRange(2024, time.December, 2024, time.January); ok {
		t.Error("monthRange(Dec..Jan of the same year) reported ok; want rejected")
	}
}
