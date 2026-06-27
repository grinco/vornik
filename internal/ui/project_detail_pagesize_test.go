package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseTaskLimit covers the page-size validator that
// guards the project-detail tasks list. The ?limit= value
// flows directly into TaskFilter.PageSize, so anything outside
// the explicit allowed set must fall back rather than letting
// a hostile caller request millions of rows.
func TestParseTaskLimit(t *testing.T) {
	allowed := []int{10, 20, 50, 100}
	const fallback = 20

	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"empty defaults to fallback", "", fallback},
		{"valid 10", "10", 10},
		{"valid 20", "20", 20},
		{"valid 50", "50", 50},
		{"valid 100", "100", 100},
		{"non-numeric falls back", "abc", fallback},
		{"negative falls back", "-1", fallback},
		{"zero falls back", "0", fallback},
		{"out-of-set falls back", "75", fallback},
		{"hostile huge falls back", "999999999", fallback},
		{"with whitespace falls back", " 20 ", fallback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTaskLimit(tc.raw, allowed, fallback)
			assert.Equal(t, tc.want, got)
		})
	}
}
