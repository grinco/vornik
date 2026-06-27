package memory

// Security regression for the recent_memory firewall bypass
// (audit pass, 2026-06). Before the fix, the companion
// `recent_memory` MCP tool called Repository.ListRecentChunksWithOptions
// directly and returned the rows verbatim — skipping the
// Policy-Aware Memory Firewall that recall (RecallWithContext /
// applyFirewall) runs. A chunk recall drops under enforce
// (refuted validation_status, credentials content_class, or an
// expired firewall policy) would still surface in the digest.
//
// These tests pin: a chunk that the firewall blocks under enforce
// MUST be absent from Searcher.RecentWithContext under enforce, and
// MUST carry a PolicyWarning (and stay present) under advisory —
// the same contract applyFirewall gives the recall path.

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/memoryfirewall"
)

// recentAuditFirewall wires an enforce/advisory FirewallDeps with a
// real evaluator and nil writer/metrics (both nil-safe at the
// observe/enqueue sites).
func recentAuditFirewall(mode memoryfirewall.EnforcementMode) *FirewallDeps {
	return &FirewallDeps{
		Evaluator:       memoryfirewall.NewEvaluator(),
		EnforcementMode: mode,
	}
}

// recentAuditExpectRecentQuery mocks ListRecentChunksWithOptions'
// SELECT (non-strict, non-untagged default), returning the two
// supplied chunk IDs newest-first.
func recentAuditExpectRecentQuery(mock sqlmock.Sqlmock, cleanID, blockedID string) {
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"id", "task_id", "source_name", "content_class",
		"content", "created_at", "repo_scope", "has_embedding",
	}).
		AddRow(cleanID, "", "companion:claude-code:note", "decision",
			"a safe architectural decision", created, "", true).
		AddRow(blockedID, "", "companion:claude-code:note", "research",
			"REFUTED hallucinated claim", created, "", true)
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WillReturnRows(rows)
}

// recentAuditExpectPolicyQuery mocks LoadChunkPolicies: the clean
// chunk has no policy columns (default-allow), the blocked chunk has
// validation_status=refuted (classifier bridge → deny-all roles).
func recentAuditExpectPolicyQuery(mock sqlmock.Sqlmock, cleanID, blockedID string) {
	cols := []string{
		"id", "tenant_id", "sensitivity_tier", "provenance_source",
		"provenance_producer", "provenance_trust", "provenance_url",
		"firewall_expires_at", "permitted_roles", "allowed_purposes",
		"policy_digest", "content_class", "validation_status",
	}
	rows := sqlmock.NewRows(cols).
		AddRow(cleanID, "", "", "", "", 0, "", nil, nil, nil, "", "decision", "").
		AddRow(blockedID, "", "", "", "", 0, "", nil, nil, nil, "", "research", "refuted")
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WillReturnRows(rows)
}

// TestRecentWithContext_EnforceDropsBlockedChunk is the core
// regression: under enforce, the refuted chunk must be dropped from
// the digest, exactly as recall drops it.
func TestRecentWithContext_EnforceDropsBlockedChunk(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	s := NewSearcher(Config{}, NewRepository(db), nil)
	s.SetFirewall(recentAuditFirewall(memoryfirewall.EnforcementEnforce))

	const cleanID, blockedID = "chunk_clean", "chunk_refuted"
	recentAuditExpectRecentQuery(mock, cleanID, blockedID)
	recentAuditExpectPolicyQuery(mock, cleanID, blockedID)

	got, err := s.RecentWithContext(
		context.Background(), "proj", 10, "", false, false,
		memoryfirewall.RequestContext{
			Role:       "companion:claude-code",
			OperatorID: "key_123",
			Purpose:    memoryfirewall.PurposeOperational,
		},
	)
	require.NoError(t, err)

	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ChunkID)
	}
	assert.Contains(t, ids, cleanID, "allowed chunk must survive")
	assert.NotContains(t, ids, blockedID,
		"refuted chunk recall drops under enforce must NOT surface in recent_memory")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRecentWithContext_AdvisoryAnnotatesBlockedChunk: under
// advisory the blocked chunk stays but carries a PolicyWarning,
// mirroring SearchResult.PolicyWarning on the recall path.
func TestRecentWithContext_AdvisoryAnnotatesBlockedChunk(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	s := NewSearcher(Config{}, NewRepository(db), nil)
	s.SetFirewall(recentAuditFirewall(memoryfirewall.EnforcementAdvisory))

	const cleanID, blockedID = "chunk_clean", "chunk_refuted"
	recentAuditExpectRecentQuery(mock, cleanID, blockedID)
	recentAuditExpectPolicyQuery(mock, cleanID, blockedID)

	got, err := s.RecentWithContext(
		context.Background(), "proj", 10, "", false, false,
		memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational},
	)
	require.NoError(t, err)

	byID := map[string]RecentChunkRow{}
	for _, r := range got {
		byID[r.ChunkID] = r
	}
	require.Contains(t, byID, blockedID, "advisory keeps the blocked chunk")
	assert.NotEmpty(t, byID[blockedID].PolicyWarning,
		"advisory must annotate the blocked chunk with a PolicyWarning")
	assert.Empty(t, byID[cleanID].PolicyWarning,
		"allowed chunk carries no warning")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRecentWithContext_RecallParity proves the digest decision
// matches recall's applyFirewall decision for the SAME chunk set +
// mode: feed equivalent SearchResult rows through applyFirewall and
// assert the blocked chunk is dropped there too — i.e. the bypass
// is closed and both surfaces agree.
func TestRecentWithContext_RecallParity(t *testing.T) {
	const cleanID, blockedID = "chunk_clean", "chunk_refuted"

	// Recall side: applyFirewall over equivalent SearchResults.
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	s := NewSearcher(Config{}, NewRepository(db), nil)
	s.SetFirewall(recentAuditFirewall(memoryfirewall.EnforcementEnforce))

	recentAuditExpectPolicyQuery(mock, cleanID, blockedID)
	recallOut := s.applyFirewall(
		context.Background(), "proj",
		[]SearchResult{{ChunkID: cleanID}, {ChunkID: blockedID}},
		memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational},
	)
	recallIDs := map[string]bool{}
	for _, r := range recallOut {
		recallIDs[r.ChunkID] = true
	}
	require.NoError(t, mock.ExpectationsWereMet())

	assert.True(t, recallIDs[cleanID], "recall keeps the allowed chunk")
	assert.False(t, recallIDs[blockedID],
		"recall drops the refuted chunk under enforce — recent_memory MUST match")
}
