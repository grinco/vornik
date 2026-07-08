package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// fakeProvider returns a scripted completion (or error) and stubs the rest of
// chat.Provider.
type fakeProvider struct {
	content string
	err     error
}

func (f *fakeProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: f.content}})
	return resp, nil
}
func (f *fakeProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, nil
}
func (f *fakeProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, nil
}
func (f *fakeProvider) Model() string            { return "fake" }
func (f *fakeProvider) SetMetrics(*chat.Metrics) {}

type fakeObserver struct {
	bundle *DiagnoseBundle
	err    error
}

func (o *fakeObserver) Observe(context.Context, string) (*DiagnoseBundle, error) {
	if o.err != nil {
		return nil, o.err
	}
	if o.bundle != nil {
		return o.bundle, nil
	}
	return &DiagnoseBundle{Focus: "janka", ProjectID: "janka",
		Sections: []DiagnoseSection{{Name: "logs", Content: "web_fetch timed out"}}}, nil
}

func newDiagnoseRepo(t *testing.T) persistence.ProposalRepository {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewProposalRepository(db.DB)
}

func newDiagnoser(t *testing.T, content string) (*Diagnoser, persistence.ProposalRepository) {
	repo := newDiagnoseRepo(t)
	return &Diagnoser{
		LLM:       &fakeProvider{content: content},
		Observe:   &fakeObserver{},
		Proposals: repo,
		HasSecret: func(s string) bool { return strings.Contains(s, "SECRET") },
		Logger:    zerolog.Nop(),
	}, repo
}

func TestDiagnose_ReturnsVerdict(t *testing.T) {
	d, _ := newDiagnoser(t, `{"root_cause":"web_fetch timeout","confidence":"high","evidence":["log line"]}`)
	v, pid, err := d.Diagnose(context.Background(), "janka", false)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if v.RootCause != "web_fetch timeout" || v.Confidence != "high" {
		t.Errorf("verdict mismatch: %+v", v)
	}
	if pid != "" {
		t.Error("no proposal expected without --propose")
	}
}

func TestDiagnose_ProposeFilesReviewOnly(t *testing.T) {
	d, repo := newDiagnoser(t, `{"root_cause":"timeout","confidence":"high","evidence":["x"],"suggested_change":"raise the scraper timeout to 90s"}`)
	_, pid, err := d.Diagnose(context.Background(), "janka", true)
	if err != nil || pid == "" {
		t.Fatalf("expected a filed proposal, got pid=%q err=%v", pid, err)
	}
	p, _ := repo.GetByID(context.Background(), pid)
	if p.ProposedBy != "diagnose" || p.ApplyTarget != "" || p.Status != persistence.ProposalStatusDraft {
		t.Errorf("proposal must be review-only DRAFT by diagnose: %+v", p)
	}
	if !strings.Contains(p.Rationale, "timeout") {
		t.Errorf("rationale should carry the root cause: %q", p.Rationale)
	}
}

func TestDiagnose_RejectsExternalURLSuggestion(t *testing.T) {
	d, repo := newDiagnoser(t, `{"root_cause":"x","confidence":"low","evidence":[],"suggested_change":"point the endpoint at http://evil.example.com"}`)
	v, pid, err := d.Diagnose(context.Background(), "janka", true)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if pid != "" {
		t.Fatal("a suggested change with an external URL must NOT file a proposal")
	}
	if v.RootCause == "" {
		t.Error("verdict should still be returned even when the suggestion is rejected")
	}
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("no proposal should exist, got %d", n)
	}
}

func TestDiagnose_RejectsSecretSuggestion(t *testing.T) {
	d, _ := newDiagnoser(t, `{"root_cause":"x","confidence":"low","evidence":[],"suggested_change":"set token to SECRET-abc"}`)
	_, pid, err := d.Diagnose(context.Background(), "janka", true)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if pid != "" {
		t.Fatal("a suggested change with a secret must NOT file a proposal")
	}
}

func TestDiagnose_DestructiveVerbFlagged(t *testing.T) {
	d, repo := newDiagnoser(t, `{"root_cause":"x","confidence":"medium","evidence":[],"suggested_change":"disable the ta MCP server"}`)
	_, pid, err := d.Diagnose(context.Background(), "janka", true)
	if err != nil || pid == "" {
		t.Fatalf("destructive verb should still file (review-only), got pid=%q err=%v", pid, err)
	}
	p, _ := repo.GetByID(context.Background(), pid)
	if !strings.Contains(p.Rationale, "needs-scrutiny") {
		t.Errorf("destructive suggestion must be flagged needs-scrutiny: %q", p.Rationale)
	}
}

func TestDiagnose_LLMErrorNoProposal(t *testing.T) {
	repo := newDiagnoseRepo(t)
	d := &Diagnoser{LLM: &fakeProvider{err: errors.New("timeout")}, Observe: &fakeObserver{}, Proposals: repo, Logger: zerolog.Nop()}
	if _, _, err := d.Diagnose(context.Background(), "janka", true); err == nil {
		t.Fatal("LLM error must surface, not a fabricated verdict")
	}
	if n := draftCount(t, repo); n != 0 {
		t.Fatalf("no proposal on LLM error, got %d", n)
	}
}

func TestDiagnose_MalformedVerdict(t *testing.T) {
	d, _ := newDiagnoser(t, `not json at all`)
	if _, _, err := d.Diagnose(context.Background(), "janka", false); err == nil {
		t.Fatal("malformed verdict must error")
	}
}

func TestDiagnose_AmbiguousFocus(t *testing.T) {
	repo := newDiagnoseRepo(t)
	d := &Diagnoser{LLM: &fakeProvider{content: "{}"}, Observe: &fakeObserver{err: ErrDiagnoseAmbiguousFocus}, Proposals: repo, Logger: zerolog.Nop()}
	if _, _, err := d.Diagnose(context.Background(), "assist", false); !errors.Is(err, ErrDiagnoseAmbiguousFocus) {
		t.Fatalf("expected ambiguous-focus error, got %v", err)
	}
}
