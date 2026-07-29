package datasubject

import (
	"strings"
	"testing"
	"time"
)

func openRequest(kind RequestKind, opened time.Time) Request {
	return Request{ID: "req-1", SubjectID: "ds-1", Kind: kind, State: StateOpen, OpenedAt: opened}
}

// THE gate. Producing an Art 15 export for an unverified requester discloses
// the subject's data to a stranger, which makes the request mechanism itself an
// attack on the deployment. Enforced in code, not in a runbook.
func TestBuildExport_RefusesUnverifiedRequest(t *testing.T) {
	req := openRequest(RequestAccess, time.Now())
	items := []Item{{Table: TableChatAuditLog, RowID: "c1", Exclusivity: ExclusiveRow, Content: "secret"}}

	_, err := BuildExport(req, items, nil)
	if err == nil {
		t.Fatal("an unverified request must produce nothing")
	}
	if err != ErrNotVerified {
		t.Errorf("want ErrNotVerified, got %v", err)
	}
	if req.MayProduceData() {
		t.Error("an open request must not be allowed to produce data")
	}
}

// Art 15(4): a row concerning other people is listed but not disclosed.
// Emitting a chunk naming three people because one asked would trade one
// Art 15 obligation for two breaches.
func TestBuildExport_Art154WithholdsSharedContent(t *testing.T) {
	req := openRequest(RequestAccess, time.Now())
	if err := req.Verify("operator", "authenticated telegram session", time.Now()); err != nil {
		t.Fatal(err)
	}
	items := []Item{
		{Table: TableChatAuditLog, RowID: "mine", Exclusivity: ExclusiveRow,
			Content: "only about me", Origin: "telegram"},
		{Table: TableProjectMemoryChunks, RowID: "ours", Exclusivity: SharedRow,
			Content: "Alice and Bob discussed X", Context: "memory chunk, 2026-07-01", Origin: "email"},
		{Table: TableTaskMessages, RowID: "dunno", Exclusivity: UnknownExclusivity,
			Content: "possibly about others"},
	}

	exp, err := BuildExport(req, items, []string{string(SourceAuthenticated)})
	if err != nil {
		t.Fatalf("BuildExport: %v", err)
	}
	if len(exp.Items) != 3 {
		t.Fatalf("all three items must be LISTED, got %d", len(exp.Items))
	}

	byRow := map[string]ExportItem{}
	for _, ei := range exp.Items {
		byRow[ei.RowID] = ei
	}
	if byRow["mine"].Content != "only about me" {
		t.Error("an exclusive row's content must be disclosed")
	}
	if byRow["ours"].Content != "" {
		t.Error("a shared row's content must be withheld")
	}
	if !strings.Contains(byRow["ours"].Withheld, "Art 15(4)") {
		t.Errorf("the withholding must cite its ground: %q", byRow["ours"].Withheld)
	}
	// Unknown must be treated as shared — guessing the other way discloses.
	if byRow["dunno"].Content != "" {
		t.Error("unknown exclusivity must be treated as shared, not disclosed")
	}
	// An item with neither content nor context would teach the subject nothing.
	if byRow["dunno"].Context == "" {
		t.Error("a withheld item still needs context so the subject knows it exists")
	}

	// The self-check must agree with the loop.
	if exp.LeaksForeignContent(items) {
		t.Error("LeaksForeignContent reports a leak in a report that should be clean")
	}
}

// The self-check must actually detect a leak, or it is decoration. Simulates the
// regression it exists to catch: content emitted for a shared row.
func TestLeaksForeignContent_DetectsALeak(t *testing.T) {
	items := []Item{{Table: TableProjectMemoryChunks, RowID: "ours", Exclusivity: SharedRow, Content: "x"}}
	bad := &Export{Items: []ExportItem{{
		Table: string(TableProjectMemoryChunks), RowID: "ours", Content: "leaked",
	}}}
	if !bad.LeaksForeignContent(items) {
		t.Fatal("a shared row emitted with content must be detected as a leak")
	}
}

// Art 20 covers only data the subject provided, so portability is a narrower
// collection than access — "export everything" would erase a distinction the
// article makes.
func TestBuildExport_PortabilityIsNarrowerThanAccess(t *testing.T) {
	req := openRequest(RequestPortability, time.Now())
	_ = req.Verify("op", "session", time.Now())
	items := []Item{
		{Table: TableChatAuditLog, RowID: "provided", Exclusivity: ExclusiveRow,
			Content: "typed by the subject", ProvidedBySubject: true},
		{Table: TableProjectMemoryChunks, RowID: "derived", Exclusivity: ExclusiveRow,
			Content: "a summary we generated"},
	}
	exp, err := BuildExport(req, items, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Items) != 1 || exp.Items[0].RowID != "provided" {
		t.Fatalf("portability must include only subject-provided data, got %+v", exp.Items)
	}

	// The same items under an access request yield both.
	req.Kind = RequestAccess
	exp2, err := BuildExport(req, items, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp2.Items) != 2 {
		t.Errorf("access must include derived data too, got %d items", len(exp2.Items))
	}
}

// The report must state the limits of its own search. An export that reads as
// exhaustive when it is best-effort misleads the person the right protects.
func TestBuildExport_StatesItsLimitations(t *testing.T) {
	req := openRequest(RequestAccess, time.Now())
	_ = req.Verify("op", "session", time.Now())

	// Without the free-text binder, the report must say so explicitly.
	exp, _ := BuildExport(req, []Item{{Table: TableChatAuditLog, RowID: "c",
		Exclusivity: ExclusiveRow}}, []string{string(SourceAuthenticated)})
	joined := strings.Join(exp.Limitations, " | ")
	for _, want := range []string{"not a guarantee", "did NOT run"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limitations missing %q: %s", want, joined)
		}
	}
	// Retained categories must name their ground, not just the table.
	if !strings.Contains(exp.RetainedCategories["channel_disclosure_log"], "Art 50") {
		t.Errorf("retained category should cite its ground: %q", exp.RetainedCategories["channel_disclosure_log"])
	}

	// With the free-text binder, that particular caveat drops.
	exp2, _ := BuildExport(req, nil, []string{string(SourceKGExtraction)})
	if strings.Contains(strings.Join(exp2.Limitations, " "), "did NOT run") {
		t.Error("the did-not-run caveat should drop once the free-text binder contributed")
	}
}

func TestBuildExport_RejectsWrongKind(t *testing.T) {
	req := openRequest(RequestErasure, time.Now())
	_ = req.Verify("op", "session", time.Now())
	if _, err := BuildExport(req, nil, nil); err == nil {
		t.Fatal("BuildExport must refuse an erasure request")
	}
}

// --- request ledger ---

func TestRequest_VerifyRequiresAttribution(t *testing.T) {
	r := openRequest(RequestAccess, time.Now())
	if err := r.Verify("", "session", time.Now()); err == nil {
		t.Error("verification without WHO must be refused")
	}
	if err := r.Verify("op", "  ", time.Now()); err == nil {
		t.Error("verification without HOW must be refused")
	}
	if err := r.Verify("op", "authenticated session", time.Now()); err != nil {
		t.Fatalf("valid verification: %v", err)
	}
	if !r.MayProduceData() {
		t.Error("a verified request may produce data")
	}
	// Not twice.
	if err := r.Verify("op", "again", time.Now()); err == nil {
		t.Error("re-verifying an already-verified request must be refused")
	}
}

func TestRequest_ActionRequiresVerificationAndAReport(t *testing.T) {
	r := openRequest(RequestAccess, time.Now())
	if err := r.Action("hash", time.Now()); err == nil {
		t.Error("actioning an unverified request must be refused")
	}
	_ = r.Verify("op", "session", time.Now())
	if err := r.Action("", time.Now()); err == nil {
		t.Error("actioning without a report hash must be refused — what was sent must stay answerable")
	}
	if err := r.Action("sha256:abc", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State != StateActioned || r.ReportHash != "sha256:abc" {
		t.Errorf("unexpected state: %+v", r)
	}
}

// The commonest lawful refusal — "we cannot identify you" under Art 12(6) —
// happens before verification, so Refuse must be reachable from Open.
func TestRequest_RefuseFromOpenWithAGround(t *testing.T) {
	r := openRequest(RequestAccess, time.Now())
	if err := r.Refuse(""); err == nil {
		t.Error("a refusal with no ground must be refused — Art 12(4) requires telling the subject why")
	}
	if err := r.Refuse("cannot identify you from a name alone (Art 12(6)); send X"); err != nil {
		t.Fatalf("refusing from open: %v", err)
	}
	if r.State != StateRefused {
		t.Errorf("state = %q", r.State)
	}
	if r.MayProduceData() {
		t.Error("a refused request must not produce data")
	}
}

// The Art 12(3) extension must be claimed within the first month; applying it
// to an already-missed deadline would be backdating.
func TestRequest_ExtensionCannotBeRetroactive(t *testing.T) {
	opened := time.Now().Add(-40 * 24 * time.Hour)
	r := openRequest(RequestAccess, opened)

	err := r.Extend("complex", time.Now())
	if err == nil {
		t.Fatal("an extension after the first month must be refused")
	}
	if !strings.Contains(err.Error(), "retroactively") {
		t.Errorf("error should explain why: %v", err)
	}

	// In time, with a reason, it works and moves the deadline.
	fresh := openRequest(RequestAccess, time.Now().Add(-5*24*time.Hour))
	if err := fresh.Extend("", time.Now()); err == nil {
		t.Error("an unreasoned extension must be refused")
	}
	if err := fresh.Extend("numerous requests from this subject", time.Now()); err != nil {
		t.Fatal(err)
	}
	if fresh.Deadline().Sub(fresh.OpenedAt) != ExtendedDeadline {
		t.Error("extending must move the deadline to three months")
	}
}

// The warning has to arrive with time left to act, not on the day the legal
// deadline expires.
func TestRequest_DeadlineWarningArrivesEarly(t *testing.T) {
	opened := time.Now().Add(-25 * 24 * time.Hour)
	r := openRequest(RequestAccess, opened)

	if r.Overdue(time.Now()) {
		t.Error("25 days in is not yet overdue")
	}
	if !r.NeedsAttention(time.Now()) {
		t.Error("25 days into a 30-day clock must already be flagged")
	}

	old := openRequest(RequestAccess, time.Now().Add(-31*24*time.Hour))
	if !old.Overdue(time.Now()) {
		t.Error("31 days in is overdue")
	}

	// A finished request is neither overdue nor in need of attention.
	done := openRequest(RequestAccess, time.Now().Add(-60*24*time.Hour))
	_ = done.Verify("op", "session", time.Now())
	_ = done.Action("h", time.Now())
	if done.Overdue(time.Now()) || done.NeedsAttention(time.Now()) {
		t.Error("an actioned request must not keep alarming")
	}
	refused := openRequest(RequestAccess, time.Now().Add(-60*24*time.Hour))
	_ = refused.Refuse("no such subject")
	if refused.Overdue(time.Now()) {
		t.Error("a refused request is answered, not overdue")
	}
}
