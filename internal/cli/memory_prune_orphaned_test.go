package cli

import (
	"strings"
	"testing"
)

// Dry-run BY DEFAULT is the property, not a convenience. This command deletes
// personal data, and an operator must see the count and its composition before
// it happens — the same reasoning as secrets scan-history (design §5.5).
func TestMemoryPruneOrphaned_isDryRunByDefault(t *testing.T) {
	f := memoryPruneOrphanedCmd.Flags().Lookup("execute")
	if f == nil {
		t.Fatal("no --execute flag: the delete would then be unconditional")
	}
	if f.DefValue != "false" {
		t.Errorf("--execute defaults to %q; a destructive default is the whole hazard", f.DefValue)
	}
	if memoryPruneOrphanedCmd.Flags().Lookup("project") == nil {
		t.Error("no --project flag: an operator cleaning one project would sweep every other one")
	}
}

// The command is reachable. A backfill nobody can run is the shape of the
// original defect — a capability built and wired to nothing.
func TestMemoryPruneOrphaned_isRegisteredUnderMemory(t *testing.T) {
	for _, c := range memoryCmd.Commands() {
		if c.Name() == "prune-orphaned-entities" {
			return
		}
	}
	t.Fatal("prune-orphaned-entities is not registered under 'memory'")
}

// The knowledge-graph hard deletes are recorded under something. --request
// carries the data-subject request when there is one; otherwise the operator is
// named — and named AS an operator, not dressed up as a request. "cli-erase:
// alice" would answer "which request authorised this?" with something that is
// not a request, which is a false attribution rather than an honest gap.
func TestEraseRequestAuthority_namesTheRequestOrTheOperator(t *testing.T) {
	t.Cleanup(func() { eraseArtifactRequest = "" })

	eraseArtifactRequest = "  dsr-2026-08-21-7  "
	if got := eraseRequestAuthority(); got != "dsr-2026-08-21-7" {
		t.Errorf("the request id must be used verbatim, got %q", got)
	}

	eraseArtifactRequest = "   "
	got := eraseRequestAuthority()
	if !strings.HasPrefix(got, "operator:") {
		t.Errorf("without a request the authority must name the OPERATOR, not imply a "+
			"request exists, got %q", got)
	}
	if strings.TrimSpace(strings.TrimPrefix(got, "operator:")) == "" {
		t.Error("the fallback authority must not be empty — the erasure would then refuse")
	}
}
