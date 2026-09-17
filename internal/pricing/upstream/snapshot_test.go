package upstream

import (
	"encoding/json"
	"testing"
)

// Upstream prices PER TOKEN; pricing.yaml is per 1M tokens. Converting in the
// generator rather than in each consumer is D5: comparing 0.35 against 3.5e-07
// silently reports every model as drifted by ~100%.
func TestDerive_ConvertsPerTokenToPerMillion(t *testing.T) {
	up := map[string]json.RawMessage{
		"gpt-5": json.RawMessage(`{"input_cost_per_token":1.25e-06,"output_cost_per_token":1.0e-05,
			"cache_read_input_token_cost":1.25e-07,"max_input_tokens":400000}`),
	}
	snap, err := Derive(up, []string{"gpt-5"}, Provenance{Commit: "abc"})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	got := snap.Models["gpt-5"]
	if got == nil {
		t.Fatal("gpt-5 should have been matched upstream")
	}
	if got.Input != 1.25 || got.Output != 10 {
		t.Errorf("per-1M conversion wrong: input=%v output=%v, want 1.25 / 10", got.Input, got.Output)
	}
	if got.CacheRead == nil || *got.CacheRead != 0.125 {
		t.Errorf("cache_read not converted: %v, want 0.125", got.CacheRead)
	}
	if got.MaxInputTokens != 400000 {
		t.Errorf("MaxInputTokens=%d, want 400000", got.MaxInputTokens)
	}
}

// D7: an id upstream has never heard of must be DISTINGUISHABLE from one whose
// price agrees. Recording it as an explicit null is what lets the check report
// the denominator it actually examined instead of implying 147.
func TestDerive_AbsentUpstreamIsExplicitNull(t *testing.T) {
	up := map[string]json.RawMessage{"gpt-5": json.RawMessage(`{"input_cost_per_token":1e-06}`)}
	snap, err := Derive(up, []string{"gpt-5", "zai.glm-5"}, Provenance{Commit: "abc"})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	entry, present := snap.Models["zai.glm-5"]
	if !present {
		t.Fatal("an unmatched id must still appear in the snapshot, as null")
	}
	if entry != nil {
		t.Errorf("unmatched id should be null, got %+v", entry)
	}
	if snap.Matched() != 1 || snap.Total() != 2 {
		t.Errorf("Matched()=%d Total()=%d, want 1 / 2", snap.Matched(), snap.Total())
	}
}

func TestCompare_ReportsDriftBeyondThreshold(t *testing.T) {
	snap := Snapshot{Models: map[string]*Entry{
		"deepseek-v4-flash": {Input: 0.300, Output: 1.200},
		"within-tolerance":  {Input: 1.00, Output: 3.00},
	}}
	ours := map[string]Rate{
		"deepseek-v4-flash": {Input: 0.065, Output: 0.180},
		"within-tolerance":  {Input: 1.05, Output: 3.10},
	}
	rep := snap.Compare(ours, 0.10)
	if len(rep.Drifted) != 1 {
		t.Fatalf("Drifted=%d, want 1: %+v", len(rep.Drifted), rep.Drifted)
	}
	d := rep.Drifted[0]
	if d.ID != "deepseek-v4-flash" {
		t.Errorf("wrong id: %s", d.ID)
	}
	// C6: both numbers reach the operator. A report carrying only "drifted"
	// invites applying upstream, and here upstream is the WORSE number.
	if d.OurInput != 0.065 || d.UpstreamInput != 0.300 {
		t.Errorf("both sides must be reported, got ours=%v upstream=%v", d.OurInput, d.UpstreamInput)
	}
}

// D7 again, as an assertion rather than prose: the denominator is what was
// compared, never the size of our table.
func TestCompare_DenominatorExcludesUnmatched(t *testing.T) {
	snap := Snapshot{Models: map[string]*Entry{
		"matched":   {Input: 1.00, Output: 3.00},
		"unmatched": nil,
	}}
	ours := map[string]Rate{
		"matched":   {Input: 1.00, Output: 3.00},
		"unmatched": {Input: 9.99, Output: 9.99},
		"unpinned":  {Input: 5.00, Output: 5.00},
	}
	rep := snap.Compare(ours, 0.10)
	if rep.Compared != 1 {
		t.Errorf("Compared=%d, want 1 — only ids with an upstream entry are comparable", rep.Compared)
	}
	if rep.Unmatched != 2 {
		t.Errorf("Unmatched=%d, want 2 (absent upstream + absent from snapshot)", rep.Unmatched)
	}
	if len(rep.Drifted) != 0 {
		t.Errorf("an unmatched id must never be reported as drift: %+v", rep.Drifted)
	}
}

// D8: an available field we lack is a different operator action from a
// disagreement, so it is a separate list.
func TestCompare_EnrichmentIsSeparateFromDrift(t *testing.T) {
	cr := 0.035
	snap := Snapshot{Models: map[string]*Entry{
		"m": {Input: 0.35, Output: 2.75, CacheRead: &cr, MaxInputTokens: 262144},
	}}
	rep := snap.Compare(map[string]Rate{"m": {Input: 0.35, Output: 2.75}}, 0.10)
	if len(rep.Drifted) != 0 {
		t.Errorf("prices agree; nothing should be drifted: %+v", rep.Drifted)
	}
	if len(rep.CacheReadAvailable) != 1 || rep.CacheReadAvailable[0].ID != "m" {
		t.Errorf("upstream cache_read we lack must be reported: %+v", rep.CacheReadAvailable)
	}
}

// The embedded artifact is the thing the daemon actually reads; a malformed or
// unpinned one would make the check silently vacuous.
func TestEmbeddedSnapshotIsUsable(t *testing.T) {
	snap, err := Load()
	if err != nil {
		t.Fatalf("Load embedded snapshot: %v", err)
	}
	if snap.Provenance.Commit == "" || snap.Provenance.SHA256 == "" {
		t.Error("snapshot must pin an upstream commit and record the fetched file's sha256")
	}
	if snap.Total() == 0 {
		t.Error("embedded snapshot has no models")
	}
	if snap.Matched() == 0 {
		t.Error("embedded snapshot matched nothing upstream — the derivation is broken")
	}
}
