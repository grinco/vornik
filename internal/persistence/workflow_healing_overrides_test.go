package persistence

import (
	"testing"
	"time"
)

// TestHealingTriggerOverride_IsMutedAt — the mute helper is the
// detector's contract surface: muted iff MutedUntil > t. Nil
// pointer and past timestamps must both return false so a stale
// mute doesn't silence the detector indefinitely.
func TestHealingTriggerOverride_IsMutedAt(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	future := now.Add(1 * time.Hour)
	past := now.Add(-1 * time.Hour)

	cases := []struct {
		name string
		o    *HealingTriggerOverride
		want bool
	}{
		{"nil override", nil, false},
		{"no mute set", &HealingTriggerOverride{}, false},
		{"mute in future", &HealingTriggerOverride{MutedUntil: &future}, true},
		{"mute in past", &HealingTriggerOverride{MutedUntil: &past}, false},
		{"mute at exactly now", &HealingTriggerOverride{MutedUntil: &now}, false}, // not strictly After
	}
	for _, c := range cases {
		if got := c.o.IsMutedAt(now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
