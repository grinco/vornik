package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// severityClass maps severity strings → CSS pill classes. Pinning
// each branch protects the template's colour contract.

func TestSeverityClass_Critical(t *testing.T) {
	assert.Equal(t, "outcome-bad", severityClass("critical"))
}

func TestSeverityClass_Warn(t *testing.T) {
	assert.Equal(t, "outcome-warn", severityClass("warn"))
}

func TestSeverityClass_UnknownFallsThrough(t *testing.T) {
	assert.Equal(t, "outcome-neutral", severityClass("info"))
	assert.Equal(t, "outcome-neutral", severityClass(""))
}

// stringField / floatField / intField / boolField are tolerant
// JSON-shape extractors. Pin every branch so a future broker
// schema addition doesn't silently corrupt the trading panel.

func TestStringField_PresentReturnsValue(t *testing.T) {
	assert.Equal(t, "hello", stringField(map[string]any{"k": "hello"}, "k"))
}

func TestStringField_WrongTypeReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", stringField(map[string]any{"k": 42}, "k"))
}

func TestStringField_MissingKeyReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", stringField(map[string]any{}, "k"))
}

func TestFloatField_AcceptsFloat64(t *testing.T) {
	assert.InDelta(t, 3.5, floatField(map[string]any{"k": float64(3.5)}, "k"), 0.001)
}

func TestFloatField_AcceptsFloat32(t *testing.T) {
	assert.InDelta(t, 2.25, floatField(map[string]any{"k": float32(2.25)}, "k"), 0.001)
}

func TestFloatField_AcceptsInt(t *testing.T) {
	assert.InDelta(t, 7.0, floatField(map[string]any{"k": 7}, "k"), 0.001)
}

func TestFloatField_AcceptsInt64(t *testing.T) {
	assert.InDelta(t, 999.0, floatField(map[string]any{"k": int64(999)}, "k"), 0.001)
}

func TestFloatField_MissingReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, floatField(map[string]any{}, "k"))
}

func TestFloatField_WrongTypeReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, floatField(map[string]any{"k": "not-a-number"}, "k"))
}

func TestIntField_AcceptsFloat64(t *testing.T) {
	assert.Equal(t, 7, intField(map[string]any{"k": float64(7.4)}, "k"))
}

func TestIntField_AcceptsInt(t *testing.T) {
	assert.Equal(t, 42, intField(map[string]any{"k": 42}, "k"))
}

func TestIntField_AcceptsInt64(t *testing.T) {
	assert.Equal(t, 100, intField(map[string]any{"k": int64(100)}, "k"))
}

func TestIntField_MissingReturnsZero(t *testing.T) {
	assert.Equal(t, 0, intField(map[string]any{}, "k"))
}

func TestIntField_WrongTypeReturnsZero(t *testing.T) {
	assert.Equal(t, 0, intField(map[string]any{"k": "abc"}, "k"))
}

func TestBoolField_True(t *testing.T) {
	assert.True(t, boolField(map[string]any{"k": true}, "k"))
}

func TestBoolField_False(t *testing.T) {
	assert.False(t, boolField(map[string]any{"k": false}, "k"))
}

func TestBoolField_Missing(t *testing.T) {
	assert.False(t, boolField(map[string]any{}, "k"))
}

func TestBoolField_WrongType(t *testing.T) {
	assert.False(t, boolField(map[string]any{"k": "yes"}, "k"))
}

// orderEffectivePrice picks LMT then STP price; MKT orders without
// a fill return 0 (volume calc skips them).

func TestOrderEffectivePrice_NilReturnsZero(t *testing.T) {
	assert.Equal(t, 0.0, orderEffectivePrice(nil))
}

func TestOrderEffectivePrice_LimitPriceWins(t *testing.T) {
	lp := 150.25
	sp := 99.0
	o := &persistence.TradingOrder{LimitPrice: &lp, StopPrice: &sp}
	assert.Equal(t, 150.25, orderEffectivePrice(o))
}

func TestOrderEffectivePrice_FallsBackToStopPrice(t *testing.T) {
	sp := 88.5
	o := &persistence.TradingOrder{StopPrice: &sp}
	assert.Equal(t, 88.5, orderEffectivePrice(o))
}

func TestOrderEffectivePrice_NoPricesReturnsZero(t *testing.T) {
	o := &persistence.TradingOrder{}
	assert.Equal(t, 0.0, orderEffectivePrice(o))
}

func TestOrderEffectivePrice_ZeroLimitFallsThroughToStop(t *testing.T) {
	lp := 0.0
	sp := 42.0
	o := &persistence.TradingOrder{LimitPrice: &lp, StopPrice: &sp}
	assert.Equal(t, 42.0, orderEffectivePrice(o),
		"zero limit price should not preempt a real stop price")
}
