package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// formatOutcomeDuration formats a ms duration as "123ms" or "1.4s"
// for compact display in the outcomes panel.

func TestFormatOutcomeDuration_NegativeReturnsDash(t *testing.T) {
	assert.Equal(t, "—", formatOutcomeDuration(-1))
}

func TestFormatOutcomeDuration_SubSecondMs(t *testing.T) {
	assert.Equal(t, "0ms", formatOutcomeDuration(0))
	assert.Equal(t, "123ms", formatOutcomeDuration(123))
	assert.Equal(t, "999ms", formatOutcomeDuration(999))
}

func TestFormatOutcomeDuration_SecondsWithOneDecimal(t *testing.T) {
	assert.Equal(t, "1.0s", formatOutcomeDuration(1000))
	assert.Equal(t, "1.4s", formatOutcomeDuration(1400))
	assert.Equal(t, "12.5s", formatOutcomeDuration(12500))
}

// outcomeCSSClass maps step-outcome strings to pill CSS classes.
// Pinning each branch keeps the template's badge colours coherent.

func TestOutcomeCSSClass_Ok(t *testing.T) {
	assert.Equal(t, "outcome-ok", outcomeCSSClass("ok"))
}

func TestOutcomeCSSClass_Pending(t *testing.T) {
	assert.Equal(t, "outcome-pending", outcomeCSSClass("pending_validation"))
}

func TestOutcomeCSSClass_CancelledIsNeutral(t *testing.T) {
	assert.Equal(t, "outcome-neutral", outcomeCSSClass("cancelled"))
}

func TestOutcomeCSSClass_SupersededIsNeutral(t *testing.T) {
	assert.Equal(t, "outcome-neutral", outcomeCSSClass("superseded"))
}

func TestOutcomeCSSClass_FailureCodesAreBad(t *testing.T) {
	for _, code := range []string{
		"parse_error", "schema_violation", "refused",
		"iteration_exhausted", "degenerate_loop",
		"downstream_rejected", "gate_failed",
		"failed", "timeout", "budget_tripwire",
	} {
		assert.Equal(t, "outcome-bad", outcomeCSSClass(code),
			"%s should map to outcome-bad", code)
	}
}

func TestOutcomeCSSClass_UnknownFallsThrough(t *testing.T) {
	assert.Equal(t, "outcome-neutral", outcomeCSSClass("invented_outcome"))
	assert.Equal(t, "outcome-neutral", outcomeCSSClass(""))
}

// stepIdentityColor picks a stable HSL colour for the n-th unique
// step using golden-angle hue spacing.

func TestStepIdentityColor_DistinctHuesForFirstFew(t *testing.T) {
	colors := []string{
		stepIdentityColor(0),
		stepIdentityColor(1),
		stepIdentityColor(2),
		stepIdentityColor(3),
	}
	seen := map[string]bool{}
	for _, c := range colors {
		seen[c] = true
	}
	assert.Equal(t, 4, len(seen), "first four step colours must all differ")
}

func TestStepIdentityColor_NegativeTreatedAsZero(t *testing.T) {
	got := stepIdentityColor(-5)
	want := stepIdentityColor(0)
	assert.Equal(t, want, got, "negative n should reuse the n=0 colour")
}
