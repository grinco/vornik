package agentbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The tripwire sets must satisfy the harness's own tier rules before any sweep
// runs them — ValidateTaskTiers is what a release gate calls, and a set that
// fails it would be discovered at run time, hours into a sweep.
func TestTripwireTaskSetsAreValid(t *testing.T) {
	sets, err := filepath.Glob("tasksets/*-tripwire-*.json")
	if err != nil || len(sets) == 0 {
		t.Fatalf("no tripwire task sets found: %v", err)
	}
	for _, path := range sets {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var tasks []TaskSpec
			if err := json.Unmarshal(raw, &tasks); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(tasks) == 0 {
				t.Fatal("empty task set")
			}
			if err := ValidateTaskTiers(tasks); err != nil {
				t.Fatalf("tier validation: %v", err)
			}
			for _, task := range tasks {
				if task.Tier != TaskTierTripwire {
					t.Errorf("task %q has tier %q in a tripwire set — a set mixing tiers "+
						"hides which tasks can alarm", task.ID, task.Tier)
				}
				// A tripwire must pass EVERY calibration repeat
				// (validateCalibrationTier), so it carries no scoring policy and
				// never enters a score denominator. One that declared scoring
				// would imply it grades something, which it cannot.
				if task.Scoring != nil {
					t.Errorf("tripwire %q declares a scoring policy; tripwires alarm, "+
						"they do not grade", task.ID)
				}
				if task.Workflow == "" || task.Prompt == "" {
					t.Errorf("task %q needs both a workflow and a prompt", task.ID)
				}
			}
		})
	}
}

// Every workflow named by a tripwire task must actually be a shipped workflow —
// a typo would produce a task that can never pass and a sweep that looks broken.
func TestTripwireWorkflowsAreShipped(t *testing.T) {
	shipped := map[string]bool{}
	files, err := filepath.Glob(filepath.Join("..", "..", "configs", "workflows", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped workflows found: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		shipped[name[:len(name)-len(".md")]] = true
	}

	sets, _ := filepath.Glob("tasksets/*-tripwire-*.json")
	covered := map[string]bool{}
	for _, path := range sets {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var tasks []TaskSpec
		if err := json.Unmarshal(raw, &tasks); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, task := range tasks {
			if !shipped[task.Workflow] {
				t.Errorf("tripwire %q names workflow %q, which is not shipped",
					task.ID, task.Workflow)
			}
			covered[task.Workflow] = true
		}
	}
	if len(covered) == 0 {
		t.Error("no workflow is covered by a tripwire — this guard is watching nothing")
	}
}

// An attachment a task set names must EXIST beside it. A missing fixture makes
// the task fail on staging — a failure unrelated to the thing being measured,
// and one that would read as "this workflow is broken".
func TestTripwireAttachmentsExist(t *testing.T) {
	sets, _ := filepath.Glob("tasksets/*-tripwire-*.json")
	checked := 0
	for _, path := range sets {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var tasks []TaskSpec
		if err := json.Unmarshal(raw, &tasks); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, task := range tasks {
			for _, att := range task.Attachments {
				checked++
				resolved := filepath.Join(filepath.Dir(path), att)
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("task %q attaches %q, which does not exist at %s",
						task.ID, att, resolved)
				}
			}
		}
	}
	if checked == 0 {
		t.Log("no attachments declared yet")
	}
}
