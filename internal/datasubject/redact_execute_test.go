package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Slice 5c execution (design §5, §4.2, §8). The decision layer is tested in
// erasure_test.go; this file tests what happens when a DispositionRedact action is
// actually carried out — the generative path, where every failure must land as
// "deferred to a human" rather than "reported as erased".

// --- fakes ---

type fakeRedactor struct {
	content    map[string]string // chunk id -> text
	hash       map[string]string // chunk id -> content hash
	result     RedactionResult
	resultFor  map[string]RedactionResult
	err        error
	loadErr    error
	calls      []string
	gotGuard   map[string]string
	gotContent map[string]string
}

func newFakeRedactor() *fakeRedactor {
	return &fakeRedactor{
		content: map[string]string{}, hash: map[string]string{},
		resultFor: map[string]RedactionResult{},
		gotGuard:  map[string]string{}, gotContent: map[string]string{},
	}
}

func (f *fakeRedactor) LoadChunk(_ context.Context, id string) (string, string, error) {
	if f.loadErr != nil {
		return "", "", f.loadErr
	}
	return f.content[id], f.hash[id], nil
}

func (f *fakeRedactor) RedactChunk(_ context.Context, id, expectedHash, newContent string) (RedactionResult, error) {
	f.calls = append(f.calls, id)
	f.gotGuard[id] = expectedHash
	f.gotContent[id] = newContent
	if f.err != nil {
		return RedactionResult{}, f.err
	}
	if r, ok := f.resultFor[id]; ok {
		return r, nil
	}
	if f.result.Outcome == "" {
		return RedactionResult{Outcome: RedactionApplied, NewHash: "hash-after-" + id}, nil
	}
	return f.result, nil
}

type fakeRewriter struct {
	out    string
	outFor map[string]string
	err    error
	seen   []string
}

func (f *fakeRewriter) RewriteWithout(_ context.Context, content string, _ []string) (string, error) {
	f.seen = append(f.seen, content)
	if f.err != nil {
		return "", f.err
	}
	if o, ok := f.outFor[content]; ok {
		return o, nil
	}
	return f.out, nil
}
func (f *fakeRewriter) ModelVersion() string { return "test-rewriter-v1" }

type fakeIDs struct {
	ids []string
	err error
}

func (f fakeIDs) Identifiers(context.Context, string) ([]string, error) { return f.ids, f.err }

type recordingDeleter struct {
	deleted []string
	err     error
}

func (r *recordingDeleter) DeleteRow(_ context.Context, _ LinkableTable, id string) error {
	if r.err != nil {
		return r.err
	}
	r.deleted = append(r.deleted, id)
	return nil
}

type noArtifacts struct{}

func (noArtifacts) EraseArtifact(context.Context, string) (ArtifactErasureCounts, error) {
	return ArtifactErasureCounts{}, nil
}

// redactPlan builds a verified plan with the given chunks marked for redaction.
func redactPlan(chunkIDs ...string) *ErasurePlan {
	p := &ErasurePlan{SubjectID: "subj-1", RequestID: "req-1"}
	for _, id := range chunkIDs {
		p.Actions = append(p.Actions, Action{
			Table: TableProjectMemoryChunks, RowID: id,
			Disposition: DispositionRedact, Reason: "shared row",
		})
	}
	return p
}

// approveAll is an operator who accepts every proposal.
func approveAll(RedactionProposal) (bool, error) { return true, nil }

func redactExecutor(r *fakeRedactor, w *fakeRewriter, ids []string,
	approve func(RedactionProposal) (bool, error)) (*Executor, *recordingDeleter) {
	del := &recordingDeleter{}
	return &Executor{
		Rows: del, Artifacts: noArtifacts{},
		Redact: &RedactDeps{
			Redactor: r, Rewriter: w, Identifiers: fakeIDs{ids: ids}, Approve: approve,
		},
	}, del
}

// --- the happy path ---

func TestExecute_RedactionAppliesAndRecordsBeforeAndAfter(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com; Peter Novak joined."
	r.hash["c1"] = "hash-before-c1"
	w := &fakeRewriter{out: "Called the client; Peter Novak joined."}
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Redacted) != 1 {
		t.Fatalf("expected one redaction, got %+v (deferred: %+v, failed: %+v)",
			res.Redacted, res.Deferred, res.Failed)
	}
	got := res.Redacted[0]
	if got.BeforeHash != "hash-before-c1" || got.AfterHash != "hash-after-c1" {
		t.Errorf("both hashes must be recorded for auditability, got %+v", got)
	}
	if got.Model != "test-rewriter-v1" {
		t.Errorf("the model must be attributable, got %q", got.Model)
	}
	if !got.Verified {
		t.Error("a committed redaction is by construction verified; the record must say so")
	}
	if got.ReviewBypassed {
		t.Error("this redaction was reviewed, so ReviewBypassed must be false")
	}
	if !res.Complete() {
		t.Error("a fully applied plan must report Complete")
	}
	// The version guard must carry the hash read at load time.
	if r.gotGuard["c1"] != "hash-before-c1" {
		t.Errorf("the write must be guarded on the loaded hash, got %q", r.gotGuard["c1"])
	}
}

// --- the floor ---

// THE HEADLINE. A rewrite that still contains the subject must never be written.
func TestExecute_RewriteThatFailsVerificationIsNotWritten(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com about the results."
	r.hash["c1"] = "h1"
	// The model "redacted" the name but left the address.
	w := &fakeRewriter{out: "Called the client at jane@example.com about the results."}
	e, del := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("a failed verification is a deferral, not an error: %v", err)
	}
	if len(r.calls) != 0 {
		t.Error("the store must not be asked to write a rewrite that failed verification")
	}
	if len(del.deleted) != 0 {
		t.Error("a failed verification must not fall back to deleting the row")
	}
	if len(res.Redacted) != 0 {
		t.Error("nothing may be reported as redacted")
	}
	if len(res.Deferred) != 1 {
		t.Fatalf("the chunk must be deferred, got %+v", res.Deferred)
	}
	if res.Complete() {
		t.Error("a deferred chunk means the request is NOT complete — this is what stops " +
			"the ledger recording a false completion")
	}
	if !strings.Contains(res.Deferred[0].Reason, "NOT been changed") {
		t.Errorf("the reason must state plainly that the record is unchanged, got %q",
			res.Deferred[0].Reason)
	}
}

// A model error is a deferral too — nothing was written, so nothing is lost.
func TestExecute_RewriterFailureDefersWithoutWriting(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "text"
	r.hash["c1"] = "h1"
	w := &fakeRewriter{err: errors.New("model timeout")}
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("a model failure must not fail the whole erasure: %v", err)
	}
	if len(r.calls) != 0 {
		t.Error("no write may be attempted after a model failure")
	}
	if len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "NOT been changed") {
		t.Errorf("expected a deferral naming the unchanged record, got %+v", res.Deferred)
	}
}

// --- the version guard, and its RECOVERY (§9) ---

// The real risk is not the guard failing to fire; it is the deferred state being
// thrown away when it does. All four post-conditions are asserted.
func TestExecute_VersionGuardFiresAndKeepsTheChunkDeferred(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com."
	r.hash["c1"] = "h1"
	r.result = RedactionResult{Outcome: RedactionVersionChanged}
	w := &fakeRewriter{out: "Called the client."}
	e, del := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("a fired guard is a deferral, not an error: %v", err)
	}
	if len(res.Redacted) != 0 { // (a) no write recorded
		t.Error("a chunk that changed under us must not be recorded as redacted")
	}
	if len(del.deleted) != 0 {
		t.Error("and must not be deleted as a fallback")
	}
	if len(res.Deferred) != 1 { // (b) still deferred
		t.Fatalf("the chunk must remain deferred, got %+v", res.Deferred)
	}
	if res.Complete() { // (c) request stays un-actioned
		t.Error("the request must not look complete while a chunk is deferred")
	}
	if !strings.Contains(res.Deferred[0].Reason, "changed while") { // (d) reason recorded
		t.Errorf("the reason must explain the race, got %q", res.Deferred[0].Reason)
	}
}

// --- collisions (§4.2) ---

// Two chunks differing only in the erased subject's data redact to the same text.
// The survivor already carries the other subjects' data, so this chunk is DELETED —
// and reported as a deletion, because that is a different thing to tell the subject.
func TestExecute_CollisionDeletesAndReportsSeparatelyFromRedaction(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com; Peter joined."
	r.hash["c1"] = "h1"
	r.result = RedactionResult{Outcome: RedactionCollision, SurvivorID: "c-existing"}
	w := &fakeRewriter{out: "Called the client; Peter joined."}
	e, del := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Redacted) != 0 {
		t.Error("a collision is not a redaction and must not be reported as one")
	}
	if len(res.CollisionDeleted) != 1 || res.CollisionDeleted[0].SurvivorID != "c-existing" {
		t.Fatalf("expected one collision deletion naming the survivor, got %+v", res.CollisionDeleted)
	}
	if len(del.deleted) != 1 || del.deleted[0] != "c1" {
		t.Errorf("the colliding chunk must actually be deleted, got %v", del.deleted)
	}
	if !res.Complete() {
		t.Error("a resolved collision leaves nothing outstanding")
	}
}

// THE SUB-CASE THAT MATTERS. If the survivor is itself awaiting redaction, deleting
// into it would preserve a chunk that still contains the subject. It must wait for
// the second pass instead.
func TestExecute_CollisionWithAPendingSurvivorRetriesInTheSecondPass(t *testing.T) {
	r := newFakeRedactor()
	for _, id := range []string{"c1", "c2"} {
		r.content[id] = "Called jane@example.com; Peter joined."
		r.hash[id] = "h-" + id
	}
	// c1 collides with c2 (which is also in the plan) on the first attempt, then
	// succeeds on the retry once c2 has been dealt with. c2 applies immediately.
	attempt := 0
	r.resultFor["c2"] = RedactionResult{Outcome: RedactionApplied, NewHash: "hash-after-c2"}
	w := &fakeRewriter{out: "Called the client; Peter joined."}
	del := &recordingDeleter{}
	e := &Executor{
		Rows: del, Artifacts: noArtifacts{},
		Redact: &RedactDeps{
			Redactor: &collisionThenApply{fakeRedactor: r, attempt: &attempt},
			Rewriter: w, Identifiers: fakeIDs{ids: []string{"jane@example.com"}},
			Approve: approveAll,
		},
	}

	res, err := e.Execute(context.Background(), redactPlan("c1", "c2"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(del.deleted) != 0 {
		t.Errorf("c1 must NOT be deleted into a survivor that still contained the subject, "+
			"deleted %v", del.deleted)
	}
	if len(res.Redacted) != 2 {
		t.Fatalf("both chunks should end up redacted after the second pass, got %+v "+
			"(deferred %+v)", res.Redacted, res.Deferred)
	}
	if !res.Complete() {
		t.Error("both resolved, so the request is complete")
	}
}

// collisionThenApply reports a collision with a pending survivor the first time c1 is
// written, then applies on the retry.
type collisionThenApply struct {
	*fakeRedactor
	attempt *int
}

func (c *collisionThenApply) RedactChunk(ctx context.Context, id, guard, content string) (RedactionResult, error) {
	if id == "c1" {
		*c.attempt++
		if *c.attempt == 1 {
			c.calls = append(c.calls, id)
			return RedactionResult{Outcome: RedactionCollision, SurvivorID: "c2"}, nil
		}
		c.calls = append(c.calls, id)
		return RedactionResult{Outcome: RedactionApplied, NewHash: "hash-after-c1"}, nil
	}
	return c.fakeRedactor.RedactChunk(ctx, id, guard, content)
}

// A chunk still colliding after the second pass is reported deferred, not retried
// forever.
func TestExecute_CollisionStillUnresolvedAfterTheSecondPassIsDeferred(t *testing.T) {
	r := newFakeRedactor()
	for _, id := range []string{"c1", "c2"} {
		r.content[id] = "Called jane@example.com."
		r.hash[id] = "h-" + id
		r.resultFor[id] = RedactionResult{Outcome: RedactionCollision, SurvivorID: otherOf(id)}
	}
	w := &fakeRewriter{out: "Called the client."}
	e, del := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1", "c2"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(del.deleted) != 0 {
		t.Errorf("nothing may be deleted while the survivor is unresolved, got %v", del.deleted)
	}
	if len(res.Deferred) != 2 {
		t.Fatalf("both must end deferred rather than looping, got %+v", res.Deferred)
	}
	for _, d := range res.Deferred {
		if !strings.Contains(d.Reason, "manual handling") {
			t.Errorf("the reason must send it to a human, got %q", d.Reason)
		}
	}
	if res.Complete() {
		t.Error("unresolved chunks mean the request is not complete")
	}
}

func otherOf(id string) string {
	if id == "c1" {
		return "c2"
	}
	return "c1"
}

// --- operator review (§8) ---

// Review is default-ON permanently. No approver wired means DEFER, never apply: the
// alternative is an unreviewed generative write to a record about a third party.
func TestExecute_NoApproverDefersRatherThanApplying(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client."}
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, nil)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.calls) != 0 {
		t.Error("a rewrite must not be committed with no operator review wired")
	}
	if len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "review") {
		t.Fatalf("expected a review deferral, got %+v", res.Deferred)
	}
}

func TestExecute_DeclinedReviewDefersAndSaysSo(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client."}
	decline := func(RedactionProposal) (bool, error) { return false, nil }
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, decline)

	res, _ := e.Execute(context.Background(), redactPlan("c1"))
	if len(r.calls) != 0 {
		t.Error("a declined proposal must not be written")
	}
	if len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "declined") {
		t.Fatalf("expected a declined-review deferral, got %+v", res.Deferred)
	}
}

// The operator sees the actual before/after text — the diff is the whole point of
// the gate, since over-redaction destroys the OTHER subject's data.
func TestExecute_ReviewSeesTheBeforeAndAfterText(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com; Peter joined."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client; Peter joined."}
	var seen RedactionProposal
	capture := func(p RedactionProposal) (bool, error) { seen = p; return true, nil }
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, capture)

	if _, err := e.Execute(context.Background(), redactPlan("c1")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen.Before != "Called jane@example.com; Peter joined." {
		t.Errorf("the operator must see the original text, got %q", seen.Before)
	}
	if seen.After != "Called the client; Peter joined." {
		t.Errorf("the operator must see the proposed text, got %q", seen.After)
	}
	if seen.Model != "test-rewriter-v1" {
		t.Errorf("and which model proposed it, got %q", seen.Model)
	}
}

// --apply bypasses review and is AUDITED — every action it commits is marked, so a
// later audit can find every record changed without a human reading it.
func TestExecute_ApplyWithoutReviewIsRecordedOnEveryAction(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client."}
	e := &Executor{
		Rows: &recordingDeleter{}, Artifacts: noArtifacts{},
		Redact: &RedactDeps{
			Redactor: r, Rewriter: w,
			Identifiers:        fakeIDs{ids: []string{"jane@example.com"}},
			ApplyWithoutReview: true, // no Approve at all
		},
	}
	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Redacted) != 1 {
		t.Fatalf("--apply must commit without an approver, got %+v / %+v", res.Redacted, res.Deferred)
	}
	if !res.Redacted[0].ReviewBypassed {
		t.Error("a bypassed review must be recorded, or the audit trail cannot find it")
	}
}

// --- identifiers ---

// Without identifiers there is nothing to verify a rewrite against, so a generated
// replacement would be written on an unverifiable basis. Defer instead.
func TestExecute_UnreadableIdentifiersDeferRatherThanGuess(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client."}
	e := &Executor{
		Rows: &recordingDeleter{}, Artifacts: noArtifacts{},
		Redact: &RedactDeps{
			Redactor: r, Rewriter: w,
			Identifiers: fakeIDs{err: errors.New("db down")},
			Approve:     approveAll,
		},
	}
	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.calls) != 0 || len(w.seen) != 0 {
		t.Error("nothing may be rewritten or written when the identifiers are unknown")
	}
	if len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "identifiers could not be read") {
		t.Fatalf("expected an identifier-read deferral, got %+v", res.Deferred)
	}
}

// --- capability absent ---

// An executor with no redaction capability still reports the row, never silently
// skips it, and does NOT error — a missing capability is not a fault. This preserves
// the 5b behaviour for callers that only delete.
func TestExecute_NoRedactionCapabilityStillReportsTheRow(t *testing.T) {
	e := &Executor{Rows: &recordingDeleter{}, Artifacts: noArtifacts{}}
	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil {
		t.Fatalf("a missing capability must not be an error: %v", err)
	}
	if len(res.Deferred) != 1 {
		t.Fatalf("the row must be reported, got %+v", res.Deferred)
	}
	if !strings.Contains(res.Deferred[0].Reason, "NOT been erased") {
		t.Errorf("the reason must be explicit that nothing happened, got %q", res.Deferred[0].Reason)
	}
	if res.Complete() {
		t.Error("a deferred row means the request is not complete")
	}
}

// --- mixed plan ---

// Deletes and redactions in one plan: each is tallied in its own bucket, so the
// report cannot present a redaction as a deletion or vice versa.
func TestExecute_MixedPlanTalliesEachDispositionSeparately(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com; Peter joined."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client; Peter joined."}
	e, del := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	plan := redactPlan("c1")
	plan.Actions = append(plan.Actions, Action{
		Table: TableChatAuditLog, RowID: "row-9",
		Disposition: DispositionDelete, Reason: "exclusive",
	})

	res, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.RowsDeleted != 1 || len(res.Redacted) != 1 {
		t.Errorf("expected 1 delete and 1 redaction, got %d deleted / %d redacted",
			res.RowsDeleted, len(res.Redacted))
	}
	if len(del.deleted) != 1 || del.deleted[0] != "row-9" {
		t.Errorf("only the exclusive row should be deleted, got %v", del.deleted)
	}
	if !res.Complete() {
		t.Errorf("everything was actioned: %+v", res)
	}
}

// A store error on the write is a FAILURE, not a deferral: something went wrong and
// the caller must see an error.
func TestExecute_StoreErrorOnRedactionIsAFailure(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com."
	r.hash["c1"] = "h1"
	r.err = errors.New("deadlock detected")
	w := &fakeRewriter{out: "Called the client."}
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if !errors.Is(err, ErrPartialErasure) {
		t.Fatalf("a store error must surface as ErrPartialErasure, got %v", err)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("the failure must be recorded, got %+v", res.Failed)
	}
	if res.Complete() {
		t.Error("a failed row means the request is not complete")
	}
}

// A load failure is likewise a failure — we cannot rewrite what we cannot read.
func TestExecute_LoadFailureIsRecorded(t *testing.T) {
	r := newFakeRedactor()
	r.loadErr = errors.New("chunk vanished")
	w := &fakeRewriter{out: "x"}
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)

	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if !errors.Is(err, ErrPartialErasure) {
		t.Fatalf("expected ErrPartialErasure, got %v", err)
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Err, "load chunk") {
		t.Fatalf("the failure must name the load step, got %+v", res.Failed)
	}
}

// pendingRedaction underpins the collision sub-case; a wrong answer here either
// deletes into a dirty survivor or defers forever.
func TestPendingRedaction(t *testing.T) {
	plan := redactPlan("c1", "c2")
	plan.Actions = append(plan.Actions, Action{
		Table: TableProjectMemoryChunks, RowID: "c3", Disposition: DispositionDelete,
	})
	if !plan.pendingRedaction("c2") {
		t.Error("c2 is awaiting redaction in this plan")
	}
	if plan.pendingRedaction("c3") {
		t.Error("c3 is a DELETE, not a pending redaction — deleting into it is safe")
	}
	if plan.pendingRedaction("c-unknown") || plan.pendingRedaction("") {
		t.Error("unknown and empty ids are not pending")
	}
	var nilPlan *ErasurePlan
	if nilPlan.pendingRedaction("c1") {
		t.Error("a nil plan has nothing pending")
	}
}

// Guard against a plan whose verification state was never checked: Execute must not
// begin an irreversible operation on an unverified request. (Belt-and-braces with
// PlanErasure, which already refuses to build such a plan.)
func TestExecute_StillRefusesAHalfWiredExecutor(t *testing.T) {
	e := &Executor{Rows: nil, Artifacts: nil}
	if _, err := e.Execute(context.Background(), redactPlan("c1")); err == nil {
		t.Fatal("a half-wired executor must refuse before touching anything")
	}
}

var _ = time.Now
