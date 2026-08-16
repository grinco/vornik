package ui

// Adoption insights — who is actually using Vornik.
// See https://docs.vornik.io
//
// Reads tasks.created_by_actor, which internal/actor writes at creation and
// which is immutable afterwards. Follows the InsightsTrends shape: a pure
// summarize function plus a thin List → summarize → render handler, so no new
// TaskRepository method is invented for a read the existing List already
// serves.
//
// THIS IS A PER-KEY LEADERBOARD, and the UI says so. A key is not a person —
// three `claude-code` keys may be one engineer on three laptops — so every
// label here is machine-level. §3.2's read-time resolution through
// user_identities is what turns this into per-person later, and it needs no
// backfill when it lands: the actor stored is what was OBSERVED, and the
// mapping is applied at read time.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/actor"
	"vornik.io/vornik/internal/persistence"
)

const (
	adoptionSampleCap = 4000 // most-recent tasks examined (List is created_at DESC)
	adoptionDays      = 30   // trailing window

	// adoptionCoverageFloor is the share of tasks that must carry an actor
	// before rows are labelled with their identity.
	//
	// §5 says a confident ranking must not be presented on thin coverage,
	// because a footnote under a table gets read as a table. It does NOT say
	// the activity must be hidden — and hiding it misleads in the other
	// direction, since a busy install with auth off then looks unused.
	//
	// So below the floor the rows are shown with IDENTITIES SUPPRESSED. That
	// enforces exactly what §5 protects — no low-coverage row is attributable
	// to a named actor — while the volume of work stays visible.
	adoptionCoverageFloor = 0.20
)

// actorRow is one leaderboard row.
type actorRow struct {
	// Actor is the stored `<kind>:<id>`. Empty when identities are suppressed.
	Actor string
	// Label is what the UI prints. Under suppression this is a positional
	// placeholder rather than the identity.
	Label string
	// Kind drives the badge; "system" rows are shown and visually distinct
	// rather than hidden, because hiding machine work makes human usage look
	// larger than it is (§5).
	Kind     string
	IsSystem bool
	// Unattributed marks the remainder row — activity with no actor recorded.
	// Rendered plainly rather than as a competitor, and never suppressed,
	// because it identifies nobody by construction.
	Unattributed bool

	Created   int
	Completed int
	Failed    int
	// SuccessPct is completed over created, the OUTCOME rather than volume.
	// A leaderboard ranked on tokens rewards waste (§4).
	SuccessPct int
}

// adoptionStats is the summarized window.
type adoptionStats struct {
	Sample int
	Days   int

	// Total and Attributed drive the coverage figure every panel carries.
	Total      int
	Attributed int
	// CoveragePct is Attributed/Total. ALWAYS displayed — a leaderboard
	// covering 1% of tasks must say "1%", not present a confident ranking of
	// the 1% (§5).
	CoveragePct int

	// IdentitiesSuppressed is true when coverage is below the floor. The
	// ranking is still rendered; the identities are not.
	IdentitiesSuppressed bool

	// HumanActors and SystemActors count DISTINCT actors of each class, so an
	// operator can see at a glance whether a number is people or machines.
	HumanActors  int
	SystemActors int

	// MachineInitiated is work that CORRECTLY has no human behind it —
	// autonomy ticks, executor-spawned children, delegations, schedules,
	// forks, checkpoints.
	//
	// It is excluded from the coverage denominator, and that is the whole
	// point. On this deployment 1,376 of 1,878 tasks in a 30-day window are
	// machine-initiated; dividing by all of them reported 0% coverage and read
	// as a failure to attribute, when attributing a cron to a person would be
	// the actual failure. The design says so in §1 — "most unattributed rows
	// are background workers with no human behind them; attributing those to a
	// person would be a lie, not a fix" — and the first version of this panel
	// quoted that and then divided by everything anyway.
	MachineInitiated int
	// HumanInitiatable is the coverage denominator: work a person could
	// plausibly have started (companion, UI, REST, A2A, chat, webhook).
	HumanInitiatable int

	// Unattributed is the count of in-window tasks carrying no actor. Shown as
	// its OWN ROW rather than dropped, because on an auth-off install that is
	// every task, and a leaderboard that renders empty there looks like nobody
	// uses Vornik — the exact misreading §5 exists to prevent, in the opposite
	// direction.
	Unattributed int

	// Feature usage (§4). Computed from the task sample alone, so these hold on
	// a deployment with auth off and NOTHING attributed — which is what makes
	// the panel demoable before any identity work lands.
	Features featureUsage

	// Activity counts every surface, not just scheduled tasks.
	Activity activityTotals

	// Keys is the per-credential leaderboard across surfaces. Ranked on RAG
	// queries first because that is the surface with real attribution today.
	Keys []keyRow

	Rows []actorRow
}

// keyRow is one credential's activity ACROSS surfaces, not just tasks.
//
// This is the row the customer actually asked for: "are the companion keys
// querying RAG, and how much". It joins two tables that already carry identity
// today — memory_retrieval_audit.actor_id and task_llm_usage.api_key_id — so it
// reports real usage without waiting for tasks.created_by_actor coverage to
// climb.
type keyRow struct {
	KeyID string
	// Label is the key name when known, else a shortened id. Keys are
	// credentials, not people: three companion keys may be one engineer.
	Label string
	// Kind is the observed channel ("companion:claude-code", "rest_api", ...).
	// Kept verbatim from memory_retrieval_audit rather than mapped onto the
	// internal/actor kinds — it is a DIFFERENT vocabulary (design §1) and
	// flattening them would invent precision.
	Kind string

	// Ephemeral marks a row the system issued to itself — per-task and
	// warm-pool agent credentials, folded together. Shown as one row rather
	// than dozens, and never mistaken for a person.
	Ephemeral bool
	// FoldedKeys is how many credentials this row represents, so an operator
	// can see that "agent (per-task keys)" is 14 keys and not one.
	FoldedKeys int

	RAGQueries int
	LLMCalls   int
	Tasks      int
	CostUSD    float64
	Tokens     int64

	// MemoryWrites is deposits into project memory — remember() and
	// rag-ingest. Reading only RETRIEVAL counted half the companion surface:
	// asking Vornik something and teaching it something are both adoption, and
	// the second is the stronger signal because it is an investment rather than
	// a query.
	MemoryWrites int

	// ActiveDays is how many distinct days this credential did anything in the
	// window — the retention signal, and the one that separates "tried it once,
	// heavily" from "uses it daily". Adoption is people coming BACK; a single
	// busy week outranks a habit on any volume-ranked board, which is why §4
	// puts stickiness above raw counts.
	ActiveDays int
	// Projects is how many distinct projects the credential touched. Breadth:
	// adoption is people using more of the product, not one person using one
	// feature harder.
	Projects int
}

// activityTotals counts what the product did, whether or not a task was
// scheduled. Most product use is NOT a task: a companion `recall` is a RAG
// query, a chat message is a turn, an agent step is a tool call. A dashboard
// built only on tasks reports a fraction of the work and reads as "barely
// used".
//
// ChatTurns and ToolCalls are DELIBERATELY ABSENT rather than shown as zero.
// chat_audit_log has no counting repo method and tool_audit_log's filter has no
// time bound, so neither can be counted here without a new repository surface.
// A zero would read as "no chat, no tool calls", which on this deployment is
// false by four orders of magnitude — and a wrong number is worse than a
// missing one on a panel whose entire job is to report usage honestly.
type activityTotals struct {
	Tasks        int
	RAGQueries   int
	MemoryWrites int
	LLMCalls     int
	CostUSD      float64
	Tokens       int64

	// Truncated marks a count that hit the per-query page cap and is therefore
	// a FLOOR, not a total.
	//
	// Caught on the first deployed render: the panel showed "4,000 RAG queries"
	// — exactly adoptionSampleCap — against 4,649 actual rows. A number that
	// happens to equal its own limit is the limit, and printing it as a total
	// understated usage by 14% on a panel whose entire job is reporting usage
	// honestly. Same failure shape as a rollup emitting a confident zero for a
	// field nothing populated.
	Truncated bool
}

// featureUsage is breadth: how much of the product is actually being used.
// Adoption is people using MORE of the product, not one person using one
// feature harder (§4).
type featureUsage struct {
	Completed   int
	Failed      int
	SuccessPct  int
	Delegations int
	Workflows   int
	Projects    int
	TaskTypes   int
	ActiveDays  int
	// BusiestDay is a human-readable "N tasks on YYYY-MM-DD".
	BusiestDay string
}

// summarizeAdoption buckets tasks by actor over the trailing window.
//
// Pure: no repository, no clock reads beyond the injected now, so the coverage
// and suppression behaviour are directly testable — which matters more here
// than usual, because the failure mode is a confident ranking of almost no data.
func summarizeAdoption(tasks []*persistence.Task, now time.Time, days int) adoptionStats {
	if days <= 0 {
		days = adoptionDays
	}
	s := adoptionStats{Sample: len(tasks), Days: days}
	cutoff := truncateDate(now).AddDate(0, 0, -(days - 1))

	b := newAdoptionBuckets()
	for _, t := range tasks {
		if t == nil || t.CreatedAt.Before(cutoff) {
			continue
		}
		s.Total++
		b.countBreadth(&s, t)
		b.countActor(&s, t)
	}
	b.finish(&s)
	return s
}

// adoptionBuckets holds the per-window accumulators. Split out of
// summarizeAdoption so each concern — breadth, attribution, ranking — reads on
// its own, and so the function stays under the complexity gate.
type adoptionBuckets struct {
	byActor   map[string]*actorRow
	workflows map[string]bool
	projects  map[string]bool
	perDay    map[string]int
}

func newAdoptionBuckets() *adoptionBuckets {
	return &adoptionBuckets{
		byActor:   map[string]*actorRow{},
		workflows: map[string]bool{},
		projects:  map[string]bool{},
		perDay:    map[string]int{},
	}
}

// countBreadth tallies the figures that hold with NO attribution at all — the
// ones that answer "is Vornik being used?" on a deployment with auth off.
func (b *adoptionBuckets) countBreadth(s *adoptionStats, t *persistence.Task) {
	switch t.Status {
	case persistence.TaskStatusCompleted:
		s.Features.Completed++
	case persistence.TaskStatusFailed:
		s.Features.Failed++
	}
	if t.ParentTaskID != nil && *t.ParentTaskID != "" {
		s.Features.Delegations++
	}
	if t.WorkflowID != nil && *t.WorkflowID != "" {
		b.workflows[*t.WorkflowID] = true
	}
	if t.ProjectID != "" {
		b.projects[t.ProjectID] = true
	}
	b.perDay[t.CreatedAt.Format("2006-01-02")]++
}

// countActor buckets one task by its actor, or into the unattributed remainder.
func (b *adoptionBuckets) countActor(s *adoptionStats, t *persistence.Task) {
	// Machine-initiated work leaves the denominator entirely: it has no human
	// to attribute and counting it as "missing" is what produced a 0% figure
	// beside a table naming a real credential.
	if machineInitiated(t.CreationSource) {
		s.MachineInitiated++
		return
	}
	s.HumanInitiatable++

	// created_by_actor is the new column and is not backfilled, so on any
	// deployment that predates it the SECOND source carries the coverage:
	// created_by_api_key_id has been populated by the companion path for
	// months. Reporting only the new column showed 0% while 75 companion tasks
	// in the same window carried a key.
	if (t.CreatedByActor == nil || *t.CreatedByActor == "") &&
		(t.CreatedByAPIKeyID == nil || *t.CreatedByAPIKeyID == "") {
		// A NULL actor is "we did not record", NOT anonymous. Anonymous is a
		// positive claim about an auth-off install and has its own kind;
		// folding the two would inflate coverage with rows that record nothing.
		s.Unattributed++
		return
	}
	if t.CreatedByActor == nil || *t.CreatedByActor == "" {
		// Attributed by key but not yet by actor: counts toward coverage, and
		// rolls up under its api_key actor so the row is nameable.
		s.Attributed++
		key := actor.APIKey(*t.CreatedByAPIKeyID)
		row := b.byActor[key.String()]
		if row == nil {
			row = &actorRow{Actor: key.String(), Kind: string(key.Kind)}
			b.byActor[key.String()] = row
		}
		row.Created++
		switch t.Status {
		case persistence.TaskStatusCompleted:
			row.Completed++
		case persistence.TaskStatusFailed:
			row.Failed++
		}
		return
	}
	parsed, err := actor.Parse(*t.CreatedByActor)
	if err != nil {
		// An unparseable actor is a bug, not a bucket. Counted out of coverage
		// so it cannot quietly pad the figure — but it IS unattributed
		// activity, so it still shows in that row.
		s.Unattributed++
		return
	}
	s.Attributed++

	key := parsed.String()
	row := b.byActor[key]
	if row == nil {
		row = &actorRow{Actor: key, Kind: string(parsed.Kind), IsSystem: parsed.IsSystem()}
		b.byActor[key] = row
	}
	row.Created++
	switch t.Status {
	case persistence.TaskStatusCompleted:
		row.Completed++
	case persistence.TaskStatusFailed:
		row.Failed++
	}
}

// finish derives the ratios and the busiest day from the accumulators.
func (b *adoptionBuckets) finish(s *adoptionStats) {
	s.Features.Workflows = len(b.workflows)
	s.Features.Projects = len(b.projects)
	s.Features.ActiveDays = len(b.perDay)
	// Coverage is over work a person could have started, NOT over everything.
	if s.HumanInitiatable > 0 {
		s.CoveragePct = int(float64(s.Attributed) / float64(s.HumanInitiatable) * 100)
	}
	if s.Total > 0 {
		s.Features.SuccessPct = int(float64(s.Features.Completed) / float64(s.Total) * 100)
	}

	var busyDay string
	var busyN int
	for day, n := range b.perDay {
		// Ties break on the later date so the figure is stable across runs
		// rather than depending on map iteration order.
		if n > busyN || (n == busyN && day > busyDay) {
			busyDay, busyN = day, n
		}
	}
	if busyN > 0 {
		s.Features.BusiestDay = fmt.Sprintf("%d on %s", busyN, busyDay)
	}
	b.rank(s)
}

// rank builds the ordered rows, appends the unattributed remainder, and applies
// identity suppression. Kept separate from the tallies because the ORDER of
// these three steps is the load-bearing part: ranking happens on real
// identities, the remainder is appended after so it never competes, and
// suppression runs last so it cannot be undone by a later write.
func (b *adoptionBuckets) rank(s *adoptionStats) {
	s.IdentitiesSuppressed = s.HumanInitiatable > 0 &&
		float64(s.Attributed)/float64(s.HumanInitiatable) < adoptionCoverageFloor

	for _, row := range b.byActor {
		if row.Created > 0 {
			row.SuccessPct = int(float64(row.Completed) / float64(row.Created) * 100)
		}
		if row.IsSystem {
			s.SystemActors++
		} else {
			s.HumanActors++
		}
		s.Rows = append(s.Rows, *row)
	}

	// Rank on COMPLETED tasks, not volume: someone who burned a lot of tokens
	// on failures is not the top adopter (§4). Ties break on the actor string
	// so the order is stable between two runs of the same query.
	sort.Slice(s.Rows, func(i, j int) bool {
		if s.Rows[i].Completed != s.Rows[j].Completed {
			return s.Rows[i].Completed > s.Rows[j].Completed
		}
		if s.Rows[i].Created != s.Rows[j].Created {
			return s.Rows[i].Created > s.Rows[j].Created
		}
		return s.Rows[i].Actor < s.Rows[j].Actor
	})

	if s.Unattributed > 0 {
		// Appended after the sort, deliberately: it is not competing with the
		// named actors, it is the remainder. Ranking it among them would put
		// "nobody" at the top of most installs.
		s.Rows = append(s.Rows, actorRow{
			Label:        "unattributed",
			Kind:         "unattributed",
			Unattributed: true,
			Created:      s.Unattributed,
		})
	}

	// Suppression happens AFTER ranking, so the ordering is real work while the
	// names are withheld — and it clears Actor too, not just Label, so an
	// identity cannot leak through a template that renders the wrong field.
	for i := range s.Rows {
		if s.Rows[i].Unattributed {
			continue // names nobody; nothing to suppress
		}
		if s.IdentitiesSuppressed {
			s.Rows[i].Actor = ""
			s.Rows[i].Label = suppressedLabel(i, s.Rows[i].IsSystem)
			continue
		}
		s.Rows[i].Label = s.Rows[i].Actor
	}
}

// suppressedLabel names a row without identifying it. System rows stay marked
// as machine work even under suppression: conflating them with people is the
// one thing that makes the panel actively misleading rather than merely vague.
func suppressedLabel(i int, isSystem bool) string {
	if isSystem {
		return "machine actor"
	}
	return "actor " + string(rune('A'+i%26))
}

// AdoptionData is the page model.
type AdoptionData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	// AllowedProjects / AllAccess mirror InsightsTrends: a project-scoped
	// session must never see instance-wide figures.
	AllowedProjects []string
	AllAccess       bool
	Notice          string
	Stats           adoptionStats
	Capabilities    capabilityStats
}

// InsightsAdoption serves GET /ui/insights/adoption.
func (s *Server) InsightsAdoption(w http.ResponseWriter, r *http.Request) {
	data := AdoptionData{
		Title:       "Insights — adoption",
		CurrentPage: "adoption",
		ProjectID:   r.URL.Query().Get("projectId"),
	}
	queryIDs, options, ok := s.resolveProjectScope(w, r, data.ProjectID)
	if !ok {
		return // 403 already written
	}
	data.AllowedProjects = options
	data.AllAccess = requestHasAllProjectAccess(r)

	if s.taskRepo == nil {
		data.Notice = "Task repository is not configured — no adoption data to show."
		s.render(w, "insights_adoption.html", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	tasks := s.listTasksForScope(ctx, queryIDs, persistence.TaskFilter{PageSize: adoptionSampleCap})
	data.Stats = summarizeAdoption(tasks, time.Now(), adoptionDays)
	data.Stats.Activity.Tasks = data.Stats.Total

	since := time.Now().AddDate(0, 0, -adoptionDays)
	s.collectKeyActivity(ctx, &data.Stats, queryIDs, data.AllowedProjects, since)
	s.resolveEphemeralKeys(ctx, &data.Stats, concreteProjects(queryIDs, data.AllowedProjects))
	data.Capabilities = s.collectCapabilities(ctx, since, queryIDs)

	switch {
	case data.Stats.Activity.Tasks == 0 && data.Stats.Activity.RAGQueries == 0 &&
		data.Stats.Activity.LLMCalls == 0:
		// Only when NOTHING happened on any surface. A task count of zero is
		// not enough: most product use never schedules a task.
		data.Notice = "No activity on any surface in the last 30 days."
	case data.Stats.Attributed == 0:
		// The expected state on any install that has not created a task since
		// migration 162: the column is new and deliberately not backfilled.
		// Deliberately NOT a Notice: a Notice replaces the whole panel, and on
		// an auth-off install — where nothing is attributed by design — that
		// would hide real activity behind an explanation. The zero-coverage
		// case is explained inline instead, above a table that still shows the
		// work.
	}
	s.render(w, "insights_adoption.html", data)
}

// collectKeyActivity builds the per-credential rows from the surfaces that
// ALREADY carry identity, and the instance-wide activity totals from those that
// do not.
//
// WHY THIS EXISTS SEPARATELY from summarizeAdoption: the first version of this
// panel was built entirely on `tasks`, and on a real deployment it reported
// almost nothing — 1,876 tasks, 0% attributed, and no sign that anyone was
// using the product. That was a measurement artefact, not the truth. The same
// window held 702 RAG queries from a single companion key, 54,462 chat turns
// and 110k LLM calls. Most product use never schedules a task, so a
// task-only dashboard understates adoption by more than an order of magnitude.
// concreteProjects turns a scope into REAL project ids.
//
// projectsToIterate returns [""] for an all-access caller, which repositories
// taking a filter read as "no filter" and repositories taking a project id read
// as "no match". Any read of the second kind needs the actual list, which the
// handler already has from resolveProjectScope.
func concreteProjects(queryIDs, allowed []string) []string {
	out := make([]string, 0, len(queryIDs)+len(allowed))
	for _, id := range queryIDs {
		if id != "" {
			out = append(out, id)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, id := range allowed {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) collectKeyActivity(ctx context.Context, st *adoptionStats, queryIDs, allowed []string, since time.Time) {
	rows := map[string]*keyRow{}
	row := func(id string) *keyRow {
		r := rows[id]
		if r == nil {
			r = &keyRow{KeyID: id, Label: shortKeyID(id)}
			rows[id] = r
		}
		return r
	}

	s.addRAGActivity(ctx, st, queryIDs, since, row)
	s.addMemoryWrites(ctx, st, concreteProjects(queryIDs, allowed), row)
	s.addSpendActivity(ctx, st, queryIDs, since, row)

	for _, r := range rows {
		st.Keys = append(st.Keys, *r)
	}
	// Ranked on RAG queries first: it is the surface with real attribution, so
	// ordering on it puts the rows a reader can trust at the top. Spend breaks
	// ties, then the id for stability between two runs of the same query.
	sort.Slice(st.Keys, func(i, j int) bool {
		if st.Keys[i].RAGQueries != st.Keys[j].RAGQueries {
			return st.Keys[i].RAGQueries > st.Keys[j].RAGQueries
		}
		if st.Keys[i].CostUSD != st.Keys[j].CostUSD {
			return st.Keys[i].CostUSD > st.Keys[j].CostUSD
		}
		return st.Keys[i].KeyID < st.Keys[j].KeyID
	})
	// Do not truncate here. Per-task credentials are folded later; taking the
	// top 20 before that can fill the page with fragments and discard every
	// real credential before the fragments collapse to one row.
}

// shortKeyID renders a credential id compactly without losing its identity —
// an operator has to be able to match it against `vornikctl` output.
func shortKeyID(id string) string {
	if len(id) <= 24 {
		return id
	}
	return id[:12] + "…" + id[len(id)-6:]
}

// addRAGActivity credits memory retrievals to the credential that made them.
// This is the surface with the best attribution today — actor_kind and actor_id
// are populated on real traffic — which is why it ranks the table.
func (s *Server) addRAGActivity(ctx context.Context, st *adoptionStats, queryIDs []string, since time.Time, row func(string) *keyRow) {
	if s.memoryRetrievalAudit == nil {
		return
	}
	seenDays := map[string]map[string]bool{}
	seenProjects := map[string]map[string]bool{}
	defer func() {
		for id, days := range seenDays {
			if r := row(id); r != nil {
				r.ActiveDays = len(days)
				r.Projects = len(seenProjects[id])
			}
		}
	}()
	for _, pid := range projectsToIterate(queryIDs) {
		audits, err := s.memoryRetrievalAudit.List(ctx, persistence.MemoryRetrievalAuditFilter{
			ProjectID: pid,
			Since:     since,
			PageSize:  adoptionSampleCap,
		})
		if err != nil {
			continue
		}
		if len(audits) >= adoptionSampleCap {
			// Saturated the page: there are more rows than we counted.
			st.Activity.Truncated = true
		}
		for _, a := range audits {
			if a == nil {
				continue
			}
			st.Activity.RAGQueries++
			// A nil actor is unattributed retrieval — a background worker, or a
			// row predating attribution. Counted in the TOTAL but credited to
			// nobody, so the total stays honest without inventing a row.
			if a.ActorID == nil || *a.ActorID == "" {
				continue
			}
			r := row(*a.ActorID)
			r.RAGQueries++
			if r.Kind == "" && a.ActorKind != nil {
				r.Kind = *a.ActorKind
			}
			// Distinct days and projects, tracked as sets keyed by credential
			// so folding ephemeral rows later unions them rather than summing
			// (summing would let 18 one-day agent keys report "18 active days").
			day := a.RetrievedAt.Format("2006-01-02")
			if seenDays[r.KeyID] == nil {
				seenDays[r.KeyID] = map[string]bool{}
				seenProjects[r.KeyID] = map[string]bool{}
			}
			seenDays[r.KeyID][day] = true
			if a.ProjectID != "" {
				seenProjects[r.KeyID][a.ProjectID] = true
			}
		}
	}
}

// addSpendActivity attaches LLM calls, tasks and spend per credential.
// AggregateByAPIKey already backs the spend UI; reused rather than re-derived so
// the two surfaces cannot quietly disagree about what a key cost.
func (s *Server) addSpendActivity(ctx context.Context, st *adoptionStats, queryIDs []string, since time.Time, row func(string) *keyRow) {
	if s.llmUsageRepo == nil {
		return
	}
	for _, scope := range spendScopes(queryIDs) {
		spends, err := s.llmUsageRepo.AggregateByAPIKey(ctx, since, time.Now(), adoptionSampleCap, scope)
		if err != nil {
			continue
		}
		if len(spends) >= adoptionSampleCap {
			st.Activity.Truncated = true
		}
		applySpendActivity(st, spends, row)
	}
}

func spendScopes(queryIDs []string) []string {
	if queryIDs == nil {
		return []string{""}
	}
	return queryIDs
}

func applySpendActivity(st *adoptionStats, spends []persistence.APIKeySpend, row func(string) *keyRow) {
	for _, sp := range spends {
		st.Activity.LLMCalls += sp.CallCount
		st.Activity.CostUSD += sp.CostUSD
		st.Activity.Tokens += sp.PromptTokens + sp.CompletionTokens
		if sp.APIKeyID == "" {
			// Unattributed spend is still real activity, but it is not a
			// credential and must not become a leaderboard row.
			continue
		}
		r := row(sp.APIKeyID)
		if sp.KeyName != "" {
			r.Label = sp.KeyName
		}
		r.LLMCalls += sp.CallCount
		r.Tasks += sp.TaskCount
		r.CostUSD += sp.CostUSD
		r.Tokens += sp.PromptTokens + sp.CompletionTokens
	}
}

// resolveEphemeralKeys collapses per-execution credentials into the actor they
// actually represent.
//
// THE DEFECT THIS FIXES. The executor mints a fresh API key for every task
// (`agent:task_<taskID>`, executor/container.go injectPerTaskKey) and the warm
// pool mints one per (project, role) (`agent:warm_…`). Both appear in
// memory_retrieval_audit under their own actor_id, so the first version of this
// panel listed them as INDIVIDUAL LEADERBOARD ROWS — a dozen `key_2026…` entries
// with 15-26 RAG queries each, zero tasks and $0.00, crowding out the real
// credentials and implying a dozen users where there was one agent runtime.
//
// A per-task key is not an actor. It is a credential the system issued to
// ITSELF to do somebody's work, so its activity belongs to whoever caused the
// task — which is precisely the "attribute once at the root, and propagate"
// rule the design is built on (§3), applied one level further out than task
// creation.
//
// Resolution, in order:
//   - `agent:task_<id>`  → that task's created_by_actor when known, else a
//     single "agent (per-task keys)" bucket. Never its own row.
//   - `agent:warm_…`     → a single "agent (warm pool)" bucket.
//   - anything else      → a real credential, left alone.
//
// warmAgentKeyNamePrefix mirrors the executor's reserved prefix for
// project-scoped warm-pool credentials. Duplicated rather than imported because
// internal/ui must not depend on internal/service; the drift risk is covered by
// the test, which asserts a warm key folds separately from a per-task one.
const warmAgentKeyNamePrefix = "agent:warm_"

// systemKeyIDPrefix is the id prefix of credentials the SYSTEM mints for itself
// — persistence.GenerateID("key"), used only by the per-task and warm-pool
// minters. Operator-facing credentials use "akey". Matched on the id rather
// than the name because a per-task key is deleted when its task ends, while the
// audit rows naming it survive, so the name is unavailable for most of them.
const systemKeyIDPrefix = "key_"

func (s *Server) resolveEphemeralKeys(ctx context.Context, st *adoptionStats, projects []string) {
	if s.apiKeyRepo == nil {
		st.Keys = limitKeyRows(st.Keys, 20)
		return
	}
	// Key id -> name, for every key the caller can see. One pass per project
	// rather than a lookup per row: the leaderboard is capped at 20 rows but
	// the audit rows behind it are thousands.
	names := map[string]string{}
	for _, pid := range projects {
		keys, err := s.apiKeyRepo.ListByProject(ctx, pid)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if k != nil {
				names[k.ID] = k.Name
			}
		}
	}
	// NO early return on an empty name map. The prefix fallback below does not
	// need names, and returning here is what made the first two attempts at this
	// fix silently do nothing on the deployed daemon: most per-task keys are
	// already deleted, so for a scope whose surviving keys are few the map comes
	// back empty and every ephemeral row survived untouched.

	// Insertion order is tracked separately from the map so the folded rows
	// are built deterministically — map iteration order would otherwise make
	// two renders of identical data disagree on tie-breaks.
	merged := map[string]*keyRow{}
	var mergedOrder []string
	fold := func(label string, src keyRow) {
		r := merged[label]
		if r == nil {
			r = &keyRow{KeyID: label, Label: label, Kind: "agent", Ephemeral: true}
			merged[label] = r
			mergedOrder = append(mergedOrder, label)
		}
		r.RAGQueries += src.RAGQueries
		r.MemoryWrites += src.MemoryWrites
		r.LLMCalls += src.LLMCalls
		r.Tasks += src.Tasks
		r.CostUSD += src.CostUSD
		r.Tokens += src.Tokens
		r.FoldedKeys++
		// Days and projects are MAXed, not summed. Eighteen single-day agent
		// keys are one agent active on some days, not eighteen active days —
		// summing would manufacture a retention figure out of key churn. Max
		// understates slightly (two keys on different days read as one), which
		// is the safe direction for a number meant to show habit.
		if src.ActiveDays > r.ActiveDays {
			r.ActiveDays = src.ActiveDays
		}
		if src.Projects > r.Projects {
			r.Projects = src.Projects
		}
	}

	resolved := make([]keyRow, 0, len(st.Keys))
	for _, row := range st.Keys {
		name, known := names[row.KeyID]
		switch {
		case known && strings.HasPrefix(name, persistence.TaskKeyNamePrefix):
			fold("agent (per-task keys)", row)
		case known && strings.HasPrefix(name, warmAgentKeyNamePrefix):
			fold("agent (warm pool)", row)
		case !known && strings.HasPrefix(row.KeyID, systemKeyIDPrefix):
			// The credential is GONE — per-task keys are deleted once the task
			// ends, while the retrieval audit that names them is not. On this
			// deployment 936 of 1,309 distinct actors in a 30-day window had no
			// surviving api_keys row, so a name lookup alone resolves barely a
			// quarter of them and the board fills with dead ids.
			//
			// The id prefix survives deletion and is a structural discriminator,
			// not a guess: `akey_` is minted only by the operator-facing key
			// handlers (api_key_handlers, admin_companion_handlers) and `key_`
			// only by the task and warm-pool minters in container_scheduler.
			// Confirmed against the data too — every one of the 645 surviving
			// `key_` rows is an agent:task_ key, and none of the 9 `akey_` rows
			// is.
			fold("agent (per-task keys)", row)
		default:
			// A real credential keeps its operator-facing NAME when it has one:
			// "vadim/migration-restore" is worth more to a reader than a key id.
			if name != "" {
				row.Label = name
			}
			resolved = append(resolved, row)
		}
	}
	for _, label := range mergedOrder {
		resolved = append(resolved, *merged[label])
	}
	st.Keys = resolved

	// Re-rank: folding changes the ordering, and an agent bucket that now
	// out-queries every human must sort where its volume puts it rather than
	// wherever its first fragment happened to land.
	sort.Slice(st.Keys, func(i, j int) bool {
		if st.Keys[i].RAGQueries != st.Keys[j].RAGQueries {
			return st.Keys[i].RAGQueries > st.Keys[j].RAGQueries
		}
		if st.Keys[i].CostUSD != st.Keys[j].CostUSD {
			return st.Keys[i].CostUSD > st.Keys[j].CostUSD
		}
		return st.Keys[i].KeyID < st.Keys[j].KeyID
	})
	st.Keys = limitKeyRows(st.Keys, 20)
}

func limitKeyRows(rows []keyRow, limit int) []keyRow {
	if limit <= 0 || len(rows) <= limit {
		return rows
	}
	return rows[:limit]
}

// machineInitiated reports whether a creation source has no human behind it by
// construction. These leave the coverage denominator: a leaderboard that counts
// an autonomy tick as "unattributed" is measuring the wrong thing, and reports
// 0% on a deployment where every human action IS attributed.
func machineInitiated(src persistence.TaskCreationSource) bool {
	switch src {
	case persistence.TaskCreationSourceAutonomous,
		persistence.TaskCreationSourceRoute,
		persistence.TaskCreationSourceDelegation,
		persistence.TaskCreationSourceScheduled,
		persistence.TaskCreationSourceCheckpoint,
		persistence.TaskCreationSourceFork:
		return true
	}
	return false
}

// addMemoryWrites credits deposits into project memory to the credential that
// made them.
//
// memory_ingest_audit carries the same (actor_kind, actor_id) pair as the
// retrieval ledger and has done for months — 141 companion deposits in the
// window this panel first shipped against. The first version simply never read
// it, so half the companion surface was invisible: the panel could say how
// often someone QUERIED memory but not how often they FED it.
//
// Agent-role deposits (actor_kind "agent", actor_id a role name like "writer")
// are counted toward the instance total but not credited to a credential —
// a role is not a credential, and mapping one onto the other would put
// "writer" on a leaderboard of people.
func (s *Server) addMemoryWrites(ctx context.Context, st *adoptionStats, projects []string, row func(string) *keyRow) {
	if s.memoryIngestAudit == nil {
		return
	}
	for _, pid := range projects {
		if pid == "" {
			// ListByProject filters on `project_id = $1`, so the empty
			// all-projects sentinel projectsToIterate returns matches NOTHING.
			// The retrieval ledger takes a FILTER whose empty ProjectID means
			// "no filter", so the same sentinel works there and silently does
			// not here — which is why memory writes rendered 0 against 494
			// rows in the database. Concrete ids are resolved by the caller.
			continue
		}
		audits, err := s.memoryIngestAudit.ListByProject(ctx, pid, adoptionSampleCap)
		if err != nil {
			continue
		}
		if len(audits) >= adoptionSampleCap {
			st.Activity.Truncated = true
		}
		for _, a := range audits {
			if a == nil {
				continue
			}
			st.Activity.MemoryWrites++
			if a.ActorID == nil || *a.ActorID == "" {
				continue
			}
			// Only credential-shaped actors. An "agent" row names a role.
			if a.ActorKind == nil || !strings.HasPrefix(*a.ActorKind, "companion:") {
				continue
			}
			r := row(*a.ActorID)
			r.MemoryWrites++
			if r.Kind == "" {
				r.Kind = *a.ActorKind
			}
		}
	}
}
