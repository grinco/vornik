package memetic

// Regression tests for the architect's corrective retry (2026-06-06).
// Operator report: generate-candidate failed repeatedly across days
// with "architect confidence below threshold: 0.00 < 0.60" — the
// architect model (minimax-m2 in the deployment) intermittently emits
// a syntactically-valid JSON object that OMITS the confidence field,
// which decoded to the float zero value and masqueraded as an honest
// low-confidence verdict. Omission is now a malformed-output error,
// and malformed output gets exactly ONE corrective retry (mirroring
// the executor's shape-retry-with-one-hint pattern). An EXPLICIT low
// confidence stays an honest no-retry rejection.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// seqProvider returns scripted responses in order, recording every
// message batch it was called with.
type seqProvider struct {
	contents []string
	calls    int
	batches  [][]chat.Message
}

func (s *seqProvider) Complete(_ context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	s.batches = append(s.batches, append([]chat.Message(nil), messages...))
	content := s.contents[len(s.contents)-1]
	if s.calls < len(s.contents) {
		content = s.contents[s.calls]
	}
	s.calls++
	return &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Index: 0, Message: chat.Message{Role: "assistant", Content: content}, FinishReason: "stop"},
		},
	}, nil
}
func (s *seqProvider) CompleteWithTools(ctx context.Context, m []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.Complete(ctx, m)
}
func (s *seqProvider) CompleteWithToolsStream(ctx context.Context, m []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.Complete(ctx, m)
}
func (s *seqProvider) Model() string              { return "seq-model" }
func (s *seqProvider) SetMetrics(_ *chat.Metrics) {}

func newArchitectWithSeq(t *testing.T, provider *seqProvider, evidence []string) (*Architect, *stubProposalSink) {
	t.Helper()
	sink := &stubProposalSink{}
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	a := New(
		provider,
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		sink,
		DefaultConfig(),
	)
	return a, sink
}

// missingConfidenceOutput is buildOutput minus the confidence field —
// a syntactically valid object that previously decoded to 0.00.
func missingConfidenceOutput(workflowID, yamlContent string, evidence []string) string {
	full := buildOutput(workflowID, yamlContent, "fix it", evidence, 0.9)
	// Drop the confidence line from the indented JSON.
	var kept []string
	for _, line := range strings.Split(full, "\n") {
		if strings.Contains(line, `"confidence"`) {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	// The field before confidence now carries a trailing comma before
	// the brace — normalise.
	out = strings.ReplaceAll(out, ",\n}", "\n}")
	return out
}

func TestParseArchitectOutput_MissingConfidenceIsMalformed(t *testing.T) {
	raw := missingConfidenceOutput("wf", "yaml", []string{"e1", "e2", "e3"})
	_, err := parseArchitectOutput(raw)
	if !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("err = %v, want ErrMalformedOutput for omitted confidence", err)
	}
	if !strings.Contains(err.Error(), "confidence") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

// TestArchitect_Propose_RetriesOnceOnMissingConfidence — first reply
// omits confidence, the corrective retry succeeds. The second call
// must carry the corrective hint (and the failed reply as context).
func TestArchitect_Propose_RetriesOnceOnMissingConfidence(t *testing.T) {
	evidence := []string{"e1", "e2", "e3"}
	bad := missingConfidenceOutput("simple-workflow", fixtureWorkflowYAML, evidence)
	good := buildOutput("simple-workflow", fixtureWorkflowYAML, "fix it", evidence, 0.9)
	p := &seqProvider{contents: []string{bad, good}}
	a, sink := newArchitectWithSeq(t, p, evidence)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got == nil || sink.inserted == nil {
		t.Fatal("no proposal inserted after successful retry")
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (one corrective retry)", p.calls)
	}
	last := p.batches[1]
	corrective := last[len(last)-1]
	if corrective.Role != "user" || !strings.Contains(corrective.Content, "confidence") {
		t.Errorf("retry must end with a corrective user hint naming the failure; got role=%q content=%q",
			corrective.Role, corrective.Content)
	}
}

// TestArchitect_Propose_MalformedTwiceFailsAfterOneRetry — the retry
// budget is exactly one; two bad replies surface ErrMalformedOutput.
func TestArchitect_Propose_MalformedTwiceFailsAfterOneRetry(t *testing.T) {
	p := &seqProvider{contents: []string{"not json at all", "still not json"}}
	a, _ := newArchitectWithSeq(t, p, nil)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("err = %v, want ErrMalformedOutput", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want exactly 2", p.calls)
	}
}

// TestArchitect_Propose_ExplicitLowConfidenceNoRetry — an explicit
// below-floor confidence is an honest verdict, not a shape failure:
// no retry (retrying would pressure the model to inflate).
func TestArchitect_Propose_ExplicitLowConfidenceNoRetry(t *testing.T) {
	evidence := []string{"e1", "e2", "e3"}
	low := buildOutput("simple-workflow", fixtureWorkflowYAML, "meh", evidence, 0.2)
	p := &seqProvider{contents: []string{low}}
	a, _ := newArchitectWithSeq(t, p, evidence)

	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrLowConfidence) {
		t.Fatalf("err = %v, want ErrLowConfidence", err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (no retry on an honest verdict)", p.calls)
	}
}
