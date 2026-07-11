package narrator

import (
	"testing"
	"time"
)

// TestKnobDefaults_ZeroValueFallsBackToDocumentedDefault — every
// tunable knob falls back to its Default* constant when left at the
// Go zero value, per each field's doc comment.
func TestKnobDefaults_ZeroValueFallsBackToDocumentedDefault(t *testing.T) {
	n := &Narrator{}
	if got := n.debounce(); got != DefaultDebounce {
		t.Errorf("debounce() = %v, want %v", got, DefaultDebounce)
	}
	if got := n.longToolThreshold(); got != DefaultLongToolThresh {
		t.Errorf("longToolThreshold() = %v, want %v", got, DefaultLongToolThresh)
	}
	if got := n.minLineInterval(); got != DefaultMinLineInterval {
		t.Errorf("minLineInterval() = %v, want %v", got, DefaultMinLineInterval)
	}
	if got := n.maxLines(); got != DefaultMaxLines {
		t.Errorf("maxLines() = %v, want %v", got, DefaultMaxLines)
	}
	if got := n.maxCostUSD(); got != DefaultMaxCostUSD {
		t.Errorf("maxCostUSD() = %v, want %v", got, DefaultMaxCostUSD)
	}
	if got := n.idlePollInterval(); got != defaultIdlePollInterval {
		t.Errorf("idlePollInterval() = %v, want %v", got, defaultIdlePollInterval)
	}
	if got := n.idleThreshold(); got != defaultIdleThreshold {
		t.Errorf("idleThreshold() = %v, want %v", got, defaultIdleThreshold)
	}
	if got := n.forceTeardownAfter(); got != defaultForceTeardown {
		t.Errorf("forceTeardownAfter() = %v, want %v", got, defaultForceTeardown)
	}
}

// TestKnobDefaults_ExplicitValueOverridesDefault — every tunable
// knob, when set, wins over the Default* constant.
func TestKnobDefaults_ExplicitValueOverridesDefault(t *testing.T) {
	n := &Narrator{
		Debounce:         1 * time.Millisecond,
		LongToolThresh:   2 * time.Millisecond,
		MinLineInterval:  3 * time.Millisecond,
		MaxLines:         7,
		MaxCostUSD:       0.5,
		IdlePollInterval: 4 * time.Millisecond,
		IdleThreshold:    5 * time.Millisecond,
		ForceTeardown:    6 * time.Millisecond,
	}
	if n.debounce() != n.Debounce {
		t.Error("debounce() should return the explicit value")
	}
	if n.longToolThreshold() != n.LongToolThresh {
		t.Error("longToolThreshold() should return the explicit value")
	}
	if n.minLineInterval() != n.MinLineInterval {
		t.Error("minLineInterval() should return the explicit value")
	}
	if n.maxLines() != n.MaxLines {
		t.Error("maxLines() should return the explicit value")
	}
	if n.maxCostUSD() != n.MaxCostUSD {
		t.Error("maxCostUSD() should return the explicit value")
	}
	if n.idlePollInterval() != n.IdlePollInterval {
		t.Error("idlePollInterval() should return the explicit value")
	}
	if n.idleThreshold() != n.IdleThreshold {
		t.Error("idleThreshold() should return the explicit value")
	}
	if n.forceTeardownAfter() != n.ForceTeardown {
		t.Error("forceTeardownAfter() should return the explicit value")
	}
}

// TestKnobs_NowAndArm_OverridableForTests — the nowFn/afterFunc test
// seams both default to the real clock/timer and honor an override.
func TestKnobs_NowAndArm_OverridableForTests(t *testing.T) {
	n := &Narrator{}
	if n.now().IsZero() {
		t.Error("default now() should return the real current time")
	}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n.nowFn = func() time.Time { return fixed }
	if got := n.now(); !got.Equal(fixed) {
		t.Errorf("now() override = %v, want %v", got, fixed)
	}

	fired := make(chan struct{}, 1)
	timer := n.arm(time.Hour, func() { fired <- struct{}{} })
	if timer == nil {
		t.Fatal("arm() with the default afterFunc should return a real *time.Timer")
	}
	timer.Stop()

	var got time.Duration
	n.afterFunc = func(d time.Duration, f func()) *time.Timer {
		got = d
		f()
		return nil
	}
	n.arm(42*time.Millisecond, func() { fired <- struct{}{} })
	if got != 42*time.Millisecond {
		t.Errorf("afterFunc override saw duration %v, want 42ms", got)
	}
	select {
	case <-fired:
	default:
		t.Error("overridden afterFunc should have invoked the callback")
	}
}
