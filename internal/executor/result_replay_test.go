package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestResultReplay walks internal/executor/testdata/result-fixtures
// and replays every captured payload through the validation chain
// (validateRequiredOutputKeys → EvaluatePlausibility) against the
// live role config from configs/swarms/. Item 12 of
// https://docs.vornik.io
//
// What this catches that unit tests don't:
//
//  1. Schema evolved; a real production payload that USED to validate
//     now fails (e.g. someone tightened the schema and didn't update
//     every callsite that produces the role's output). Unit tests
//     built off the new schema all pass; the fixture is the
//     independent witness that the contract changed.
//
//  2. Validator regressed; a real production payload that USED to
//     get rejected now passes (e.g. a refactor weakened an
//     assertion). Unit tests against synthetic fixtures don't catch
//     this if they too get refactored alongside.
//
// Each fixture declares its own expect (pass/fail) so the corpus
// captures both shapes — happy path AND every distinct failure mode
// we've found worth pinning.
func TestResultReplay(t *testing.T) {
	swarms, err := registry.LoadSwarms("../../configs")
	if err != nil {
		t.Fatalf("LoadSwarms: %v", err)
	}

	corpusRoot := "testdata/result-fixtures"
	entries, err := os.ReadDir(corpusRoot)
	if err != nil {
		t.Fatalf("read corpus root: %v", err)
	}

	fixtureCount := 0
	for _, swarmDir := range entries {
		if !swarmDir.IsDir() {
			continue
		}
		swarmFixtures, err := os.ReadDir(filepath.Join(corpusRoot, swarmDir.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", swarmDir.Name(), err)
		}
		for _, fixture := range swarmFixtures {
			if fixture.IsDir() || !strings.HasSuffix(fixture.Name(), ".json") {
				continue
			}
			fixtureCount++
			path := filepath.Join(corpusRoot, swarmDir.Name(), fixture.Name())
			subtest := swarmDir.Name() + "/" + strings.TrimSuffix(fixture.Name(), ".json")
			t.Run(subtest, func(t *testing.T) {
				replayFixture(t, path, swarms)
			})
		}
	}

	// Guard against silent corpus loss — a refactor that moves the
	// fixture root will otherwise pass with zero fixtures replayed.
	if fixtureCount == 0 {
		t.Fatalf("no fixtures replayed from %s — verify the corpus directory still exists", corpusRoot)
	}
}

func replayFixture(t *testing.T, path string, swarms map[string]*registry.Swarm) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fix struct {
		Swarm   string          `json:"swarm"`
		Role    string          `json:"role"`
		Payload json.RawMessage `json:"payload"`
		Expect  string          `json:"expect"`
		Reason  string          `json:"reason"`
	}
	if err := json.Unmarshal(raw, &fix); err != nil {
		t.Fatalf("parse fixture: %v\nfull contents:\n%s", err, raw)
	}
	if fix.Swarm == "" || fix.Role == "" {
		t.Fatalf("fixture missing swarm/role: %s", path)
	}
	if fix.Expect != "pass" && fix.Expect != "fail" {
		t.Fatalf("fixture %s has unsupported expect %q (want \"pass\" or \"fail\")", path, fix.Expect)
	}

	swarm := swarms[fix.Swarm]
	if swarm == nil {
		t.Fatalf("fixture references unknown swarm %q (configs/swarms/ doesn't have it)", fix.Swarm)
	}
	var role *registry.SwarmRole
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == fix.Role {
			role = &swarm.Roles[i]
			break
		}
	}
	if role == nil {
		t.Fatalf("fixture references unknown role %q in swarm %s", fix.Role, fix.Swarm)
	}

	// Run the full validation chain. The fixture's expect declares
	// pass/fail at the chain level — any missing-key OR plausibility
	// violation counts as fail. Pinning the specific violation type
	// would couple the corpus to the validator's internal taxonomy,
	// making refactors churn fixtures unnecessarily.
	missing := validateRequiredOutputKeys(fix.Payload, role.RequiredOutputKeys)
	violations := EvaluatePlausibility(fix.Payload, role.PlausibilityRules)
	failed := len(missing) > 0 || len(violations) > 0

	switch fix.Expect {
	case "pass":
		if failed {
			t.Errorf("fixture expected to PASS validation but failed.\n"+
				"  reason: %s\n"+
				"  missing keys: %v\n"+
				"  plausibility violations: %v",
				fix.Reason, missing, violations)
		}
	case "fail":
		if !failed {
			t.Errorf("fixture expected to FAIL validation but passed.\n"+
				"  reason: %s",
				fix.Reason)
		}
	}
}
