package memetic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// Validated WORKFLOW.md fixture. Mirrors configs/workflows/simple-
// workflow.md verbatim so registry.ValidateWorkflowMarkdown
// approves it.
const fixtureWorkflowYAML = `---
workflowId: "simple-workflow"
displayName: "Simple Development Workflow"
description: "Lightweight plan → implement → review pipeline for small one-shot development tasks; the reviewer can loop the coder once if the work isn't approved."
version: "1.1.0"
maxStepVisits: 3
maxWallClock: "1h"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "implement"
    on_fail: "failed"
    timeout: "15m"
  implement:
    type: "agent"
    role: "coder"
    on_success: "review"
    on_fail: "failed"
    timeout: "60m"
    retryPolicy:
      maxRetries: 2
      backoff: "exponential"
  review:
    type: "agent"
    role: "reviewer"
    on_fail: "failed"
    timeout: "30m"
    gates:
      - condition: "review.approved == true"
        target: "complete"
      - condition: "review.approved == false"
        target: "implement"
terminals:
  complete:
    status: "COMPLETED"
    message: "Task completed successfully"
  failed:
    status: "FAILED"
    message: "Task failed"
  cancelled:
    status: "CANCELLED"
    message: "Task was cancelled"
---

# Simple Development Workflow

## Prompts

### plan

Analyze the task and create an implementation plan.

### implement

Implement the feature or fix according to the plan.

### review

Review the implementation for correctness and quality.
`

// stubProvider implements chat.Provider for the architect tests.
// Mirrors the dispatcher/intent_judge_test.go pattern so the
// surfaces don't drift.
type stubProvider struct {
	content        string
	err            error
	model          string
	lastMessages   []chat.Message
	lastRespFormat *chat.ResponseFormat
}

func (s *stubProvider) Complete(ctx context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	s.lastMessages = append([]chat.Message(nil), messages...)
	s.lastRespFormat = chat.ResponseFormatStructFromContext(ctx)
	if s.err != nil {
		return nil, s.err
	}
	return &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Index: 0, Message: chat.Message{Role: "assistant", Content: s.content}, FinishReason: "stop"},
		},
	}, nil
}
func (s *stubProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.Complete(context.TODO(), nil)
}
func (s *stubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.Complete(context.TODO(), nil)
}
func (s *stubProvider) Model() string {
	if s.model == "" {
		return "stub-model"
	}
	return s.model
}
func (s *stubProvider) SetMetrics(_ *chat.Metrics) {}

var _ chat.Provider = (*stubProvider)(nil)

type stubTelemetry struct {
	rollup *workflowtelemetry.Rollup
	err    error
}

func (s *stubTelemetry) ForWorkflow(_ context.Context, workflowID string, _ time.Time) (*workflowtelemetry.Rollup, error) {
	return s.rollup, s.err
}

type stubWorkflowSource struct {
	yaml []byte
	err  error
}

func (s *stubWorkflowSource) Load(_ context.Context, _ string) ([]byte, error) {
	return s.yaml, s.err
}

type stubExecLookup struct {
	validIDs map[string]bool
	err      error
}

func (s *stubExecLookup) BelongsTo(_ context.Context, _ string, ids []string) ([]string, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	var valid []string
	allValid := true
	for _, id := range ids {
		if s.validIDs[id] {
			valid = append(valid, id)
		} else {
			allValid = false
		}
	}
	return valid, allValid, nil
}

type stubProposalSink struct {
	inserted *persistence.WorkflowProposal
	err      error
}

func (s *stubProposalSink) Insert(_ context.Context, p *persistence.WorkflowProposal) error {
	if s.err != nil {
		return s.err
	}
	s.inserted = p
	return nil
}

// buildOutput renders the canonical LLM-output JSON used across
// happy-path tests. Takes the YAML as input so a test can pre-
// invalidate it (e.g. blank string for the YAML-validity test).
func buildOutput(workflowID, yamlContent, motivation string, evidence []string, confidence float32) string {
	out := ArchitectOutput{
		WorkflowID:     workflowID,
		ProposedYAML:   yamlContent,
		Motivation:     motivation,
		EvidenceRunIDs: evidence,
		Confidence:     confidence,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b)
}

func newArchitectWithStubs(t *testing.T, content string, evidence []string) (*Architect, *stubProposalSink) {
	t.Helper()
	sink := &stubProposalSink{}
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	a := New(
		&stubProvider{content: content},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		sink,
		DefaultConfig(),
	)
	return a, sink
}

// TestArchitect_Propose_HappyPath — LLM returns a well-formed JSON
// proposal, all gates pass, repository receives the row.
func TestArchitect_Propose_HappyPath(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"based on 9 runs, the implement→review→implement loop is the dominant failure path",
		evidence, 0.75)
	a, sink := newArchitectWithStubs(t, out, evidence)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got == nil || sink.inserted == nil {
		t.Fatal("expected an inserted proposal")
	}
	if got.WorkflowID != "simple-workflow" {
		t.Errorf("workflow_id not threaded: %q", got.WorkflowID)
	}
	if got.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("status should default to pending: %q", got.Status)
	}
	if got.ArchitectModel != "stub-model" {
		t.Errorf("architect_model: %q", got.ArchitectModel)
	}
	if len(got.EvidenceRunIDs) != 3 {
		t.Errorf("evidence not threaded: %v", got.EvidenceRunIDs)
	}
	if got.ID == "" || !strings.HasPrefix(got.ID, "wpr_") {
		t.Errorf("ID should start with wpr_: %q", got.ID)
	}
}

// TestArchitect_ProposeWithEvidence_AddsDetectorRunsToPrompt pins
// the blackbox-trigger handoff: evidence captured by the detector is
// available to the LLM, then still validated after the LLM cites it.
func TestArchitect_ProposeWithEvidence_AddsDetectorRunsToPrompt(t *testing.T) {
	evidence := []string{"exec_1", "exec_2", "exec_3", "exec_4", "exec_5"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"five detector runs show the same failure class",
		evidence[:3], 0.75)
	provider := &stubProvider{content: out}
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	a := New(
		provider,
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		&stubProposalSink{},
		DefaultConfig(),
	)
	if _, err := a.ProposeWithEvidence(context.Background(), "simple-workflow", evidence); err != nil {
		t.Fatalf("ProposeWithEvidence: %v", err)
	}
	if len(provider.lastMessages) != 2 {
		t.Fatalf("messages: %d, want 2", len(provider.lastMessages))
	}
	user := provider.lastMessages[1].Content
	for _, want := range []string{"Candidate evidence run IDs from detector", "exec_1", "exec_5"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestArchitect_Propose_PerWorkflowDisabled — LEVEL 2 of the kill
// switch. A workflow whose frontmatter sets architect_enabled: false
// short-circuits Propose before any LLM call.
func TestArchitect_Propose_PerWorkflowDisabled(t *testing.T) {
	disabledYAML := "---\nworkflowId: \"wf-x\"\narchitect_enabled: false\nsteps:\n  a:\n    type: agent\n"
	a := New(
		&stubProvider{content: "{}"},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "wf-x", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(disabledYAML)},
		&stubExecLookup{validIDs: map[string]bool{}},
		&stubProposalSink{},
		DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "wf-x")
	if !errors.Is(err, ErrArchitectDisabledForWorkflow) {
		t.Fatalf("want ErrArchitectDisabledForWorkflow, got %v", err)
	}
}

// TestArchitect_Propose_PerWorkflowEnabledByDefault — absent
// architect_enabled key means enabled (fail-open). Pins that a
// workflow without the flag still gets proposals.
func TestArchitect_Propose_PerWorkflowEnabledByDefault(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "m", evidence, 0.75)
	a, sink := newArchitectWithStubs(t, out, evidence)
	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if sink.inserted == nil {
		t.Fatal("expected proposal inserted when architect_enabled is absent")
	}
}

// TestArchitect_Propose_ThreadsKind — a kind in the architect output
// lands on the proposal row.
func TestArchitect_Propose_ThreadsKind(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := ArchitectOutput{
		WorkflowID: "simple-workflow", ProposedYAML: fixtureWorkflowYAML,
		Motivation: "m", EvidenceRunIDs: evidence, Confidence: 0.75,
		Kind: string(persistence.WorkflowProposalKindChangeTimeout),
	}
	b, _ := json.Marshal(out)
	a, sink := newArchitectWithStubs(t, string(b), evidence)
	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Kind != persistence.WorkflowProposalKindChangeTimeout {
		t.Errorf("kind not threaded: %q", got.Kind)
	}
	_ = sink
}

// TestArchitect_Propose_DefaultsKindUnspecified — output without a
// kind defaults to the sentinel.
func TestArchitect_Propose_DefaultsKindUnspecified(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "m", evidence, 0.75)
	a, _ := newArchitectWithStubs(t, out, evidence)
	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Kind != persistence.WorkflowProposalKindUnspecified {
		t.Errorf("kind should default to unspecified, got %q", got.Kind)
	}
}

// TestArchitect_Propose_UnknownKindRejected — a hallucinated kind
// outside the closed set is rejected as malformed output.
func TestArchitect_Propose_UnknownKindRejected(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := ArchitectOutput{
		WorkflowID: "simple-workflow", ProposedYAML: fixtureWorkflowYAML,
		Motivation: "m", EvidenceRunIDs: evidence, Confidence: 0.75,
		Kind: "change_prompt", // out of the closed set
	}
	b, _ := json.Marshal(out)
	a, _ := newArchitectWithStubs(t, string(b), evidence)
	if _, err := a.Propose(context.Background(), "simple-workflow"); !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("want ErrMalformedOutput for unknown kind, got %v", err)
	}
}

// TestArchitect_Propose_PerClassDisabled — LEVEL 3 of the kill
// switch. A proposal whose kind is in DisabledKinds is dropped before
// insert.
func TestArchitect_Propose_PerClassDisabled(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := ArchitectOutput{
		WorkflowID: "simple-workflow", ProposedYAML: fixtureWorkflowYAML,
		Motivation: "m", EvidenceRunIDs: evidence, Confidence: 0.75,
		Kind: string(persistence.WorkflowProposalKindAddStep),
	}
	b, _ := json.Marshal(out)
	sink := &stubProposalSink{}
	lookup := &stubExecLookup{validIDs: map[string]bool{"exec_a": true, "exec_b": true, "exec_c": true}}
	cfg := DefaultConfig()
	cfg.DisabledKinds = map[string]bool{string(persistence.WorkflowProposalKindAddStep): true}
	a := New(
		&stubProvider{content: string(b)},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup, sink, cfg,
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrProposalKindDisabled) {
		t.Fatalf("want ErrProposalKindDisabled, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("disabled-kind proposal must NOT be inserted")
	}
}

// TestArchitect_Propose_LowConfidence — confidence below 0.6 floor
// is dropped before the YAML validator or DB insert run.
func TestArchitect_Propose_LowConfidence(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "weak signal", evidence, 0.4)
	a, sink := newArchitectWithStubs(t, out, evidence)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrLowConfidence) {
		t.Fatalf("want ErrLowConfidence, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("repository should NOT receive a row for low-confidence proposals")
	}
}

// TestArchitect_Propose_InsufficientEvidence — < 3 evidence IDs
// rejected before any other gate.
func TestArchitect_Propose_InsufficientEvidence(t *testing.T) {
	evidence := []string{"only-one"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "trying", evidence, 0.8)
	a, sink := newArchitectWithStubs(t, out, evidence)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrInsufficientEvidence) {
		t.Fatalf("want ErrInsufficientEvidence, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("no insert on insufficient-evidence")
	}
}

// TestArchitect_Propose_EvidenceFromWrongWorkflow — IDs that don't
// belong to this workflow's executions are rejected. Guards
// against the LLM padding evidence with unrelated runs.
func TestArchitect_Propose_EvidenceFromWrongWorkflow(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "motivation", evidence, 0.8)
	sink := &stubProposalSink{}
	// Lookup says only "a" and "b" belong; "c" doesn't.
	lookup := &stubExecLookup{validIDs: map[string]bool{"a": true, "b": true}}
	a := New(
		&stubProvider{content: out},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup, sink, DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrEvidenceInvalid) {
		t.Fatalf("want ErrEvidenceInvalid, got %v", err)
	}
}

// TestArchitect_Propose_WorkflowMismatch — LLM emits a proposal
// targeting a different workflow than the one we asked about.
// Catches hallucination.
func TestArchitect_Propose_WorkflowMismatch(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	out := buildOutput("wrong-workflow", fixtureWorkflowYAML, "motivation", evidence, 0.8)
	a, sink := newArchitectWithStubs(t, out, evidence)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrWorkflowMismatch) {
		t.Fatalf("want ErrWorkflowMismatch, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("no insert on workflow mismatch")
	}
}

// TestArchitect_Propose_InvalidYAML — proposed YAML fails
// WORKFLOW.md validation; the proposal is dropped before insert.
func TestArchitect_Propose_InvalidYAML(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	// Frontmatter without required workflowId field.
	badYAML := "---\ndisplayName: \"x\"\n---\n# body\n"
	out := buildOutput("simple-workflow", badYAML, "motivation", evidence, 0.8)
	a, sink := newArchitectWithStubs(t, out, evidence)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrProposalYAMLInvalid) {
		t.Fatalf("want ErrProposalYAMLInvalid, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("no insert on invalid YAML")
	}
}

// TestArchitect_Propose_EmptyYAML — proposed_yaml is "" (or
// whitespace). Caught before the validator runs.
func TestArchitect_Propose_EmptyYAML(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	out := buildOutput("simple-workflow", "   \n", "motivation", evidence, 0.8)
	a, _ := newArchitectWithStubs(t, out, evidence)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrProposalYAMLInvalid) {
		t.Fatalf("want ErrProposalYAMLInvalid for empty YAML, got %v", err)
	}
}

// TestArchitect_Propose_MalformedJSON — LLM returns non-JSON
// text. Maps to ErrMalformedOutput so the admin endpoint returns
// 502.
func TestArchitect_Propose_MalformedJSON(t *testing.T) {
	a, _ := newArchitectWithStubs(t, "not json at all", nil)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("want ErrMalformedOutput, got %v", err)
	}
}

// TestArchitect_Propose_OutputTooLarge — completion past
// MaxOutputBytes rejected before the parse attempt. Guards
// against a runaway emit.
func TestArchitect_Propose_OutputTooLarge(t *testing.T) {
	a, _ := newArchitectWithStubs(t, strings.Repeat("a", 1024), nil)
	a.cfg.MaxOutputBytes = 100
	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("want ErrMalformedOutput for oversize output, got %v", err)
	}
}

// TestArchitect_Propose_StripsCodeFence — open-weight models
// often wrap their output in ```json … ``` even when told not to.
// The parser tolerates one leading + trailing fence so a single-
// fence emission doesn't kill the architect run.
func TestArchitect_Propose_StripsCodeFence(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	body := buildOutput("simple-workflow", fixtureWorkflowYAML, "motivation", evidence, 0.7)
	fenced := "```json\n" + body + "\n```"
	a, sink := newArchitectWithStubs(t, fenced, evidence)
	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("fenced output should parse, got %v", err)
	}
	if sink.inserted == nil {
		t.Error("no insert after fenced-output round trip")
	}
}

// TestArchitect_Propose_StripsProsePreamble reproduces the
// blackbox "Generate candidate" failure: the architect model
// emitted a prose preamble ("Looking at the telemetry…") ahead of
// the JSON object, which the strict decoder rejected with
// "invalid character 'L' looking for beginning of value". The
// parser now carves out the outermost {…} span so a model that
// ignores the JSON-only instruction (intermittently, depending on
// the per-workflow prompt) still yields a usable proposal.
func TestArchitect_Propose_StripsProsePreamble(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	body := buildOutput("simple-workflow", fixtureWorkflowYAML, "motivation", evidence, 0.7)
	noisy := "Looking at the telemetry, the dominant failure path suggests a structural fix:\n\n" + body + "\n\nThat is my recommendation."
	a, sink := newArchitectWithStubs(t, noisy, evidence)
	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("prose-wrapped output should parse, got %v", err)
	}
	if sink.inserted == nil {
		t.Error("no insert after prose-wrapped-output round trip")
	}
}

// TestArchitect_Propose_SetsJSONResponseFormat asserts the architect
// stamps a json_object response_format on the request context before
// the LLM call — the provider-level half of the JSON-reliability fix.
// Providers that honor it (OpenAI-compatible / bedrock) won't emit a
// prose preamble in the first place.
func TestArchitect_Propose_SetsJSONResponseFormat(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "motivation", evidence, 0.8)
	a, _ := newArchitectWithStubs(t, out, evidence)
	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	sp, ok := a.provider.(*stubProvider)
	if !ok {
		t.Fatalf("provider is not *stubProvider: %T", a.provider)
	}
	if sp.lastRespFormat == nil {
		t.Fatal("architect did not set a response_format on the request context")
	}
	if sp.lastRespFormat.Type != "json_object" {
		t.Errorf("response_format type = %q, want json_object", sp.lastRespFormat.Type)
	}
}

// TestArchitect_Propose_RateLimitPropagated — repository returns
// ErrProposalRateLimited (the partial-unique-index 23505 case);
// the architect surfaces it verbatim so the API returns 429.
func TestArchitect_Propose_RateLimitPropagated(t *testing.T) {
	evidence := []string{"a", "b", "c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "motivation", evidence, 0.8)
	sink := &stubProposalSink{err: persistence.ErrProposalRateLimited}
	lookup := &stubExecLookup{validIDs: map[string]bool{"a": true, "b": true, "c": true}}
	a := New(
		&stubProvider{content: out},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup, sink, DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, persistence.ErrProposalRateLimited) {
		t.Fatalf("want ErrProposalRateLimited propagated, got %v", err)
	}
}

// TestArchitect_Propose_LLMError — provider returns an error;
// the architect wraps it so the caller can read context.
func TestArchitect_Propose_LLMError(t *testing.T) {
	sink := &stubProposalSink{}
	a := New(
		&stubProvider{err: fmt.Errorf("gateway timeout")},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		nil, sink, DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if err == nil || !strings.Contains(err.Error(), "LLM call failed") {
		t.Fatalf("want LLM-failure error, got %v", err)
	}
}

// TestArchitect_Propose_TelemetryError — telemetry source fails;
// architect surfaces a wrapped error.
func TestArchitect_Propose_TelemetryError(t *testing.T) {
	a := New(
		&stubProvider{content: "{}"},
		&stubTelemetry{err: fmt.Errorf("db down")},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		nil, &stubProposalSink{}, DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if err == nil || !strings.Contains(err.Error(), "telemetry rollup") {
		t.Fatalf("want telemetry error, got %v", err)
	}
}

// TestArchitect_Propose_WorkflowSourceError — workflow loader
// fails; architect surfaces a wrapped error.
func TestArchitect_Propose_WorkflowSourceError(t *testing.T) {
	a := New(
		&stubProvider{content: "{}"},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{}},
		&stubWorkflowSource{err: fmt.Errorf("file not found")},
		nil, &stubProposalSink{}, DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if err == nil || !strings.Contains(err.Error(), "load workflow") {
		t.Fatalf("want load-workflow error, got %v", err)
	}
}

// TestArchitect_Propose_EmptyWorkflowID — guard catches the empty
// input before any IO.
func TestArchitect_Propose_EmptyWorkflowID(t *testing.T) {
	a := New(&stubProvider{}, &stubTelemetry{}, &stubWorkflowSource{}, nil, &stubProposalSink{}, DefaultConfig())
	if _, err := a.Propose(context.Background(), ""); err == nil {
		t.Error("empty workflowID should error")
	}
}

// TestDefaultConfig_PinsDesignValues — pins the design doc's
// invariants so a "let's lower the floor" change has to update
// both this test and the doc.
func TestDefaultConfig_PinsDesignValues(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MinEvidenceRunIDs != 3 {
		t.Errorf("MinEvidenceRunIDs design value is 3, got %d", cfg.MinEvidenceRunIDs)
	}
	if cfg.MinConfidence != 0.6 {
		t.Errorf("MinConfidence design value is 0.6, got %v", cfg.MinConfidence)
	}
	if cfg.Lookback != 7*24*time.Hour {
		t.Errorf("Lookback design value is 7d, got %v", cfg.Lookback)
	}
}
