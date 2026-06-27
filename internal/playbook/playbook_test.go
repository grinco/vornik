package playbook

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// TestLookup_KnownClassReturnsSpecificEntry — happy path: a known
// class returns its specific entry, not the unknown fallback. Pinning
// one class (TOOL_ITERATION_LIMIT) here is enough; the corpus
// coverage test below catches any class missing from the map.
func TestLookup_KnownClassReturnsSpecificEntry(t *testing.T) {
	got := Lookup(persistence.TaskFailureClassToolIterationLimit)
	assert.Equal(t, persistence.TaskFailureClassToolIterationLimit, got.Class)
	assert.True(t,
		strings.Contains(strings.ToLower(got.Cause), "iteration"),
		"cause should mention 'iteration'; got: %q", got.Cause)
	assert.NotEmpty(t, got.Suggestions)
}

// TestLookup_UnknownClassReturnsFallback — the contract says Lookup
// never returns a zero Entry. Operators always get something to render
// even if a new failure class hasn't been added to the corpus yet.
func TestLookup_UnknownClassReturnsFallback(t *testing.T) {
	got := Lookup("MADE_UP_CLASS_THAT_NEVER_EXISTED")
	assert.Equal(t, "MADE_UP_CLASS_THAT_NEVER_EXISTED", got.Class)
	assert.Contains(t, got.Cause, "Unrecognised")
	assert.NotEmpty(t, got.Suggestions)
}

// TestPlaybookCoversAllFailureClasses — every TaskFailureClass*
// constant in persistence/models.go should have a corpus entry.
// Adding a new class without a corresponding playbook entry would
// silently fall through to the "unknown class" fallback, which is
// fine UX-wise but defeats the purpose of the playbook. Locking
// this in tests forces the author to write the entry.
func TestPlaybookCoversAllFailureClasses(t *testing.T) {
	classes := []string{
		persistence.TaskFailureClassLLMError,
		persistence.TaskFailureClassTimeout,
		persistence.TaskFailureClassToolError,
		persistence.TaskFailureClassInvalidOutput,
		persistence.TaskFailureClassMergeFailed,
		persistence.TaskFailureClassGateFailed,
		persistence.TaskFailureClassBudgetBlocked,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassWorkflowRole,
		persistence.TaskFailureClassWorkflowCfg,
		persistence.TaskFailureClassOrphaned,
		persistence.TaskFailureClassCancelled,
		persistence.TaskFailureClassRuntimeError,
		persistence.TaskFailureClassUnknown,
		persistence.TaskFailureClassLeaseExpired,
		persistence.TaskFailureClassWorkflowDrift,
		persistence.TaskFailureClassStuckExecution,
		persistence.TaskFailureClassToolIterationLimit,
		persistence.TaskFailureClassSecretLeak,
	}
	for _, c := range classes {
		t.Run(c, func(t *testing.T) {
			entry, ok := corpus[c]
			if !ok {
				t.Fatalf("playbook corpus missing entry for failure class %q", c)
			}
			assert.Equal(t, c, entry.Class, "entry.Class must match its key")
			assert.NotEmpty(t, strings.TrimSpace(entry.Cause), "Cause is required")
			assert.NotEmpty(t, entry.Suggestions, "at least one Suggestion required")
			// 2026.6.0 SaaS-readiness: every shipped class must
			// carry a HumanMessage so the end-user-facing UI
			// has a non-jargon explanation. Falling back to Cause
			// works but is operator-tone, which is what we're
			// avoiding for the SaaS surface.
			assert.NotEmpty(t, strings.TrimSpace(entry.HumanMessage),
				"HumanMessage is required for the end-user-facing failed-task surface; class %q must have a non-jargon one-line explanation", c)
		})
	}
}

// TestHumanFriendly_PrefersHumanMessageOverCause anchors the
// fallback contract: HumanMessage wins when set, Cause is the
// fallback when not. Saves every consumer the same nil-check.
func TestHumanFriendly_PrefersHumanMessageOverCause(t *testing.T) {
	with := Entry{HumanMessage: "user-friendly text", Cause: "operator jargon"}
	assert.Equal(t, "user-friendly text", with.HumanFriendly(),
		"HumanMessage wins when set so the end-user surface stays jargon-free")

	without := Entry{Cause: "operator jargon only"}
	assert.Equal(t, "operator jargon only", without.HumanFriendly(),
		"HumanFriendly falls back to Cause when HumanMessage is empty so legacy entries don't render blank")

	empty := Entry{}
	assert.Equal(t, "", empty.HumanFriendly(),
		"a fully-empty Entry returns empty — callers may want to gate on this rather than render nothing")
}

// TestHumanMessages_AvoidObviousOperatorJargon — a sanity pass on
// the corpus. Walks every HumanMessage and refuses words like
// "ITERATION_LIMIT" or "modelFallback" that signal we're showing
// the wrong audience the wrong text. The list is intentionally
// short — exhaustive style policing is out of scope; the test
// catches gross regressions (someone copy-pasting Cause into
// HumanMessage by accident).
func TestHumanMessages_AvoidObviousOperatorJargon(t *testing.T) {
	jargon := []string{
		"VORNIK_",
		"YAML",
		"modelFallback",
		"requiredOutputKeys",
		"workflow.maxWallClock",
		"tool_audit_log",
		"_ITERATION_LIMIT",
		"on_success",
		"on_fail",
		"podman",
	}
	for _, entry := range corpus {
		msg := entry.HumanMessage
		for _, j := range jargon {
			assert.NotContains(t, msg, j,
				"HumanMessage for class %q contains operator jargon %q — that text belongs in Cause/Suggestions, not the user-facing one-liner", entry.Class, j)
		}
	}
}

// TestAll_ReturnsEverySortedAlpha — the ordered list powers the CLI
// table view; clients shouldn't need to re-sort on each render.
func TestAll_ReturnsEverySortedAlpha(t *testing.T) {
	all := All()
	assert.Equal(t, len(corpus), len(all), "All() must include every entry")

	classes := make([]string, len(all))
	for i, e := range all {
		classes[i] = e.Class
	}
	sorted := make([]string, len(classes))
	copy(sorted, classes)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	assert.True(t, reflect.DeepEqual(classes, sorted), "All() must be sorted by class")
}
