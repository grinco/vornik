package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// titlerFakeProvider mirrors the scripted-response fake the graph
// package uses for stage tests. Kept package-local so the title path's
// regressions surface independently of the KG pipeline's.
type titlerFakeProvider struct {
	calls   atomic.Int32
	replies []titlerReply
}

type titlerReply struct {
	content string
	err     error
}

func (f *titlerFakeProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.replies) {
		return &chat.ChatResponse{}, nil
	}
	r := f.replies[idx]
	if r.err != nil {
		return nil, r.err
	}
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: r.content}, FinishReason: "stop"})
	// Realistic Usage shape — production providers always
	// populate this; tests that assert on cost-recording need
	// non-zero tokens for the row to land.
	resp.Usage.PromptTokens = 120
	resp.Usage.CompletionTokens = 30
	resp.Usage.TotalTokens = 150
	return resp, nil
}

// fakeUsageRecorder captures every Record call so tests can assert
// the exact task_llm_usage row shape produced by the titler.
type fakeUsageRecorder struct {
	mu   sync.Mutex
	rows []*persistence.TaskLLMUsage
}

func (f *fakeUsageRecorder) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, u)
	return nil
}

// fixedPricing is a deterministic PricingTable for testing: $1 per
// 1k prompt tokens + $2 per 1k completion tokens, regardless of model.
type fixedPricing struct{}

func (fixedPricing) CostUSD(_ string, prompt, completion int) float64 {
	return float64(prompt)/1000 + 2*float64(completion)/1000
}

func (f *titlerFakeProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *titlerFakeProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *titlerFakeProvider) Model() string            { return "fake" }
func (f *titlerFakeProvider) SetMetrics(*chat.Metrics) {}

func TestTitler_Title_HappyPath(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "Quarterly Sales Forecast"}}}
	tr := NewTitler(fp, "")
	got, err := tr.Title(context.Background(), "Some content about Q3 sales numbers", "", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "Quarterly Sales Forecast" {
		t.Errorf("got %q, want %q", got, "Quarterly Sales Forecast")
	}
}

func TestTitler_Title_EmptyContent(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "Should Not Be Used"}}}
	tr := NewTitler(fp, "")
	got, err := tr.Title(context.Background(), "   \n\t ", "", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty title for whitespace-only content, got %q", got)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("expected zero LLM calls, got %d", fp.calls.Load())
	}
}

func TestTitler_Title_StripsCommonNoise(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"quoted", `"Topic Label"`, "Topic Label"},
		{"smart quoted", `“Topic Label”`, "Topic Label"},
		{"trailing period", "Topic Label.", "Topic Label"},
		{"output prefix", "OUTPUT: Topic Label", "Topic Label"},
		{"multi-line keeps first", "Topic Label\nReason: it's about topics", "Topic Label"},
		{"fenced", "```\nTopic Label\n```", "Topic Label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := &titlerFakeProvider{replies: []titlerReply{{content: c.raw}}}
			tr := NewTitler(fp, "")
			got, err := tr.Title(context.Background(), "anything", "", "")
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestTitler_Title_RejectsGarbage(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "...!!!"}}}
	tr := NewTitler(fp, "")
	got, err := tr.Title(context.Background(), "anything", "", "")
	// Garbage response → empty title + non-nil err so the backfill
	// CLI can count it as a failure but the worker still falls back.
	if err == nil {
		t.Errorf("expected error for garbage response, got nil")
	}
	if got != "" {
		t.Errorf("expected empty title for garbage, got %q", got)
	}
}

func TestTitler_Title_RetriesOnTransientError(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{err: errors.New("connection reset")},
		{content: "Recovered Title"},
	}}
	tr := NewTitler(fp, "")
	tr.MaxAttempts = 2
	got, err := tr.Title(context.Background(), "anything", "", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "Recovered Title" {
		t.Errorf("got %q, want %q", got, "Recovered Title")
	}
	if fp.calls.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", fp.calls.Load())
	}
}

func TestTitler_Title_BoundsLongResponse(t *testing.T) {
	// Some misaligned models will ignore the word cap and emit a
	// paragraph. Cap protects the UI layout.
	long := strings.Repeat("Word ", 50) // > 80 chars
	fp := &titlerFakeProvider{replies: []titlerReply{{content: long}}}
	tr := NewTitler(fp, "")
	got, err := tr.Title(context.Background(), "anything", "", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) > 80 {
		t.Errorf("title %q exceeds 80-char cap (%d)", got, len(got))
	}
}

func TestTitler_NilReceiver(t *testing.T) {
	var tr *Titler
	if _, err := tr.Title(context.Background(), "x", "", ""); err == nil {
		t.Errorf("expected error from nil Titler, got nil")
	}
}

func TestTitler_RecordsUsage(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "Q3 Pipeline Review"}}}
	rec := &fakeUsageRecorder{}
	tr := NewTitler(fp, "")
	tr.LLMUsage = rec
	tr.Pricing = fixedPricing{}

	got, err := tr.Title(context.Background(), "some chunk content", "proj-7", "chunk-42")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "Q3 Pipeline Review" {
		t.Errorf("got %q, want %q", got, "Q3 Pipeline Review")
	}
	if len(rec.rows) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(rec.rows))
	}
	r := rec.rows[0]
	if r.Role != titlerRole {
		t.Errorf("role: got %q, want %q", r.Role, titlerRole)
	}
	if r.Source != persistence.TaskLLMUsageSourceMemoryTitler {
		t.Errorf("source: got %q, want %q", r.Source, persistence.TaskLLMUsageSourceMemoryTitler)
	}
	if r.ProjectID != "proj-7" {
		t.Errorf("project_id: got %q, want %q", r.ProjectID, "proj-7")
	}
	if r.StepID != "chunk-42" {
		t.Errorf("step_id (chunk): got %q, want %q", r.StepID, "chunk-42")
	}
	if r.TaskID != nil {
		t.Errorf("task_id: got %v, want nil (background consumer)", r.TaskID)
	}
	if r.ExecutionID != nil {
		t.Errorf("execution_id: got %v, want nil", r.ExecutionID)
	}
	if r.PromptTokens != 120 || r.CompletionTokens != 30 {
		t.Errorf("tokens: got prompt=%d completion=%d, want 120/30", r.PromptTokens, r.CompletionTokens)
	}
	// 120/1000 + 2*30/1000 = 0.18
	if want := 0.18; r.CostUSD < want-0.0001 || r.CostUSD > want+0.0001 {
		t.Errorf("cost_usd: got %f, want %f", r.CostUSD, want)
	}
	if r.Model != "fake" {
		t.Errorf("model: got %q, want %q", r.Model, "fake")
	}
}

func TestTitler_SkipsUsageWhenUnconfigured(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "Topic"}}}
	rec := &fakeUsageRecorder{}
	tr := NewTitler(fp, "")
	tr.LLMUsage = rec

	// Empty projectID + chunkID → skip recording even though
	// LLMUsage is wired. Lets tests + nil-safe paths run without
	// polluting the dashboard.
	if _, err := tr.Title(context.Background(), "content", "", ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(rec.rows) != 0 {
		t.Errorf("expected zero usage rows for empty attribution, got %d", len(rec.rows))
	}
}

func TestTitler_RecordsUsageEvenOnUnusableResponse(t *testing.T) {
	// Tokens were spent — the bill should land even when the
	// response was unparseable.
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "...!!!"}}}
	rec := &fakeUsageRecorder{}
	tr := NewTitler(fp, "")
	tr.LLMUsage = rec
	tr.Pricing = fixedPricing{}

	_, err := tr.Title(context.Background(), "content", "proj", "chunk")
	if err == nil {
		t.Errorf("expected error for unusable response")
	}
	if len(rec.rows) != 1 {
		t.Fatalf("expected the spent tokens to be recorded, got %d rows", len(rec.rows))
	}
}

func TestExtractContentTitle_Precedence(t *testing.T) {
	cases := []struct {
		name         string
		contentTitle string
		preview      string
		fallback     string
		want         string
	}{
		{
			name:         "content_title wins over heading and fallback",
			contentTitle: "Topic From LLM",
			preview:      "# Heading From Doc\nbody",
			fallback:     "raw-filename.md",
			want:         "Topic From LLM",
		},
		{
			name:         "heading used when content_title empty",
			contentTitle: "",
			preview:      "## Sub Heading\nbody",
			fallback:     "raw-filename.md",
			want:         "Sub Heading",
		},
		{
			name:         "fallback used when content_title empty and no heading",
			contentTitle: "",
			preview:      "no heading here\njust prose",
			fallback:     "raw-filename.md",
			want:         "raw-filename.md",
		},
		{
			name:         "whitespace-only content_title falls through to heading",
			contentTitle: "   ",
			preview:      "# Heading From Doc\nbody",
			fallback:     "raw-filename.md",
			want:         "Heading From Doc",
		},
		{
			name:         "H1 preferred when both H1 and H2 exist",
			contentTitle: "",
			preview:      "# Top Heading\n## Sub Heading\nbody",
			fallback:     "raw.md",
			// Implementation walks lines top-to-bottom and accepts
			// the first heading hit; ## appears second so H1 wins.
			want: "Top Heading",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractContentTitle(c.contentTitle, c.preview, c.fallback)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
