package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// fixedScorer is a trivial persistence.InstinctScorer for the repo
// contract tests. It avoids importing internal/instinct so the suite
// stays dependency-light: it asserts the repository correctly derives
// support/contradict counts from evidence and persists the scorer's
// output, NOT the Wilson math (which is unit-tested in internal/instinct).
type fixedScorer struct{}

func (fixedScorer) Score(in persistence.InstinctScoreInput) (float64, string) {
	total := in.SupportCount + in.ContradictCount
	if total == 0 {
		return 0, persistence.InstinctStatusCandidate
	}
	conf := float64(in.SupportCount) / float64(total)
	status := persistence.InstinctStatusCandidate
	if conf >= 0.6 && in.SupportCount >= 3 {
		status = persistence.InstinctStatusActive
	}
	return conf, status
}

// capturingScorer records the last InstinctScoreInput it was handed, so a
// test can assert what RecomputeConfidence derived and passed to the scorer
// (counts, application tallies) without coupling to the Wilson math.
type capturingScorer struct {
	last persistence.InstinctScoreInput
}

func (c *capturingScorer) Score(in persistence.InstinctScoreInput) (float64, string) {
	c.last = in
	return fixedScorer{}.Score(in)
}

// RunInstinctSuite exercises persistence.InstinctRepository end to end.
// The repo passed in must connect to empty instinct* tables.
func RunInstinctSuite(t *testing.T, repo persistence.InstinctRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	tkey := uniqueID("tk")

	t.Run("Upsert_inserts_then_dedups", func(t *testing.T) {
		in := &persistence.Instinct{
			ProjectID: project, Domain: persistence.InstinctDomainRecovery,
			TriggerKey: tkey, Action: "retry resolved it",
			Trigger: []byte(`{"role":"lead"}`),
		}
		id1, err := repo.Upsert(ctx, in)
		if err != nil {
			t.Fatalf("Upsert insert: %v", err)
		}
		if id1 == "" {
			t.Fatal("Upsert returned empty id")
		}
		// Second upsert of the same (scope, project, trigger_key)
		// returns the SAME id and updates the action — never a dup.
		id2, err := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: project, Domain: persistence.InstinctDomainRecovery,
			TriggerKey: tkey, Action: "retry resolved it (v2)",
		})
		if err != nil {
			t.Fatalf("Upsert update: %v", err)
		}
		if id2 != id1 {
			t.Fatalf("dedup failed: id1=%s id2=%s", id1, id2)
		}
		got, err := repo.Get(ctx, id1)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Action != "retry resolved it (v2)" {
			t.Errorf("action not updated on upsert: %q", got.Action)
		}
		if got.Status != persistence.InstinctStatusCandidate {
			t.Errorf("fresh instinct status = %q, want candidate", got.Status)
		}
	})

	t.Run("AddEvidence_idempotent_and_recompute", func(t *testing.T) {
		p := uniqueID("proj")
		id, err := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: p, Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "x", LastSeenAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// Add 3 support + 1 contradict.
		for i, oid := range []string{"o1", "o2", "o3"} {
			ins, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{
				InstinctID: id, OutcomeID: oid, Polarity: persistence.InstinctPolaritySupport,
			})
			if err != nil {
				t.Fatalf("AddEvidence %d: %v", i, err)
			}
			if !ins {
				t.Errorf("AddEvidence %s should be a new row", oid)
			}
		}
		if _, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{
			InstinctID: id, OutcomeID: "oc", Polarity: persistence.InstinctPolarityContradict,
		}); err != nil {
			t.Fatalf("AddEvidence contradict: %v", err)
		}
		// Re-seeing o1 is a no-op (idempotency contract).
		ins, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{
			InstinctID: id, OutcomeID: "o1", Polarity: persistence.InstinctPolaritySupport,
		})
		if err != nil {
			t.Fatalf("AddEvidence re-see: %v", err)
		}
		if ins {
			t.Error("re-seen outcome should NOT insert a new evidence row")
		}

		if err := repo.RecomputeConfidence(ctx, id, fixedScorer{}); err != nil {
			t.Fatalf("RecomputeConfidence: %v", err)
		}
		got, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SupportCount != 3 || got.ContradictCount != 1 {
			t.Errorf("counts = (%d,%d), want (3,1)", got.SupportCount, got.ContradictCount)
		}
		if got.Confidence <= 0.7 || got.Confidence >= 0.8 {
			t.Errorf("confidence = %f, want 0.75", got.Confidence)
		}
		if got.Status != persistence.InstinctStatusActive {
			t.Errorf("status = %q, want active", got.Status)
		}
	})

	t.Run("List_filters", func(t *testing.T) {
		p := uniqueID("proj")
		_, _ = repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: p, Domain: persistence.InstinctDomainRecovery, TriggerKey: uniqueID("tk"), Action: "a",
		})
		_, _ = repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: p, Domain: persistence.InstinctDomainQuality, TriggerKey: uniqueID("tk"), Action: "b",
		})
		dom := persistence.InstinctDomainRecovery
		got, err := repo.List(ctx, persistence.InstinctFilter{ProjectID: &p, Domain: &dom, PageSize: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("domain filter returned %d, want 1", len(got))
		}
	})

	t.Run("CountByDomainStatus_buckets_by_delta", func(t *testing.T) {
		// The suite shares one DB across subtests, so assert on the
		// delta for a specific (domain, status) bucket rather than an
		// absolute total. New instincts default to status=candidate.
		bucket := func() int {
			counts, err := repo.CountByDomainStatus(ctx)
			if err != nil {
				t.Fatalf("CountByDomainStatus: %v", err)
			}
			for _, c := range counts {
				if c.Domain == persistence.InstinctDomainQuality && c.Status == persistence.InstinctStatusCandidate {
					return c.Count
				}
			}
			return 0
		}
		before := bucket()
		for i := 0; i < 3; i++ {
			if _, err := repo.Upsert(ctx, &persistence.Instinct{
				ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainQuality,
				TriggerKey: uniqueID("tk"), Action: "a",
			}); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
		}
		if got := bucket() - before; got != 3 {
			t.Errorf("(quality,candidate) delta = %d, want 3", got)
		}
	})

	t.Run("Retire", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		if err := repo.Retire(ctx, id); err != nil {
			t.Fatalf("Retire: %v", err)
		}
		got, _ := repo.Get(ctx, id)
		if got.Status != persistence.InstinctStatusRetired {
			t.Errorf("status = %q, want retired", got.Status)
		}
		if err := repo.Retire(ctx, "does-not-exist"); err != persistence.ErrNotFound {
			t.Errorf("Retire(missing) = %v, want ErrNotFound", err)
		}
	})

	t.Run("Applications_round_trip", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id, TaskID: "t1",
			Surface: persistence.InstinctSurfaceFailedTaskUI, Result: persistence.InstinctResultSucceeded,
			ExecutionID: "exec1", StepID: "plan",
		}); err != nil {
			t.Fatalf("RecordApplication: %v", err)
		}
		apps, err := repo.ListApplications(ctx, id, 10)
		if err != nil {
			t.Fatalf("ListApplications: %v", err)
		}
		if len(apps) != 1 || apps[0].Surface != persistence.InstinctSurfaceFailedTaskUI {
			t.Errorf("apps = %+v", apps)
		}
		if apps[0].ExecutionID != "exec1" || apps[0].StepID != "plan" {
			t.Errorf("execution/step ids did not round-trip: %+v", apps[0])
		}
	})

	t.Run("RecordApplication_persists_execution_step_ids", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id,
			Surface:    persistence.InstinctSurfaceLeadRecovery, Result: persistence.InstinctResultIgnored,
			ExecutionID: "exec-xyz", StepID: "implement",
		}); err != nil {
			t.Fatalf("RecordApplication: %v", err)
		}
		apps, err := repo.ListApplications(ctx, id, 10)
		if err != nil {
			t.Fatalf("ListApplications: %v", err)
		}
		if len(apps) != 1 {
			t.Fatalf("want 1 app, got %d", len(apps))
		}
		if apps[0].ExecutionID != "exec-xyz" || apps[0].StepID != "implement" {
			t.Errorf("execution/step ids not persisted: %+v", apps[0])
		}
	})

	t.Run("ListPendingRecoveryApplications_returns_only_pending", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		// Pending: lead_recovery + ignored + execution_id set.
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery,
			Result: persistence.InstinctResultIgnored, ExecutionID: "exec-pending", StepID: "s1",
		}); err != nil {
			t.Fatalf("RecordApplication pending: %v", err)
		}
		// Not pending: already resolved (succeeded).
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery,
			Result: persistence.InstinctResultSucceeded, ExecutionID: "exec-done", StepID: "s2",
		}); err != nil {
			t.Fatalf("RecordApplication resolved: %v", err)
		}
		// Not pending: wrong surface.
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceFailedTaskUI,
			Result: persistence.InstinctResultIgnored, ExecutionID: "exec-ui", StepID: "s3",
		}); err != nil {
			t.Fatalf("RecordApplication wrong surface: %v", err)
		}
		// Not pending: ignored lead_recovery but no execution_id.
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery,
			Result: persistence.InstinctResultIgnored,
		}); err != nil {
			t.Fatalf("RecordApplication no exec: %v", err)
		}
		// Pending (v2): an auto_applied lead_recovery directive is also awaiting
		// resolution, so it must surface alongside the ignored advisory row.
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery,
			Result: persistence.InstinctResultAutoApplied, ExecutionID: "exec-auto", StepID: "s4",
		}); err != nil {
			t.Fatalf("RecordApplication auto_applied: %v", err)
		}
		pending, err := repo.ListPendingRecoveryApplications(ctx, 100)
		if err != nil {
			t.Fatalf("ListPendingRecoveryApplications: %v", err)
		}
		got := map[string]bool{}
		for _, p := range pending {
			if p.InstinctID == id {
				got[p.ExecutionID] = true
			}
		}
		if len(got) != 2 || !got["exec-pending"] || !got["exec-auto"] {
			t.Errorf("pending rows for instinct = %v, want {exec-pending, exec-auto}", got)
		}
	})

	t.Run("ResolveApplication_flips_in_place", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		app := &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery,
			Result: persistence.InstinctResultIgnored, ExecutionID: "exec-flip", StepID: "s1",
		}
		if err := repo.RecordApplication(ctx, app); err != nil {
			t.Fatalf("RecordApplication: %v", err)
		}
		if err := repo.ResolveApplication(ctx, app.ID, persistence.InstinctResultSucceeded); err != nil {
			t.Fatalf("ResolveApplication: %v", err)
		}
		apps, _ := repo.ListApplications(ctx, id, 10)
		if len(apps) != 1 || apps[0].Result != persistence.InstinctResultSucceeded {
			t.Errorf("after resolve apps = %+v, want one succeeded", apps)
		}
		// Second resolve is a no-op: row is no longer 'ignored'.
		if err := repo.ResolveApplication(ctx, app.ID, persistence.InstinctResultFailed); err != persistence.ErrNotFound {
			t.Errorf("second ResolveApplication = %v, want ErrNotFound", err)
		}

		// v2: an auto_applied row is also resolvable (flips to failed here).
		auto := &persistence.InstinctApplication{
			InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery,
			Result: persistence.InstinctResultAutoApplied, ExecutionID: "exec-auto-flip", StepID: "s2",
		}
		if err := repo.RecordApplication(ctx, auto); err != nil {
			t.Fatalf("RecordApplication auto: %v", err)
		}
		if err := repo.ResolveApplication(ctx, auto.ID, persistence.InstinctResultFailed); err != nil {
			t.Errorf("ResolveApplication(auto_applied) = %v, want nil", err)
		}
	})

	t.Run("ResolveApplication_missing_returns_ErrNotFound", func(t *testing.T) {
		if err := repo.ResolveApplication(ctx, "does-not-exist", persistence.InstinctResultSucceeded); err != persistence.ErrNotFound {
			t.Errorf("ResolveApplication(missing) = %v, want ErrNotFound", err)
		}
	})

	t.Run("ListApplicationCounts_aggregation", func(t *testing.T) {
		idA, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		idB, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "b",
		})
		rec := func(instinctID, result string) {
			if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
				InstinctID: instinctID, Surface: persistence.InstinctSurfaceLeadRecovery, Result: result,
			}); err != nil {
				t.Fatalf("RecordApplication(%s,%s): %v", instinctID, result, err)
			}
		}
		// A: 3 succeeded, 1 failed, 1 rejected, 2 ignored, 1 accepted.
		for range []int{0, 1, 2} {
			rec(idA, persistence.InstinctResultSucceeded)
		}
		rec(idA, persistence.InstinctResultFailed)
		rec(idA, persistence.InstinctResultRejected)
		rec(idA, persistence.InstinctResultIgnored)
		rec(idA, persistence.InstinctResultIgnored)
		rec(idA, persistence.InstinctResultAccepted)
		// B: 1 ignored.
		rec(idB, persistence.InstinctResultIgnored)

		ghost := uniqueID("ghost")
		counts, err := repo.ListApplicationCounts(ctx, []string{idA, idB, ghost})
		if err != nil {
			t.Fatalf("ListApplicationCounts: %v", err)
		}
		a := counts[idA]
		if a == nil {
			t.Fatalf("no counts for A")
		}
		if a.Succeeded != 3 {
			t.Errorf("A.Succeeded = %d, want 3", a.Succeeded)
		}
		if a.Failed != 2 { // failed + rejected
			t.Errorf("A.Failed = %d, want 2 (failed+rejected)", a.Failed)
		}
		if a.Ignored != 2 { // accepted excluded
			t.Errorf("A.Ignored = %d, want 2", a.Ignored)
		}
		if b := counts[idB]; b == nil || b.Ignored != 1 {
			t.Errorf("B counts = %+v, want Ignored 1", b)
		}
		if _, ok := counts[ghost]; ok {
			t.Errorf("ghost id should be absent from counts map")
		}

		// Empty / nil input returns an empty map, no error.
		empty, err := repo.ListApplicationCounts(ctx, nil)
		if err != nil {
			t.Fatalf("ListApplicationCounts(nil): %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("nil ids returned %d entries, want 0", len(empty))
		}
	})

	t.Run("Get_missing_is_not_found", func(t *testing.T) {
		if _, err := repo.Get(ctx, "nope"); err != persistence.ErrNotFound {
			t.Errorf("Get(missing) = %v, want ErrNotFound", err)
		}
	})

	t.Run("Upsert_validation", func(t *testing.T) {
		if _, err := repo.Upsert(ctx, nil); err == nil {
			t.Error("Upsert(nil) should error")
		}
		// Missing required fields (domain / action / trigger_key).
		if _, err := repo.Upsert(ctx, &persistence.Instinct{ProjectID: "p"}); err == nil {
			t.Error("Upsert with missing required fields should error")
		}
	})

	t.Run("AddEvidence_validation_and_polarity_default", func(t *testing.T) {
		if _, err := repo.AddEvidence(ctx, nil); err == nil {
			t.Error("AddEvidence(nil) should error")
		}
		if _, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{OutcomeID: "o"}); err == nil {
			t.Error("AddEvidence with no instinct_id should error")
		}
		// Polarity defaults to support when omitted.
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		if _, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{InstinctID: id, OutcomeID: "od"}); err != nil {
			t.Fatalf("AddEvidence default polarity: %v", err)
		}
		if err := repo.RecomputeConfidence(ctx, id, fixedScorer{}); err != nil {
			t.Fatalf("RecomputeConfidence: %v", err)
		}
		got, _ := repo.Get(ctx, id)
		if got.SupportCount != 1 {
			t.Errorf("defaulted-polarity evidence should count as support, got %d", got.SupportCount)
		}
	})

	t.Run("RecomputeConfidence_validation", func(t *testing.T) {
		if err := repo.RecomputeConfidence(ctx, "", fixedScorer{}); err == nil {
			t.Error("RecomputeConfidence with empty id should error")
		}
		if err := repo.RecomputeConfidence(ctx, "x", nil); err == nil {
			t.Error("RecomputeConfidence with nil scorer should error")
		}
	})

	t.Run("RecordApplication_validation", func(t *testing.T) {
		if err := repo.RecordApplication(ctx, nil); err == nil {
			t.Error("RecordApplication(nil) should error")
		}
		if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{InstinctID: "i"}); err == nil {
			t.Error("RecordApplication missing surface/result should error")
		}
	})

	t.Run("List_filters_and_pagination", func(t *testing.T) {
		p := uniqueID("proj")
		// Three instincts, distinct scopes / confidences so the filters bite.
		mk := func(scope, domain string) string {
			id, _ := repo.Upsert(ctx, &persistence.Instinct{
				Scope: scope, ProjectID: p, Domain: domain, TriggerKey: uniqueID("tk"), Action: "a",
			})
			return id
		}
		idA := mk(persistence.InstinctScopeProject, persistence.InstinctDomainRecovery)
		_ = mk(persistence.InstinctScopeProject, persistence.InstinctDomainQuality)
		idC := mk(persistence.InstinctScopeGlobal, persistence.InstinctDomainRecovery)

		// Give idA strong confidence so MinConfidence isolates it.
		for _, o := range []string{"1", "2", "3", "4", "5"} {
			_, _ = repo.AddEvidence(ctx, &persistence.InstinctEvidence{InstinctID: idA, OutcomeID: o})
		}
		_ = repo.RecomputeConfidence(ctx, idA, fixedScorer{}) // conf 1.0

		// Scope filter.
		scope := persistence.InstinctScopeGlobal
		got, err := repo.List(ctx, persistence.InstinctFilter{ProjectID: &p, Scope: &scope, PageSize: 10})
		if err != nil {
			t.Fatalf("List scope: %v", err)
		}
		if len(got) != 1 || got[0].ID != idC {
			t.Errorf("scope filter = %d rows, want just idC", len(got))
		}

		// Status filter.
		active := persistence.InstinctStatusActive
		got, _ = repo.List(ctx, persistence.InstinctFilter{ProjectID: &p, Status: &active, PageSize: 10})
		if len(got) != 1 || got[0].ID != idA {
			t.Errorf("status=active filter = %d rows, want just idA", len(got))
		}

		// MinConfidence filter.
		minc := 0.9
		got, _ = repo.List(ctx, persistence.InstinctFilter{ProjectID: &p, MinConfidence: &minc, PageSize: 10})
		if len(got) != 1 || got[0].ID != idA {
			t.Errorf("min-confidence filter = %d rows, want just idA", len(got))
		}

		// Pagination: PageSize 1 + Offset walks the ordered set.
		page1, _ := repo.List(ctx, persistence.InstinctFilter{ProjectID: &p, PageSize: 1})
		page2, _ := repo.List(ctx, persistence.InstinctFilter{ProjectID: &p, PageSize: 1, Offset: 1})
		if len(page1) != 1 || len(page2) != 1 {
			t.Fatalf("pagination returned %d/%d rows, want 1/1", len(page1), len(page2))
		}
		if page1[0].ID == page2[0].ID {
			t.Error("offset pagination returned the same row twice")
		}
		// Highest confidence first → idA leads.
		if page1[0].ID != idA {
			t.Errorf("ordering: first page = %s, want highest-confidence idA", page1[0].ID)
		}
	})

	t.Run("CountActiveProjects_cross_project", func(t *testing.T) {
		// The same trigger_key under two different projects, both pushed
		// to active by enough support, should count as 2 distinct
		// projects. A third project left as a candidate must NOT count.
		shared := uniqueID("tk")
		mkActive := func(proj string) {
			id, err := repo.Upsert(ctx, &persistence.Instinct{
				ProjectID: proj, Domain: persistence.InstinctDomainRecovery,
				TriggerKey: shared, Action: "a", LastSeenAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			for _, o := range []string{"a", "b", "c"} {
				if _, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{
					InstinctID: id, OutcomeID: proj + o, Polarity: persistence.InstinctPolaritySupport,
				}); err != nil {
					t.Fatalf("AddEvidence: %v", err)
				}
			}
			if err := repo.RecomputeConfidence(ctx, id, fixedScorer{}); err != nil {
				t.Fatalf("RecomputeConfidence: %v", err)
			}
		}
		mkActive(uniqueID("pa"))
		mkActive(uniqueID("pb"))
		// Candidate-only project (1 support → fixedScorer keeps it
		// candidate since support < 3): must not be counted.
		candID, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("pc"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: shared, Action: "a",
		})
		_, _ = repo.AddEvidence(ctx, &persistence.InstinctEvidence{InstinctID: candID, OutcomeID: "c1"})
		_ = repo.RecomputeConfidence(ctx, candID, fixedScorer{})

		n, err := repo.CountActiveProjects(ctx, shared)
		if err != nil {
			t.Fatalf("CountActiveProjects: %v", err)
		}
		if n != 2 {
			t.Errorf("CountActiveProjects = %d, want 2 (candidate project excluded)", n)
		}
		// An unknown trigger_key counts zero; empty is a defined no-op.
		if got, _ := repo.CountActiveProjects(ctx, uniqueID("nope")); got != 0 {
			t.Errorf("CountActiveProjects(unknown) = %d, want 0", got)
		}
		if got, _ := repo.CountActiveProjects(ctx, ""); got != 0 {
			t.Errorf("CountActiveProjects(empty) = %d, want 0", got)
		}
	})

	t.Run("List_by_trigger_key", func(t *testing.T) {
		p := uniqueID("proj")
		tk := uniqueID("tk")
		idMatch, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: p, Domain: persistence.InstinctDomainRecovery, TriggerKey: tk, Action: "a",
		})
		_, _ = repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: p, Domain: persistence.InstinctDomainRecovery, TriggerKey: uniqueID("tk"), Action: "b",
		})
		got, err := repo.List(ctx, persistence.InstinctFilter{TriggerKey: &tk, PageSize: 10})
		if err != nil {
			t.Fatalf("List by trigger_key: %v", err)
		}
		if len(got) != 1 || got[0].ID != idMatch {
			t.Errorf("trigger_key filter = %d rows, want just idMatch", len(got))
		}
	})

	t.Run("RecomputeConfidence_aggregates_applications", func(t *testing.T) {
		// Slice 7: RecomputeConfidence must aggregate instinct_applications
		// into the score input, mapping the five result types into the
		// scorer's three buckets:
		//   succeeded            → AppSucceeded
		//   failed, rejected     → AppFailed   (surfacing didn't help)
		//   ignored              → AppIgnored
		//   accepted             → excluded    (intermediate; the eventual
		//                                        succeeded/failed row counts)
		id, err := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a", LastSeenAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		rec := func(result string, n int) {
			for i := 0; i < n; i++ {
				if err := repo.RecordApplication(ctx, &persistence.InstinctApplication{
					InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery, Result: result,
				}); err != nil {
					t.Fatalf("RecordApplication(%s): %v", result, err)
				}
			}
		}
		rec(persistence.InstinctResultSucceeded, 2)
		rec(persistence.InstinctResultFailed, 1)
		rec(persistence.InstinctResultRejected, 1)
		rec(persistence.InstinctResultIgnored, 3)
		rec(persistence.InstinctResultAccepted, 1)

		cap := &capturingScorer{}
		if err := repo.RecomputeConfidence(ctx, id, cap); err != nil {
			t.Fatalf("RecomputeConfidence: %v", err)
		}
		got := cap.last
		if got.AppSucceeded != 2 {
			t.Errorf("AppSucceeded = %d, want 2", got.AppSucceeded)
		}
		if got.AppFailed != 2 { // failed + rejected
			t.Errorf("AppFailed = %d, want 2 (failed+rejected)", got.AppFailed)
		}
		if got.AppIgnored != 3 {
			t.Errorf("AppIgnored = %d, want 3", got.AppIgnored)
		}
		// W1: RecomputeConfidence must thread the row's created_at into the
		// score input (backs the evidence-freshness creation grace). It is set
		// at Upsert time, so it must be non-zero here.
		if got.CreatedAt.IsZero() {
			t.Errorf("CreatedAt not threaded into score input (zero)")
		}
	})

	t.Run("ListApplications_limit", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "a",
		})
		for range []int{0, 1, 2} {
			_ = repo.RecordApplication(ctx, &persistence.InstinctApplication{
				InstinctID: id, Surface: persistence.InstinctSurfaceLeadRecovery, Result: persistence.InstinctResultIgnored,
			})
		}
		got, err := repo.ListApplications(ctx, id, 2)
		if err != nil {
			t.Fatalf("ListApplications: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("limit=2 returned %d rows", len(got))
		}
	})

	// W6 per-action evidence partitioning (2026-06-15): RecomputeConfidence
	// counts only evidence recorded under the instinct's CURRENT action, so
	// when the action changes the displaced action's evidence stops counting —
	// without deleting it.
	t.Run("PerActionEvidencePartitioning", func(t *testing.T) {
		tk := uniqueID("tk")
		id, err := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: tk, Action: "action-A", LastSeenAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("Upsert A: %v", err)
		}
		// 3 support outcomes corroborate action-A (Action left empty → resolved
		// from the instinct's current action).
		for _, oid := range []string{"a1", "a2", "a3"} {
			if _, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{
				InstinctID: id, OutcomeID: oid, Polarity: persistence.InstinctPolaritySupport,
			}); err != nil {
				t.Fatalf("AddEvidence %s: %v", oid, err)
			}
		}
		if err := repo.RecomputeConfidence(ctx, id, fixedScorer{}); err != nil {
			t.Fatalf("RecomputeConfidence A: %v", err)
		}
		if got, _ := repo.Get(ctx, id); got.SupportCount != 3 {
			t.Fatalf("action-A support = %d, want 3", got.SupportCount)
		}

		// The trigger now maps to a different action. Upsert overwrites action.
		if _, err := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: idProjectOf(t, repo, id), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: tk, Action: "action-B", LastSeenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Upsert B: %v", err)
		}
		// Recompute BEFORE any new evidence: action-A's 3 outcomes must no
		// longer count for action-B.
		if err := repo.RecomputeConfidence(ctx, id, fixedScorer{}); err != nil {
			t.Fatalf("RecomputeConfidence B (empty): %v", err)
		}
		if got, _ := repo.Get(ctx, id); got.SupportCount != 0 {
			t.Errorf("action-B inherited action-A evidence: support = %d, want 0", got.SupportCount)
		}

		// New evidence under action-B counts; action-A's rows still don't.
		if _, err := repo.AddEvidence(ctx, &persistence.InstinctEvidence{
			InstinctID: id, OutcomeID: "b1", Polarity: persistence.InstinctPolaritySupport,
		}); err != nil {
			t.Fatalf("AddEvidence b1: %v", err)
		}
		if err := repo.RecomputeConfidence(ctx, id, fixedScorer{}); err != nil {
			t.Fatalf("RecomputeConfidence B: %v", err)
		}
		if got, _ := repo.Get(ctx, id); got.SupportCount != 1 {
			t.Errorf("action-B support = %d, want 1 (only its own evidence)", got.SupportCount)
		}
	})

	// W6 action-version history (2026-06-15): RecordActionVersion appends,
	// ListActionHistory returns newest-first, capped by limit.
	t.Run("ActionVersionHistory", func(t *testing.T) {
		id, _ := repo.Upsert(ctx, &persistence.Instinct{
			ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
			TriggerKey: uniqueID("tk"), Action: "current",
		})

		if err := repo.RecordActionVersion(ctx, &persistence.InstinctActionVersion{
			InstinctID: id, Action: "old-1", Confidence: 0.5, SupportCount: 4, ContradictCount: 1,
			Reason: "action_change", RecordedAt: time.Now().UTC().Add(-2 * time.Minute),
		}); err != nil {
			t.Fatalf("RecordActionVersion 1: %v", err)
		}
		if err := repo.RecordActionVersion(ctx, &persistence.InstinctActionVersion{
			InstinctID: id, Action: "old-2", Confidence: 0.8, SupportCount: 9, ContradictCount: 0,
			Reason: "w6_replace", RecordedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("RecordActionVersion 2: %v", err)
		}

		hist, err := repo.ListActionHistory(ctx, id, 100)
		if err != nil {
			t.Fatalf("ListActionHistory: %v", err)
		}
		if len(hist) != 2 {
			t.Fatalf("history len = %d, want 2", len(hist))
		}
		// Newest first.
		if hist[0].Action != "old-2" || hist[0].Reason != "w6_replace" {
			t.Errorf("newest = (%q,%q), want (old-2, w6_replace)", hist[0].Action, hist[0].Reason)
		}
		if hist[1].Action != "old-1" || hist[1].SupportCount != 4 {
			t.Errorf("oldest = (%q, support %d), want (old-1, 4)", hist[1].Action, hist[1].SupportCount)
		}
		// limit caps the result.
		if capped, _ := repo.ListActionHistory(ctx, id, 1); len(capped) != 1 {
			t.Errorf("limit=1 returned %d rows", len(capped))
		}
	})
}

// idProjectOf reads back the project_id for an instinct so a follow-up Upsert
// targets the same (scope, project_id, trigger_key) dedup key.
func idProjectOf(t *testing.T, repo persistence.InstinctRepository, id string) string {
	t.Helper()
	got, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get for project_id: %v", err)
	}
	return got.ProjectID
}
