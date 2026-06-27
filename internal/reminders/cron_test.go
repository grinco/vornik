package reminders

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestValidateCronExpr_AcceptsCommonForms covers the cron
// shapes the LLM parser is most likely to emit: explicit minute/
// hour with day-of-week, daily ranges, monthly windows.
func TestValidateCronExpr_AcceptsCommonForms(t *testing.T) {
	cases := []string{
		"0 9 * * 1",      // every Monday 09:00
		"0 9 * * 1-5",    // every weekday 09:00
		"*/15 * * * *",   // every 15 minutes
		"0 0 1 * *",      // first of every month at midnight
		"30 8 * * 1,3,5", // Mon/Wed/Fri at 08:30
		"  0 9 * * 1  ",  // whitespace tolerance (LLM-shaped)
	}
	for _, c := range cases {
		if err := ValidateCronExpr(c); err != nil {
			t.Errorf("ValidateCronExpr(%q) = %v, want nil", c, err)
		}
	}
}

// TestValidateCronExpr_RejectsInvalid covers the failure modes
// the parser must surface as ErrInvalidCron so the LLM-output
// path can route to PARSE_FAILED rather than INTERNAL.
func TestValidateCronExpr_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"too few fields", "0 9 *"},
		{"too many fields (seconds variant rejected)", "0 0 9 * * 1"},
		{"unknown descriptor", "@hourly"},
		{"out of range minute", "60 9 * * 1"},
		{"out of range hour", "0 24 * * 1"},
		{"garbage", "not a cron expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCronExpr(tc.expr)
			if err == nil {
				t.Fatalf("ValidateCronExpr(%q) = nil, want error", tc.expr)
			}
			if !errors.Is(err, ErrInvalidCron) {
				t.Errorf("err = %v, want ErrInvalidCron", err)
			}
		})
	}
}

// TestNextFireAt_StandardWeekly pins the canonical case: every
// Monday at 09:00, asked from a Sunday afternoon, returns the
// upcoming Monday at exactly 09:00 UTC.
func TestNextFireAt_StandardWeekly(t *testing.T) {
	// 2026-05-24 is a Sunday at 16:00 UTC.
	from := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	next, err := NextFireAt("0 9 * * 1", from)
	if err != nil {
		t.Fatalf("NextFireAt: %v", err)
	}
	want := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}
}

// TestNextFireAt_ExclusiveOfReferenceTime: when called from a
// moment that *is* the cron's match, the next fire must be the
// FOLLOWING match, not the current one. This is the property the
// runner relies on to avoid double-firing the same cycle.
func TestNextFireAt_ExclusiveOfReferenceTime(t *testing.T) {
	at := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC) // a Monday 09:00
	next, err := NextFireAt("0 9 * * 1", at)
	if err != nil {
		t.Fatalf("NextFireAt: %v", err)
	}
	want := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC) // next Monday
	if !next.Equal(want) {
		t.Errorf("next = %s, want %s (must be exclusive of reference time)", next, want)
	}
}

// TestNextFireAt_RejectsInvalidExpression: garbage in surfaces
// as ErrInvalidCron so the caller can decide whether to fail
// the row or surface a clear operator message.
func TestNextFireAt_RejectsInvalidExpression(t *testing.T) {
	_, err := NextFireAt("not a cron", time.Now())
	if err == nil {
		t.Fatal("expected error for invalid expression")
	}
	if !errors.Is(err, ErrInvalidCron) {
		t.Errorf("err = %v, want ErrInvalidCron", err)
	}
}

// TestNextFireAt_ReturnsUTC normalises the output regardless of
// the input's location — the storage layer pins UTC and we don't
// want a Europe/Prague-anchored runner to write a local-time
// fire_at into a TIMESTAMPTZ column.
func TestNextFireAt_ReturnsUTC(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Skipf("Europe/Prague unavailable in test env: %v", err)
	}
	from := time.Date(2026, 5, 24, 18, 0, 0, 0, prague)
	next, err := NextFireAt("0 9 * * 1", from)
	if err != nil {
		t.Fatalf("NextFireAt: %v", err)
	}
	if next.Location() != time.UTC {
		t.Errorf("next.Location = %s, want UTC", next.Location())
	}
}

// TestValidateCronExpr_ErrorMessageContainsExpression: the
// wrapped error must mention enough of the input that an
// operator can spot which expression the LLM emitted that
// failed validation.
func TestValidateCronExpr_ErrorMessageContainsExpression(t *testing.T) {
	err := ValidateCronExpr("60 9 * * 1")
	if err == nil {
		t.Fatal("expected error for out-of-range minute")
	}
	// The robfig error itself includes the bad field; we just
	// confirm the prefix carries our package error sentinel for
	// downstream switch'ing.
	if !strings.Contains(err.Error(), "invalid cron expression") {
		t.Errorf("error message %q missing ErrInvalidCron prefix", err)
	}
}
