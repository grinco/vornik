package persistence

import (
	"testing"
)

func TestLiftOutcomesSuccRateZeroN(t *testing.T) {
	tests := []struct {
		name     string
		outcomes LiftOutcomes
		expected float64
	}{
		{
			name:     "N==0 must not panic, returns 0",
			outcomes: LiftOutcomes{},
			expected: 0,
		},
		{
			name:     "N>0, compute success rate",
			outcomes: LiftOutcomes{N: 4, Successes: 3},
			expected: 0.75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.outcomes.SuccRate()
			if got != tt.expected {
				t.Errorf("SuccRate() = %v, want %v", got, tt.expected)
			}
		})
	}
}
