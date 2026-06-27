package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
)

func TestNoopReranker_PassThrough(t *testing.T) {
	in := []SearchResult{{ChunkID: "a", Score: 0.1}, {ChunkID: "b", Score: 0.9}}
	out, err := NoopReranker{}.Rerank(context.Background(), "q", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ChunkID != "a" {
		t.Fatalf("changed order: %+v", out)
	}
}

func TestLLMReranker_NilGuards(t *testing.T) {
	var nilR *LLMReranker
	in := []SearchResult{{ChunkID: "a"}}
	out, err := nilR.Rerank(context.Background(), "q", in)
	if err != nil || len(out) != 1 {
		t.Fatalf("nil receiver: %v %v", out, err)
	}
	// Empty query.
	rr := &LLMReranker{Client: &titlerFakeProvider{}}
	out, _ = rr.Rerank(context.Background(), "  ", in)
	if len(out) != 1 {
		t.Fatal("empty query should pass-through")
	}
	// Single candidate skipped (nothing to re-order).
	out, _ = rr.Rerank(context.Background(), "q", in)
	if len(out) != 1 {
		t.Fatal("single candidate should pass-through")
	}
	// Nil client.
	rr2 := &LLMReranker{}
	multi := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	out, _ = rr2.Rerank(context.Background(), "q", multi)
	if len(out) != 2 {
		t.Fatal("nil client should pass-through")
	}
}

func TestLLMReranker_ReordersByScore(t *testing.T) {
	// LLM ranks b > a > c. The reranker must reorder accordingly.
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `{"scores":{"0":0.2,"1":0.9,"2":0.5}}`},
	}}
	rr := &LLMReranker{Client: fp, Logger: zerolog.Nop()}
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}, {ChunkID: "c"}}
	out, err := rr.Rerank(context.Background(), "q", in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b", "c", "a"}
	for i, id := range want {
		if out[i].ChunkID != id {
			t.Errorf("pos %d: got %q want %q (full: %+v)", i, out[i].ChunkID, id, out)
		}
	}
	// Score field is stamped from the LLM verdict.
	if out[0].Score != 0.9 || out[1].Score != 0.5 || out[2].Score != 0.2 {
		t.Fatalf("scores not stamped: %+v", out)
	}
}

func TestLLMReranker_TailUnchanged(t *testing.T) {
	// 4 candidates, MaxCandidates=2 → head is reranked, tail keeps RRF order.
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `{"scores":{"0":0.1,"1":0.9}}`},
	}}
	rr := &LLMReranker{Client: fp, MaxCandidates: 2, Logger: zerolog.Nop()}
	in := []SearchResult{
		{ChunkID: "a"}, {ChunkID: "b"}, {ChunkID: "c"}, {ChunkID: "d"},
	}
	out, _ := rr.Rerank(context.Background(), "q", in)
	if out[0].ChunkID != "b" || out[1].ChunkID != "a" {
		t.Fatalf("head not reranked: %+v", out)
	}
	if out[2].ChunkID != "c" || out[3].ChunkID != "d" {
		t.Fatalf("tail order changed: %+v", out)
	}
}

func TestLLMReranker_LLMErrorDegradesToInput(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{err: errors.New("rate limit")}}}
	rr := &LLMReranker{Client: fp, Logger: zerolog.Nop()}
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	out, err := rr.Rerank(context.Background(), "q", in)
	if err != nil {
		t.Fatal("must not propagate")
	}
	if out[0].ChunkID != "a" || out[1].ChunkID != "b" {
		t.Fatal("order should be preserved on error")
	}
}

func TestLLMReranker_BadJSONDegradesToInput(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "not json at all"}}}
	rr := &LLMReranker{Client: fp, Logger: zerolog.Nop()}
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	out, _ := rr.Rerank(context.Background(), "q", in)
	if out[0].ChunkID != "a" {
		t.Fatal("must keep RRF order on parse failure")
	}
}

func TestLLMReranker_EmptyChoicesDegrades(t *testing.T) {
	fp := &emptyChoiceProvider{}
	rr := &LLMReranker{Client: fp, Logger: zerolog.Nop()}
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	out, _ := rr.Rerank(context.Background(), "q", in)
	if len(out) != 2 || out[0].ChunkID != "a" {
		t.Fatal("must degrade")
	}
}

type emptyChoiceProvider struct{ titlerFakeProvider }

func (e *emptyChoiceProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return &chat.ChatResponse{}, nil
}

func TestLLMReranker_TimeoutHonoured(t *testing.T) {
	// Provider sleeps past the reranker's timeout.
	fp := &slowProvider{delay: 200 * time.Millisecond}
	rr := &LLMReranker{Client: fp, Timeout: 20 * time.Millisecond, Logger: zerolog.Nop()}
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	start := time.Now()
	out, _ := rr.Rerank(context.Background(), "q", in)
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("timeout not enforced")
	}
	if out[0].ChunkID != "a" {
		t.Fatal("must degrade on timeout")
	}
}

type slowProvider struct {
	titlerFakeProvider
	delay time.Duration
}

func (s *slowProvider) Complete(ctx context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return &chat.ChatResponse{}, nil
	}
}

func TestBuildRerankPrompt(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", SourceName: "alpha.md", Content: "alpha body"},
		{ChunkID: "b", SourceName: "beta.md", Content: strings.Repeat("x", 1000)},
	}
	got := buildRerankPrompt("how to ship", in, 50)
	if !strings.Contains(got, "QUERY:\nhow to ship") {
		t.Fatal("missing query")
	}
	if !strings.Contains(got, "[0] (source: alpha.md)") || !strings.Contains(got, "[1] (source: beta.md)") {
		t.Fatalf("missing index/source labels: %q", got)
	}
	// Long candidate truncated at snippet cap.
	if strings.Count(got, "x") > 60 {
		t.Fatal("snippet cap not applied")
	}
}

func TestParseRerankScores(t *testing.T) {
	// Happy path.
	got, err := parseRerankScores(`{"scores":{"0":0.8,"1":0.2}}`, 2)
	if err != nil || got[0] != 0.8 || got[1] != 0.2 {
		t.Fatalf("got %v %v", got, err)
	}
	// Code fence stripped.
	got, err = parseRerankScores("```json\n{\"scores\":{\"0\":0.9}}\n```", 1)
	if err != nil || got[0] != 0.9 {
		t.Fatalf("fence: %v %v", got, err)
	}
	// Bare ``` fence.
	if _, err := parseRerankScores("```\n{\"scores\":{\"0\":0.5}}\n```", 1); err != nil {
		t.Fatal(err)
	}
	// Out-of-range and negative indices ignored.
	got, err = parseRerankScores(`{"scores":{"99":0.9,"-1":0.5,"0":0.1,"bad":0.3}}`, 1)
	if err != nil || got[0] != 0.1 {
		t.Fatalf("got %v %v", got, err)
	}
	// Clamping out-of-range scores.
	got, _ = parseRerankScores(`{"scores":{"0":-0.5,"1":1.7}}`, 2)
	if got[0] != 0 || got[1] != 1 {
		t.Fatalf("clamp: %v", got)
	}
	// Bad JSON.
	if _, err := parseRerankScores("nope", 1); err == nil {
		t.Fatal("want err")
	}
	// Empty scores object.
	if _, err := parseRerankScores(`{"scores":{}}`, 1); err == nil {
		t.Fatal("want err")
	}
}

func TestTruncateHelper(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Fatalf("under: %q", got)
	}
	if got := truncate("alphabet", 3); got != "alp…" {
		t.Fatalf("over: %q", got)
	}
}
