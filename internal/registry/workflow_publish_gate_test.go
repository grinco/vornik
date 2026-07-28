package registry

// Corpus invariant: a publisher step's success path must not be able to reach
// a COMPLETED terminal while the publisher itself reported failure.
//
// Incident T-1089 (2026-07-28): the publisher role's outputSchema deliberately
// blesses `published.ok: false` as a schema-VALID result (its plausibility rule
// only requires a `reason` when ok is false). A role honestly self-reporting "I
// did not publish" is therefore a valid SUCCESS, so the step's `on_success` edge
// fires. Every shipped publishing workflow routed publish.on_success straight
// to a COMPLETED terminal, so a failed publish reported the task COMPLETED. The
// shape-retry / model-fallback ladder does not help: it gates on schema
// validity, never on a declared outcome.
//
// The fix is a `gate` step on the publisher's success edge — the engine already
// evaluates gate conditions against the previous step's result.json via
// lookupJSONPath, so `published.ok == true` is directly expressible. Re-running
// the publisher is deliberately NOT the remedy: the publisher prompt warns that
// a retry can double-publish, so the gate routes rather than retries.
//
// This test pins the invariant across the whole shipped corpus so a new
// publishing workflow cannot silently reintroduce the hole.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishGateCondition is the condition a publisher's success gate must carry.
const publishGateCondition = "published.ok == true"

// TestShippedWorkflows_PublisherSuccessIsGated asserts that for every agent
// step using the `publisher` role, the success edge lands on a gate that can
// only reach a COMPLETED terminal when published.ok == true, and that every
// other outcome (ok false, or a malformed result with no published.ok at all)
// lands on a FAILED terminal.
func TestShippedWorkflows_PublisherSuccessIsGated(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	dir := filepath.Join(root, "configs", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read configs/workflows: %v", err)
	}

	checkedAny := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}
		wf, parseErr := ParseWorkflowMarkdown(content, entry.Name())
		if parseErr != nil || wf == nil {
			// Parse failures are the other tests' business, not this one's.
			continue
		}
		for stepID, step := range wf.Steps {
			if step.Type != "agent" || step.Role != "publisher" {
				continue
			}
			checkedAny = true
			assertPublisherGated(t, entry.Name(), wf, stepID, step)
		}
	}
	if !checkedAny {
		t.Fatal("no publisher steps found in configs/workflows — this invariant test would silently pass forever; check the corpus or the role name")
	}
}

func assertPublisherGated(t *testing.T, file string, wf *Workflow, stepID string, step WorkflowStep) {
	t.Helper()

	target := step.OnSuccess
	if target == "" {
		t.Errorf("%s: publisher step %q has no on_success target", file, stepID)
		return
	}
	// The success edge must NOT go straight to a terminal — that is the hole.
	if term, isTerminal := wf.Terminals[target]; isTerminal {
		t.Errorf("%s: publisher step %q routes on_success directly to terminal %q (status %s) — "+
			"a publisher reporting published.ok:false would report that status. "+
			"Route through a gate on %q instead (T-1089).",
			file, stepID, target, term.Status, publishGateCondition)
		return
	}
	gate, ok := wf.Steps[target]
	if !ok {
		t.Errorf("%s: publisher step %q on_success targets %q which is neither a step nor a terminal",
			file, stepID, target)
		return
	}
	if gate.Type != "gate" {
		t.Errorf("%s: publisher step %q on_success targets %q of type %q — expected a %q step enforcing %q (T-1089)",
			file, stepID, target, gate.Type, "gate", publishGateCondition)
		return
	}

	// The gate must have an ok==true branch, and it must not lead to FAILED.
	var okBranch *WorkflowGate
	for i := range gate.Gates {
		if normalizeCondition(gate.Gates[i].Condition) == publishGateCondition {
			okBranch = &gate.Gates[i]
			break
		}
	}
	if okBranch == nil {
		t.Errorf("%s: gate %q (guarding publisher %q) has no %q condition; got %v",
			file, target, stepID, publishGateCondition, conditionList(gate))
		return
	}
	if term, isTerminal := wf.Terminals[okBranch.Target]; isTerminal && term.Status == "FAILED" {
		t.Errorf("%s: gate %q routes the successful-publish branch to FAILED terminal %q",
			file, target, okBranch.Target)
	}

	// Every non-matching outcome must end FAILED. Two ways that can leak:
	//
	//  1. on_success set on the gate — runGateStep treats "no condition
	//     matched" + on_success as a CLEAN default fall-through, so a
	//     malformed publisher result (no published.ok key at all) would take
	//     it. If that route reaches COMPLETED the hole is reopened.
	//  2. on_fail unset or pointing somewhere that isn't a FAILED terminal —
	//     then a rejected publish has no defined failing destination.
	if gate.OnSuccess != "" {
		if term, isTerminal := wf.Terminals[gate.OnSuccess]; !isTerminal || term.Status != "FAILED" {
			t.Errorf("%s: gate %q sets on_success=%q — a publisher result with no published.ok "+
				"key falls through there cleanly. Leave on_success unset (or point it at a FAILED "+
				"terminal) so only the %q branch can complete (T-1089).",
				file, target, gate.OnSuccess, publishGateCondition)
		}
	}
	if gate.OnFail == "" {
		t.Errorf("%s: gate %q has no on_fail — a rejected publish has no failing destination (T-1089)",
			file, target)
		return
	}
	assertReachesFailed(t, file, wf, target, gate.OnFail)
}

// assertReachesFailed checks that dest is a FAILED terminal, or a step whose
// own success path ends at one (publish.md / research-and-publish.md interpose
// a `recover` plan step before the FAILED terminal, which is legitimate).
func assertReachesFailed(t *testing.T, file string, wf *Workflow, gateID, dest string) {
	t.Helper()
	seen := map[string]bool{}
	cur := dest
	for i := 0; i < 8; i++ {
		if seen[cur] {
			break
		}
		seen[cur] = true
		if term, isTerminal := wf.Terminals[cur]; isTerminal {
			if term.Status != "FAILED" {
				t.Errorf("%s: gate %q rejection path reaches terminal %q with status %s — expected FAILED (T-1089)",
					file, gateID, cur, term.Status)
			}
			return
		}
		step, ok := wf.Steps[cur]
		if !ok {
			t.Errorf("%s: gate %q rejection path references unknown target %q", file, gateID, cur)
			return
		}
		if step.OnSuccess == "" {
			t.Errorf("%s: gate %q rejection path stalls at step %q with no on_success", file, gateID, cur)
			return
		}
		cur = step.OnSuccess
	}
	t.Errorf("%s: gate %q rejection path did not reach a terminal within 8 hops", file, gateID)
}

// normalizeCondition collapses internal whitespace so
// "published.ok  ==  true" compares equal to the canonical form.
func normalizeCondition(c string) string {
	return strings.Join(strings.Fields(c), " ")
}

func conditionList(step WorkflowStep) []string {
	out := make([]string, 0, len(step.Gates))
	for _, g := range step.Gates {
		out = append(out, g.Condition+" -> "+g.Target)
	}
	return out
}
