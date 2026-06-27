package memory

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestNewConfiguredReranker_DisabledIsNoop(t *testing.T) {
	r := NewConfiguredReranker(false, &titlerFakeProvider{}, "m", 20, 8, 600, zerolog.Nop())
	if _, ok := r.(NoopReranker); !ok {
		t.Fatalf("disabled must yield NoopReranker, got %T", r)
	}
}

func TestNewConfiguredReranker_NilClientIsNoop(t *testing.T) {
	r := NewConfiguredReranker(true, nil, "m", 20, 8, 600, zerolog.Nop())
	if _, ok := r.(NoopReranker); !ok {
		t.Fatalf("nil client must yield NoopReranker, got %T", r)
	}
}

func TestNewConfiguredReranker_EnabledBuildsLLMReranker(t *testing.T) {
	r := NewConfiguredReranker(true, &titlerFakeProvider{}, "gpt-oss:20b", 15, 8, 400, zerolog.Nop())
	llm, ok := r.(*LLMReranker)
	if !ok {
		t.Fatalf("enabled with a client must yield *LLMReranker, got %T", r)
	}
	if llm.Model != "gpt-oss:20b" {
		t.Errorf("Model = %q", llm.Model)
	}
	if llm.Timeout != 8*time.Second {
		t.Errorf("Timeout = %v, want 8s", llm.Timeout)
	}
	if llm.MaxCandidates != 15 || llm.MaxSnippetBytes != 400 {
		t.Errorf("MaxCandidates=%d MaxSnippetBytes=%d", llm.MaxCandidates, llm.MaxSnippetBytes)
	}
}

// The configured reranker must satisfy rerankerActive() so scored-sufficiency
// turns on: an LLM reranker is "active", a Noop is not.
func TestConfiguredReranker_DrivesRerankerActive(t *testing.T) {
	s := &Searcher{}
	s.SetReranker(NewConfiguredReranker(true, &titlerFakeProvider{}, "", 20, 8, 600, zerolog.Nop()))
	if !s.rerankerActive() {
		t.Fatal("an LLM reranker must make rerankerActive() true")
	}
	s.SetReranker(NewConfiguredReranker(false, &titlerFakeProvider{}, "", 20, 8, 600, zerolog.Nop()))
	if s.rerankerActive() {
		t.Fatal("a Noop reranker must keep rerankerActive() false")
	}
}
