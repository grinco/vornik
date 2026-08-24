package cli

import "testing"

// The repair is additive, but it still changes provenance, so it previews first
// — the same discipline as prune-orphaned-entities, which deletes.
func TestMemoryBackfillMentions_isDryRunByDefault(t *testing.T) {
	f := memoryBackfillMentionsCmd.Flags().Lookup("execute")
	if f == nil {
		t.Fatal("no --execute flag: the write would then be unconditional")
	}
	if f.DefValue != "false" {
		t.Errorf("--execute defaults to %q; an operator must see the count first", f.DefValue)
	}
	if memoryBackfillMentionsCmd.Flags().Lookup("project") == nil {
		t.Error("no --project flag: an operator repairing one project would touch every other")
	}
}

// A repair nobody can run is the shape of the defect it repairs — a capability
// built and wired to nothing.
func TestMemoryBackfillMentions_isRegisteredUnderMemory(t *testing.T) {
	for _, c := range memoryCmd.Commands() {
		if c.Name() == "backfill-entity-mentions" {
			return
		}
	}
	t.Fatal("backfill-entity-mentions is not registered under 'memory'")
}
