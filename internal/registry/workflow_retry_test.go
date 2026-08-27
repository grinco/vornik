package registry

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Ten workflows in the deployed tree declared a per-step `retry:` block whose
// four keys matched no Go field, so lenient yaml.Unmarshal dropped it
// silently. The comments above those blocks assert they were tuned from
// production telemetry at a stated confidence; the policy never executed.
//
// Design: https://docs.vornik.io
// (D1, D3)

const retryStepYAML = `
workflowId: "wf"
entrypoint: "research"
steps:
  research:
    type: agent
    role: researcher
    on_success: done
    on_fail: failed
    retry:
      on: ["llm_call_failed", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "30s"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
`

// TestRetryBlockIsNotSilentlyDropped — the regression this whole change
// exists for. Named for discoverability, not for the file it was found in.
func TestRetryBlockIsNotSilentlyDropped(t *testing.T) {
	var w Workflow
	if err := yaml.Unmarshal([]byte(retryStepYAML), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	step, ok := w.Steps["research"]
	if !ok {
		t.Fatal("step research did not parse")
	}
	if len(step.Retry.On) != 2 {
		t.Fatalf("retry.on dropped: got %#v", step.Retry.On)
	}
	if step.Retry.MaxAttempts != 5 {
		t.Errorf("retry.max_attempts = %d, want 5", step.Retry.MaxAttempts)
	}
	if step.Retry.Backoff != "exponential" {
		t.Errorf("retry.backoff = %q, want exponential", step.Retry.Backoff)
	}
	if step.Retry.InitialDelay != "30s" {
		t.Errorf("retry.initial_delay = %q, want 30s", step.Retry.InitialDelay)
	}
}

// D3: an `on:` entry naming a class the executor can never emit would never
// match, which is exactly the silent failure this design closes. The ten
// deployed configs name `container_non_zero_exit`, which migration 170
// removed — so without this validation, wiring retry.on would recreate the
// defect on the same day it was fixed.
func TestRetryOnRejectsUnknownErrorClass(t *testing.T) {
	src := strings.Replace(retryStepYAML,
		`["llm_call_failed", "context_timeout"]`,
		`["container_non_zero_exit"]`, 1)
	var w Workflow
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal("a retry.on naming a class the executor cannot emit must fail validation")
	}
	msg := err.Error()
	if !strings.Contains(msg, "container_non_zero_exit") {
		t.Errorf("error must name the offending class: %q", msg)
	}
	// The operator has to be able to fix it without reading Go source.
	if !strings.Contains(msg, "llm_call_failed") && !strings.Contains(msg, "unclassified") {
		t.Errorf("error must list valid classes so the fix is obvious: %q", msg)
	}
}

// Known classes validate cleanly.
func TestRetryOnAcceptsKnownErrorClasses(t *testing.T) {
	var w Workflow
	if err := yaml.Unmarshal([]byte(retryStepYAML), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := w.Validate("wf.md"); err != nil {
		t.Fatalf("valid classes must validate: %v", err)
	}
}

// An absent retry block is the overwhelmingly common case and must stay valid
// — G3 says omitting the config reproduces today's behaviour exactly.
func TestRetryBlockIsOptional(t *testing.T) {
	src := strings.Replace(retryStepYAML, `    retry:
      on: ["llm_call_failed", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "30s"
`, "", 1)
	var w Workflow
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := w.Validate("wf.md"); err != nil {
		t.Fatalf("a step with no retry block must validate: %v", err)
	}
	if len(w.Steps["research"].Retry.On) != 0 {
		t.Error("absent retry block must leave a zero value, not a default list")
	}
}

// A field that is accepted and then quietly ignored is the defect this whole
// change closes. Two of them shipped in the first cut and were caught by
// review: `initial_delay: "30"` (no unit) parsed as YAML, failed
// time.ParseDuration at runtime and silently fell back to the default; and
// `backoff:` was accepted with any value while the ladder is exponential
// regardless. Both now fail at load.
func TestRetryInitialDelayMustParse(t *testing.T) {
	src := strings.Replace(retryStepYAML, `initial_delay: "30s"`, `initial_delay: "30"`, 1)
	var w Workflow
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal(`initial_delay: "30" has no unit and must be rejected, not silently ignored`)
	}
	if !strings.Contains(err.Error(), "initial_delay") {
		t.Errorf("error must name the field: %v", err)
	}
}

func TestRetryBackoffRejectsUnsupportedStrategy(t *testing.T) {
	src := strings.Replace(retryStepYAML, `backoff: "exponential"`, `backoff: "fibonacci"`, 1)
	var w Workflow
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal("an unsupported backoff strategy must be rejected rather than accepted and ignored")
	}
	if !strings.Contains(err.Error(), "backoff") {
		t.Errorf("error must name the field: %v", err)
	}
}

// The supported value, and an omitted one, both stay valid.
func TestRetryBackoffAcceptsSupportedAndEmpty(t *testing.T) {
	for _, v := range []string{`backoff: "exponential"`, ""} {
		src := strings.Replace(retryStepYAML, `      backoff: "exponential"`, "      "+v, 1)
		var w Workflow
		if err := yaml.Unmarshal([]byte(src), &w); err != nil {
			t.Fatalf("unmarshal %q: %v", v, err)
		}
		if err := w.Validate("wf.md"); err != nil {
			t.Errorf("backoff %q must validate: %v", v, err)
		}
	}
}

// stepFieldNames reflects over WorkflowStep's tags to decide which keys are
// known. That loop does not traverse embedded structs, so if one is ever
// added its fields would silently become "unknown" and every config using
// them would fail to load. Fail here instead, at the point the mistake is made.
func TestWorkflowStepHasNoEmbeddedFields(t *testing.T) {
	tp := reflect.TypeOf(WorkflowStep{})
	for i := 0; i < tp.NumField(); i++ {
		if tp.Field(i).Anonymous {
			t.Fatalf("WorkflowStep gained an embedded field %q; stepFieldNames does not "+
				"traverse embedded structs, so its fields would be rejected as unknown keys",
				tp.Field(i).Name)
		}
	}
}
