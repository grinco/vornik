package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory"
)

// memCorrector is the in-memory MemoryCorrector stub. It records
// every call so the test can assert on the args the dispatcher
// passed through.
type memCorrector struct {
	refuted         []memory.RefutedChunk
	refuteErr       error
	insertErr       error
	lastRefuteClaim string
	lastRefuteMax   int
	lastInsertText  string
	lastInsertProj  string
	lastRefuteProj  string
	insertedChunkID string
}

func (m *memCorrector) RefuteByClaim(_ context.Context, projectID, claim string, max int) ([]memory.RefutedChunk, error) {
	m.lastRefuteProj = projectID
	m.lastRefuteClaim = claim
	m.lastRefuteMax = max
	if m.refuteErr != nil {
		return nil, m.refuteErr
	}
	return m.refuted, nil
}
func (m *memCorrector) InsertCorrection(_ context.Context, projectID, content, _ string) (string, error) {
	m.lastInsertProj = projectID
	m.lastInsertText = content
	if m.insertErr != nil {
		return "", m.insertErr
	}
	if m.insertedChunkID == "" {
		m.insertedChunkID = "chunk_corr_1"
	}
	return m.insertedChunkID, nil
}

// newTestExecutor returns a ToolExecutor wired with just the
// minimal surface memory_correct needs.
func newTestExecutor(c MemoryCorrector) *ToolExecutor {
	return &ToolExecutor{
		memoryCorrector: c,
		logger:          zerolog.Nop(),
	}
}

// TestMemoryCorrect_HappyPath — three matching chunks land
// refuted; the correction lands as a new verified chunk. The
// tool output must mention both side effects so the LLM relays
// them to the operator.
func TestMemoryCorrect_HappyPath(t *testing.T) {
	c := &memCorrector{
		refuted: []memory.RefutedChunk{
			{ID: "chunk_a", SourceName: "cv-2024.md", Preview: "Janka, born 1985 …", Score: 0.72},
			{ID: "chunk_b", SourceName: "cv-2023.md", Preview: "Janka 1985 …", Score: 0.61},
		},
	}
	te := newTestExecutor(c)
	args := `{"wrong_claim":"Janka was born in 1985","correction":"Janka was born in 1990.","project_id":"janka","max_refutes":5}`

	res := te.memoryCorrect(context.Background(), args, "janka", nil)
	body := res.Content
	for _, want := range []string{
		"Refuted 2 memory chunk",
		"chunk_a",
		"chunk_b",
		"chunk_corr_1",
		"Stored correction",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tool output missing %q. body=%s", want, body)
		}
	}
	// Args passed through cleanly to the corrector.
	if c.lastRefuteClaim != "Janka was born in 1985" {
		t.Errorf("wrong_claim passthrough: %q", c.lastRefuteClaim)
	}
	if c.lastInsertText != "Janka was born in 1990." {
		t.Errorf("correction passthrough: %q", c.lastInsertText)
	}
	if c.lastRefuteProj != "janka" || c.lastInsertProj != "janka" {
		t.Errorf("project passthrough wrong: refute=%q insert=%q", c.lastRefuteProj, c.lastInsertProj)
	}
	if c.lastRefuteMax != 5 {
		t.Errorf("max_refutes passthrough: %d", c.lastRefuteMax)
	}
}

// TestMemoryCorrect_NoMatchesStillInsertsCorrection — when the
// hybrid search returns no hits, we still want to record the
// correction (operator may be teaching the system a new fact).
// The tool output mentions the no-op refute path so the LLM can
// report the asymmetry to the operator.
func TestMemoryCorrect_NoMatchesStillInsertsCorrection(t *testing.T) {
	c := &memCorrector{refuted: nil}
	te := newTestExecutor(c)
	args := `{"wrong_claim":"unknown fact","correction":"Janka's real birthday is 1990-05-12.","project_id":"janka"}`

	res := te.memoryCorrect(context.Background(), args, "janka", nil)
	body := res.Content
	if !strings.Contains(body, "No memory chunks matched") {
		t.Errorf("missing no-match notice: %s", body)
	}
	if !strings.Contains(body, "chunk_corr_1") {
		t.Errorf("correction should still land: %s", body)
	}
}

// TestMemoryCorrect_RejectsMissingArgs — wrong_claim and
// correction are required. Operator-facing validation lives in
// the tool handler so the LLM gets a clear error string back.
func TestMemoryCorrect_RejectsMissingArgs(t *testing.T) {
	te := newTestExecutor(&memCorrector{})
	cases := []struct {
		args    string
		wantMsg string
	}{
		{`{}`, "wrong_claim is required"},
		{`{"wrong_claim":"  "}`, "wrong_claim is required"},
		{`{"wrong_claim":"x"}`, "correction is required"},
		{`{"wrong_claim":"x","correction":"  "}`, "correction is required"},
	}
	for _, tc := range cases {
		t.Run(tc.args, func(t *testing.T) {
			res := te.memoryCorrect(context.Background(), tc.args, "janka", nil)
			if !strings.Contains(res.Content, tc.wantMsg) {
				t.Errorf("body = %q, want substring %q", res.Content, tc.wantMsg)
			}
		})
	}
}

// TestMemoryCorrect_PartialFailureReports — refute succeeds,
// insert errors. The tool MUST report both states so the LLM
// can warn the user "I refuted the bad chunks but didn't store
// the correction — please repeat".
func TestMemoryCorrect_PartialFailureReports(t *testing.T) {
	c := &memCorrector{
		refuted: []memory.RefutedChunk{
			{ID: "chunk_a", SourceName: "x", Preview: "wrong", Score: 0.5},
		},
		insertErr: errors.New("DB connection lost"),
	}
	te := newTestExecutor(c)
	args := `{"wrong_claim":"x","correction":"y","project_id":"janka"}`
	res := te.memoryCorrect(context.Background(), args, "janka", nil)
	for _, want := range []string{
		"Refuted 1 chunk",
		"insert failed",
		"DB connection lost",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %q in body: %s", want, res.Content)
		}
	}
}

// TestMemoryCorrect_DisabledMessage — when no corrector is
// wired (single-tenant deployments without memory), the tool
// returns a clean "not enabled" message rather than 500-ing.
func TestMemoryCorrect_DisabledMessage(t *testing.T) {
	te := newTestExecutor(nil)
	res := te.memoryCorrect(context.Background(),
		`{"wrong_claim":"x","correction":"y"}`, "janka", nil)
	if !strings.Contains(res.Content, "Memory correction is not enabled") {
		t.Errorf("body = %q, want disabled notice", res.Content)
	}
}

// TestMemoryCorrect_ScopedProjectRejected — caller specifies
// project_id that's outside the session's allowed set. The
// dispatcher's existing scoping helper (`resolveProjectAllowed`)
// must short-circuit before any DB mutation; we assert via the
// "Access to project" message it returns.
func TestMemoryCorrect_ScopedProjectRejected(t *testing.T) {
	c := &memCorrector{}
	te := newTestExecutor(c)
	args := `{"wrong_claim":"x","correction":"y","project_id":"janka"}`
	res := te.memoryCorrect(context.Background(), args, "snake", []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") &&
		!strings.Contains(res.Content, "not allowed") {
		t.Errorf("expected scope rejection; body=%s", res.Content)
	}
	if c.lastRefuteClaim != "" {
		t.Error("corrector called despite scope rejection — IDOR leak")
	}
}

// TestMemoryCorrectDescriptor — pin the LLM-visible shape so a
// silent rename / param drop trips this test rather than
// silently breaking the conversational contract the LLM was
// trained against in production.
func TestMemoryCorrectDescriptor(t *testing.T) {
	d := memoryCorrectDescriptor()
	if d.Function.Name != "memory_correct" {
		t.Errorf("tool name = %q, want memory_correct", d.Function.Name)
	}
	for _, want := range []string{"wrong_claim", "correction"} {
		if !strings.Contains(string(d.Function.Parameters), want) {
			t.Errorf("schema missing required field %q", want)
		}
	}
	// Compile-time guard: the descriptor satisfies the chat.Tool shape.
	requireToolDescriptor(d)
}

func requireToolDescriptor(chat.Tool) {}
