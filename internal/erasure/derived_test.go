package erasure

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Article 17 erasure removed the chunks and left everything derived from them.
//
// A chunk delete cascades entity_mentions by FK and stops. knowledge_entities
// and knowledge_edges have no FK to chunks at all, and
// project_memory_quarantine's is ON DELETE SET NULL — so it keeps the chunk's
// full CONTENT with the pointer nulled. All of it survived an erasure we
// reported as complete, and entities/edges are on the retrieval path
// (query_expander seeds from entities, graph/searcher searches entities, edges
// and mentions).
//
// Measured read-only on production 2026-08-21: 3,795 entities with no surviving
// mention (456 PERSON, 254 VENDOR, all carrying embeddings) and 72 quarantine
// rows already detached from any chunk.

// derivedSpy records what the erasure asked the derived-data store to do.
type derivedSpy struct {
	captured     []string // chunk ids the sweep was scoped to
	requestIDs   []string // the erasure request id passed with each hard delete
	entitiesGone []string
	edgesGone    []string
	quarantine   []string // artifact ids whose quarantine rows were purged
	sweptChunks  []string // chunk ids handed to the sweep, for the source_chunks prune
	// captureChunks is what the capture reports as about to be erased. It must
	// match what the erasure actually deletes, or the sweep is knowingly
	// covering less than was destroyed.
	captureChunks []string
	failWith      error
	// mentionedElsewhere are entities a SURVIVING chunk still mentions, which
	// must be kept — they are another document's data.
	mentionedElsewhere map[string]bool
}

func (d *derivedSpy) CaptureDerivation(_ context.Context, artifactID string, _ []string) (Derivation, error) {
	d.captured = append(d.captured, artifactID)
	if d.failWith != nil {
		return Derivation{}, d.failWith
	}
	chunks := d.captureChunks
	if chunks == nil {
		chunks = []string{"chunk-1"}
	}
	return Derivation{
		EntityIDs: []string{"ent-erased", "ent-shared"},
		ChunkIDs:  chunks,
	}, nil
}

func (d *derivedSpy) DeleteOrphanedDerived(_ context.Context, requestID string, in Derivation) (DerivedCounts, error) {
	d.requestIDs = append(d.requestIDs, requestID)
	d.sweptChunks = append(d.sweptChunks, in.ChunkIDs...)
	if d.failWith != nil {
		return DerivedCounts{}, d.failWith
	}
	var counts DerivedCounts
	for _, id := range in.EntityIDs {
		if d.mentionedElsewhere[id] {
			continue // still mentioned by a surviving chunk — keep
		}
		d.entitiesGone = append(d.entitiesGone, id)
		counts.Entities++
	}
	d.edgesGone = append(d.edgesGone, "edge-1")
	counts.Edges = 1
	return counts, nil
}

func (d *derivedSpy) DeleteQuarantinedForArtifact(_ context.Context, requestID, artifactID string) (int, error) {
	d.requestIDs = append(d.requestIDs, requestID)
	if d.failWith != nil {
		return 0, d.failWith
	}
	d.quarantine = append(d.quarantine, artifactID)
	return 3, nil
}

// An entity mentioned ONLY by erased chunks goes; one still mentioned by a
// surviving chunk stays. That distinction is why this cannot be a wholesale
// project delete the way the benchmark clear is.
func TestEraseDerived_deletesOrphansAndKeepsSharedEntities(t *testing.T) {
	spy := &derivedSpy{mentionedElsewhere: map[string]bool{"ent-shared": true}}

	counts, err := eraseDerived(context.Background(), spy, "req-1", "artifact-1",
		Derivation{
			EntityIDs: []string{"ent-erased", "ent-shared"},
			ChunkIDs:  []string{"chunk-1", "chunk-2"},
		}, 2)
	if err != nil {
		t.Fatalf("eraseDerived: %v", err)
	}
	if len(spy.entitiesGone) != 1 || spy.entitiesGone[0] != "ent-erased" {
		t.Errorf("only the orphaned entity may be deleted; got %v", spy.entitiesGone)
	}
	if counts.Entities != 1 || counts.Edges != 1 {
		t.Errorf("counts not reported: %+v", counts)
	}
}

// The quarantine table holds the chunk's full text with failed_gate and
// failure_detail beside it, and its FK is SET NULL — so an erasure that stops
// at chunks leaves the most sensitive copy behind.
func TestEraseDerived_purgesTheQuarantinedCopy(t *testing.T) {
	spy := &derivedSpy{}

	counts, err := eraseDerived(context.Background(), spy, "req-1", "artifact-1", Derivation{EntityIDs: []string{"ent-erased"}, ChunkIDs: []string{"chunk-1"}}, 1)
	if err != nil {
		t.Fatalf("eraseDerived: %v", err)
	}
	if len(spy.quarantine) != 1 || spy.quarantine[0] != "artifact-1" {
		t.Errorf("the quarantined pre-ingest copy must be purged; got %v", spy.quarantine)
	}
	if counts.Quarantined != 3 {
		t.Errorf("Quarantined = %d, want 3", counts.Quarantined)
	}
}

// Hard deletes are gated on an erasure request id, not merely named
// distinctively: this is the only place in the codebase where deleting a graph
// row is legitimate, and a required argument is a constraint where a naming
// convention is only a hope.
func TestEraseDerived_refusesWithoutARequestID(t *testing.T) {
	spy := &derivedSpy{}

	if _, err := eraseDerived(context.Background(), spy, "   ", "artifact-1", Derivation{EntityIDs: []string{"ent-erased"}, ChunkIDs: []string{"chunk-1"}}, 1); err == nil {
		t.Fatal("a hard delete of derived personal data must carry the request that authorises it")
	}
	if len(spy.entitiesGone) != 0 || len(spy.quarantine) != 0 {
		t.Error("nothing may be deleted when the authorisation is missing")
	}
}

// Every hard delete carries the request id through, so the audit can name what
// authorised it — the erasure report is the only evidence once the rows are gone.
func TestEraseDerived_threadsTheRequestIDToEveryDelete(t *testing.T) {
	spy := &derivedSpy{}

	if _, err := eraseDerived(context.Background(), spy, "req-42", "artifact-1", Derivation{EntityIDs: []string{"ent-erased"}, ChunkIDs: []string{"chunk-1"}}, 1); err != nil {
		t.Fatalf("eraseDerived: %v", err)
	}
	if len(spy.requestIDs) == 0 {
		t.Fatal("no delete recorded a request id")
	}
	for _, got := range spy.requestIDs {
		if got != "req-42" {
			t.Errorf("request id not threaded through: %q", got)
		}
	}
}

// A failure must surface. A partially-erased subject reported as erased is the
// defect this whole change exists to fix, one level along.
func TestEraseDerived_surfacesFailure(t *testing.T) {
	spy := &derivedSpy{failWith: errors.New("deadlock detected")}

	_, err := eraseDerived(context.Background(), spy, "req-1", "artifact-1", Derivation{EntityIDs: []string{"ent-erased"}, ChunkIDs: []string{"chunk-1"}}, 1)
	if err == nil {
		t.Fatal("a failed derived-data erasure must not be reported as success")
	}
	if !strings.Contains(err.Error(), "deadlock detected") {
		t.Errorf("the cause must survive: %v", err)
	}
}

// No chunks erased means nothing was derived from them; the sweep must not run
// a global orphan hunt, which would delete rows this request did not derive.
func TestEraseDerived_noChunksMeansNoEntitySweep(t *testing.T) {
	spy := &derivedSpy{}

	counts, err := eraseDerived(context.Background(), spy, "req-1", "artifact-1", Derivation{}, 0)
	if err != nil {
		t.Fatalf("eraseDerived: %v", err)
	}
	if len(spy.entitiesGone) != 0 {
		t.Error("no chunks erased must mean no entity sweep")
	}
	// The artifact's quarantined copies are still purged: they are keyed to the
	// artifact, not to any chunk that survived to be deleted.
	if len(spy.quarantine) != 1 {
		t.Error("quarantined rows are keyed to the artifact and must still be purged")
	}
	if counts.Entities != 0 {
		t.Errorf("Entities = %d, want 0", counts.Entities)
	}
}

// Erase() must thread the whole cascade: capture before the chunk deletes, sweep
// after. A retention caller leaves Derived nil and keeps its old behaviour —
// pruning quarantines edges, it does not hard-delete graph rows.
func TestErase_runsTheDerivedCascade(t *testing.T) {
	// The fixture erases 15 chunks (12 by extraction + 3 direct); the capture
	// has to account for all of them or the sweep is knowingly incomplete.
	spy := &derivedSpy{captureChunks: make([]string, 15)}
	svc := newTestServiceWithDerived(t, spy, "req-7")

	res, err := svc.Erase(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.Derived.Quarantined != 3 {
		t.Errorf("the quarantined copy must be purged and reported; got %+v", res.Derived)
	}
	if len(spy.captured) == 0 {
		t.Error("entities must be captured BEFORE the chunks are deleted")
	}
}

// Retention prunes; it does not erase. With no Derived store wired, Erase keeps
// working and touches no graph row — quarantine remains the right terminal
// state there.
func TestErase_withoutDerivedStoreIsUnchanged(t *testing.T) {
	svc := newTestServiceWithDerived(t, nil, "")

	res, err := svc.Erase(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("Erase must remain usable for retention callers: %v", err)
	}
	if res.Derived.Total() != 0 {
		t.Errorf("no derived rows may be touched without a store: %+v", res.Derived)
	}
}

// A Derived store wired without the authorising request is a misconfigured Art
// 17 path, and must fail rather than silently skip the derived cascade.
func TestErase_derivedStoreWithoutRequestIDRefuses(t *testing.T) {
	svc := newTestServiceWithDerived(t, &derivedSpy{}, "")

	if _, err := svc.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a wired derived store with no request id must refuse, not skip silently")
	}
}

// newTestServiceWithDerived reuses the package's existing fakes and adds the
// derived-data store under test.
func newTestServiceWithDerived(t *testing.T, derived DerivedStore, requestID string) *Service {
	t.Helper()
	svc, _, _, _ := newService(t)
	if derived != nil {
		svc.Derived = derived
	}
	svc.RequestID = requestID
	return svc
}

// A chunk that arrives between the capture and the delete is erased without its
// derived rows being in scope. The capture and the chunk deletes are separate
// transactions, so the window is real; what must not happen is covering less
// than was deleted and reporting success anyway.
func TestEraseDerived_refusesToReportSuccessWhenMoreWasDeletedThanCaptured(t *testing.T) {
	spy := &derivedSpy{}

	// Two chunks deleted, one captured: something landed mid-erasure.
	_, err := eraseDerived(context.Background(), spy, "req-1", "artifact-1",
		Derivation{EntityIDs: []string{"ent-erased"}, ChunkIDs: []string{"chunk-1"}}, 2)
	if err == nil {
		t.Fatal("an erasure that swept less than it deleted must not report success")
	}
	if !strings.Contains(err.Error(), "prune-orphaned-entities") {
		t.Errorf("the error must name the remedy, got: %v", err)
	}
}

// The ordinary case must not trip it: capturing MORE chunks than were deleted
// is normal, because a chunk can be removed by something else between the two.
func TestEraseDerived_capturingMoreThanWasDeletedIsFine(t *testing.T) {
	spy := &derivedSpy{}

	if _, err := eraseDerived(context.Background(), spy, "req-1", "artifact-1",
		Derivation{EntityIDs: []string{"ent-erased"}, ChunkIDs: []string{"chunk-1", "chunk-2"}}, 1); err != nil {
		t.Fatalf("a shrinking chunk set is not an incomplete sweep: %v", err)
	}
}
