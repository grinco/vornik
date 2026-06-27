package reminders

// Cron-expression validator + next-fire calculator. Thin wrapper
// over github.com/robfig/cron/v3 so the rest of the package
// doesn't reach into a third-party parser surface — that lets
// the runner re-arm logic mock NextFireAt for deterministic
// tests, and pins the supported grammar to the 5-field POSIX
// variant ("min hour dom mon dow") that operator-facing inputs
// produce.
//
// The 6-field robfig-with-seconds variant is deliberately
// rejected: operator natural-language inputs never carry
// sub-minute precision, and the parser LLM tends to produce
// 5-field exprs unprompted. Refusing the seconds variant
// surfaces drift loudly instead of letting a seconds-granularity
// cron quietly fire 60× per minute.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser is the package-scope parser for the 5-field
// "standard" grammar (no seconds, no descriptors). robfig's
// parser is goroutine-safe so one shared instance is fine.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ErrInvalidCron means the supplied expression isn't a valid
// 5-field POSIX cron. Wrapped so callers (parser, runner) can
// distinguish it from delivery errors via errors.Is.
var ErrInvalidCron = errors.New("reminder: invalid cron expression")

// ValidateCronExpr returns nil for a well-formed 5-field
// expression, ErrInvalidCron wrapping the parser error
// otherwise. Trims surrounding whitespace — LLM output
// commonly carries it.
func ValidateCronExpr(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return fmt.Errorf("%w: empty expression", ErrInvalidCron)
	}
	if _, err := cronParser.Parse(expr); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCron, err)
	}
	return nil
}

// NextFireAt returns the first scheduled fire time strictly
// AFTER `after`. Robfig's Schedule.Next is exclusive of the
// reference time, which is exactly what the runner wants — when
// a row just fired at 09:00:00 the next fire must be the *next*
// occurrence, not the same one.
//
// Returns ErrInvalidCron if expr fails to parse. The caller's
// own clock injection should flow through `after` so tests stay
// deterministic.
func NextFireAt(expr string, after time.Time) (time.Time, error) {
	expr = strings.TrimSpace(expr)
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrInvalidCron, err)
	}
	// robfig computes next-fire in the supplied time's location;
	// callers stamp UTC and the storage layer is UTC, so we
	// normalise here.
	next := sched.Next(after.UTC())
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: expression %q yielded no future fire after %s",
			ErrInvalidCron, expr, after.UTC().Format(time.RFC3339))
	}
	return next.UTC(), nil
}
