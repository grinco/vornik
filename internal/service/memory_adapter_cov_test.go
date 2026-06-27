package service

// Coverage-uplift sweep (2026-06-18). Complements
// memory_adapter_test.go + memory_adapter_converters_test.go (the
// earlier Tier-2 sweep) without duplicating them. Those already pin
// gateActionToString, newAuditLookupFunc's guards/match, and the
// response-cache stats adapter. This file fills the remaining no-DB
// gaps:
//   - the "stronger side wins" branch of newAuditLookupFunc's
//     per-claim score selection (output-side beats input-side).
//   - the nil-repo / nil-receiver guards on the firewall, evictor,
//     and extracted-document adapters — the half-wired degraded-mode
//     contract that keeps container_http.go skipping a capability
//     instead of serving a nil-deref.
//
// Everything here runs without Postgres, podman, network, or an LLM.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// memAdapterStubAuditRepo is a function-local ToolAuditRepository fake.
// Prefixed with the source base name per the collision rule.
type memAdapterStubAuditRepo struct {
	rows []*persistence.ToolAuditEntry
}

func (s *memAdapterStubAuditRepo) Log(context.Context, *persistence.ToolAuditEntry) error {
	return nil
}
func (s *memAdapterStubAuditRepo) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return s.rows, nil
}
func (s *memAdapterStubAuditRepo) CountByTool(context.Context, string) (map[string]int64, error) {
	return nil, nil
}

// TestNewAuditLookupFunc_PicksStrongerSide exercises the best := inScore;
// if outScore > best branch: a claim that hits the output side exactly
// but the input side only loosely must record the higher (output) score.
func TestNewAuditLookupFunc_PicksStrongerSide(t *testing.T) {
	repo := &memAdapterStubAuditRepo{
		rows: []*persistence.ToolAuditEntry{
			{ID: "r1", ToolInput: "make build other thing", ToolOutput: "deploy production now"},
		},
	}
	out, err := newAuditLookupFunc(repo)(context.Background(), "exec", []memory.Claim{
		{Value: "deploy production now"},
	})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.True(t, out[0].Found)
	assert.Equal(t, "r1", out[0].AuditRowID)
	// Exact substring hit on the output side scores 1.0 — the stronger
	// of the two sides.
	assert.Equal(t, 1.0, out[0].MatchScore)
}

// --- constructor nil guards: the half-wired degraded-mode contract ---

func TestMemoryAdapterConstructors_NilRepoReturnsNil(t *testing.T) {
	// Each returns nil so container_http.go skips the corresponding
	// api/ui capability instead of serving a nil-deref.
	assert.Nil(t, newExtractedDocumentIndexerAdapter(nil))
	assert.Nil(t, newMemoryFirewallEditorAdapter(nil))
	assert.Nil(t, newUIFirewallEditorAdapter(nil))
}

func TestMemoryAdapterMethods_NilReceiversReturnEmpty(t *testing.T) {
	ctx := context.Background()

	// extractedDocumentIndexerAdapter with nil indexer → (0, nil).
	var idx *extractedDocumentIndexerAdapter
	n, err := idx.IngestExtractedSections(ctx, "p", "t", "src", "doc", nil)
	require.NoError(t, err)
	assert.Zero(t, n)

	// memoryFirewallEditorAdapter with nil repo → empty / zero.
	var fw *memoryFirewallEditorAdapter
	rows, err := fw.LoadChunkPolicies(ctx, []string{"c1"})
	require.NoError(t, err)
	assert.Nil(t, rows)
	affected, err := fw.UpdateChunkPolicy(ctx, api.ChunkPolicyRow{ChunkID: "c1"})
	require.NoError(t, err)
	assert.Zero(t, affected)

	// uiFirewallEditorAdapter with nil repo → nil.
	var uifw *uiFirewallEditorAdapter
	uirows, err := uifw.LoadChunkPolicies(ctx, []string{"c1"})
	require.NoError(t, err)
	assert.Nil(t, uirows)

	// memoryEvictorAdapter with nil corrector/repo → zero / nil.
	ev := &memoryEvictorAdapter{}
	count, err := ev.HardEvict(ctx, "p", []string{"c1"}, "reason", "by")
	require.NoError(t, err)
	assert.Zero(t, count)
	audits, err := ev.ListEvictionAudits(ctx, "p", 10)
	require.NoError(t, err)
	assert.Nil(t, audits)
}
