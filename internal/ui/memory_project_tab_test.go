// Tests for the 2026-05-16 memory_project tab resolver.
//
// The /ui/memory/<project> page reorganised nine sibling panels
// into three tabs (Health, Search&Inspect, Operate). The active
// tab is decided by resolveMemoryProjectTab; the page defaults to
// Operate when there's something requiring action and to Health on
// a quiet project so the operator lands somewhere informative
// either way.

package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveMemoryProjectTab_ExplicitTabsHonoured pins the
// shareable-URL contract: ?tab=<known> always wins, even when the
// "needs attention" default would have picked something else.
// Operators link each other these URLs and expect them to land on
// the named tab regardless of project state at click time.
func TestResolveMemoryProjectTab_ExplicitTabsHonoured(t *testing.T) {
	for _, name := range []string{"health", "search", "operate"} {
		// Pile on a noisy state so the default would diverge.
		assert.Equal(t, name, resolveMemoryProjectTab(name, 99, 99),
			"explicit ?tab=%s must win over the needs-attention default", name)
	}
}

// TestResolveMemoryProjectTab_UnknownFallsThrough — an arbitrary
// ?tab=junk should not strand the user on a blank page. The
// resolver treats unknown values as "no preference" and runs the
// default rule below.
func TestResolveMemoryProjectTab_UnknownFallsThrough(t *testing.T) {
	assert.Equal(t, "health", resolveMemoryProjectTab("garbage", 0, 0))
	assert.Equal(t, "operate", resolveMemoryProjectTab("garbage", 1, 0))
}

// TestResolveMemoryProjectTab_QuarantinePendingForcesOperate pins
// the operator's #1 use case: there's quarantined content waiting
// for triage, so land them on Operate without forcing a click
// through Health first.
func TestResolveMemoryProjectTab_QuarantinePendingForcesOperate(t *testing.T) {
	assert.Equal(t, "operate", resolveMemoryProjectTab("", 0, 1))
}

// TestResolveMemoryProjectTab_QueueDepthForcesOperate — pending
// ingest backlog is the other signal that "something needs
// attention right now". Land on Operate so the rollback /
// quarantine tables are first thing on screen.
func TestResolveMemoryProjectTab_QueueDepthForcesOperate(t *testing.T) {
	assert.Equal(t, "operate", resolveMemoryProjectTab("", 5, 0))
}

// TestResolveMemoryProjectTab_QuietDefaultsHealth — when both
// signals are zero, default to Health. The funnel + epoch
// timeline are the most useful at-a-glance signal for a project
// that's running cleanly.
func TestResolveMemoryProjectTab_QuietDefaultsHealth(t *testing.T) {
	assert.Equal(t, "health", resolveMemoryProjectTab("", 0, 0))
}

// TestResolveMemoryProjectTab_NegativeCountsTreatedAsZero —
// defensive: a malformed Counter (in-progress repo refactor that
// returns -1 to mean "unknown") must not flip the default. We
// don't currently emit negatives, but the rule "any positive
// signal means Operate" is the safe one to pin.
func TestResolveMemoryProjectTab_NegativeCountsTreatedAsZero(t *testing.T) {
	assert.Equal(t, "health", resolveMemoryProjectTab("", -1, -1),
		"negative counts are not a positive signal; must NOT force operate")
}
