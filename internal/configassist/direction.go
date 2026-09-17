package configassist

import (
	"strconv"
	"strings"
	"time"
)

// spendDirection labels a budget change by direction: raising a cap is
// spend (D) either way — the class is D regardless — but the reason says
// which way it moved so the operator is not left guessing.
func spendDirection(ch Change, what string) string {
	if numericIncreased(ch) {
		return what + " raised: spend"
	}
	if ch.Before != "" && ch.After != "" {
		return what + " lowered (still class D: spend keys never auto-apply)"
	}
	return what + " set: spend"
}

// numericIncreased reports whether After > Before as numbers. An unparseable
// pair reports true (an unknown direction is treated as an increase — the
// conservative reading review R7 asks for).
func numericIncreased(ch Change) bool {
	b, errB := strconv.ParseFloat(strings.TrimSpace(ch.Before), 64)
	a, errA := strconv.ParseFloat(strings.TrimSpace(ch.After), 64)
	if errA != nil {
		return ch.After != "" // non-numeric new value: conservative
	}
	if errB != nil {
		return true // set where absent: conservative
	}
	return a > b
}

// removesApproval reports whether a change to autonomy.requireApproval ends
// with approval NOT required. Two shapes reach that state and only one used
// to be recognised: an explicit `false`, and DELETING the key — which the
// loader decodes to the zero value, the same false (audit 2026-09-15 CA-01).
// A change is judged by the runtime state it produces, never by whether the
// model wrote a word.
func removesApproval(ch Change) bool {
	was := strings.EqualFold(strings.TrimSpace(ch.Before), "true")
	if !was {
		return false // it was already off; this change does not remove it
	}
	now := strings.TrimSpace(ch.After)
	return now == "" || strings.EqualFold(now, "false")
}

// removesNumericLimit reports whether a change takes a positive cap to a
// value the runtime reads as NO cap: zero, a negative, or the deleted key
// (which decodes to zero). `MaxTasksPerHour <= 0` is checked as "no limit"
// in autonomy.Manager.checkRateLimit, so 4 -> 0 is not a reduction of four
// units, it is the removal of the limit (audit 2026-09-15 CA-01).
func removesNumericLimit(ch Change) bool {
	before, err := strconv.ParseFloat(strings.TrimSpace(ch.Before), 64)
	if err != nil || before <= 0 {
		return false // there was no effective cap to remove
	}
	after := strings.TrimSpace(ch.After)
	if after == "" {
		return true // deleted: decodes to the zero sentinel
	}
	v, err := strconv.ParseFloat(after, 64)
	return err == nil && v <= 0
}

// durationShortened reports whether After < Before as durations (a shorter
// cadence = more ticks). Unparseable → conservative true only when both
// are present and differ.
func durationShortened(ch Change) bool {
	b, errB := parseLooseDuration(ch.Before)
	a, errA := parseLooseDuration(ch.After)
	if errA != nil || errB != nil {
		return ch.Before != "" && ch.After != "" && ch.Before != ch.After
	}
	return a < b
}

// durationLengthened reports whether After > Before as durations.
func durationLengthened(ch Change) bool {
	b, errB := parseLooseDuration(ch.Before)
	a, errA := parseLooseDuration(ch.After)
	if errA != nil {
		return ch.After != "" && ch.After != ch.Before
	}
	if errB != nil {
		return true
	}
	return a > b
}

// parseLooseDuration accepts Go durations plus the day/week suffixes the
// registry's cadence parser accepts ("60d", "2w").
func parseLooseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if v, err := strconv.ParseFloat(n, 64); err == nil {
			return time.Duration(v * float64(24*time.Hour)), nil
		}
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		if v, err := strconv.ParseFloat(n, 64); err == nil {
			return time.Duration(v * float64(7*24*time.Hour)), nil
		}
	}
	return time.ParseDuration(s)
}

// valueIncreased reports whether a retry/timeout/iteration value grew:
// durations compare as durations, numbers as numbers; an unparseable
// or newly-set value is conservatively an increase (review R7).
func valueIncreased(ch Change) bool {
	if _, errA := parseLooseDuration(ch.After); errA == nil {
		if _, errB := parseLooseDuration(ch.Before); errB == nil {
			return durationLengthened(ch)
		}
		return true // duration set where absent/unparseable
	}
	return numericIncreased(ch)
}
