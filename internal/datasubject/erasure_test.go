package datasubject

import (
	"strings"
	"testing"
	"time"
)

// Increment 5 — Art 17 erasure policy (design §4.6, §5.3).
//
// This file tests the DECISION layer only: given a verified request, its erasure
// ground, and the linked items, what should happen to each row. Execution
// (cascade, redact-and-re-embed, row deletes) is a separate slice — the decision
// is worth isolating because it is the part that is irreversible if wrong, and it
// is pure.

func verifiedErasure(ground ErasureGround) Request {
	now := time.Now()
	return Request{
		ID: "req-1", SubjectID: "subj-1", Kind: RequestErasure,
		State: StateVerified, OpenedAt: now,
		VerifiedBy: "operator", VerifiedHow: "channel session", VerifiedAt: now,
		ErasureGround: ground,
	}
}

// --- the ground and what it does to discretion ---

// The whole point of capturing the ground at intake: two of the six Art 17(1)
// grounds leave the controller no discretion to retain any part of the data, so
// the shared-row default of redaction is unavailable under them.
func TestErasureGround_RemovesRetentionDiscretion(t *testing.T) {
	for _, tc := range []struct {
		ground ErasureGround
		want   bool
	}{
		{GroundNoLongerNecessary, false}, // 17(1)(a)
		{GroundConsentWithdrawn, false},  // 17(1)(b)
		{GroundObjection, false},         // 17(1)(c)
		{GroundUnlawfulProcessing, true}, // 17(1)(d) — cannot lawfully retain what was unlawfully processed
		{GroundLegalObligation, true},    // 17(1)(e) — the obligation is to erase; redaction is not compliance
		{GroundChildServices, false},     // 17(1)(f)
	} {
		t.Run(string(tc.ground), func(t *testing.T) {
			if got := tc.ground.RemovesRetentionDiscretion(); got != tc.want {
				t.Errorf("%s.RemovesRetentionDiscretion() = %v, want %v", tc.ground, got, tc.want)
			}
		})
	}
}

// The article letters end up in a subject-facing report, so they are pinned.
// The design doc had (a) for consent withdrawal and (c) for legal obligation;
// both are off by one against the Regulation, and a wrong citation in a
// compliance artefact is a defect in the artefact.
func TestErasureGround_ArticleLetteringMatchesTheRegulation(t *testing.T) {
	for ground, wantLetter := range map[ErasureGround]string{
		GroundNoLongerNecessary:  "a",
		GroundConsentWithdrawn:   "b",
		GroundObjection:          "c",
		GroundUnlawfulProcessing: "d",
		GroundLegalObligation:    "e",
		GroundChildServices:      "f",
	} {
		t.Run(string(ground), func(t *testing.T) {
			if got := ground.ArticleLetter(); got != wantLetter {
				t.Errorf("%s letter = %q, want %q", ground, got, wantLetter)
			}
			if !strings.Contains(ground.Article(), "17(1)("+wantLetter+")") {
				t.Errorf("%s Article() = %q, should cite 17(1)(%s)", ground, ground.Article(), wantLetter)
			}
		})
	}
}

func TestErasureGround_ValidateRejectsUnknown(t *testing.T) {
	if err := ErasureGround("art17_1_z_invented").Validate(); err == nil {
		t.Error("an invented ground must not validate")
	}
	if err := ErasureGround("").Validate(); err == nil {
		t.Error("an empty ground must not validate")
	}
	if err := GroundUnlawfulProcessing.Validate(); err != nil {
		t.Errorf("a real ground must validate, got %v", err)
	}
}

// --- the gates ---

// Same identity gate as the export path, and for a stronger reason: erasure is
// irreversible, so acting for an unverified requester destroys someone's data on
// a stranger's word.
func TestPlanErasure_RefusesUnverifiedRequest(t *testing.T) {
	req := verifiedErasure(GroundConsentWithdrawn)
	req.State = StateOpen
	if _, err := PlanErasure(req, nil, nil); err == nil {
		t.Fatal("an unverified erasure request must produce no plan")
	}
}

func TestPlanErasure_RefusesWrongKind(t *testing.T) {
	req := verifiedErasure(GroundConsentWithdrawn)
	req.Kind = RequestAccess
	if _, err := PlanErasure(req, nil, nil); err == nil {
		t.Fatal("PlanErasure must refuse a non-erasure request")
	}
}

// Fail closed on a missing ground. Without it the shared-row rule cannot be
// evaluated, and defaulting to redaction would quietly pick the MORE retentive
// option — substituting a policy preference for a legal requirement, which is
// exactly what design §5.3 refuses.
func TestPlanErasure_RefusesMissingGround(t *testing.T) {
	req := verifiedErasure("")
	_, err := PlanErasure(req, []Item{{Table: TableChatAuditLog, RowID: "r1"}}, nil)
	if err == nil {
		t.Fatal("an erasure request with no recorded ground must not be planned")
	}
	if !strings.Contains(err.Error(), "ground") {
		t.Errorf("the error should name the missing ground, got %v", err)
	}
}

// --- the dispositions ---

func TestPlanErasure_ExclusiveRowsAreDeleted(t *testing.T) {
	req := verifiedErasure(GroundConsentWithdrawn)
	items := []Item{
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
		{Table: TableOperatorProfile, RowID: "r2", Exclusivity: ExclusiveRow},
	}
	plan, err := PlanErasure(req, items, nil)
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	for _, a := range plan.Actions {
		if a.Disposition != DispositionDelete {
			t.Errorf("%s/%s = %s, want delete", a.Table, a.RowID, a.Disposition)
		}
	}
}

// The ground-dependent default, and the reason increment 5 needed a decision
// layer at all: a chunk naming two people cannot be half-deleted.
func TestPlanErasure_SharedRowsRedactUnlessTheGroundRemovesDiscretion(t *testing.T) {
	shared := []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Exclusivity: SharedRow},
		{Table: TableProjectMemoryChunks, RowID: "c2", Exclusivity: UnknownExclusivity},
	}
	for _, tc := range []struct {
		ground ErasureGround
		want   Disposition
	}{
		{GroundConsentWithdrawn, DispositionRedact}, // discretion retained → preserve the other subject
		{GroundNoLongerNecessary, DispositionRedact},
		{GroundObjection, DispositionRedact},
		{GroundUnlawfulProcessing, DispositionDelete}, // no discretion → full deletion
		{GroundLegalObligation, DispositionDelete},
	} {
		t.Run(string(tc.ground), func(t *testing.T) {
			plan, err := PlanErasure(verifiedErasure(tc.ground), shared, nil)
			if err != nil {
				t.Fatalf("PlanErasure: %v", err)
			}
			for _, a := range plan.Actions {
				if a.Disposition != tc.want {
					t.Errorf("%s under %s = %s, want %s", a.RowID, tc.ground, a.Disposition, tc.want)
				}
				if a.Reason == "" {
					t.Errorf("%s carries no recorded reason — the choice must be auditable per row", a.RowID)
				}
			}
		})
	}
}

// `unknown` must behave as shared, never as exclusive. Guessing exclusive
// authorises deleting another person's data on no evidence.
func TestPlanErasure_UnknownExclusivityIsNeverTreatedAsExclusive(t *testing.T) {
	plan, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn),
		[]Item{{Table: TableProjectMemoryChunks, RowID: "c1", Exclusivity: UnknownExclusivity}}, nil)
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	if got := plan.Actions[0].Disposition; got != DispositionRedact {
		t.Errorf("unknown exclusivity = %s, want redact (shared treatment)", got)
	}
}

// --- the report ---

// A subject told "erased" while records remain has been misled (design §4.6).
// The retained categories and their grounds come from the same closed map the
// coverage test polices, so the report cannot drift from the code.
func TestPlanErasure_ReportsRetainedCategoriesWithGrounds(t *testing.T) {
	plan, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn),
		[]Item{{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow}}, nil)
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	if len(plan.RetainedCategories) == 0 {
		t.Fatal("the plan must name what is retained, not just what is erased")
	}
	for _, mustName := range []string{"channel_disclosure_log", "data_subject_requests", "security_incidents"} {
		ground, ok := plan.RetainedCategories[mustName]
		if !ok {
			t.Errorf("retained categories omit %q", mustName)
			continue
		}
		if strings.TrimSpace(ground) == "" {
			t.Errorf("%q is retained with no stated ground", mustName)
		}
	}
}

// The honest-limits statement. Erasure over free text is best-effort, and the
// report has to say so rather than implying certainty (design §5.1).
func TestPlanErasure_CarriesTheBestEffortLimitation(t *testing.T) {
	plan, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn),
		[]Item{{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow}},
		[]string{"authenticated_identity"})
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	if len(plan.Limitations) == 0 {
		t.Fatal("the plan must state its limits — 'erased' must not read as 'we are certain nothing remains'")
	}
	joined := strings.ToLower(strings.Join(plan.Limitations, " "))
	if !strings.Contains(joined, "best-effort") && !strings.Contains(joined, "best effort") {
		t.Errorf("limitations should say the free-text search is best-effort, got %v", plan.Limitations)
	}
	if len(plan.MethodsRun) != 1 || plan.MethodsRun[0] != "authenticated_identity" {
		t.Errorf("the plan must record which identification methods ran, got %v", plan.MethodsRun)
	}
}

// A plan over no items is a legitimate outcome, not an error: the subject is
// entitled to be told nothing identifiable was found, with the methods that ran.
func TestPlanErasure_EmptyLinkSetStillProducesAReport(t *testing.T) {
	plan, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn), nil, []string{"email_envelope"})
	if err != nil {
		t.Fatalf("an empty link set is a valid outcome: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("no items should mean no actions, got %d", len(plan.Actions))
	}
	if len(plan.RetainedCategories) == 0 || len(plan.Limitations) == 0 {
		t.Error("even an empty erasure must report retained categories and limitations")
	}
}

// A link may never name a table no executor can act on — otherwise the erasure
// silently skips it while the report claims success.
func TestPlanErasure_RefusesAnUnactionableTable(t *testing.T) {
	_, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn),
		[]Item{{Table: LinkableTable("tool_audit_log"), RowID: "r1", Exclusivity: ExclusiveRow}}, nil)
	if err == nil {
		t.Fatal("planning against a deliberately-uncovered table must fail rather than silently skip it")
	}
}

// Counts are what the operator and the subject actually read; derive them from
// the actions so the summary cannot disagree with the detail.
func TestPlanErasure_SummaryCountsMatchTheActions(t *testing.T) {
	items := []Item{
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
		{Table: TableChatAuditLog, RowID: "r2", Exclusivity: ExclusiveRow},
		{Table: TableProjectMemoryChunks, RowID: "c1", Exclusivity: SharedRow},
	}
	plan, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn), items, nil)
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	if plan.DeleteCount() != 2 {
		t.Errorf("DeleteCount = %d, want 2", plan.DeleteCount())
	}
	if plan.RedactCount() != 1 {
		t.Errorf("RedactCount = %d, want 1", plan.RedactCount())
	}
}

// Backups and replicas are a real limit on any erasure and the report must not
// imply otherwise. Raised by the 5b code review: the limitations named
// sub-processors but not the operator's own backups, which is the more likely
// place a copy survives.
func TestPlanErasure_LimitationsNameBackups(t *testing.T) {
	plan, err := PlanErasure(verifiedErasure(GroundConsentWithdrawn),
		[]Item{{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow}}, nil)
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	joined := strings.ToLower(strings.Join(plan.Limitations, " "))
	if !strings.Contains(joined, "backup") {
		t.Errorf("limitations must acknowledge backups — a restored backup reintroduces erased data: %v",
			plan.Limitations)
	}
}
