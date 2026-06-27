package memoryfirewall

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed clock for deterministic expiry tests.
func clockAt(s string) func() time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return func() time.Time { return t }
}

// helper builds a Chunk with the supplied policy applied; the
// digest is filled with PolicyDigest(p) so per-row provenance
// stays consistent.
func chunkWith(p Policy) Chunk {
	return Chunk{ID: "c1", Policy: p, Digest: PolicyDigest(p)}
}

func TestEvaluator_AllowAllDefault(t *testing.T) {
	e := NewEvaluator()
	// Empty policy = all dimensions unset; any request allows.
	dec, reason := e.Decide(chunkWith(Policy{}), RequestContext{Role: "coder"})
	assert.Equal(t, DecisionAllow, dec)
	assert.Empty(t, reason)
}

func TestEvaluator_BlocksExpired(t *testing.T) {
	expiry := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := NewEvaluatorWithClock(clockAt("2026-02-01T00:00:00Z"))
	dec, reason := e.Decide(chunkWith(Policy{ExpiresAt: &expiry}), RequestContext{})
	assert.Equal(t, DecisionBlockExpired, dec)
	assert.Contains(t, reason, "expired")
}

func TestEvaluator_PreExpiry_Allows(t *testing.T) {
	expiry := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	e := NewEvaluatorWithClock(clockAt("2026-05-01T00:00:00Z"))
	dec, _ := e.Decide(chunkWith(Policy{ExpiresAt: &expiry}), RequestContext{})
	assert.Equal(t, DecisionAllow, dec)
}

func TestEvaluator_TenantMismatch(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{TenantID: "tenant-a"})
	dec, reason := e.Decide(c, RequestContext{TenantID: "tenant-b"})
	assert.Equal(t, DecisionBlockTenantMismatch, dec)
	assert.Contains(t, reason, "tenant-a")
	assert.Contains(t, reason, "tenant-b")
}

func TestEvaluator_BothEmptyTenant_Matches(t *testing.T) {
	// Legacy single-tenant world: both sides empty = match.
	e := NewEvaluator()
	dec, _ := e.Decide(chunkWith(Policy{}), RequestContext{})
	assert.Equal(t, DecisionAllow, dec)
}

// TestEvaluator_TaggedChunkBlankRequest_Blocked locks the fail-closed
// posture for the cross-tenant-leak vector: a tenant-TAGGED chunk must
// NEVER be served to a request that carries no tenant_id. A regression
// here would leak one tenant's restricted memory to an untenanted (e.g.
// autonomy / legacy Search) request.
func TestEvaluator_TaggedChunkBlankRequest_Blocked(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{TenantID: "tenant-a"})
	dec, reason := e.Decide(c, RequestContext{}) // blank request tenant
	assert.Equal(t, DecisionBlockTenantMismatch, dec)
	assert.Contains(t, reason, "tenant-a")
}

// TestEvaluator_UntaggedChunkTaggedRequest_Blocked locks the other
// mixed case: a request scoped to tenant-b must not read an untagged
// (legacy) chunk.
func TestEvaluator_UntaggedChunkTaggedRequest_Blocked(t *testing.T) {
	e := NewEvaluator()
	dec, _ := e.Decide(chunkWith(Policy{}), RequestContext{TenantID: "tenant-b"})
	assert.Equal(t, DecisionBlockTenantMismatch, dec)
}

// TestEvaluator_StrictTenantIsolation locks the opt-in fail-closed mode:
// with strict isolation ON, a blank-tenant request matches NOTHING (not
// even untagged chunks), while a request carrying the matching tenant
// still reads its own tagged chunk. Default (OFF) behaviour is unchanged
// and covered by TestEvaluator_BothEmptyTenant_Matches.
func TestEvaluator_StrictTenantIsolation(t *testing.T) {
	e := NewEvaluator().WithStrictTenantIsolation(true)

	// Blank request, untagged chunk — allowed in default mode, BLOCKED here.
	dec, reason := e.Decide(chunkWith(Policy{}), RequestContext{})
	assert.Equal(t, DecisionBlockTenantMismatch, dec)
	assert.Contains(t, reason, "strict tenant isolation")

	// Blank request, tagged chunk — blocked (as in default mode too).
	dec, _ = e.Decide(chunkWith(Policy{TenantID: "tenant-a"}), RequestContext{})
	assert.Equal(t, DecisionBlockTenantMismatch, dec)

	// Properly-tenanted request reading its own tagged chunk — allowed.
	dec, _ = e.Decide(chunkWith(Policy{TenantID: "tenant-a"}), RequestContext{TenantID: "tenant-a"})
	assert.Equal(t, DecisionAllow, dec)
}

// TestEvaluator_StrictOff_IsDefaultBehaviour guards that the production
// constructor leaves strict isolation OFF (single-tenant must keep
// working): blank-vs-blank still matches.
func TestEvaluator_StrictOff_IsDefaultBehaviour(t *testing.T) {
	e := NewEvaluator()
	dec, _ := e.Decide(chunkWith(Policy{}), RequestContext{})
	assert.Equal(t, DecisionAllow, dec)
}

func TestEvaluator_BlocksRoleNotInSet(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{PermittedRoles: []string{"analyst", "executor"}})
	dec, reason := e.Decide(c, RequestContext{Role: "coder"})
	assert.Equal(t, DecisionBlockRoleNotPermitted, dec)
	assert.Contains(t, reason, "coder")
}

func TestEvaluator_AllowsRoleInSet(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{PermittedRoles: []string{"analyst", "executor"}})
	dec, _ := e.Decide(c, RequestContext{Role: "analyst"})
	assert.Equal(t, DecisionAllow, dec)
}

func TestEvaluator_RefutedChunkDenyAll(t *testing.T) {
	// Bridge translates validation_status='refuted' to an
	// explicit empty role list — every role gets blocked.
	e := NewEvaluator()
	p := ApplyClassifierSignal(Policy{}, "", "refuted")
	dec, _ := e.Decide(chunkWith(p), RequestContext{Role: "anybody"})
	assert.Equal(t, DecisionBlockRoleNotPermitted, dec)
}

func TestEvaluator_PurposeBlocking(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{AllowedPurposes: []Purpose{PurposeOperational}})
	// Caller asking for training_data on an operational-only chunk → block.
	dec, reason := e.Decide(c, RequestContext{Purpose: PurposeTrainingData})
	assert.Equal(t, DecisionBlockPurposeNotAllowed, dec)
	assert.Contains(t, reason, "training_data")
}

func TestEvaluator_EmptyPurpose_DefaultsToOperational(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{AllowedPurposes: []Purpose{PurposeOperational}})
	dec, _ := e.Decide(c, RequestContext{}) // empty purpose
	assert.Equal(t, DecisionAllow, dec)
}

func TestEvaluator_RestrictedRequiresOperator(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{Sensitivity: SensitivityRestricted})
	// Anonymous (autonomy-driven) request.
	dec, reason := e.Decide(c, RequestContext{})
	assert.Equal(t, DecisionBlockSensitivityTier, dec)
	assert.Contains(t, reason, "operator_id")

	// With operator_id present → allows.
	dec, _ = e.Decide(c, RequestContext{OperatorID: "vadim"})
	assert.Equal(t, DecisionAllow, dec)
}

func TestEvaluator_ShortCircuits_FirstBlockWins(t *testing.T) {
	// Both expiry AND tenant mismatch — expiry check runs
	// first so its decision wins. Pins the documented order.
	expiry := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := NewEvaluatorWithClock(clockAt("2026-02-01T00:00:00Z"))
	c := chunkWith(Policy{
		ExpiresAt: &expiry,
		TenantID:  "tenant-a",
	})
	dec, _ := e.Decide(c, RequestContext{TenantID: "tenant-b"})
	assert.Equal(t, DecisionBlockExpired, dec, "expiry check must run before tenant")
}

func TestPolicyDigest_StableUnderReordering(t *testing.T) {
	// Same policy with role / purpose slices in different
	// orders should produce the same digest. Pins
	// canonicalisation.
	a := Policy{
		PermittedRoles:  []string{"coder", "analyst"},
		AllowedPurposes: []Purpose{PurposeTrainingData, PurposeOperational},
	}
	b := Policy{
		PermittedRoles:  []string{"analyst", "coder"},
		AllowedPurposes: []Purpose{PurposeOperational, PurposeTrainingData},
	}
	assert.Equal(t, PolicyDigest(a), PolicyDigest(b))
}

func TestPolicyDigest_StableUnderDuplicates(t *testing.T) {
	// Duplicates in the slice canonicalise out.
	a := Policy{PermittedRoles: []string{"coder", "coder"}}
	b := Policy{PermittedRoles: []string{"coder"}}
	assert.Equal(t, PolicyDigest(a), PolicyDigest(b))
}

func TestPolicyDigest_ChangesOnRevision(t *testing.T) {
	a := Policy{Sensitivity: SensitivityPublic}
	b := Policy{Sensitivity: SensitivityRestricted}
	assert.NotEqual(t, PolicyDigest(a), PolicyDigest(b))
}

func TestDefaultPolicyForSource_OperatorCorrection(t *testing.T) {
	p := DefaultPolicyForSource(ProvenanceOperatorCorrection, "")
	assert.Equal(t, ProvenanceOperatorCorrection, p.Provenance.Source)
	assert.Equal(t, 95, p.Provenance.TrustLevel)
	assert.Equal(t, SensitivityInternal, p.Sensitivity)
}

func TestDefaultPolicyForSource_CredentialsOverridesToRestricted(t *testing.T) {
	// Even an operator-pasted credential is still a credential.
	p := DefaultPolicyForSource(ProvenanceOperatorCorrection, "credentials")
	assert.Equal(t, SensitivityRestricted, p.Sensitivity)
}

func TestDefaultPolicyForSource_ExternalFetchIsPublic(t *testing.T) {
	p := DefaultPolicyForSource(ProvenanceExternalFetch, "")
	assert.Equal(t, SensitivityPublic, p.Sensitivity)
	assert.Equal(t, 30, p.Provenance.TrustLevel)
}

// recordingSink captures BatchInsert calls so the writer test
// can assert on batching + ordering.
type recordingSink struct {
	mu      sync.Mutex
	batches [][]EvaluationRow
	err     error
}

func (s *recordingSink) BatchInsert(_ context.Context, rows []EvaluationRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	// Deep-copy: the caller may reuse the slice; record the
	// snapshot so test assertions see stable state.
	cp := make([]EvaluationRow, len(rows))
	copy(cp, rows)
	s.batches = append(s.batches, cp)
	return nil
}

func (s *recordingSink) snapshot() [][]EvaluationRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]EvaluationRow, len(s.batches))
	copy(out, s.batches)
	return out
}

func TestAuditWriter_BatchesAndFlushes(t *testing.T) {
	sink := &recordingSink{}
	w := NewAuditWriter(sink, zerolog.Nop())
	w.flushEvery = 20 * time.Millisecond // tighter for test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	for i := 0; i < 3; i++ {
		w.Enqueue(EvaluationRow{ID: "r", ChunkID: "c", Decision: DecisionAllow})
	}
	// Wait for at least one interval flush.
	require.Eventually(t, func() bool {
		return len(sink.snapshot()) > 0
	}, time.Second, 5*time.Millisecond)

	all := 0
	for _, b := range sink.snapshot() {
		all += len(b)
	}
	assert.Equal(t, 3, all)
}

func TestAuditWriter_StopDrainsRemainingRows(t *testing.T) {
	sink := &recordingSink{}
	w := NewAuditWriter(sink, zerolog.Nop())
	w.flushEvery = time.Hour // disable interval flush
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	for i := 0; i < 5; i++ {
		w.Enqueue(EvaluationRow{ID: "r", ChunkID: "c", Decision: DecisionAllow})
	}
	w.Stop()
	all := 0
	for _, b := range sink.snapshot() {
		all += len(b)
	}
	assert.Equal(t, 5, all, "Stop must drain pending rows before returning")
}

func TestAuditWriter_FullQueueDropsRowNotPanics(t *testing.T) {
	sink := &recordingSink{}
	w := NewAuditWriter(sink, zerolog.Nop())
	// Don't start the goroutine — buffer fills + we just
	// verify Enqueue doesn't block / panic.
	for i := 0; i < 1000; i++ {
		w.Enqueue(EvaluationRow{ID: "r", ChunkID: "c", Decision: DecisionAllow})
	}
	// Channel has cap 500; the extra 500 should have been dropped silently.
	assert.Len(t, w.queue, 500)
}
