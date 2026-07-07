package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunSkillSuite is the backend-agnostic contract for
// persistence.SkillRepository. Both the Postgres and SQLite impls run
// it; a failure that reproduces on only one backend is a protocol
// divergence — fix the diverging backend, not the suite.
//
// Fixtures use the "p1"/"p2" literal project IDs the repotest cleanup
// sweep already purges. Each behavior lives in its own helper so the
// dispatcher stays flat (gocognit) and failures name the behavior.
func RunSkillSuite(t *testing.T, repo persistence.SkillRepository) {
	t.Helper()
	t.Run("Create_then_GetByID_round_trips_all_fields", func(t *testing.T) { skillRoundTrip(t, repo) })
	t.Run("Create_duplicate_natural_key_conflicts", func(t *testing.T) { skillDuplicateConflicts(t, repo) })
	t.Run("same_name_distinct_scopes_are_separate", func(t *testing.T) { skillDistinctScopes(t, repo) })
	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) { skillGetUnknown(t, repo) })
	t.Run("List_scope_isolation", func(t *testing.T) { skillScopeIsolation(t, repo) })
	t.Run("List_maturity_and_role_filters", func(t *testing.T) { skillMaturityRoleFilters(t, repo) })
	t.Run("Upsert_bumps_version_and_resets_to_draft", func(t *testing.T) { skillUpsertVersionBump(t, repo) })
	t.Run("SetMaturity_transitions", func(t *testing.T) { skillSetMaturity(t, repo) })
	t.Run("RecordFeedback_increments_counters", func(t *testing.T) { skillRecordFeedback(t, repo) })
	t.Run("ListForMaturityScan_only_active_trusted", func(t *testing.T) { skillMaturityScan(t, repo) })
	t.Run("ListDrafts_only_drafts", func(t *testing.T) { skillListDrafts(t, repo) })
	t.Run("Create_global_round_trips", func(t *testing.T) { skillCreateGlobalRoundTrip(t, repo) })
	t.Run("SetGlobal_flips_without_touching_maturity", func(t *testing.T) { skillSetGlobal(t, repo) })
	t.Run("Upsert_preserves_is_global", func(t *testing.T) { skillUpsertPreservesGlobal(t, repo) })
	t.Run("List_IncludeGlobal_Isolation", func(t *testing.T) { skillListIncludeGlobalIsolation(t, repo) })
}

// skillListIncludeGlobalIsolation is the named cross-project contract
// test (LLD review #3/#7): with IncludeGlobal set, a project sees its own
// skills PLUS any global skill from another project, but NEVER another
// project's non-global skill; and an empty projectID never widens to all
// rows (the OR-to-true guard).
func skillListIncludeGlobalIsolation(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	// Home project p1: one local, one global.
	local := newTestSkill("ig-p1-local", "p1", "", "ig-p1-loc")
	local.Maturity = persistence.SkillMaturityActive
	mustCreateSkill(t, repo, local)
	glob := newTestSkill("ig-p1-global", "p1", "", "ig-p1-glob")
	glob.Maturity = persistence.SkillMaturityActive
	glob.IsGlobal = true
	mustCreateSkill(t, repo, glob)
	// Other project p2: a non-global skill that must stay isolated.
	other := newTestSkill("ig-p2-local", "p2", "", "ig-p2-loc")
	other.Maturity = persistence.SkillMaturityActive
	mustCreateSkill(t, repo, other)

	// From p2's perspective, with IncludeGlobal: p2's own + p1's global,
	// but NOT p1's non-global local.
	got, err := repo.List(ctx, "p2", persistence.SkillListFilter{
		Maturities:    []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		IncludeGlobal: true,
	})
	if err != nil {
		t.Fatalf("List p2 IncludeGlobal: %v", err)
	}
	ids := idset(got)
	if !ids["ig-p2-local"] {
		t.Errorf("p2 must see its own skill: %v", keys(ids))
	}
	if !ids["ig-p1-global"] {
		t.Errorf("p2 must see p1's GLOBAL skill: %v", keys(ids))
	}
	if ids["ig-p1-local"] {
		t.Errorf("isolation leak: p2 saw p1's non-global skill: %v", keys(ids))
	}

	// Without IncludeGlobal, p2 sees ONLY its own — no global bleed.
	got, _ = repo.List(ctx, "p2", persistence.SkillListFilter{
		Maturities: []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
	})
	ids = idset(got)
	if ids["ig-p1-global"] || ids["ig-p1-local"] {
		t.Errorf("IncludeGlobal off: p2 must NOT see any p1 skill: %v", keys(ids))
	}

	// OR-to-true guard: an empty projectID with IncludeGlobal must NOT
	// match all rows — it returns nothing (no project matches "").
	got, _ = repo.List(ctx, "", persistence.SkillListFilter{
		Maturities:    []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		IncludeGlobal: true,
	})
	if len(got) != 0 {
		t.Errorf("empty projectID must never widen to all rows, got %d", len(got))
	}
}

// skillCreateGlobalRoundTrip: a skill created with IsGlobal=true reads
// back global; the default (unset) reads back false.
func skillCreateGlobalRoundTrip(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	g := newTestSkill("cg-global", "p1", "github.com/x/cg", "cg-glob")
	g.IsGlobal = true
	mustCreateSkill(t, repo, g)
	mustCreateSkill(t, repo, newTestSkill("cg-local", "p1", "github.com/x/cg", "cg-loc"))

	got, err := repo.GetByID(ctx, "cg-global")
	if err != nil {
		t.Fatalf("GetByID global: %v", err)
	}
	if !got.IsGlobal {
		t.Errorf("created-global skill must read back is_global=true: %+v", got)
	}
	loc, _ := repo.GetByID(ctx, "cg-local")
	if loc.IsGlobal {
		t.Errorf("default skill must read back is_global=false: %+v", loc)
	}
}

// skillSetGlobal: SetGlobal flips is_global both directions without
// changing maturity, and returns ErrNotFound for an unknown id.
func skillSetGlobal(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	s := newTestSkill("sg-1", "p1", "github.com/x/sg", "sg-skill")
	s.Maturity = persistence.SkillMaturityTrusted
	mustCreateSkill(t, repo, s)

	if err := repo.SetGlobal(ctx, "sg-1", true); err != nil {
		t.Fatalf("SetGlobal true: %v", err)
	}
	got, _ := repo.GetByID(ctx, "sg-1")
	if !got.IsGlobal {
		t.Errorf("SetGlobal(true) did not stick: %+v", got)
	}
	if got.Maturity != persistence.SkillMaturityTrusted {
		t.Errorf("SetGlobal must NOT touch maturity: got %s", got.Maturity)
	}

	if err := repo.SetGlobal(ctx, "sg-1", false); err != nil {
		t.Fatalf("SetGlobal false: %v", err)
	}
	got, _ = repo.GetByID(ctx, "sg-1")
	if got.IsGlobal {
		t.Errorf("SetGlobal(false) did not demote: %+v", got)
	}

	if err := repo.SetGlobal(ctx, "no-such", true); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("SetGlobal unknown id: expected ErrNotFound, got %v", err)
	}
}

// skillUpsertPreservesGlobal: editing a skill in place (re-propose) must
// not clear an already-set is_global flag.
func skillUpsertPreservesGlobal(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	orig := newTestSkill("upg-1", "p1", "github.com/x/upg", "upg-skill")
	orig.IsGlobal = true
	mustCreateSkill(t, repo, orig)

	edit := newTestSkill("upg-ignored", "p1", "github.com/x/upg", "upg-skill")
	edit.Body = "# upg-skill\n\nrewritten"
	edit.IsGlobal = false // a re-propose carries the default; must NOT clear the flag
	stored, err := repo.Upsert(ctx, edit)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !stored.IsGlobal {
		t.Errorf("Upsert must preserve is_global across an edit: %+v", stored)
	}
}

func skillListDrafts(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	d := newTestSkill("draft-only", "p1", "github.com/x/drafts", "d-draft")
	mustCreateSkill(t, repo, d)
	a := newTestSkill("active-not-draft", "p1", "github.com/x/drafts", "d-active")
	a.Maturity = persistence.SkillMaturityActive
	mustCreateSkill(t, repo, a)

	got, err := repo.ListDrafts(ctx, 0)
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	ids := idset(got)
	if !ids["draft-only"] {
		t.Fatalf("draft must be listed: %v", keys(ids))
	}
	if ids["active-not-draft"] {
		t.Fatalf("active skill must not appear in drafts: %v", keys(ids))
	}
}

func skillMaturityScan(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	mkm := func(id, name, maturity string) {
		s := newTestSkill(id, "p1", "github.com/x/scan", name)
		s.Maturity = maturity
		mustCreateSkill(t, repo, s)
	}
	mkm("scan-active", "s-active", persistence.SkillMaturityActive)
	mkm("scan-trusted", "s-trusted", persistence.SkillMaturityTrusted)
	mkm("scan-draft", "s-draft", persistence.SkillMaturityDraft)
	mkm("scan-retired", "s-retired", persistence.SkillMaturityRetired)

	got, err := repo.ListForMaturityScan(ctx)
	if err != nil {
		t.Fatalf("ListForMaturityScan: %v", err)
	}
	ids := idset(got)
	if ids["scan-draft"] || ids["scan-retired"] {
		t.Fatalf("scan must exclude draft/retired: %v", keys(ids))
	}
	if !ids["scan-active"] || !ids["scan-trusted"] {
		t.Fatalf("scan must include active + trusted: %v", keys(ids))
	}
}

func newTestSkill(id, proj, scope, name string) *persistence.Skill {
	now := time.Now().UTC()
	return &persistence.Skill{
		ID: id, ProjectID: proj, RepoScope: scope, Name: name,
		Description:  "when debugging " + name,
		Body:         "# " + name + "\n\ndo the thing",
		BodySHA256:   "hash-" + id,
		Domain:       "software",
		Tags:         []string{"debug", "net"},
		Roles:        []string{"researcher"},
		Maturity:     persistence.SkillMaturityDraft,
		Version:      1,
		OriginClient: "claude-code",
		CreatedAt:    now, UpdatedAt: now,
	}
}

func mustCreateSkill(t *testing.T, repo persistence.SkillRepository, s *persistence.Skill) {
	t.Helper()
	if err := repo.Create(context.Background(), s); err != nil {
		t.Fatalf("Create %s: %v", s.ID, err)
	}
}

func skillRoundTrip(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	s := newTestSkill("sk-1", "p1", "github.com/x/a", "trace-hang")
	mustCreateSkill(t, repo, s)
	got, err := repo.GetByID(ctx, "sk-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "trace-hang" || got.Description != s.Description {
		t.Errorf("name/description mismatch: %+v", got)
	}
	if got.Body != s.Body || got.BodySHA256 != "hash-sk-1" {
		t.Errorf("body/hash mismatch: %+v", got)
	}
	if got.Domain != "software" || got.Maturity != persistence.SkillMaturityDraft || got.Version != 1 {
		t.Errorf("domain/maturity/version mismatch: %+v", got)
	}
	if got.OriginClient != "claude-code" {
		t.Errorf("origin_client mismatch: %q", got.OriginClient)
	}
	assertStrings(t, "tags", got.Tags, []string{"debug", "net"})
	assertStrings(t, "roles", got.Roles, []string{"researcher"})
}

func skillDuplicateConflicts(t *testing.T, repo persistence.SkillRepository) {
	mustCreateSkill(t, repo, newTestSkill("sk-2a", "p1", "github.com/x/dup", "same"))
	err := repo.Create(context.Background(), newTestSkill("sk-2b", "p1", "github.com/x/dup", "same"))
	if !errors.Is(err, persistence.ErrSkillNameConflict) {
		t.Fatalf("expected ErrSkillNameConflict, got %v", err)
	}
}

func skillDistinctScopes(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	mustCreateSkill(t, repo, newTestSkill("sk-3a", "p1", "github.com/x/svcA", "restart"))
	mustCreateSkill(t, repo, newTestSkill("sk-3b", "p1", "github.com/x/svcB", "restart"))
	a, err := repo.Get(ctx, "p1", "github.com/x/svcA", "restart")
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if a.ID != "sk-3a" {
		t.Fatalf("scope-qualified Get returned wrong row: %s", a.ID)
	}
}

func skillGetUnknown(t *testing.T, repo persistence.SkillRepository) {
	_, err := repo.Get(context.Background(), "p1", "github.com/x/none", "nope")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func skillScopeIsolation(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	const p = "p2"
	mustCreateSkill(t, repo, newTestSkill("iso-x", p, "github.com/x/repoX", "x-only"))
	mustCreateSkill(t, repo, newTestSkill("iso-y", p, "github.com/x/repoY", "y-only"))
	mustCreateSkill(t, repo, newTestSkill("iso-star", p, "*", "everywhere"))
	mustCreateSkill(t, repo, newTestSkill("iso-null", p, "", "uncategorized"))

	got, err := repo.List(ctx, p, persistence.SkillListFilter{RepoScope: "github.com/x/repoX"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := idset(got)
	if ids["iso-y"] {
		t.Fatalf("repoY skill leaked into repoX scope: %v", keys(ids))
	}
	if !ids["iso-x"] || !ids["iso-star"] || !ids["iso-null"] {
		t.Fatalf("expected repoX + wildcard + NULL, got %v", keys(ids))
	}

	got, _ = repo.List(ctx, p, persistence.SkillListFilter{RepoScope: "github.com/x/repoX", StrictScope: true})
	ids = idset(got)
	if ids["iso-null"] {
		t.Fatalf("StrictScope must drop NULL-scoped skills: %v", keys(ids))
	}
	if !ids["iso-x"] || !ids["iso-star"] {
		t.Fatalf("StrictScope should still return exact + wildcard: %v", keys(ids))
	}
}

func skillMaturityRoleFilters(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	const p = "p1"
	act := newTestSkill("mat-active", p, "github.com/x/mat", "m-active")
	act.Maturity = persistence.SkillMaturityActive
	draft := newTestSkill("mat-draft", p, "github.com/x/mat", "m-draft")
	anyRole := newTestSkill("mat-anyrole", p, "github.com/x/mat", "m-anyrole")
	anyRole.Maturity = persistence.SkillMaturityActive
	anyRole.Roles = nil
	writerOnly := newTestSkill("mat-writer", p, "github.com/x/mat", "m-writer")
	writerOnly.Maturity = persistence.SkillMaturityActive
	writerOnly.Roles = []string{"writer"}
	for _, s := range []*persistence.Skill{act, draft, anyRole, writerOnly} {
		mustCreateSkill(t, repo, s)
	}

	got, err := repo.List(ctx, p, persistence.SkillListFilter{
		RepoScope:  "github.com/x/mat",
		Maturities: []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		Role:       "researcher",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := idset(got)
	if ids["mat-draft"] {
		t.Fatalf("draft leaked past maturity filter: %v", keys(ids))
	}
	if ids["mat-writer"] {
		t.Fatalf("writer-only skill leaked for researcher role: %v", keys(ids))
	}
	if !ids["mat-active"] || !ids["mat-anyrole"] {
		t.Fatalf("expected researcher-active + any-role-active: %v", keys(ids))
	}
}

func skillUpsertVersionBump(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	orig := newTestSkill("up-1", "p1", "github.com/x/up", "evolving")
	orig.Maturity = persistence.SkillMaturityActive
	mustCreateSkill(t, repo, orig)

	edit := newTestSkill("up-ignored-id", "p1", "github.com/x/up", "evolving")
	edit.Body = "# evolving\n\nrewritten"
	edit.BodySHA256 = "hash-v2"
	edit.Maturity = persistence.SkillMaturityActive // caller asks active; upsert forces draft
	stored, err := repo.Upsert(ctx, edit)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if stored.Version != 2 {
		t.Errorf("expected version bump to 2, got %d", stored.Version)
	}
	if stored.Maturity != persistence.SkillMaturityDraft {
		t.Errorf("edited skill must reset to draft, got %s", stored.Maturity)
	}
	if stored.Body != "# evolving\n\nrewritten" || stored.BodySHA256 != "hash-v2" {
		t.Errorf("upsert did not replace body/hash: %+v", stored)
	}
}

func skillSetMaturity(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	mustCreateSkill(t, repo, newTestSkill("sm-1", "p1", "github.com/x/sm", "gate"))
	if err := repo.SetMaturity(ctx, "sm-1", persistence.SkillMaturityActive); err != nil {
		t.Fatalf("SetMaturity: %v", err)
	}
	got, _ := repo.GetByID(ctx, "sm-1")
	if got.Maturity != persistence.SkillMaturityActive {
		t.Errorf("expected active, got %s", got.Maturity)
	}
	if err := repo.SetMaturity(ctx, "no-such", persistence.SkillMaturityActive); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("SetMaturity unknown id: expected ErrNotFound, got %v", err)
	}
}

func skillRecordFeedback(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	mustCreateSkill(t, repo, newTestSkill("fb-1", "p1", "github.com/x/fb", "counted"))
	for _, sig := range []string{persistence.SkillSignalFired, persistence.SkillSignalFired, persistence.SkillSignalCorrected} {
		if err := repo.RecordFeedback(ctx, "fb-1", sig); err != nil {
			t.Fatalf("RecordFeedback %s: %v", sig, err)
		}
	}
	got, _ := repo.GetByID(ctx, "fb-1")
	if got.UsageFired != 2 || got.UsageCorrected != 1 || got.UsageWorked != 0 {
		t.Errorf("counter mismatch: fired=%d worked=%d corrected=%d",
			got.UsageFired, got.UsageWorked, got.UsageCorrected)
	}
	if got.LastFiredAt == nil {
		t.Error("fired signal must stamp last_fired_at")
	}
}

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s length mismatch: got %v want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] mismatch: got %v want %v", label, i, got, want)
		}
	}
}

func idset(skills []*persistence.Skill) map[string]bool {
	m := make(map[string]bool, len(skills))
	for _, s := range skills {
		m[s.ID] = true
	}
	return m
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
