package registry

import (
	"strings"
	"testing"
)

// D4 of the step-retry-configuration design.
//
// The root cause of the ten inert `retry:` blocks was not that key. It was
// that ANY unknown key in a workflow step is silently dropped by lenient
// yaml.Unmarshal, so a typo or an invented block is inert with no signal.
//
// Design: https://docs.vornik.io

const strictBaseYAML = `---
name: strict-check
workflowId: "strict-check"
description: "Fixture for strict-key validation."
version: "1.0.0"
entrypoint: "research"
steps:
  research:
    type: agent
    role: researcher
    on_success: done
    on_fail: failed
%s
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
---
## Prompts
### research
Do the thing.
`

func TestParseWorkflowMarkdown_RejectsUnknownStepKey(t *testing.T) {
	src := strings.Replace(strictBaseYAML, "%s", `    totally_invented_key: ["nothing", "reads", "this"]`, 1)
	_, err := ParseWorkflowMarkdown([]byte(src), "strict.md")
	if err == nil {
		t.Fatal("an unknown step key must fail to load, not be silently dropped")
	}
	if !strings.Contains(err.Error(), "totally_invented_key") {
		t.Errorf("the error must name the offending key: %v", err)
	}
}

// The dead spelling must fail loudly AND point at the replacement. A key that
// never worked has no gentle migration to offer, but the operator still needs
// the fix rather than only the failure.
func TestParseWorkflowMarkdown_RetryPolicyNamesItsReplacement(t *testing.T) {
	src := strings.Replace(strictBaseYAML, "%s", "    retryPolicy:\n      maxRetries: 3", 1)
	_, err := ParseWorkflowMarkdown([]byte(src), "strict.md")
	if err == nil {
		t.Fatal("retryPolicy was removed and must now fail to load")
	}
	msg := err.Error()
	if !strings.Contains(msg, "retryPolicy") {
		t.Errorf("error must name the removed key: %v", err)
	}
	if !strings.Contains(msg, "retry") {
		t.Errorf("error must point at the replacement spelling: %v", err)
	}
}

// The whole point: the block that shipped in ten workflows must now load.
func TestRetryBlockLoadsThroughTheMarkdownParser(t *testing.T) {
	src := strings.Replace(strictBaseYAML, "%s",
		"    retry:\n      on: [\"llm_call_failed\"]\n      max_attempts: 4", 1)
	wf, err := ParseWorkflowMarkdown([]byte(src), "strict.md")
	if err != nil {
		t.Fatalf("the shipped retry block must load: %v", err)
	}
	if wf.Steps["research"].Retry.MaxAttempts != 4 {
		t.Errorf("max_attempts = %d, want 4", wf.Steps["research"].Retry.MaxAttempts)
	}
}

// THE GATE CONTRACT. `vornikctl workflow validate` is the pre-deploy gate, and
// a gate that cannot see what the loader rejects is worse than no gate: it
// passes, and the daemon then fails at boot.
//
// Measured before this change: a workflow whose step carried BOTH an invented
// key and the dead retry block validated with 0 errors and exit 0. The
// validator parsed frontmatter into an eight-field shape struct and never
// looked at `steps:` at all.
func TestValidateWorkflowMarkdown_ReportsWhatTheLoaderRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		step string
	}{
		{"invented key", `    totally_invented_key: ["nothing", "reads", "this"]`},
		{"removed retryPolicy", "    retryPolicy:\n      maxRetries: 3"},
		{"unknown retry class", "    retry:\n      on: [\"container_non_zero_exit\"]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(strictBaseYAML, "%s", tc.step, 1)
			// The load path is parse THEN Validate — that is what
			// LoadWorkflows does (workflow.go:1406), and validation is where
			// the class check lives. Comparing against parse alone would test
			// the wrong level.
			loadErr := loadPathRejects([]byte(src), "strict.md")
			report := ValidateWorkflowMarkdown([]byte(src), "strict.md")
			if loadErr == nil {
				t.Fatal("fixture should be rejected by the load path")
			}
			if !report.HasErrors() {
				t.Fatalf("the loader rejects this but the validator reports clean — "+
					"the deploy gate would pass and the daemon would fail at boot. loader said: %v", loadErr)
			}
		})
	}
}

// ...and the converse: the validator must not invent errors the loader would
// accept, or the gate blocks good deploys.
func TestValidateWorkflowMarkdown_AcceptsWhatTheLoaderAccepts(t *testing.T) {
	src := strings.Replace(strictBaseYAML, "%s",
		"    retry:\n      on: [\"llm_call_failed\"]\n      max_attempts: 4", 1)
	if err := loadPathRejects([]byte(src), "strict.md"); err != nil {
		t.Fatalf("fixture should load: %v", err)
	}
	if report := ValidateWorkflowMarkdown([]byte(src), "strict.md"); report.HasErrors() {
		t.Errorf("validator reports errors on a config the loader accepts: %+v", report.Findings)
	}
}

// loadPathRejects is the SCHEMA half of what the load path does to one file:
// the strict decode, then the retry-class check. Scoped to match
// appendWorkflowSchemaFindings — the agreement contract is about the schema
// rules the validator could not previously see, not about workflowId or
// transition reachability, which the envelope validator has never owned.
func loadPathRejects(content []byte, filename string) error {
	wf, err := ParseWorkflowMarkdown(content, filename)
	if err != nil {
		return err
	}
	return wf.validateRetryClasses(filename)
}
