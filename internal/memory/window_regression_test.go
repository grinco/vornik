package memory

import (
	"testing"
	"time"
)

func TestParseWindow_SeasonWordsNeedWholeQualifiedExpressions(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{"Springfield rollout", "offspring records", "spring tension", "last yearly report"} {
		if _, _, ok := ParseWindow(q, now); ok {
			t.Errorf("%q unexpectedly became a date filter", q)
		}
	}
}

func TestParseWindow_ThisWinterInDecemberStartsNow(t *testing.T) {
	now := time.Date(2026, time.December, 10, 12, 0, 0, 0, time.UTC)
	from, to, ok := ParseWindow("this winter", now)
	if !ok || from.Year() != 2026 || from.Month() != time.December ||
		to.Year() != 2027 || to.Month() != time.February {
		t.Fatalf("this winter = %v..%v ok=%v, want Dec 2026..Feb 2027", from, to, ok)
	}
}

func TestParseWindow_MultipleSeasonsUsesFirstMentionDeterministically(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		from, _, ok := ParseWindow("last spring compared with last summer", now)
		if !ok || from.Month() != time.March {
			t.Fatalf("iteration %d chose %v, want the first expression (spring)", i, from)
		}
	}
}

func TestParseWindow_RejectsRelativeDatesOutsideSupportedYears(t *testing.T) {
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	if _, _, ok := ParseWindow("999999999 months ago", now); ok {
		t.Fatal("an overflowing relative date must not create a nonsensical window")
	}
}
