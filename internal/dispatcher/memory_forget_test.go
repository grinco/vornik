package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// TestMemoryForget_HappyPath — the corrector evicts the named chunk;
// the tool output truthfully reports what was evicted (id + source +
// preview) and passes the chunk id + project through unchanged.
func TestMemoryForget_HappyPath(t *testing.T) {
	c := &memCorrector{
		forgetResult: &memory.RefutedChunk{
			ID:         "chunk_x",
			SourceName: "notes.md",
			Preview:    "the stale fact",
		},
	}
	te := newTestExecutor(c)
	args := `{"chunk_id":"chunk_x","project_id":"janka"}`

	res := te.memoryForget(context.Background(), args, "janka", nil)
	body := res.Content
	for _, want := range []string{"Evicted memory chunk", "chunk_x", "notes.md", "the stale fact"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q. body=%s", want, body)
		}
	}
	if c.lastForgetID != "chunk_x" || c.lastForgetProj != "janka" {
		t.Errorf("passthrough wrong: id=%q proj=%q", c.lastForgetID, c.lastForgetProj)
	}
}

// TestMemoryForget_NoSuchChunk — corrector returns (nil, nil) for an
// unknown / foreign-project id. The tool must say "nothing was
// evicted" rather than implying an eviction.
func TestMemoryForget_NoSuchChunk(t *testing.T) {
	c := &memCorrector{forgetResult: nil}
	te := newTestExecutor(c)
	res := te.memoryForget(context.Background(), `{"chunk_id":"chunk_ghost","project_id":"janka"}`, "janka", nil)
	if !strings.Contains(res.Content, "nothing was evicted") {
		t.Errorf("missing no-op notice: %s", res.Content)
	}
	if strings.Contains(res.Content, "Evicted memory chunk") {
		t.Errorf("must not claim an eviction happened: %s", res.Content)
	}
}

// TestMemoryForget_RejectsMissingChunkID — chunk_id is required.
func TestMemoryForget_RejectsMissingChunkID(t *testing.T) {
	te := newTestExecutor(&memCorrector{})
	for _, args := range []string{`{}`, `{"chunk_id":"  "}`} {
		res := te.memoryForget(context.Background(), args, "janka", nil)
		if !strings.Contains(res.Content, "chunk_id is required") {
			t.Errorf("args %s: body = %q, want 'chunk_id is required'", args, res.Content)
		}
	}
}

// TestMemoryForget_DisabledMessage — no corrector wired.
func TestMemoryForget_DisabledMessage(t *testing.T) {
	te := newTestExecutor(nil)
	res := te.memoryForget(context.Background(), `{"chunk_id":"c"}`, "janka", nil)
	if !strings.Contains(res.Content, "not enabled") {
		t.Errorf("body = %q, want disabled notice", res.Content)
	}
}

// TestMemoryForget_ScopedProjectRejected — a project_id outside the
// session's allowed set is rejected before any corrector call (IDOR
// guard shared with memory_correct).
func TestMemoryForget_ScopedProjectRejected(t *testing.T) {
	c := &memCorrector{}
	te := newTestExecutor(c)
	res := te.memoryForget(context.Background(), `{"chunk_id":"c","project_id":"janka"}`, "snake", []string{"snake"})
	if !strings.Contains(res.Content, "not permitted") && !strings.Contains(res.Content, "not allowed") {
		t.Errorf("expected scope rejection; body=%s", res.Content)
	}
	if c.lastForgetID != "" {
		t.Error("corrector called despite scope rejection — IDOR leak")
	}
}

// TestMemoryForget_ErrorReported — corrector error surfaces to the LLM.
func TestMemoryForget_ErrorReported(t *testing.T) {
	c := &memCorrector{forgetErr: errors.New("DB down")}
	te := newTestExecutor(c)
	res := te.memoryForget(context.Background(), `{"chunk_id":"c","project_id":"janka"}`, "janka", nil)
	if !strings.Contains(res.Content, "Forget failed") || !strings.Contains(res.Content, "DB down") {
		t.Errorf("error not surfaced: %s", res.Content)
	}
}

// TestMemoryForgetDescriptor — pin the LLM-visible shape.
func TestMemoryForgetDescriptor(t *testing.T) {
	d := memoryForgetDescriptor()
	if d.Function.Name != "memory_forget" {
		t.Errorf("tool name = %q, want memory_forget", d.Function.Name)
	}
	if !strings.Contains(string(d.Function.Parameters), "chunk_id") {
		t.Error("schema missing required field chunk_id")
	}
	requireToolDescriptor(d)
}
