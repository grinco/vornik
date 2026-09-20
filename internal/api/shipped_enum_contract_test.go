package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// The shipped config's enum declarations must say what they mean, because D6
// made them bind. A contract test rather than a unit test: the thing that can
// rot here is the YAML, not the walker.

func loadShippedDevSwarm(t *testing.T) *registry.Swarm {
	t.Helper()
	path := filepath.Join("..", "..", "configs", "swarms", "dev-swarm.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped dev-swarm: %v", err)
	}
	swarm, err := registry.ParseSwarmMarkdown(data, path)
	if err != nil {
		t.Fatalf("parse shipped dev-swarm: %v", err)
	}
	if err := swarm.Validate(path); err != nil {
		t.Fatalf("shipped dev-swarm fails validation: %v", err)
	}
	return swarm
}

func TestShippedComplexityEnumDeclaresWarn(t *testing.T) {
	// If this ever flips to the default (fail), an unrecognised tier starts
	// failing the analyst step, inverting dynamic-tool-budget-design.md §4's
	// "degrades rather than fails" — the 2026-06-13 incident's own fix.
	swarm := loadShippedDevSwarm(t)
	for _, role := range swarm.Roles {
		if role.OutputSchema == nil {
			continue
		}
		analysis := role.OutputSchema.Properties["analysis"]
		if analysis == nil {
			continue
		}
		complexity := analysis.Properties["complexity"]
		if complexity == nil {
			continue
		}
		if len(complexity.Enum) == 0 {
			t.Fatalf("role %q: analysis.complexity lost its enum", role.Name)
		}
		if got := complexity.ViolationMode(); got != registry.ViolationWarn {
			t.Fatalf("role %q: analysis.complexity onViolation = %q, want warn — "+
				"dynamic-tool-budget-design.md §4 resolves an unrecognised tier to 1.0x and "+
				"failing the step here inverts the 2026-06-13 fix", role.Name, got)
		}
		// Optionality is the other half of §4's protection: an enum constrains
		// a value, never a presence, so omission must stay available.
		for _, required := range analysis.Required {
			if required == "complexity" {
				t.Fatalf("role %q: analysis.complexity became REQUIRED; §4's absent->1.0x leg "+
					"is only reachable while the field may be omitted", role.Name)
			}
		}
		return
	}
	t.Fatal("no role in the shipped dev-swarm declares analysis.complexity; the contract this pins is gone")
}

func TestShippedTesterCaseStatusEnumBindsHard(t *testing.T) {
	// The counterpart: a closed correctness contract keeps the default.
	swarm := loadShippedDevSwarm(t)
	for _, role := range swarm.Roles {
		if role.OutputSchema == nil {
			continue
		}
		testing0 := role.OutputSchema.Properties["testing"]
		if testing0 == nil || testing0.Properties["cases"] == nil {
			continue
		}
		items := testing0.Properties["cases"].Items
		if items == nil {
			t.Fatalf("role %q: testing.cases lost its item schema — the pin walks "+
				"testing->cases->items->id and returns the schema unchanged if this is gone", role.Name)
		}
		status := items.Properties["status"]
		if status == nil || len(status.Enum) == 0 {
			t.Fatalf("role %q: testing.cases[].status lost its enum", role.Name)
		}
		if got := status.ViolationMode(); got != registry.ViolationFail {
			t.Fatalf("role %q: testing.cases[].status onViolation = %q, want fail", role.Name, got)
		}
		if items.Properties["id"] == nil {
			t.Fatalf("role %q: testing.cases[].id is gone; applyPinnedCaseEnum would silently "+
				"return the schema unchanged and the constraint would just disappear", role.Name)
		}
		return
	}
	t.Fatal("no role in the shipped dev-swarm declares testing.cases[]; the pinned-case contract is gone")
}

func TestBaselinesReadmeDoesNotPointAtExtraCaseCountForProviderEnforcement(t *testing.T) {
	// Test 61 — D6.4 promises this README update; without a check it is a
	// documented behaviour nothing implements, which stops the next person
	// looking. The backstop zeroes ExtraCaseCount, so a reader told to use it
	// as the provider-enforcement signal reads a guaranteed zero as good news.
	path := filepath.Join("..", "..", "internal", "agentbench", "baselines", "README.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baselines README: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "output_enum_violation_total") {
		t.Error("baselines/README.md does not name the counter that replaced ExtraCaseCount as the " +
			"provider-enforcement signal; D6.4 promises it does")
	}
	if !strings.Contains(text, "backstop") && !strings.Contains(text, "D6") {
		t.Error("baselines/README.md does not tell a reader that the receipt-time backstop changed " +
			"what extraCaseCount can mean")
	}
}
