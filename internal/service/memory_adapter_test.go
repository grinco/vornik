package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

type stubToolAuditRepo struct {
	listFn func(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error)
}

func (s *stubToolAuditRepo) Log(ctx context.Context, entry *persistence.ToolAuditEntry) error {
	return nil
}

func (s *stubToolAuditRepo) List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if s.listFn != nil {
		return s.listFn(ctx, filter)
	}
	return nil, nil
}

func (s *stubToolAuditRepo) CountByTool(ctx context.Context, executionID string) (map[string]int64, error) {
	return map[string]int64{}, nil
}

func TestGateActionToString(t *testing.T) {
	assert.Equal(t, "allow", gateActionToString(memory.GateAllow))
	assert.Equal(t, "redact", gateActionToString(memory.GateRedact))
	assert.Equal(t, "quarantine", gateActionToString(memory.GateQuarantine))
	assert.Equal(t, "reject", gateActionToString(memory.GateReject))
	assert.Equal(t, "unknown", gateActionToString(memory.GateAction(999)))
}

func TestGateOutcomeAndTrailToUI(t *testing.T) {
	in := memory.GateOutcome{
		Gate:         memory.GateClaimAuditOverlap,
		Action:       memory.GateAllow,
		Detail:       "partial_audit: 1/2",
		NewContent:   "sanitized",
		ShadowSignal: true,
	}

	got := gateOutcomeToUI(in)
	assert.Equal(t, "claim_audit_overlap", got.Gate)
	assert.Equal(t, "allow", got.Action)
	assert.Equal(t, "partial_audit: 1/2", got.Detail)
	assert.Equal(t, "sanitized", got.NewContent)
	assert.True(t, got.ShadowSignal)

	trail := gateTrailToUI([]memory.GateOutcome{
		in,
		{Gate: memory.GateSecretScan, Action: memory.GateRedact, Detail: "redacted"},
	})
	require.Len(t, trail, 2)
	assert.Equal(t, "allow", trail[0].Action)
	assert.Equal(t, "redact", trail[1].Action)
	assert.Equal(t, "secret_scan", trail[1].Gate)
}

func TestNewAuditLookupFunc_GuardsAndError(t *testing.T) {
	lookupNil := newAuditLookupFunc(nil)
	matches, err := lookupNil(context.Background(), "exec_1", []memory.Claim{{Category: memory.ClaimURL, Value: "https://example.com"}})
	require.NoError(t, err)
	assert.Nil(t, matches)

	repoErr := &stubToolAuditRepo{
		listFn: func(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
			return nil, errors.New("db down")
		},
	}
	lookupErr := newAuditLookupFunc(repoErr)
	matches, err = lookupErr(context.Background(), "exec_2", []memory.Claim{{Category: memory.ClaimBacktickCommand, Value: "go test ./..."}})
	require.Error(t, err)
	assert.Nil(t, matches)
	assert.Contains(t, err.Error(), "db down")
}

func TestNewAuditLookupFunc_MatchesClaimsAgainstToolInputAndOutput(t *testing.T) {
	repo := &stubToolAuditRepo{}
	lookup := newAuditLookupFunc(repo)

	repo.listFn = func(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
		require.NotNil(t, filter.ExecutionID)
		assert.Equal(t, "exec_3", *filter.ExecutionID)
		assert.Equal(t, 1000, filter.PageSize)

		return []*persistence.ToolAuditEntry{
			{ID: "row-1", ToolInput: "run go test ./internal/service", ToolOutput: "ok"},
			nil,
			{ID: "row-2", ToolInput: "curl https://example.com", ToolOutput: "HTTP/200"},
		}, nil
	}

	claims := []memory.Claim{
		{Category: memory.ClaimBacktickCommand, Value: "  go test ./internal/service  "}, // trim-space path
		{Category: memory.ClaimURL, Value: "https://example.com"},
		{Category: memory.ClaimFilePath, Value: "   "}, // blank after trim => skipped
		{Category: memory.ClaimGitSHA, Value: "deadbeef"},
	}
	got, err := lookup(context.Background(), "exec_3", claims)
	require.NoError(t, err)
	require.Len(t, got, len(claims))

	assert.True(t, got[0].Found)
	assert.Equal(t, "row-1", got[0].AuditRowID)
	assert.True(t, got[1].Found)
	assert.Equal(t, "row-2", got[1].AuditRowID)
	assert.False(t, got[2].Found)
	assert.Empty(t, got[2].AuditRowID)
	assert.False(t, got[3].Found)
}

func TestDryRunResultToUI_MapsFieldsAndClaims(t *testing.T) {
	adapter := &pipelineDryRunAdapter{}
	ttl := 72 * time.Hour
	in := memory.DryRunResult{
		Final: memory.GateOutcome{Gate: memory.GatePolicyMatch, Action: memory.GateQuarantine, Detail: "matched deny pattern"},
		Trail: []memory.GateOutcome{{Gate: memory.GateSchemaMatch, Action: memory.GateAllow}},
		Class: memory.ClassDecision,
		Policy: memory.ClassPolicy{
			TTL:               ttl,
			DefaultConfidence: 0.91,
		},
		RoleOfRecordEligible: true,
		PostRedactContent:    "after-redact",
		Claims: []memory.ClaimMatch{{
			Claim:      memory.Claim{Category: memory.ClaimEntityID, Value: "task_12345678"},
			Found:      true,
			AuditRowID: "audit-1",
		}},
	}

	got := adapter.dryRunResultToUI(in)
	assert.Equal(t, "quarantine", got.Final.Action)
	assert.Equal(t, "policy_match", got.Final.Gate)
	assert.Equal(t, "decision", got.Class)
	assert.Equal(t, 3, got.TTLDays)
	assert.InDelta(t, float32(0.91), got.DefaultConfidence, 0.0001)
	assert.True(t, got.RoleOfRecordEligible)
	assert.Equal(t, "after-redact", got.PostRedactContent)
	require.Len(t, got.Claims, 1)
	assert.Equal(t, "entity_id", got.Claims[0].Category)
	assert.Equal(t, "task_12345678", got.Claims[0].Value)
	assert.True(t, got.Claims[0].Found)
	assert.Equal(t, "audit-1", got.Claims[0].AuditRowID)
}

// stubResponseCache satisfies memory.ResponseCache; the
// CacheStats-via-interface lookup in newResponseCacheStatsAdapter
// uses a type assertion so the stub embeds the CacheStats method
// directly to exercise the success path.
type stubResponseCache struct {
	stats memory.ResponseCacheStats
	err   error
}

func (s *stubResponseCache) Get(_ context.Context, _ string) (string, int, int, bool, error) {
	return "", 0, 0, false, nil
}
func (s *stubResponseCache) Put(_ context.Context, _, _, _, _ string, _, _ int) error {
	return nil
}
func (s *stubResponseCache) CacheStats(_ context.Context) (memory.ResponseCacheStats, error) {
	return s.stats, s.err
}

// statslessResponseCache satisfies memory.ResponseCache but
// deliberately omits CacheStats so the adapter degrades to nil.
type statslessResponseCache struct{}

func (statslessResponseCache) Get(_ context.Context, _ string) (string, int, int, bool, error) {
	return "", 0, 0, false, nil
}
func (statslessResponseCache) Put(_ context.Context, _, _, _, _ string, _, _ int) error {
	return nil
}

func TestNewResponseCacheStatsAdapter_NilSourceReturnsNil(t *testing.T) {
	if got := newResponseCacheStatsAdapter(nil); got != nil {
		t.Errorf("expected nil for nil cache, got %T", got)
	}
}

func TestNewResponseCacheStatsAdapter_NoStatsMethodReturnsNil(t *testing.T) {
	if got := newResponseCacheStatsAdapter(statslessResponseCache{}); got != nil {
		t.Errorf("expected nil when CacheStats absent, got %T", got)
	}
}

func TestResponseCacheStatsAdapter_PassesThrough(t *testing.T) {
	src := &stubResponseCache{
		stats: memory.ResponseCacheStats{
			RowCount:         12,
			ApproxBytes:      4096,
			DistinctPurposes: 3,
			TotalHits:        87,
		},
	}
	adapter := newResponseCacheStatsAdapter(src)
	require.NotNil(t, adapter)
	got, err := adapter.CacheStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(12), got.RowCount)
	assert.Equal(t, int64(4096), got.ApproxBytes)
	assert.Equal(t, 3, got.DistinctPurposes)
	assert.Equal(t, int64(87), got.TotalHits)
}

func TestResponseCacheStatsAdapter_PropagatesError(t *testing.T) {
	src := &stubResponseCache{err: errors.New("db down")}
	adapter := newResponseCacheStatsAdapter(src)
	require.NotNil(t, adapter)
	_, err := adapter.CacheStats(context.Background())
	require.Error(t, err)
}

func TestResponseCacheStatsAdapter_NilReceiverSafe(t *testing.T) {
	var a *responseCacheStatsAdapter
	got, err := a.CacheStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), got.RowCount)
}
