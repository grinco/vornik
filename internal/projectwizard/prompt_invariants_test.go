package projectwizard

import (
	"strings"
	"testing"
)

// TestSystemPrompt_ConvergenceInvariants asserts the wizard system prompt
// carries the single-autonomy + param-hygiene invariants so a capable
// model avoids the shapes the normalizer would otherwise have to repair
// (2026-07-16 convergence hardening, the ceiling to the normalizer floor).
func TestSystemPrompt_ConvergenceInvariants(t *testing.T) {
	// Collapse whitespace so line-wrapping in the prompt template doesn't
	// break substring checks.
	p := strings.Join(strings.Fields(strings.ToLower(systemPromptTemplate)), " ")
	for _, want := range []string{
		"exactly one autonomy style",
		"rag_source",
		"only use params",
		"the chosen base template declares",
		"always set projectid",
	} {
		if !strings.Contains(p, strings.ToLower(want)) {
			t.Errorf("system prompt missing invariant %q", want)
		}
	}
	// The prompt must NOT instruct the model to invent a `topic` param —
	// that instruction produced the incident's "unknown parameter topic".
	if strings.Contains(p, "a topic (a short phrase") {
		t.Error("system prompt still tells the model to add a `topic` param (incident cause)")
	}
}
