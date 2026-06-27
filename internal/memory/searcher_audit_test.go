package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRetrievalContext_StampAndRead — the context helpers must be
// nil-safe and round-trip cleanly. Stamping nil leaves ctx
// unchanged; reading from a clean ctx returns the zero value.
func TestRetrievalContext_StampAndRead(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, RetrievalContext{}, retrievalContextFromContext(ctx))

	// Nil bag is a no-op; reading still returns zero.
	ctx2 := WithRetrievalContext(ctx, nil)
	assert.Equal(t, RetrievalContext{}, retrievalContextFromContext(ctx2))

	rc := &RetrievalContext{TaskID: "t1", ExecutionID: "e1", StepID: "s1", Role: "researcher"}
	ctx3 := WithRetrievalContext(ctx, rc)
	got := retrievalContextFromContext(ctx3)
	assert.Equal(t, "t1", got.TaskID)
	assert.Equal(t, "e1", got.ExecutionID)
	assert.Equal(t, "s1", got.StepID)
	assert.Equal(t, "researcher", got.Role)
}

// TestRetrievalContext_ActorRoundTrip — LLD 22 added ActorKind +
// ActorID to the bag. The fields must round-trip cleanly so the
// companion adapter can stamp "companion"/key.ID before calling
// Searcher.Search and the audit row inherits them.
func TestRetrievalContext_ActorRoundTrip(t *testing.T) {
	rc := &RetrievalContext{ActorKind: "companion", ActorID: "akey-mem"}
	ctx := WithRetrievalContext(context.Background(), rc)
	got := retrievalContextFromContext(ctx)
	assert.Equal(t, "companion", got.ActorKind)
	assert.Equal(t, "akey-mem", got.ActorID)
}

// (Skip a Search()-level integration test here — Searcher.Search
// drives a real SQL query against the embedder + repo. The
// retrieval-audit write path is exercised by the postgres
// integration suite when a memory_retrieval_audit row appears.)
