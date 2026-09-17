package membench

import (
	"strings"
	"testing"
)

// Partial() has FIVE distinct causes and the CLI printed ONE sentence for all of
// them: "the external system's configuration could not be read". On a
// single-system run that cause is not merely unlikely, it is unreachable —
// Partial() short-circuits on f.SingleSystem before it ever tests
// ExternalConfigSHA256 — so every partial vornik-side run was told the one thing
// that could not have happened.
//
// Found 2026-09-17 running the release benchmark. The run passed both
// --our-extraction-model and --recall-method, which is what the backlog recorded
// as the cause of a PARTIAL key on 2026-09-08, and was STILL partial: the actual
// empty fields were observed_embedder and daemon_revision. A message naming a
// speculative cause sent the reader at the wrong flags, twice.
//
// PartialReasons names the fields that are actually missing, so the note can say
// what is true rather than what is typical.
func TestPartialReasons_NamesTheMissingFieldNotAGuess(t *testing.T) {
	complete := ComparabilityFields{
		ObservedEmbedder:     "qwen3-embedding:0.6b",
		ObservedRecallMethod: "context-assembly",
		CorpusRegime:         CorpusRegimeCold,
		DaemonRevision:       "2026.9.4-203-gb37d6de2a",
		SingleSystem:         true,
	}
	if got := complete.PartialReasons(); len(got) != 0 {
		t.Errorf("a complete single-system key must have no reasons, got %v", got)
	}
	if complete.Partial() {
		t.Error("a complete single-system key must not be partial")
	}

	// The shape the release run actually hit.
	f := complete
	f.ObservedEmbedder = ""
	f.DaemonRevision = ""
	got := f.PartialReasons()
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "observed_embedder") || !strings.Contains(joined, "daemon_revision") {
		t.Errorf("reasons must name BOTH missing fields, got %v", got)
	}
	if strings.Contains(joined, "external") {
		t.Errorf("a single-system run must never be told about an external config: %v", got)
	}

	// Every reason must correspond to a real cause: reasons and Partial() agree.
	for _, tc := range []struct {
		name string
		mut  func(*ComparabilityFields)
	}{
		{"embedder", func(c *ComparabilityFields) { c.ObservedEmbedder = "" }},
		{"recall method", func(c *ComparabilityFields) { c.ObservedRecallMethod = "" }},
		{"corpus regime", func(c *ComparabilityFields) { c.CorpusRegime = CorpusRegimeUnknown }},
		{"daemon revision", func(c *ComparabilityFields) { c.DaemonRevision = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := complete
			tc.mut(&c)
			if !c.Partial() {
				t.Fatal("expected partial")
			}
			if len(c.PartialReasons()) == 0 {
				t.Error("Partial() is true but no reason is named — the note would have nothing to say")
			}
		})
	}
}

// The external-config cause is real for a TWO-system run and must survive.
func TestPartialReasons_ExternalConfigStillNamedWhenItApplies(t *testing.T) {
	f := ComparabilityFields{
		ObservedEmbedder:     "e",
		ObservedRecallMethod: "context-assembly",
		CorpusRegime:         CorpusRegimeCold,
		DaemonRevision:       "rev",
		SingleSystem:         false,
	}
	if !f.Partial() {
		t.Fatal("a two-system run with no external config sha must be partial")
	}
	if got := strings.Join(f.PartialReasons(), ","); !strings.Contains(got, "external") {
		t.Errorf("the external-config cause must still be named when it applies, got %v", got)
	}
}
