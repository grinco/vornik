package erasure

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// Slice 5c: the generative step. Its output is never trusted — the caller verifies it
// — so these tests cover the wiring and the fail-closed cases rather than rewrite
// quality, which no unit test can assert.

type fakeProvider struct {
	reply    string
	err      error
	messages []chat.Message
}

func (f *fakeProvider) Complete(_ context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	f.messages = m
	if f.err != nil {
		return nil, f.err
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: f.reply}})
	return resp, nil
}

func TestNewRewriter_RefusesAHalfBuiltValue(t *testing.T) {
	if _, err := NewRewriter(nil, "m"); err == nil {
		t.Error("a nil provider must be refused at construction, not surface as a deferral " +
			"on every chunk of a live erasure")
	}
	if _, err := NewRewriter(&fakeProvider{}, "   "); err == nil {
		t.Error("a rewrite of someone's personal data must be attributable to a named model")
	}
	r, err := NewRewriter(&fakeProvider{}, "test-model")
	if err != nil {
		t.Fatalf("valid construction failed: %v", err)
	}
	if r.ModelVersion() != "test-model" {
		t.Errorf("ModelVersion = %q", r.ModelVersion())
	}
}

func TestRewriteWithout_SendsTheIdentifiersAndTheRecord(t *testing.T) {
	p := &fakeProvider{reply: "Called the client; Peter joined."}
	r, _ := NewRewriter(p, "test-model")

	got, err := r.RewriteWithout(context.Background(),
		"Called jane@example.com; Peter joined.", []string{"jane@example.com", "Jane Doe"})
	if err != nil {
		t.Fatalf("RewriteWithout: %v", err)
	}
	if got != "Called the client; Peter joined." {
		t.Errorf("unexpected rewrite: %q", got)
	}
	if len(p.messages) != 2 || p.messages[0].Role != "system" {
		t.Fatalf("expected a system + user pair, got %+v", p.messages)
	}
	user := p.messages[1].Content
	for _, want := range []string{"jane@example.com", "Jane Doe", "Called jane@example.com; Peter joined."} {
		if !strings.Contains(user, want) {
			t.Errorf("the prompt must carry %q", want)
		}
	}
	// The instructions that protect the OTHER subject are the ones the mechanical
	// check cannot enforce, so their presence is pinned.
	sys := p.messages[0].Content
	for _, want := range []string{"PRESERVE EVERYTHING ELSE", "DO NOT summarise"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the system prompt must instruct %q — over-redaction destroys third-party "+
				"data and verification cannot detect it", want)
		}
	}
}

// An empty reply must be an ERROR, never an empty rewrite: writing "" would delete
// the record, including the other subjects' data it was kept for.
func TestRewriteWithout_EmptyReplyIsAFailureNotAnEmptyRedaction(t *testing.T) {
	for _, reply := range []string{"", "   ", "\n\t"} {
		r, _ := NewRewriter(&fakeProvider{reply: reply}, "m")
		if _, err := r.RewriteWithout(context.Background(), "text", []string{"jane@example.com"}); err == nil {
			t.Errorf("reply %q must be rejected, not written as the record's new content", reply)
		}
	}
}

func TestRewriteWithout_ProviderErrorPropagates(t *testing.T) {
	r, _ := NewRewriter(&fakeProvider{err: errors.New("rate limited")}, "m")
	_, err := r.RewriteWithout(context.Background(), "text", []string{"jane@example.com"})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("the provider error must surface, got %v", err)
	}
}

// No identifiers means a model would be asked to remove nothing and would echo the
// text back — which could then verify clean and be reported as a redaction that never
// happened. Refuse instead.
func TestRewriteWithout_RefusesWithNoIdentifiers(t *testing.T) {
	r, _ := NewRewriter(&fakeProvider{reply: "unchanged"}, "m")
	for _, ids := range [][]string{nil, {}, {""}, {"  "}} {
		if _, err := r.RewriteWithout(context.Background(), "text", ids); err == nil {
			t.Errorf("identifiers %q must be refused — echoing the text back and calling it a "+
				"redaction would be a false report", ids)
		}
	}
}

func TestRewriteWithout_RefusesEmptyRecord(t *testing.T) {
	r, _ := NewRewriter(&fakeProvider{reply: "x"}, "m")
	if _, err := r.RewriteWithout(context.Background(), "   ", []string{"jane@example.com"}); err == nil {
		t.Error("an empty record cannot be redacted")
	}
}

// Models wrap output in code fences unprompted; left in place the backticks would be
// written into the stored record.
func TestRewriteWithout_StripsACodeFence(t *testing.T) {
	p := &fakeProvider{reply: "```\nCalled the client; Peter joined.\n```"}
	r, _ := NewRewriter(p, "m")
	got, err := r.RewriteWithout(context.Background(), "orig", []string{"jane@example.com"})
	if err != nil {
		t.Fatalf("RewriteWithout: %v", err)
	}
	if got != "Called the client; Peter joined." {
		t.Errorf("code fence not stripped: %q", got)
	}
	// A fenced block with a language tag, and text that merely CONTAINS backticks.
	p.reply = "```markdown\nBody here.\n```"
	if got, _ := r.RewriteWithout(context.Background(), "orig", []string{"x@y.z"}); got != "Body here." {
		t.Errorf("language-tagged fence not stripped: %q", got)
	}
	p.reply = "Use the ``` marker in code."
	if got, _ := r.RewriteWithout(context.Background(), "orig", []string{"x@y.z"}); got != "Use the ``` marker in code." {
		t.Errorf("non-fenced text containing backticks must be left alone: %q", got)
	}
}

// A nil receiver must not panic — the executor holds this behind an interface.
func TestRewriteWithout_NilReceiverErrors(t *testing.T) {
	var r *Rewriter
	if _, err := r.RewriteWithout(context.Background(), "t", []string{"a@b.c"}); err == nil {
		t.Error("a nil rewriter must error rather than panic")
	}
	if r.ModelVersion() != "unknown" {
		t.Error("a nil rewriter should still report a model string")
	}
}
