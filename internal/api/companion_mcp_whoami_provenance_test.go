package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The memory benchmark's comparability key exists to prove two runs may be
// compared. Two of its fields were silently always empty, so it could not.
//
// Measured 2026-09-17 while running the release benchmark on
// 2026.9.4-203-gb37d6de2a: every run came back PARTIAL with both
// --our-extraction-model and --recall-method set. The empty fields were
// observed_embedder and daemon_revision:
//
//   - ObservedEmbedder reads GET /api/v1/memory/stats, which is admin-only. The
//     harness authenticates with a COMPANION key, and companion keys are
//     restricted to /api/v1/mcp/companion, so that call returns 403 on every run.
//     This is the SAME defect already fixed for embedding_readiness, whose fix
//     comment in companionToolWhoami says it "returned 403 on every run ever
//     made" — one field over, still unfixed.
//   - daemon_revision was never emitted by any companion surface at all.
//
// Both sit INSIDE ComparabilityFields.Key(), so an always-empty value is a
// constant: two runs from different releases, or on different embedding models,
// hash to the SAME key and compare clean. That is exactly the hazard the design
// records for ObservedEmbedder — "two runs on different embedding models produce
// the same key, which is how a titan-versus-cohere comparison once matched
// clean" — and it was live for the field meant to prevent it.
func TestWhoami_ReportsProvenanceTheComparabilityKeyNeeds(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	// Wire the version the way the DAEMON does — lazily. initHTTPServer builds
	// the API options before container.SetVersion runs, so the eager
	// s.buildVersion field is empty on every real daemon. Setting the field here
	// would make this test pass against a handler that reports nothing in
	// production, which is exactly what it did on the first attempt: whoami read
	// the field, the test set the field, and the live daemon returned no
	// daemon_revision at all.
	WithBuildVersionFunc(func() string { return "2026.9.4-203-gb37d6de2a" })(srv)
	srv.memoryEmbedder = fakeEmbedderReporter{provider: "openai", model: "qwen3-embedding:0.6b", dims: 1024}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{"name": "whoami", "arguments": map[string]any{}})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "whoami must not flag IsError: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	assert.Equal(t, "2026.9.4-203-gb37d6de2a", out["daemon_revision"],
		"without this the key cannot tell one release from another, which is the "+
			"one distinction a release-over-release table depends on")

	emb, ok := out["embedder"].(map[string]any)
	require.True(t, ok, "whoami must report the resolved embedder — the harness's only "+
		"door is the companion surface, and /api/v1/memory/stats 403s for a companion key")
	assert.Equal(t, "qwen3-embedding:0.6b", emb["model"])
	assert.Equal(t, "openai", emb["provider"])
	assert.Equal(t, float64(1024), emb["dimensions"])
}

// Absence must remain distinguishable from a value, on the same principle the
// readiness field already follows: a key stamped with a guessed build would be
// worse than one honestly marked partial.
func TestWhoami_OmitsProvenanceItCannotDetermine(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	// Neither source wired: the honest answer is absence.
	srv.memoryEmbedder = nil
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{"name": "whoami", "arguments": map[string]any{}})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	if _, present := out["daemon_revision"]; present {
		t.Error("a daemon that cannot identify its build must omit the field, not send an empty one")
	}
	if _, present := out["embedder"]; present {
		t.Error("no embedder wired must be absent, not an empty object")
	}
}

type fakeEmbedderReporter struct {
	provider, model string
	dims            int
}

func (f fakeEmbedderReporter) Embedder() (string, string, int) {
	return f.provider, f.model, f.dims
}

var _ = context.Background
