package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// parseCheckpointView / parseScratchpadQuestions / buildPhaseTracker
// are pure JSON-shape parsers — pinning their happy / sad paths is
// cheap insurance against a future shape change silently breaking
// the task detail page.

func TestParseCheckpointView_HappyPath(t *testing.T) {
	raw := []byte(`{
		"kind": "decision",
		"question": "Pick a colour",
		"task_for_human": "review the swatch",
		"draft": "draft body",
		"expected_by": "2026-05-20",
		"default_if_no_response": "blue",
		"options": [
			{"id": "r", "label": "Red"},
			{"id": "b", "label": "Blue"}
		]
	}`)
	got := parseCheckpointView(raw)
	if assert.NotNil(t, got) {
		assert.Equal(t, "decision", got.Kind)
		assert.Equal(t, "Pick a colour", got.Question)
		assert.Equal(t, "review the swatch", got.TaskForHuman)
		assert.Equal(t, "draft body", got.Draft)
		assert.Equal(t, "2026-05-20", got.ExpectedBy)
		assert.Equal(t, "blue", got.DefaultIfNoResponse)
		assert.Len(t, got.Options, 2)
		assert.Equal(t, "r", got.Options[0].ID)
		assert.Equal(t, "Red", got.Options[0].Label)
	}
}

func TestParseCheckpointView_InvalidJSONReturnsNil(t *testing.T) {
	assert.Nil(t, parseCheckpointView([]byte("not json")))
}

func TestParseCheckpointView_EmptyJSONReturnsZeroValue(t *testing.T) {
	got := parseCheckpointView([]byte("{}"))
	if assert.NotNil(t, got) {
		assert.Equal(t, "", got.Kind)
		assert.Empty(t, got.Options)
	}
}

func TestParseScratchpadQuestions_HappyPath(t *testing.T) {
	got := parseScratchpadQuestions([]byte(`["q1","q2","q3"]`))
	assert.Equal(t, []string{"q1", "q2", "q3"}, got)
}

func TestParseScratchpadQuestions_NilInput(t *testing.T) {
	assert.Nil(t, parseScratchpadQuestions(nil))
}

func TestParseScratchpadQuestions_EmptyBytes(t *testing.T) {
	assert.Nil(t, parseScratchpadQuestions([]byte{}))
}

func TestParseScratchpadQuestions_InvalidJSONReturnsNil(t *testing.T) {
	assert.Nil(t, parseScratchpadQuestions([]byte("garbage")))
}

func TestBuildPhaseTracker_NilScratchpadReturnsNil(t *testing.T) {
	assert.Nil(t, buildPhaseTracker(nil))
}

func TestBuildPhaseTracker_EmptyHistoryReturnsNil(t *testing.T) {
	sp := &persistence.TaskScratchpad{PhaseHistory: []byte{}}
	assert.Nil(t, buildPhaseTracker(sp))
}

func TestBuildPhaseTracker_InvalidJSONReturnsNil(t *testing.T) {
	sp := &persistence.TaskScratchpad{PhaseHistory: []byte("bad-json")}
	assert.Nil(t, buildPhaseTracker(sp))
}

func TestBuildPhaseTracker_MarksCurrentPhase(t *testing.T) {
	current := "design"
	sp := &persistence.TaskScratchpad{
		PhaseHistory: []byte(`[
			{"name":"intake","status":"done"},
			{"name":"design","status":"active"},
			{"name":"build","status":"pending"}
		]`),
		CurrentPhase: &current,
	}
	got := buildPhaseTracker(sp)
	if assert.Len(t, got, 3) {
		assert.Equal(t, "intake", got[0].Name)
		assert.False(t, got[0].IsCurrent)
		assert.Equal(t, "design", got[1].Name)
		assert.True(t, got[1].IsCurrent, "current phase should be marked")
		assert.Equal(t, "build", got[2].Name)
		assert.False(t, got[2].IsCurrent)
	}
}

func TestBuildPhaseTracker_NoCurrentPhaseAllFalse(t *testing.T) {
	sp := &persistence.TaskScratchpad{
		PhaseHistory: []byte(`[{"name":"a","status":"done"},{"name":"b","status":"active"}]`),
	}
	got := buildPhaseTracker(sp)
	if assert.Len(t, got, 2) {
		assert.False(t, got[0].IsCurrent)
		assert.False(t, got[1].IsCurrent)
	}
}
