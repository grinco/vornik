package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompanionRecallDescription_CarriesRAGFirstSteering pins the RAG-first
// steering clause on the recall tool Description (LLD 2026-07-12-companion-rag-
// first-guidance §4 secondary lever). This is the cross-client signal — for
// Codex (no SessionStart hooks) the Description is the ONLY RAG-first nudge, so
// a refactor that drops it must fail loudly. The `| USAGE:` separator marks
// the functional-vs-policy boundary.
func TestCompanionRecallDescription_CarriesRAGFirstSteering(t *testing.T) {
	defs := companionToolDefs()
	var recall *mcpToolDef
	for i := range defs {
		if defs[i].Name == "recall" {
			recall = &defs[i]
			break
		}
	}
	require.NotNil(t, recall, "recall tool must be defined")
	assert.Contains(t, recall.Description, "| USAGE:",
		"recall Description must carry the functional|USAGE separator (cross-client steering layer)")
	assert.Contains(t, recall.Description, "BEFORE a deep code dive",
		"recall Description must steer toward recall-before-code on design questions")
	assert.True(t, strings.Contains(recall.Description, "authoritative design record"),
		"recall Description must state the LLD-as-authoritative trust contract")
}
