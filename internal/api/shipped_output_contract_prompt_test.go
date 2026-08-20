package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Measured on the bench instance 2026-08-20, execution
// exec_20260820103337_ebc6c9ff68117391: dev-pipeline's `report` step failed on
// all four rungs of the retry ladder (report → report_shape_retry →
// report_model_fallback → report_model_fallback_shape_retry) with
// effective_tool_budget=25 and tool_calls_used=26 on every rung, 25 run_shell
// calls and ZERO file_write calls. The step never wrote its declared output
// file, so require_output_glob failed it every time.
//
// The cause was instruction ORDER, not the model and not the budget. The prompt
// asked the agent to "identify all commits that belong to this feature (since
// the last feature was completed)" at step 2, and to write the contract's
// output file at step 3. On a repo whose history carries no feature boundary —
// the bench project's log is a wall of "auto-commit: leftover work from
// task_..." with no tags and no merges — step 2 is unsatisfiable, so the agent
// spent its entire budget on git archaeology and never reached the write. The
// harness's own degenerate-loop message named this correctly ("commonly an
// unsatisfiable instruction"); one rung died that way after repeating the same
// run_shell four times.
//
// Raising the budget is not the fix: it buys more archaeology, not a write.
// Ordering is the fix, and it generalises — a step under require_output_glob
// must satisfy its contract from information it already has BEFORE it does any
// open-ended history discovery, so that an unbounded search can cost patch
// quality but never the contract itself.
//
// This test is deliberately not about the report step. It asserts the invariant
// for every shipped step that declares require_output_glob, because the trap is
// the shape (contract-critical write gated behind an unbounded search), and the
// next workflow to add an output contract will not remember this incident.
func TestShippedWorkflows_OutputContractWritePrecedesHistorySearch(t *testing.T) {
	files, err := filepath.Glob(shippedConfigsDir + "/workflows/*.md")
	if err != nil {
		t.Fatalf("glob shipped workflows: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no shipped workflows found under %s/workflows", shippedConfigsDir)
	}

	checked := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(raw)
		for stepID, glob := range outputGlobSteps(t, path, body) {
			section, ok := promptSection(body, stepID)
			if !ok {
				// No prose section: the contract is declared but the step has no
				// prompt to order. Nothing to assert.
				continue
			}
			checked++
			writeAt := strings.Index(section, glob)
			if writeAt < 0 {
				t.Errorf("%s step %q declares require_output_glob %q but its prompt "+
					"never names that path — the agent is asked to satisfy a contract "+
					"it is never told about",
					filepath.Base(path), stepID, glob)
				continue
			}
			searchAt := firstHistorySearch(section)
			if searchAt >= 0 && searchAt < writeAt {
				t.Errorf("%s step %q tells the agent to search git history (at offset "+
					"%d) BEFORE writing its declared output %q (at offset %d). An "+
					"unbounded search ahead of the contract-critical write is what "+
					"burned all 25 tool calls with zero writes in "+
					"exec_20260820103337_ebc6c9ff68117391; write the file first, then "+
					"discover",
					filepath.Base(path), stepID, searchAt, glob, writeAt)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no shipped step with require_output_glob AND a prompt section was " +
			"checked — the guard would pass vacuously")
	}
}

// historySearchCommands are read-only git commands whose result set is open
// ended on a real repo: the agent cannot know from the output alone whether it
// has found the boundary it was asked for, which is what makes them unbounded.
var historySearchCommands = []string{"git log", "git tag", "git show", "git rev-list"}

// firstHistorySearch returns the offset of the earliest history-search command
// in the section, or -1 if the prompt asks for none.
func firstHistorySearch(section string) int {
	first := -1
	for _, cmd := range historySearchCommands {
		if at := strings.Index(section, cmd); at >= 0 && (first < 0 || at < first) {
			first = at
		}
	}
	return first
}

// outputGlobSteps returns stepID -> require_output_glob for every step in the
// workflow's YAML front matter that declares one.
func outputGlobSteps(t *testing.T, path, body string) map[string]string {
	t.Helper()
	front, ok := frontMatter(body)
	if !ok {
		return nil
	}
	var doc struct {
		Steps map[string]struct {
			RequireOutputGlob string `yaml:"require_output_glob"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		t.Fatalf("parse front matter of %s: %v", path, err)
	}
	out := map[string]string{}
	for id, step := range doc.Steps {
		if step.RequireOutputGlob != "" {
			out[id] = step.RequireOutputGlob
		}
	}
	return out
}

// frontMatter returns the YAML block delimited by the leading --- fences.
func frontMatter(body string) (string, bool) {
	if !strings.HasPrefix(body, "---") {
		return "", false
	}
	rest := body[len("---"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// promptSection returns the prose under `### <stepID>`, up to the next heading
// at the same or a higher level.
func promptSection(body, stepID string) (string, bool) {
	header := "\n### " + stepID + "\n"
	start := strings.Index(body, header)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(header):]
	end := len(rest)
	for _, next := range []string{"\n### ", "\n## "} {
		if at := strings.Index(rest, next); at >= 0 && at < end {
			end = at
		}
	}
	return rest[:end], true
}
