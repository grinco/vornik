package dispatcher

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SLICE 3 (part 2) of the chat memory-write design §5.3.2 — the shared-scope state machine
// wired into the tool: PROPOSED → (ack in the receiver) → AUTHORIZED. These tests exercise
// ToolExecutor.remember directly with in-memory fakes, no model and no real DB.

// Fixed identities the shared-scope tool tests exercise. The originating channel + session are
// what WithCallSiteForTest stamps and what ToolExecutor.remember reads back.
const (
	testMemChannel  = "slack"
	testMemSession  = "sess"
	testMemOperator = "slack:UALICE"
)

// fakeConfirmRepo is an in-memory persistence.ChatMemoryWriteConfirmationRepository. It shares
// an ops log with fakeAuditRepo so a test can assert the audit row is written BEFORE the
// pending row is deleted (design §5.3.3).
type fakeConfirmRepo struct {
	mu         sync.Mutex
	rows       map[string]persistence.ChatMemoryWriteConfirmation
	ops        *[]string
	getErr     error
	proposeErr error
}

func newFakeConfirmRepo(ops *[]string) *fakeConfirmRepo {
	return &fakeConfirmRepo{rows: map[string]persistence.ChatMemoryWriteConfirmation{}, ops: ops}
}

func confirmKey(channel, session string) string { return channel + "|" + session }

func (f *fakeConfirmRepo) log(op string) {
	if f.ops != nil {
		*f.ops = append(*f.ops, op)
	}
}

func (f *fakeConfirmRepo) Propose(_ context.Context, c *persistence.ChatMemoryWriteConfirmation) error {
	if f.proposeErr != nil {
		return f.proposeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("propose")
	cp := *c
	cp.AcknowledgedAt = nil // upsert clears any previous acknowledgement
	f.rows[confirmKey(c.Channel, c.SessionID)] = cp
	return nil
}

func (f *fakeConfirmRepo) Get(_ context.Context, channel, session string) (*persistence.ChatMemoryWriteConfirmation, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[confirmKey(channel, session)]
	if !ok {
		return nil, nil
	}
	cp := row
	if row.AcknowledgedAt != nil {
		t := *row.AcknowledgedAt
		cp.AcknowledgedAt = &t
	}
	return &cp, nil
}

func (f *fakeConfirmRepo) Acknowledge(_ context.Context, channel, session, operatorID string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := confirmKey(channel, session)
	row, ok := f.rows[k]
	if !ok || row.OperatorID != operatorID {
		return false, nil
	}
	t := at
	row.AcknowledgedAt = &t
	f.rows[k] = row
	f.log("acknowledge")
	return true, nil
}

func (f *fakeConfirmRepo) Delete(_ context.Context, channel, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("delete")
	delete(f.rows, confirmKey(channel, session))
	return nil
}

func (f *fakeConfirmRepo) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, row := range f.rows {
		if !now.Before(row.ExpiresAt) {
			delete(f.rows, k)
			n++
		}
	}
	return n, nil
}

// seedAcknowledged inserts a row that has already been acknowledged — the state the receiver
// hook would have produced from a human turn.
func (f *fakeConfirmRepo) seedAcknowledged(rec persistence.ChatMemoryWriteConfirmation, ackAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec.AcknowledgedAt = &ackAt
	f.rows[confirmKey(rec.Channel, rec.SessionID)] = rec
}

func (f *fakeConfirmRepo) get(channel string) (persistence.ChatMemoryWriteConfirmation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[confirmKey(channel, testMemSession)]
	return row, ok
}

// fakeAuditRepo is an in-memory append-only persistence.ChatMemoryWriteAuditRepository.
type fakeAuditRepo struct {
	mu   sync.Mutex
	rows []persistence.ChatMemoryWriteAudit
	ops  *[]string
	err  error
}

func newFakeAuditRepo(ops *[]string) *fakeAuditRepo {
	return &fakeAuditRepo{ops: ops}
}

func (f *fakeAuditRepo) Record(_ context.Context, a *persistence.ChatMemoryWriteAudit) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ops != nil {
		*f.ops = append(*f.ops, "record")
	}
	f.rows = append(f.rows, *a)
	return nil
}

func (f *fakeAuditRepo) ListByFingerprint(_ context.Context, fp string) ([]persistence.ChatMemoryWriteAudit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []persistence.ChatMemoryWriteAudit
	for _, r := range f.rows {
		if r.ContentFingerprint == fp {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeAuditRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// sharedCtx builds a context that carries the originating channel/session (as the receiver
// stamps it) AND the operator id (as the dispatcher stamps it from Request.OperatorID).
func sharedCtx() context.Context {
	ctx := WithCallSiteForTest(context.Background(), testMemChannel, testMemSession)
	return WithOperatorID(ctx, testMemOperator)
}

func sharedExecutor(confirms *fakeConfirmRepo, audit *fakeAuditRepo) *ToolExecutor {
	return &ToolExecutor{
		memoryWrite:    &stubMemoryWriteGate{allow: map[string]bool{"slack|sess": true}},
		memoryConfirms: confirms,
		memoryAudit:    audit,
	}
}

// PROPOSED: a shared-scope remember with no acknowledged row stores a pending confirmation and
// returns a request that lists the accepted phrases verbatim (design §5.3.3). No write, and the
// model must not be told anything was saved.
func TestRememberShared_ProposesAndListsPhrases(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	te := sharedExecutor(confirms, audit)

	res := te.remember(sharedCtx(),
		`{"content":"the roadmap slipped to Q4","scope":"shared"}`, "")

	low := strings.ToLower(res.Content)
	if !strings.Contains(low, "everyone") {
		t.Errorf("confirmation request must name who can read it; got: %s", res.Content)
	}
	// Every accepted phrase must be listed verbatim, EN and CS.
	for _, phrase := range []string{"share it", "confirm share", "potvrzuji sdílení", "sdilej to"} {
		if !strings.Contains(res.Content, phrase) {
			t.Errorf("confirmation request must list %q verbatim; got: %s", phrase, res.Content)
		}
	}
	// It must not imply the write happened: no claim of authorization, and it must say plainly
	// nothing has been saved yet.
	if strings.Contains(low, "authorized") {
		t.Errorf("a PROPOSED response must not claim authorization: %s", res.Content)
	}
	if !strings.Contains(low, "nothing has been saved") {
		t.Errorf("a PROPOSED response must state nothing was saved yet: %s", res.Content)
	}
	// A pending, unacknowledged row now exists.
	row, ok := confirms.get("slack")
	if !ok || row.Acknowledged() {
		t.Fatalf("expected a pending unacknowledged row, got ok=%v row=%+v", ok, row)
	}
	if row.OperatorID != "slack:UALICE" || row.Scope != string(memoryScopeShared) {
		t.Errorf("pending row has wrong operator/scope: %+v", row)
	}
	if audit.count() != 0 {
		t.Errorf("PROPOSED must write no audit rows; got %d", audit.count())
	}
}

// THE parser.go:186 REGRESSION TEST (design §9). The acknowledgement cannot originate in a tool
// argument, so no number of remember() calls within one turn can advance PROPOSED → AUTHORIZED:
// advancing requires an inbound human turn the model cannot author. Calling remember twice in a
// row must re-list the phrases and never authorize a write.
func TestRememberShared_RepeatedCallsNeverReachAuthorized(t *testing.T) {
	ops := []string{}
	confirms := newFakeConfirmRepo(&ops)
	audit := newFakeAuditRepo(&ops)
	te := sharedExecutor(confirms, audit)
	ctx := sharedCtx()
	args := `{"content":"the api key rotates monthly","scope":"shared"}`

	first := te.remember(ctx, args, "")
	if strings.Contains(strings.ToLower(first.Content), "authorized") {
		t.Fatalf("first call must PROPOSE, not authorize: %s", first.Content)
	}

	// The model tries again in the same turn — no acknowledgement has arrived because only the
	// receiver, from a human inbound turn, can produce one.
	second := te.remember(ctx, args, "")
	low := strings.ToLower(second.Content)
	if strings.Contains(low, "authorized") {
		t.Fatalf("a second remember() in the same turn must NOT authorize a write: %s", second.Content)
	}
	if !strings.Contains(second.Content, "share it") {
		t.Errorf("the 'already proposed' response must re-list the phrases: %s", second.Content)
	}
	if audit.count() != 0 {
		t.Errorf("no audit row may be written without a human acknowledgement; got %d", audit.count())
	}
	// The row is still pending and was never deleted (never authorized).
	if row, ok := confirms.get("slack"); !ok || row.Acknowledged() {
		t.Errorf("the pending row must remain unacknowledged after repeated tool calls: ok=%v row=%+v", ok, row)
	}
	for _, op := range ops {
		if op == "delete" || op == "record" || op == "acknowledge" {
			t.Errorf("no acknowledge/record/delete may happen from tool calls alone; ops=%v", ops)
		}
	}
}

// AUTHORIZED: once the receiver has stamped an acknowledgement, the next remember() for the same
// fingerprint grants. The append-only audit row is written BEFORE the pending row is deleted
// (§5.3.3), and the response reports the write authorized-but-not-persisted (slices 4-5).
func TestRememberShared_AuthorizedWritesAuditBeforeDeleteAndReportsNotBuilt(t *testing.T) {
	ops := []string{}
	confirms := newFakeConfirmRepo(&ops)
	audit := newFakeAuditRepo(&ops)
	te := sharedExecutor(confirms, audit)

	content := "the incident postmortem is due Friday"
	fp := sharedWriteFingerprint(content)
	now := time.Now()
	confirms.seedAcknowledged(persistence.ChatMemoryWriteConfirmation{
		Channel: "slack", SessionID: "sess", ContentFingerprint: fp,
		Scope: string(memoryScopeShared), OperatorID: "slack:UALICE",
		ProposedAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(12 * time.Minute),
	}, now.Add(-2*time.Minute))

	res := te.remember(sharedCtx(),
		`{"content":"`+content+`","scope":"shared"}`, "")

	low := strings.ToLower(res.Content)
	if !strings.Contains(low, "authorized") {
		t.Fatalf("an acknowledged, matching, unexpired, same-operator write must authorize: %s", res.Content)
	}
	if !strings.Contains(low, "not built") && !strings.Contains(low, "not yet") {
		t.Errorf("the response must say the persist path is not built (slices 4-5): %s", res.Content)
	}
	// Audit written, then pending row deleted — in that order (§5.3.3).
	if audit.count() != 1 {
		t.Fatalf("exactly one audit row must be written on grant; got %d", audit.count())
	}
	recIdx, delIdx := -1, -1
	for i, op := range ops {
		switch op {
		case "record":
			recIdx = i
		case "delete":
			delIdx = i
		}
	}
	if recIdx < 0 || delIdx < 0 || recIdx > delIdx {
		t.Errorf("audit row must be written BEFORE the pending delete; ops=%v", ops)
	}
	// One-shot: the pending row is gone.
	if _, ok := confirms.get("slack"); ok {
		t.Error("the pending row must be deleted after a granted write (one-shot)")
	}
	// The audit row stores the fingerprint, not the content.
	rows, _ := audit.ListByFingerprint(context.Background(), fp)
	if len(rows) != 1 || rows[0].OperatorID != "slack:UALICE" || rows[0].ContentFingerprint != fp {
		t.Errorf("audit row wrong: %+v", rows)
	}
	if rows[0].GrantedAt.IsZero() || rows[0].AcknowledgedAt.IsZero() {
		t.Error("audit row must carry granted_at and acknowledged_at")
	}
}

// A grant with no audit sink must NOT proceed: an authorized shared write without its Art 5(2)
// accountability record is exactly what review round 4 refused to ship. The pending row is left
// in place rather than one-shot-deleted.
func TestRememberShared_AuthorizedRefusesWithoutAuditSink(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	te := &ToolExecutor{
		memoryWrite:    &stubMemoryWriteGate{allow: map[string]bool{"slack|sess": true}},
		memoryConfirms: confirms,
		// memoryAudit intentionally nil
	}
	content := "the vendor contract renews in March"
	now := time.Now()
	confirms.seedAcknowledged(persistence.ChatMemoryWriteConfirmation{
		Channel: "slack", SessionID: "sess", ContentFingerprint: sharedWriteFingerprint(content),
		Scope: string(memoryScopeShared), OperatorID: "slack:UALICE",
		ProposedAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(12 * time.Minute),
	}, now.Add(-2*time.Minute))

	res := te.remember(sharedCtx(),
		`{"content":"`+content+`","scope":"shared"}`, "")

	if !strings.Contains(strings.ToLower(res.Content), "not authorized") {
		t.Errorf("must refuse (not authorize) without an audit sink: %s", res.Content)
	}
	if _, ok := confirms.get("slack"); !ok {
		t.Error("the pending row must be preserved when the write cannot be audited")
	}
}

// A shared proposal binds to the speaker who made it (only they may acknowledge). With no
// resolvable operator identity there is nobody who could ever discharge it, so the tool refuses
// rather than storing an undischargeable row (§5.6.5).
func TestRememberShared_RefusesWithoutOperatorIdentity(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	te := sharedExecutor(confirms, newFakeAuditRepo(nil))

	// Call site is set, but NO operator id is stamped on the context.
	res := te.remember(WithCallSiteForTest(context.Background(), "slack", "sess"),
		`{"content":"something sensitive","scope":"shared"}`, "")

	if !strings.Contains(strings.ToLower(res.Content), "who is speaking") {
		t.Errorf("must refuse a shared write with no speaker identity: %s", res.Content)
	}
	if _, ok := confirms.get("slack"); ok {
		t.Error("no pending row may be stored when the speaker is unknown")
	}
}

// When the confirmation store is not wired at all, a shared write reports the save path as
// not-built (the slice-2 shape) rather than erroring — the feature degrades cleanly.
func TestRememberShared_UnwiredStoreReportsNotBuilt(t *testing.T) {
	te := &ToolExecutor{memoryWrite: &stubMemoryWriteGate{allow: map[string]bool{"slack|sess": true}}}
	res := te.remember(sharedCtx(),
		`{"content":"x","scope":"shared"}`, "")
	if !strings.Contains(res.Content, "NOT implemented yet") {
		t.Errorf("an unwired confirmation store must report the save path not built: %s", res.Content)
	}
}
