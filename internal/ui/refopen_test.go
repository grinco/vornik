package ui

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestRefOpen(t *testing.T) {
	cases := []struct {
		name     string
		status   any
		openWhen []string
		want     bool
	}{
		{"terminal match", "COMPLETED", []string{"COMPLETED", "FAILED", "CANCELLED"}, true},
		{"running not in set", "RUNNING", []string{"COMPLETED", "FAILED", "CANCELLED"}, false},
		{"awaiting not in set", "AWAITING_INPUT", []string{"COMPLETED"}, false},
		{"empty openWhen always closed", "COMPLETED", nil, false},
		{"typed status matches", persistence.TaskStatus("FAILED"), []string{"FAILED"}, true},
		{"failed in terminal set", "FAILED", []string{"COMPLETED", "FAILED", "CANCELLED"}, true},
		{"nil status never matches", nil, []string{"COMPLETED"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refOpen(tc.status, tc.openWhen...); got != tc.want {
				t.Errorf("refOpen(%v, %v) = %v, want %v", tc.status, tc.openWhen, got, tc.want)
			}
		})
	}
}
