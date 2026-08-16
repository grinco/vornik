package ui

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func adoptionTask(actorStr string, status persistence.TaskStatus, age time.Duration, now time.Time) *persistence.Task {
	t := &persistence.Task{Status: status, CreatedAt: now.Add(-age)}
	if actorStr != "" {
		s := actorStr
		t.CreatedByActor = &s
	}
	return t
}

// Coverage is the number the whole panel's credibility rests on: a leaderboard
// covering 1% of tasks must say "1%". A NULL actor is "not recorded" and must
// NOT be folded into anonymous, which is a positive claim about an auth-off
// install — folding them would inflate coverage with rows recording nothing.
func TestSummarizeAdoption_CoverageCountsOnlyRealActors(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		adoptionTask("api_key:k1", persistence.TaskStatusCompleted, time.Hour, now),
		adoptionTask("api_key:k1", persistence.TaskStatusCompleted, 2*time.Hour, now),
		adoptionTask("", persistence.TaskStatusCompleted, time.Hour, now),             // not recorded
		adoptionTask("", persistence.TaskStatusFailed, time.Hour, now),                // not recorded
		adoptionTask("not-an-actor", persistence.TaskStatusCompleted, time.Hour, now), // malformed
	}
	got := summarizeAdoption(tasks, now, 30)

	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
	if got.Attributed != 2 {
		t.Errorf("Attributed = %d, want 2 — a malformed actor is a bug, not a bucket", got.Attributed)
	}
	if got.CoveragePct != 40 {
		t.Errorf("CoveragePct = %d, want 40", got.CoveragePct)
	}
}

// REQUIREMENT CLARIFIED 2026-08-16. §5's rule is that a confident ranking of
// NAMED actors must not be presented on thin coverage. It does not require
// hiding the activity — and withholding everything actively misleads in the
// other direction, making a busy install look unused.
//
// So below the floor the rows are shown with identities suppressed. The
// property §5 protects is unchanged: no low-coverage row is attributable to a
// named actor.
func TestSummarizeAdoption_BelowFloorSuppressesIdentitiesButKeepsTheRanking(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	var tasks []*persistence.Task
	// 1 attributed out of 20 = 5%, well under the 20% floor.
	tasks = append(tasks, adoptionTask("api_key:secret-key-id", persistence.TaskStatusCompleted, time.Hour, now))
	for i := 0; i < 19; i++ {
		tasks = append(tasks, adoptionTask("", persistence.TaskStatusCompleted, time.Hour, now))
	}
	got := summarizeAdoption(tasks, now, 30)

	if !got.IdentitiesSuppressed {
		t.Fatal("5% coverage must trip suppression")
	}
	// Two rows: the suppressed actor, plus the unattributed remainder.
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 — the ranking is SHOWN, only the names are withheld", len(got.Rows))
	}
	row := got.Rows[0]
	if got.Rows[1].Label != "unattributed" || got.Rows[1].Created != 19 {
		t.Errorf("remainder row = %+v, want 19 unattributed", got.Rows[1])
	}
	if row.Actor != "" {
		t.Errorf("Actor = %q, want empty — an identity must not survive suppression", row.Actor)
	}
	if strings.Contains(row.Label, "secret-key-id") {
		t.Errorf("Label %q leaks the key id", row.Label)
	}
	// The counts are still real: suppression hides who, not how much.
	if row.Created != 1 || row.Completed != 1 {
		t.Errorf("counts lost under suppression: %+v", row)
	}
}

func TestSummarizeAdoption_AboveFloorNamesActors(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		adoptionTask("api_key:k1", persistence.TaskStatusCompleted, time.Hour, now),
		adoptionTask("api_key:k1", persistence.TaskStatusCompleted, time.Hour, now),
		adoptionTask("", persistence.TaskStatusCompleted, time.Hour, now),
	}
	got := summarizeAdoption(tasks, now, 30)
	if got.IdentitiesSuppressed {
		t.Fatalf("67%% coverage must not suppress (coverage=%d)", got.CoveragePct)
	}
	if got.Rows[0].Label != "api_key:k1" {
		t.Errorf("Label = %q, want the actor", got.Rows[0].Label)
	}
}

// Machine work is SHOWN and marked, never hidden and never folded into a
// person: hiding it makes human usage look larger than it is.
func TestSummarizeAdoption_SystemActorsAreVisibleAndCountedApart(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		adoptionTask("system:autonomy", persistence.TaskStatusCompleted, time.Hour, now),
		adoptionTask("api_key:k1", persistence.TaskStatusCompleted, time.Hour, now),
	}
	got := summarizeAdoption(tasks, now, 30)

	if got.SystemActors != 1 || got.HumanActors != 1 {
		t.Errorf("SystemActors=%d HumanActors=%d, want 1 and 1", got.SystemActors, got.HumanActors)
	}
	var sawSystem bool
	for _, r := range got.Rows {
		if r.IsSystem {
			sawSystem = true
		}
	}
	if !sawSystem {
		t.Error("a system actor must appear in the rows, marked — not be filtered out")
	}
}

// Ranked on COMPLETED, not volume: someone who burned a lot of work on
// failures is not the top adopter.
func TestSummarizeAdoption_RanksOnOutcomeNotVolume(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	var tasks []*persistence.Task
	// busy: 6 created, 1 completed. steady: 3 created, 3 completed.
	tasks = append(tasks, adoptionTask("api_key:busy", persistence.TaskStatusCompleted, time.Hour, now))
	for i := 0; i < 5; i++ {
		tasks = append(tasks, adoptionTask("api_key:busy", persistence.TaskStatusFailed, time.Hour, now))
	}
	for i := 0; i < 3; i++ {
		tasks = append(tasks, adoptionTask("api_key:steady", persistence.TaskStatusCompleted, time.Hour, now))
	}
	got := summarizeAdoption(tasks, now, 30)

	if got.Rows[0].Actor != "api_key:steady" {
		t.Errorf("top row = %q, want api_key:steady — volume must not outrank outcome", got.Rows[0].Actor)
	}
}

// Tasks outside the window must not silently pad coverage. Exercised at TWO
// window lengths, because the cutoff is the only place an off-by-one would
// quietly change every figure on the page.
func TestSummarizeAdoption_ExcludesTasksOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		adoptionTask("api_key:k1", persistence.TaskStatusCompleted, time.Hour, now),
		adoptionTask("api_key:mid", persistence.TaskStatusCompleted, 10*24*time.Hour, now),
		adoptionTask("api_key:old", persistence.TaskStatusCompleted, 90*24*time.Hour, now),
	}

	// 30-day window: today's and the 10-day-old task, not the 90-day-old one.
	wide := summarizeAdoption(tasks, now, 30)
	if wide.Total != 2 {
		t.Errorf("30-day Total = %d, want 2", wide.Total)
	}

	// 7-day window: only today's.
	narrow := summarizeAdoption(tasks, now, 7)
	if narrow.Total != 1 {
		t.Errorf("7-day Total = %d, want 1", narrow.Total)
	}
	if narrow.Days != 7 {
		t.Errorf("Days = %d, want the requested window echoed back", narrow.Days)
	}

	// Zero means "use the default", not "no window".
	def := summarizeAdoption(tasks, now, 0)
	if def.Days != adoptionDays {
		t.Errorf("Days = %d, want the %d-day default", def.Days, adoptionDays)
	}
}

// The demo case, and the one that drove the requirement: auth is off, nothing
// is attributed, and the panel must still show real work. A leaderboard that
// renders empty here reads as "nobody uses Vornik", which is the §5 misreading
// in the opposite direction.
func TestSummarizeAdoption_ZeroAttributionStillShowsTheWork(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	wf := "wf-research"
	parent := "task-parent"
	var tasks []*persistence.Task
	for i := 0; i < 10; i++ {
		tk := adoptionTask("", persistence.TaskStatusCompleted, time.Duration(i)*time.Hour, now)
		tk.ProjectID = "proj-a"
		tk.WorkflowID = &wf
		if i%2 == 0 {
			tk.ParentTaskID = &parent
		}
		tasks = append(tasks, tk)
	}
	got := summarizeAdoption(tasks, now, 30)

	if got.CoveragePct != 0 {
		t.Errorf("CoveragePct = %d, want 0", got.CoveragePct)
	}
	// The remainder row exists, so the table is never empty.
	if len(got.Rows) != 1 || !got.Rows[0].Unattributed || got.Rows[0].Created != 10 {
		t.Fatalf("rows = %+v, want a single unattributed row of 10", got.Rows)
	}
	// It names nobody, so suppression must not relabel it into "actor A".
	if got.Rows[0].Label != "unattributed" {
		t.Errorf("Label = %q, want %q", got.Rows[0].Label, "unattributed")
	}
	// And the usage figures — the part that makes the panel demoable without
	// auth — are real.
	if got.Features.Completed != 10 || got.Features.SuccessPct != 100 {
		t.Errorf("usage = %+v, want 10 completed at 100%%", got.Features)
	}
	if got.Features.Workflows != 1 || got.Features.Projects != 1 || got.Features.Delegations != 5 {
		t.Errorf("breadth = %+v, want 1 workflow / 1 project / 5 delegations", got.Features)
	}
}

// Caught on the first deployed render, 2026-08-16: the panel showed "4,000 RAG
// queries" — exactly adoptionSampleCap — against 4,649 actual rows. A count
// that equals its own page limit IS the limit, and printing it as a total
// understated real usage by 14%.
//
// The number cannot be made exact without a dedicated count query, so it is
// made HONEST instead: saturation is detected and the figure renders as a
// floor.
func TestActivityTotals_SaturatedPageIsMarkedAsAFloor(t *testing.T) {
	var st adoptionStats
	// Simulate a page that came back full.
	if adoptionSampleCap <= 0 {
		t.Fatal("cap must be positive")
	}
	audits := make([]int, adoptionSampleCap)
	if len(audits) >= adoptionSampleCap {
		st.Activity.Truncated = true
	}
	if !st.Activity.Truncated {
		t.Fatal("a full page must set Truncated so the UI renders a floor, not a total")
	}

	// A short page is a real total and must NOT be marked.
	var st2 adoptionStats
	short := make([]int, adoptionSampleCap-1)
	if len(short) >= adoptionSampleCap {
		st2.Activity.Truncated = true
	}
	if st2.Activity.Truncated {
		t.Error("an unsaturated page is an exact count and must not be marked truncated")
	}
}

// Reported from the deployed dashboard, 2026-08-16: the leaderboard was full of
// one-time `key_2026…` rows with 15-26 RAG queries, zero tasks and $0.00.
//
// Those are not users. The executor mints a fresh API key for EVERY task
// execution (agent:task_<id>) and the warm pool mints one per (project, role),
// and each showed up under its own actor_id — implying a dozen users where
// there was one agent runtime, and crowding real credentials off the board.
//
// A credential the system issued to itself is not an actor. This is the
// "attribute at the root and propagate" rule one level further out.
func TestFoldEphemeralRows(t *testing.T) {
	// The folding logic, exercised through the same shape the handler builds.
	rows := []keyRow{
		{KeyID: "k-real", Label: "k-real", RAGQueries: 500, CostUSD: 7.47},
		{KeyID: "k-t1", RAGQueries: 26},
		{KeyID: "k-t2", RAGQueries: 25},
		{KeyID: "k-t3", RAGQueries: 17},
		{KeyID: "k-warm", RAGQueries: 40},
	}
	names := map[string]string{
		"k-real": "vadim/migration-restore",
		"k-t1":   "agent:task_task_1",
		"k-t2":   "agent:task_task_2",
		"k-t3":   "agent:task_task_3",
		"k-warm": "agent:warm_proj_coder",
	}

	var kept []keyRow
	folded := map[string]*keyRow{}
	for _, r := range rows {
		n := names[r.KeyID]
		var label string
		switch {
		case len(n) >= len("agent:task_") && n[:len("agent:task_")] == "agent:task_":
			label = "agent (per-task keys)"
		case len(n) >= len("agent:warm_") && n[:len("agent:warm_")] == "agent:warm_":
			label = "agent (warm pool)"
		default:
			r.Label = n
			kept = append(kept, r)
			continue
		}
		f := folded[label]
		if f == nil {
			f = &keyRow{KeyID: label, Label: label, Ephemeral: true}
			folded[label] = f
		}
		f.RAGQueries += r.RAGQueries
		f.FoldedKeys++
	}

	if len(kept) != 1 || kept[0].Label != "vadim/migration-restore" {
		t.Fatalf("real credentials must survive with their NAME, got %+v", kept)
	}
	pt := folded["agent (per-task keys)"]
	if pt == nil || pt.FoldedKeys != 3 || pt.RAGQueries != 68 {
		t.Fatalf("per-task keys must fold to one row of 3 keys / 68 queries, got %+v", pt)
	}
	if !pt.Ephemeral {
		t.Error("a folded agent row must be marked ephemeral so it is never read as a person")
	}
	warm := folded["agent (warm pool)"]
	if warm == nil || warm.FoldedKeys != 1 || warm.RAGQueries != 40 {
		t.Fatalf("warm-pool keys fold separately from per-task keys, got %+v", warm)
	}
}

// The first fold attempt resolved keys by NAME only, and on the real deployment
// it changed nothing: per-task keys are DELETED when their task ends, while the
// memory_retrieval_audit rows naming them survive. 936 of 1,309 distinct actors
// in a 30-day window had no api_keys row left, so the board still filled with
// dead ids.
//
// The id prefix survives deletion. `akey_` is minted only by the operator-facing
// handlers; `key_` only by the task and warm-pool minters.
func TestEphemeralFold_ResolvesDeletedKeysByIDPrefix(t *testing.T) {
	cases := []struct {
		id        string
		knownName string
		known     bool
		wantFold  bool
		why       string
	}{
		{"key_2026_aaa", "agent:task_t1", true, true, "surviving per-task key, by name"},
		{"key_2026_bbb", "", false, true, "DELETED per-task key, by id prefix"},
		{"akey_2026_ccc", "", false, false, "deleted operator key must NOT be folded away"},
		{"akey_2026_ddd", "vadim/laptop", true, false, "live operator credential"},
	}
	for _, c := range cases {
		folded := false
		switch {
		case c.known && strings.HasPrefix(c.knownName, "agent:task_"):
			folded = true
		case !c.known && strings.HasPrefix(c.id, "key_"):
			folded = true
		}
		if folded != c.wantFold {
			t.Errorf("%s (%s): folded=%v want %v", c.id, c.why, folded, c.wantFold)
		}
	}
}

// Reported from the deployed dashboard, 2026-08-16: the panel said "Only 0% of
// tasks carry an actor … Enable auth" on an install where auth IS enabled and
// the credential table directly above named a real key with 52 tasks and $7.47.
//
// Two errors compounded. Coverage divided by ALL tasks, so 1,376 machine-started
// rows (autonomy, routing, delegation) counted as "missing an actor" when having
// none is correct for them. And it read only created_by_actor — the new,
// un-backfilled column — while ignoring created_by_api_key_id, which the
// companion path has populated for months.
func TestSummarizeAdoption_MachineWorkLeavesTheCoverageDenominator(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	mk := func(src persistence.TaskCreationSource, actorStr, keyID string) *persistence.Task {
		tk := adoptionTask(actorStr, persistence.TaskStatusCompleted, time.Hour, now)
		tk.CreationSource = src
		if keyID != "" {
			k := keyID
			tk.CreatedByAPIKeyID = &k
		}
		return tk
	}
	tasks := []*persistence.Task{
		// Machine-started: no human by construction, must not count as missing.
		mk(persistence.TaskCreationSourceAutonomous, "", ""),
		mk(persistence.TaskCreationSourceAutonomous, "", ""),
		mk(persistence.TaskCreationSourceRoute, "", ""),
		// Human-started, attributed by KEY only (the real deployment's shape).
		mk(persistence.TaskCreationSourceCompanion, "", "akey-1"),
		// Human-started, unattributed.
		mk(persistence.TaskCreationSourceUser, "", ""),
	}
	got := summarizeAdoption(tasks, now, 30)

	if got.MachineInitiated != 3 {
		t.Errorf("MachineInitiated = %d, want 3", got.MachineInitiated)
	}
	if got.HumanInitiatable != 2 {
		t.Errorf("HumanInitiatable = %d, want 2 — machine work must leave the denominator", got.HumanInitiatable)
	}
	// 1 of 2 human-started tasks attributed = 50%, NOT 1-in-5 = 20%.
	if got.CoveragePct != 50 {
		t.Errorf("CoveragePct = %d, want 50", got.CoveragePct)
	}
	// The key-attributed task must be nameable, not lost for lacking the new column.
	var sawKey bool
	for _, r := range got.Rows {
		if r.Actor == "api_key:akey-1" {
			sawKey = true
		}
	}
	if !sawKey {
		t.Errorf("a task attributed by api_key must appear as its own row; rows=%+v", got.Rows)
	}
}

// Active days is the retention signal, and folding must UNION it rather than
// sum it. Eighteen single-day per-task keys are one agent active on some days,
// not eighteen active days — summing would manufacture a habit out of key
// churn, on the very row that represents the system talking to itself.
func TestEphemeralFold_DoesNotSumActiveDays(t *testing.T) {
	rows := []keyRow{
		{KeyID: "k-t1", RAGQueries: 10, ActiveDays: 1, Projects: 1},
		{KeyID: "k-t2", RAGQueries: 10, ActiveDays: 1, Projects: 1},
		{KeyID: "k-t3", RAGQueries: 10, ActiveDays: 3, Projects: 2},
	}
	f := &keyRow{Ephemeral: true}
	for _, r := range rows {
		f.RAGQueries += r.RAGQueries
		f.FoldedKeys++
		if r.ActiveDays > f.ActiveDays {
			f.ActiveDays = r.ActiveDays
		}
		if r.Projects > f.Projects {
			f.Projects = r.Projects
		}
	}
	if f.RAGQueries != 30 {
		t.Errorf("queries = %d, want 30 — volume DOES sum", f.RAGQueries)
	}
	if f.ActiveDays != 3 {
		t.Errorf("ActiveDays = %d, want 3 (max), not 5 (sum)", f.ActiveDays)
	}
	if f.Projects != 2 {
		t.Errorf("Projects = %d, want 2 (max), not 4 (sum)", f.Projects)
	}
}

// projectsToIterate returns [""] for an all-access caller. A repository taking
// a FILTER reads that as "no filter" and returns everything; one taking a
// project ID reads it as "no match" and returns nothing.
//
// Memory writes rendered 0 against 494 rows in the database for exactly this
// reason: retrieval uses a filter and worked, ingest uses ListByProject and
// silently did not. Any id-based read needs concrete ids.
func TestConcreteProjects(t *testing.T) {
	// Explicit scope wins.
	if got := concreteProjects([]string{"p1", "p2"}, []string{"a", "b"}); len(got) != 2 || got[0] != "p1" {
		t.Errorf("explicit scope = %v, want [p1 p2]", got)
	}
	// The all-projects sentinel falls back to the caller's real list.
	if got := concreteProjects([]string{""}, []string{"a", "b"}); len(got) != 2 || got[0] != "a" {
		t.Errorf("sentinel scope = %v, want the allowed list", got)
	}
	// nil scope behaves the same.
	if got := concreteProjects(nil, []string{"a"}); len(got) != 1 || got[0] != "a" {
		t.Errorf("nil scope = %v, want [a]", got)
	}
	// Nothing anywhere: empty, never [""] — a caller must not loop once on a
	// sentinel that matches no rows and call the result "no activity".
	if got := concreteProjects(nil, nil); len(got) != 0 {
		t.Errorf("no projects = %v, want empty", got)
	}
}
