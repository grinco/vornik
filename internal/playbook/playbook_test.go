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

// The hand-kept TestPlaybookCoversAllFailureClasses used to live here. It
// listed its classes in a Go slice and therefore mirrored the registry it was
// meant to protect — it named 19 of 23 task classes and knew nothing of the 19
// step classes at all, which is how the fleet's largest failure class came to
// answer "Unrecognised failure class" (Finding B, 2026-08-26).
//
// Replaced by vocabulary_test.go, which derives both vocabularies from the
// declarations with go/ast. Deleted rather than extended: extending it would
// have reproduced the defect.

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
