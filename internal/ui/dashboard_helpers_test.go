package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHumanizeAgo_ZeroAndNegative(t *testing.T) {
	assert.Equal(t, "just now", humanizeAgo(0))
	assert.Equal(t, "just now", humanizeAgo(-5*time.Second))
}

func TestHumanizeAgo_SubMinute(t *testing.T) {
	assert.Equal(t, "30s ago", humanizeAgo(30*time.Second))
	assert.Equal(t, "1s ago", humanizeAgo(time.Second))
}

func TestHumanizeAgo_SubHour(t *testing.T) {
	got := humanizeAgo(3*time.Minute + 24*time.Second)
	assert.Equal(t, "3m 24s ago", got)
}

func TestHumanizeAgo_MultiHour(t *testing.T) {
	got := humanizeAgo(2*time.Hour + 15*time.Minute)
	assert.Equal(t, "2h 15m ago", got)
}

func TestHumanizeEta_ZeroAndNegative(t *testing.T) {
	assert.Equal(t, "due now", humanizeEta(0))
	assert.Equal(t, "due now", humanizeEta(-5*time.Second))
}

func TestHumanizeEta_SubMinute(t *testing.T) {
	assert.Equal(t, "in 45s", humanizeEta(45*time.Second))
}

func TestHumanizeEta_SubHour(t *testing.T) {
	got := humanizeEta(1*time.Minute + 36*time.Second)
	assert.Equal(t, "in 1m 36s", got)
}

func TestHumanizeEta_MultiHour(t *testing.T) {
	got := humanizeEta(3*time.Hour + 5*time.Minute)
	assert.Equal(t, "in 3h 5m", got)
}
