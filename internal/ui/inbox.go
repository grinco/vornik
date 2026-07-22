package ui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
)

// inboxItem is one actionable row in the "Needs you" inbox.
type inboxItem struct {
	TaskID    string
	ProjectID string
	Kind      string // human label: "Needs approval" / "Needs input" / "Failed" / "Blocked"
	// Title is the plain-language headline — the task's prompt excerpt
	// (taskSummary, same extractor as the tasks page). Empty when the
	// payload carries no recognisable prompt; the template then falls
	// back to the raw TaskID (2026-07-12 mobile-usability pass: a row
	// must say what it's about, not just its id).
	Title  string
	Detail string // error class for failures, etc.
	Age    string // "5m ago"
	Action string // CTA label, e.g. "Approve / reject"
	Href   string // where the action lives (task detail)

	// Inline-action fields (task 4.3, design §5.6). Populated only for the
	// kinds that carry an inline action; zero-valued (harmless) otherwise.

	// CheckpointID + Checkpoint back the "Needs input" row's inline
	// answer control — the open checkpoint's message ID (required by
	// uiAnswerCheckpoint's checkpoint_id form field) and its parsed
	// payload (decision options vs free-text question). Best-effort:
	// nil/empty when no taskMessageRepo is wired or no checkpoint is
	// open (the row still renders, just without the control).
	CheckpointID string
	Checkpoint   *CheckpointView

	// FixItURL is the Phase-3 "Help me fix this" deep link for a
	// "Failed" row — /ui/fixit/failed_task/<id>, same URL
	// recovery_actions.go already offers on the task detail page.
	FixItURL string

	rank      int       // category rank for sorting (lower = more urgent)
	createdAt time.Time // within-category tiebreak (oldest first)
}

// InboxData backs templates/inbox.html.
type InboxData struct {
	Title       string
	CurrentPage string // "inbox" → Orchestration nav area
	Items       []inboxItem
	Count       int

	// Requests are the "Your requests" cards (task 4.2, Outcome Inbox
	// design §5.3/§5.5): the attention queue's tasks folded to one card
	// per request-root. Inline attention actions are 4.3 — a card's
	// attention items link out to the task/panel, same as Items today.
	Requests []requestCard

	// HasMoreAttention flags that the single scoped attention query hit
	// inboxAttentionPageSize — more actionable rows may exist beyond what
	// rendered. Review-2627 fix: the cap must never silently drop rows;
	// this (plus the accompanying log.Warn in Inbox) is the non-silent
	// signal, surfaced in inbox.html as a "…and more waiting" note.
	HasMoreAttention  bool
	AttentionPageSize int

	// RecentRequests is the broader "Your requests" list (task 4.4,
	// design §5.7): recent request-root cards in the viewer's scope,
	// ANY status — not just the attention subset Requests above covers.
	// This is what makes a purely COMPLETED/RUNNING request (with no
	// attention-flagged descendant) reachable as a card at all — the
	// gap task 4.2 deferred. Deduplicated against Requests: a request
	// already pinned in the attention section above is not repeated
	// here. Recency-ordered (freshest request activity first), built
	// via the SAME buildRequestCards machinery as Requests (no
	// reimplementation).
	RecentRequests []requestCard

	// HasMoreRecent mirrors HasMoreAttention for the broader query: the
	// recent-tasks window hit inboxRecentRequestsPageSize, so more
	// requests may exist beyond what rendered. Non-silent (review-2627
	// discipline extended to this section) — logged + surfaced as a note.
	HasMoreRecent          bool
	RecentRequestsPageSize int

	// WebWrites are the pending supervised web-write approvals
	// (supervised-web-write-actions Task 6): each is a "Needs approval" card
	// whose detail renders the filled-form screenshot, the read-only field
	// table (name + value + provenance), the target host and the submission
	// id. Approve/reject are authenticated CSRF POSTs to
	// /ui/inbox/web-write/{id}/{approve|reject}; approve mints the capability
	// token. Empty when the web-write repo is unwired or no row is pending.
	WebWrites []webWriteCard
}

// NavAttentionCount implements navAttentionCounter (nav_model.go) so the
// "My requests" nav destination shows a live "(N)" badge while the
// inbox page is being rendered — the same attention-item count the
// pinned "Needs your attention" queue above renders (design §5.7 Q4).
func (d InboxData) NavAttentionCount() int { return d.Count }

// inboxCategory pairs a task status with its presentation.
type inboxCategory struct {
	status persistence.TaskStatus
	rank   int
	kind   string
	action string
}

// inboxItem.Kind values — named so the Go-side dispatch (which kind gets
// which inline action, task 4.3 §5.6) can't drift from the template's
// string comparisons via a typo. The literal values themselves are
// unchanged from before 4.3 (rendered in the page today).
const (
	inboxKindNeedsApproval = "Needs approval"
	inboxKindNeedsInput    = "Needs input"
	inboxKindFailed        = "Failed"
	inboxKindBlocked       = "Blocked on children"
	// inboxKindRetrying is an INFORMATIONAL row (no inline action): a task the
	// operator retried that is back in flight. It renders in the queue so a
	// requeue stays visible until the re-run terminates, but does NOT count
	// toward the "needs you" badge (Inbox sets Count excluding this kind).
	inboxKindRetrying = "Retrying"
)

// retryingRank sorts the informational "Retrying…" rows after every
// actionable category (ranks 1–4) — they progress on their own, nothing is
// blocked on them.
const retryingRank = 5

// isRetryInFlightStatus reports whether a status is one a retried task passes
// through before terminating — the states ListRetryInFlight returns.
func isRetryInFlightStatus(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusQueued, persistence.TaskStatusLeased, persistence.TaskStatusRunning:
		return true
	default:
		return false
	}
}

// countActionableItems counts the attention rows a human is actually blocked
// on — every kind EXCEPT the informational "Retrying…" rows, which render in
// the queue but must not inflate the "needs you" badge.
func countActionableItems(items []inboxItem) int {
	n := 0
	for _, it := range items {
		if it.Kind != inboxKindRetrying {
			n++
		}
	}
	return n
}

// newRetryingItem builds the informational "Retrying…" row for a task an
// operator retried that is back in flight. No inline action, no FixItURL — the
// re-run's outcome (COMPLETED → row clears; FAILED → row returns as "Failed")
// is what resolves it, not an operator click.
func newRetryingItem(t *persistence.Task) inboxItem {
	return inboxItem{
		TaskID:    t.ID,
		ProjectID: t.ProjectID,
		Kind:      inboxKindRetrying,
		Title:     taskSummary(t.Payload),
		Age:       humanizeSince(time.Since(t.CreatedAt)) + " ago",
		Action:    "Retrying…",
		Href:      "/ui/tasks/" + t.ID,
		rank:      retryingRank,
		createdAt: t.CreatedAt,
	}
}

// inboxCategories is the fixed set backing the attention queue: a human
// is blocked on AWAITING_APPROVAL/AWAITING_INPUT (ranked first), then
// recent FAILED and WAITING_FOR_CHILDREN (already terminal / waiting).
// AWAITING_EXTERNAL is deliberately absent — waiting on an external
// system isn't human-actionable (Outcome Inbox design §5.2/§4).
var inboxCategories = []inboxCategory{
	{persistence.TaskStatusAwaitingApproval, 1, inboxKindNeedsApproval, "Approve / reject"},
	{persistence.TaskStatusAwaitingInput, 2, inboxKindNeedsInput, "Answer checkpoint"},
	{persistence.TaskStatusFailed, 3, inboxKindFailed, "Review / retry"},
	{persistence.TaskStatusWaitingForChildren, 4, inboxKindBlocked, "Inspect"},
}

// newAttentionItem builds one attention-queue row from a task already
// known to match category c (the caller looked c up via the byStatus
// map). Returns ok=false only for the one content-level exclusion the
// category match can't express: a FAILED task older than the 24h
// recency window (matches the dashboard's failed-recently card).
//
// Extracted from Inbox's per-task loop (task 4.3) so the same
// classification also backs renderInboxItemFragment's post-action
// re-render — the pinned queue and the HTMX fragment must never
// disagree about which kind a task's row is.
func newAttentionItem(t *persistence.Task, c inboxCategory, failedCutoff time.Time) (inboxItem, bool) {
	if t.Status == persistence.TaskStatusFailed && t.UpdatedAt.Before(failedCutoff) {
		return inboxItem{}, false
	}
	detail := ""
	if t.LastErrorClass != nil {
		detail = *t.LastErrorClass
	}
	item := inboxItem{
		TaskID:    t.ID,
		ProjectID: t.ProjectID,
		Kind:      c.kind,
		Title:     taskSummary(t.Payload),
		Detail:    detail,
		Age:       humanizeSince(time.Since(t.CreatedAt)) + " ago",
		Action:    c.action,
		Href:      "/ui/tasks/" + t.ID,
		rank:      c.rank,
		createdAt: t.CreatedAt,
	}
	if c.kind == inboxKindFailed {
		// Phase-3 fix-it deep link (recovery_actions.go's "Help me fix
		// this" URL convention) — surfaced inline on the attention row
		// too, not just the task detail's recovery card.
		item.FixItURL = "/ui/fixit/failed_task/" + t.ID
	}
	return item, true
}

// failedRecencyWindow is the FAILED-task lookback window shared by the
// attention query (Inbox, below) and the post-action HTMX fragment
// re-render (renderInboxItemFragment, inbox_actions.go) — task 4.4
// review nit: the cutoff duration was duplicated as the literal
// `-24*time.Hour` in both places; extracted here so there is exactly one
// place to change it. Both call sites compute their own cutoff = time.Now()
// .Add(-failedRecencyWindow) rather than sharing a precomputed time.Time,
// since they run at different moments (page render vs. post-action
// re-render) and each needs "now" evaluated at its own call time.
const failedRecencyWindow = 24 * time.Hour

// inboxAttentionPageSize bounds the single scoped attention query. Set to
// 4x the old per-category cap (200) so the combined single-query result
// preserves the same total item budget the four-query loop it replaced
// used to fetch, even though the cap is no longer per-category.
//
// Not a silent cap (review-2627): Inbox treats a full-budget result
// (len(tasks) >= inboxAttentionPageSize) as "more may exist", logs a
// warning, and sets InboxData.HasMoreAttention so the page renders an
// explicit "…and more waiting" note instead of quietly dropping rows.
const inboxAttentionPageSize = 800

// Inbox renders the unified "what needs me" queue: AWAITING_APPROVAL +
// AWAITING_INPUT (a human is blocked — ranked first) then recent FAILED and
// WAITING_FOR_CHILDREN (already terminal / waiting). Promotes the dashboard's
// count-only operator-queue cards to a drill-in list with per-item metadata
// and a direct link to where the action lives. Project-scoped.
//
// The queue is ONE scoped query (TaskFilter.Statuses over all four kinds),
// not one List call per status — the Outcome Inbox design's load-bearing
// invariant (https://docs.vornik.io §5.2). FAILED's
// 24h recency window is still applied in assembly, same as before.
//
// Task 4.2 additionally folds the same query's tasks to request-root
// cards ("Your requests", §5.3/§5.5) — see buildRequestCards.
func (s *Server) Inbox(w http.ResponseWriter, r *http.Request) {
	data := InboxData{
		Title:                  "Needs you",
		CurrentPage:            "inbox",
		AttentionPageSize:      inboxAttentionPageSize,
		RecentRequestsPageSize: inboxRecentRequestsPageSize,
	}

	if s.taskRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		byStatus := make(map[persistence.TaskStatus]inboxCategory, len(inboxCategories))
		statuses := make([]persistence.TaskStatus, 0, len(inboxCategories))
		for _, c := range inboxCategories {
			byStatus[c.status] = c
			statuses = append(statuses, c.status)
		}
		failedCutoff := time.Now().Add(-failedRecencyWindow)
		// Scope the DB query itself: a global latest-N slice lets a busy
		// instance's other-project rows bury a scoped caller's own
		// actionable items past the cap (ui-cross-project-visibility audit).
		// nil = all-access (single global query); else per-project merge.
		queryIDs := scopeQueryIDs(r)

		tasks := s.listTasksForScope(ctx, queryIDs, persistence.TaskFilter{
			Statuses: statuses,
			PageSize: inboxAttentionPageSize,
		})
		if len(tasks) >= inboxAttentionPageSize {
			// The query returned a full page: more actionable rows may
			// exist beyond the cap. Never drop this silently (review-2627)
			// — log it and let the page say so.
			data.HasMoreAttention = true
			s.logger.Warn().
				Int("page_size", inboxAttentionPageSize).
				Int("returned", len(tasks)).
				Msg("inbox attention query hit its page-size cap; some actionable rows may not be shown")
		}

		// filtered is the same in-scope, recency-windowed set Items
		// renders from — reused (not re-queried) to fold into request
		// cards so both sections agree on exactly which tasks count.
		filtered := make([]*persistence.Task, 0, len(tasks))
		for _, t := range tasks {
			if t == nil || !api.RequestAllowsProject(r, t.ProjectID) {
				continue
			}
			c, ok := byStatus[t.Status]
			if !ok {
				// Defensive: a status outside the requested Statuses set
				// should never come back, but don't render a mislabeled
				// row if it somehow does.
				continue
			}
			item, ok := newAttentionItem(t, c, failedCutoff)
			if !ok {
				// FAILED older than the 24h recency window.
				continue
			}
			filtered = append(filtered, t)
			data.Items = append(data.Items, item)
		}

		// Inline "Needs input" answer control (task 4.3, §5.6): resolve
		// each such row's open checkpoint so the fragment can render the
		// decision options / text-answer input. Per-item lookup (not
		// batched) — acceptable at inbox scale per the design's §5.6
		// note; a batch GetOpenCheckpoint variant is a future
		// optimisation if the queue ever grows large enough to matter.
		if s.taskMessageRepo != nil {
			for i := range data.Items {
				if data.Items[i].Kind != inboxKindNeedsInput {
					continue
				}
				cp, err := s.taskMessageRepo.GetOpenCheckpoint(ctx, data.Items[i].TaskID)
				if err != nil || cp == nil {
					continue
				}
				data.Items[i].CheckpointID = cp.ID
				if cp.Metadata != nil {
					data.Items[i].Checkpoint = parseCheckpointView(cp.Metadata)
				}
			}
		}

		// "Retrying…" rows (fix/retry/dismiss design 2026-07-22): tasks an
		// operator retried that are back in flight (RequeueTerminalTask stamped
		// retry_requested_at). Kept visible until the re-run terminates so a
		// requeue no longer vanishes as if resolved. Fetched separately from the
		// status-filtered attention query (they're QUEUED/LEASED/RUNNING, not one
		// of the four attention statuses) and NOT folded into request cards —
		// buildRequestCards ran on `filtered` above, which excludes them.
		if retrying, err := s.taskRepo.ListRetryInFlight(ctx, queryIDs, failedCutoff); err != nil {
			s.logger.Warn().Err(err).Msg("inbox: retry-in-flight query failed; Retrying rows suppressed")
		} else {
			for _, t := range retrying {
				if t == nil || !api.RequestAllowsProject(r, t.ProjectID) || !isRetryInFlightStatus(t.Status) {
					continue
				}
				data.Items = append(data.Items, newRetryingItem(t))
			}
		}

		data.Requests = s.buildRequestCards(ctx, filtered)

		// Broader "Your requests" list (task 4.4, design §5.7 — the gap
		// task 4.2 deferred): recent request-root cards in the viewer's
		// scope, ANY status, not just the four attention statuses above.
		// A SEPARATE scoped query (not reused from the attention query,
		// which is status-filtered by design) over a recent-activity
		// window, folded through the SAME buildRequestCards machinery so
		// a purely COMPLETED/RUNNING request — unreachable via the
		// attention query alone — now gets a card.
		recentTasks := s.listTasksForScope(ctx, queryIDs, persistence.TaskFilter{
			PageSize: inboxRecentRequestsPageSize,
		})
		if len(recentTasks) >= inboxRecentRequestsPageSize {
			data.HasMoreRecent = true
			s.logger.Warn().
				Int("page_size", inboxRecentRequestsPageSize).
				Int("returned", len(recentTasks)).
				Msg("inbox recent-requests query hit its page-size cap; some requests may not be shown")
		}
		recentFiltered := make([]*persistence.Task, 0, len(recentTasks))
		for _, t := range recentTasks {
			if t == nil || !api.RequestAllowsProject(r, t.ProjectID) {
				continue
			}
			if t.Status == persistence.TaskStatusFailed && t.UpdatedAt.Before(failedCutoff) {
				// Same 24h recency window the attention query applies
				// (newAttentionItem) — a stale FAILED task must not
				// resurface here just because it's unfiltered-by-status.
				continue
			}
			recentFiltered = append(recentFiltered, t)
		}
		recentCards := s.buildRequestCards(ctx, recentFiltered)

		// Dedup against the pinned attention section: a request already
		// rendered above (Requests) is not repeated in the broader list.
		pinned := make(map[string]bool, len(data.Requests))
		for _, c := range data.Requests {
			pinned[c.RequestID] = true
		}
		data.RecentRequests = make([]requestCard, 0, len(recentCards))
		for _, c := range recentCards {
			if pinned[c.RequestID] {
				continue
			}
			data.RecentRequests = append(data.RecentRequests, c)
		}
		// Recency order: buildRequestCards sorts by rollup rank first
		// (needs-you > working > done), which is right for the pinned
		// attention section but wrong here — the broader list's whole
		// point is "what's recently happened", so re-sort purely by the
		// card's createdAt (freshest first).
		sort.SliceStable(data.RecentRequests, func(i, j int) bool {
			return data.RecentRequests[i].createdAt.After(data.RecentRequests[j].createdAt)
		})
	}

	// Pending supervised web-write approvals (Task 6). Independent of the
	// task-queue block above (a web-write is its own actionable row, keyed by
	// submission_id, not a task status) — loaded whenever the web-write repo
	// is wired regardless of taskRepo.
	data.WebWrites = s.loadPendingWebWrites(r)

	// Rank by urgency, then oldest-first within a category.
	sort.SliceStable(data.Items, func(i, j int) bool {
		if data.Items[i].rank != data.Items[j].rank {
			return data.Items[i].rank < data.Items[j].rank
		}
		return data.Items[i].createdAt.Before(data.Items[j].createdAt)
	})
	// Count is the "needs you" badge total: actionable attention rows +
	// pending web-write approvals (both are things a human is blocked on).
	// The informational "Retrying…" rows render in the queue but are excluded
	// — nothing is blocked on them, so they must not inflate the badge.
	data.Count = countActionableItems(data.Items) + len(data.WebWrites)

	// vornik_ui_inbox_views_total{role} (design §5.8) — every inbox
	// render, regardless of whether taskRepo is wired (a view is a
	// view; an unwired repo just renders an empty-clear page).
	s.inboxMetrics.RecordView(inboxMetricsRole(r))

	s.render(w, "inbox.html", data)
}

// inboxRecentRequestsPageSize bounds the broader "Your requests" query
// (task 4.4): a recent-activity window over ANY task status, in scope,
// folded to request-root cards. Not a silent cap (same review-2627
// discipline as inboxAttentionPageSize above) — a full-budget result
// sets InboxData.HasMoreRecent and logs a warning instead of quietly
// dropping requests past the cap.
const inboxRecentRequestsPageSize = 200

// inboxMetricsRole resolves the Prometheus role label for a inbox view:
// the session role when one is set, or "none" for an unauthenticated /
// auth-disabled request (dev mode) — never an empty label value.
func inboxMetricsRole(r *http.Request) string {
	if role := api.SessionRoleFromContext(r.Context()); role != "" {
		return role
	}
	return "none"
}

// ---------------------------------------------------------------------
// Request cards (task 4.2, design §5.3/§5.5)
// ---------------------------------------------------------------------

// Request-card status buckets — the rollup precedence table's three
// outcomes (§5.3).
const (
	requestBucketNeedsYou = "needs-you"
	requestBucketWorking  = "working"
	requestBucketDone     = "done"
)

// requestCard is one "My requests" card: a request-root task folded
// together with its attention-flagged descendants.
type requestCard struct {
	RequestID   string
	ProjectID   string
	Href        string // links out to the task/panel — no inline actions here (4.3)
	Bucket      string // requestBucket* constant, for the badge color
	BucketLabel string // "Needs you" / "Working" / "Done"
	StatusLine  string // latest execution_narration line, or a fallback
	Age         string // "5m ago", request-root creation age

	// CTALabel is the state-specific call to action the card's link
	// renders ("Approve or reject", "Answer the question", "Fix or
	// retry", "Watch progress", "See what you got") — computed from the
	// rollup winner's status so the card says what is expected of the
	// USER, not a uniform "View" (2026-07-12 mobile-usability pass).
	CTALabel string

	// Subtasks is the request-root's direct-child count (the "N steps"
	// pill). 0 hides the pill (CountChildrenForParents' map-absent
	// convention — a leaf request has no pill).
	Subtasks int

	// Deliverables are the active execution's OUTPUT-class artifacts,
	// chip-rendered (name + download link).
	Deliverables []DeliverableCard

	// Origin is the batch-resolved channel badge: "created here" (no
	// chat origin), "from Telegram"/"from web chat"/etc, or "" when a
	// ChatTurnID exists but couldn't be resolved (omit rather than
	// mislabel).
	Origin string

	// fallbackLabel is the rollup winner's status-kind label ("Needs
	// approval", "Blocked on children", ...) — used as StatusLine when
	// no narration line is available and the winner isn't FAILED (which
	// gets the playbook fallback instead). Not rendered directly.
	fallbackLabel string

	rank      int       // winning child's rollup rank, for card sort order
	createdAt time.Time // tiebreak (oldest first, mirrors inboxItem)
}

// requestRollupRank is one row of the §5.3 status-rollup precedence
// table.
type requestRollupRank struct {
	rank   int
	bucket string
	label  string
}

// requestRollupRanks is the exact §5.3 precedence table. A status
// absent from this map (AWAITING_EXTERNAL, or any other TaskStatus not
// listed in the design's table) never wins a rollup and is silently
// skipped — the same "excluded, not zero-valued" convention
// CountChildrenForParents/GetByTaskIDs use elsewhere in this codebase.
var requestRollupRanks = map[persistence.TaskStatus]requestRollupRank{
	persistence.TaskStatusAwaitingApproval:   {1, requestBucketNeedsYou, "Needs approval"},
	persistence.TaskStatusAwaitingInput:      {2, requestBucketNeedsYou, "Needs input"},
	persistence.TaskStatusFailed:             {3, requestBucketNeedsYou, "Failed"},
	persistence.TaskStatusRunning:            {4, requestBucketWorking, "Running"},
	persistence.TaskStatusQueued:             {5, requestBucketWorking, "Queued"},
	persistence.TaskStatusWaitingForChildren: {6, requestBucketWorking, "Blocked on children"},
	persistence.TaskStatusCompleted:          {7, requestBucketDone, "Completed"},
	persistence.TaskStatusCancelled:          {8, requestBucketDone, "Cancelled (done)"},
}

// requestBucketLabels maps a bucket constant to its card-facing text.
var requestBucketLabels = map[string]string{
	requestBucketNeedsYou: "Needs you",
	requestBucketWorking:  "Working",
	requestBucketDone:     "Done",
}

// requestCTALabels maps a rollup winner's status to the card's call to
// action — what the USER is expected to do next (2026-07-12
// mobile-usability pass). Keyed by winner status (not bucket) because
// the three needs-you states each demand a different act. A status
// absent here (can't happen for rollup winners — the table below covers
// every requestRollupRanks key) falls back to "View" in newRequestCard.
var requestCTALabels = map[persistence.TaskStatus]string{
	persistence.TaskStatusAwaitingApproval:   "Approve or reject",
	persistence.TaskStatusAwaitingInput:      "Answer the question",
	persistence.TaskStatusFailed:             "Fix or retry",
	persistence.TaskStatusRunning:            "Watch progress",
	persistence.TaskStatusQueued:             "Watch progress",
	persistence.TaskStatusWaitingForChildren: "Watch progress",
	persistence.TaskStatusCompleted:          "See what you got",
	persistence.TaskStatusCancelled:          "See details",
}

// rollupBucketForStatuses is the §5.3 precedence table applied to a raw
// status set: the highest-ranked (numerically lowest rank) status among
// statuses wins. Ranks 1-3 → needs-you (always wins over 4-8); 4-6 →
// working; 7-8 → done only when nothing higher-ranked is present.
// AWAITING_EXTERNAL and any unlisted status are excluded — they never
// win and never block another status from winning. ok=false only when
// every status in the input is excluded (nothing to roll up).
//
// A pure function over statuses (no Task objects) so the precedence
// logic is exhaustively unit-testable independent of how the caller
// gathered the status set — see rollupWinner for the Task-aware wrapper
// request-card assembly actually uses.
func rollupBucketForStatuses(statuses []persistence.TaskStatus) (bucket string, winner persistence.TaskStatus, ok bool) {
	bestRank := -1
	for _, st := range statuses {
		rr, matched := requestRollupRanks[st]
		if !matched {
			continue
		}
		if bestRank == -1 || rr.rank < bestRank {
			bestRank = rr.rank
			bucket = rr.bucket
			winner = st
			ok = true
		}
	}
	return bucket, winner, ok
}

// rollupWinner scans a request's member tasks and returns the
// highest-precedence one (§5.3) plus its bucket — the task whose
// execution/error-class backs the card's status line and deliverable
// chips. Ties (same rank, e.g. two FAILED children) break to the most
// recently updated member. ok=false when every member's status is
// excluded from the rollup table — shouldn't occur for attention-query-
// sourced members (all four attention statuses are ranked) but guards
// a future caller passing a wider status set.
func rollupWinner(members []*persistence.Task) (winner *persistence.Task, bucket string, ok bool) {
	statuses := make([]persistence.TaskStatus, 0, len(members))
	for _, m := range members {
		if m != nil {
			statuses = append(statuses, m.Status)
		}
	}
	bucket, winnerStatus, ok := rollupBucketForStatuses(statuses)
	if !ok {
		return nil, "", false
	}
	for _, m := range members {
		if m == nil || m.Status != winnerStatus {
			continue
		}
		if winner == nil || m.UpdatedAt.After(winner.UpdatedAt) {
			winner = m
		}
	}
	return winner, bucket, true
}

// requestGroup is one request-root's fold: the root task itself plus
// the attention-query members (its own row and/or descendants) that
// resolved to it via ResolveRequestRoots.
type requestGroup struct {
	root    *persistence.Task
	members []*persistence.Task
}

// chatAuditBatchLookup is the batch capability the origin badge needs —
// GetChatAuditsByTurnIDs (4.1) resolves every card's origin in one
// round-trip. Declared locally (checked via a type assertion on
// s.chatAudit) rather than widening chatorigin.ChatAuditLookup itself:
// that package's interface deliberately stays minimal for its own
// single-task resolve chain. Every production wiring of s.chatAudit
// (WithChatAuditRepository) is backed by the full
// persistence.ChatAuditRepository, so the assertion succeeds in
// practice; a lighter fake that only implements GetByID just means
// origin badges are omitted (informational, never load-bearing).
type chatAuditBatchLookup interface {
	GetChatAuditsByTurnIDs(ctx context.Context, turnIDs []string) (map[string]persistence.ChatAuditEntry, error)
}

// buildRequestCards folds tasks (the same in-scope, recency-windowed
// set Items renders from) to one card per request-root (§5.3) and
// resolves each card's rollup status, status line, deliverable chips,
// origin badge, and subtasks pill.
//
// Batched throughout — mirrors 4.1's discipline, no per-descendant
// queries: one ResolveRequestRoots walk, one CountChildrenForParents
// call, one ExecutionRepository.GetByTaskIDs call, and one
// GetChatAuditsByTurnIDs call for the WHOLE card set. Narration lines
// and deliverable artifacts are then resolved once per DISTINCT active
// execution — bounded by the (already capped) card count, never by the
// raw descendant-task count.
func (s *Server) buildRequestCards(ctx context.Context, tasks []*persistence.Task) []requestCard {
	if s.taskRepo == nil || len(tasks) == 0 {
		return nil
	}

	roots, err := persistence.ResolveRequestRoots(ctx, s.taskRepo, tasks, 0)
	if err != nil {
		s.logger.Warn().Err(err).Msg("request-root resolution failed; My requests cards suppressed")
		return nil
	}

	groups, rootIDs := groupTasksByRequestRoot(tasks, roots)
	if len(rootIDs) == 0 {
		return nil
	}

	// Subtasks pill: one batch call for every card (§5.3).
	var childCounts map[string]int
	if counts, err := s.taskRepo.CountChildrenForParents(ctx, rootIDs); err == nil {
		childCounts = counts
	} else {
		s.logger.Debug().Err(err).Msg("CountChildrenForParents failed; request-card subtasks pill suppressed")
	}

	cards := make([]*requestCard, 0, len(rootIDs))
	representative := make(map[string]*persistence.Task, len(rootIDs)) // rootID -> winning member task
	for _, rootID := range rootIDs {
		g := groups[rootID]
		winner, bucket, ok := rollupWinner(g.members)
		if !ok {
			continue
		}
		representative[rootID] = winner
		cards = append(cards, newRequestCard(rootID, g, winner, bucket, childCounts))
	}

	rootsByID := make(map[string]*persistence.Task, len(groups))
	for id, g := range groups {
		rootsByID[id] = g.root
	}
	s.attachExecutionDerivedFields(ctx, cards, representative)
	s.attachOriginBadges(ctx, cards, rootsByID)
	finishStatusLines(cards, representative)

	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].rank != cards[j].rank {
			return cards[i].rank < cards[j].rank
		}
		return cards[i].createdAt.Before(cards[j].createdAt)
	})

	out := make([]requestCard, len(cards))
	for i, c := range cards {
		out[i] = *c
	}
	return out
}

// groupTasksByRequestRoot folds tasks to their request-root (resolved via
// ResolveRequestRoots into roots) and returns the per-root fold plus the
// roots' first-seen order (for stable pre-sort output). nil tasks and
// tasks ResolveRequestRoots couldn't resolve (shouldn't happen — it
// always has an entry for every non-nil input — but never trust that
// blindly) are skipped.
func groupTasksByRequestRoot(tasks []*persistence.Task, roots map[string]*persistence.Task) (map[string]*requestGroup, []string) {
	groups := make(map[string]*requestGroup)
	var rootIDs []string
	for _, t := range tasks {
		if t == nil {
			continue
		}
		root := roots[t.ID]
		if root == nil {
			continue
		}
		g, ok := groups[root.ID]
		if !ok {
			g = &requestGroup{root: root}
			groups[root.ID] = g
			rootIDs = append(rootIDs, root.ID)
		}
		g.members = append(g.members, t)
	}
	return groups, rootIDs
}

// newRequestCard builds one card's static (pre-execution-lookup) fields
// from its rollup winner. Origin defaults to "created here" for a
// request with no ChatTurnID — attachOriginBadges overwrites it when a
// chat origin resolves.
func newRequestCard(rootID string, g *requestGroup, winner *persistence.Task, bucket string, childCounts map[string]int) *requestCard {
	card := &requestCard{
		RequestID:     rootID,
		ProjectID:     g.root.ProjectID,
		Href:          "/ui/tasks/" + winner.ID,
		Bucket:        bucket,
		BucketLabel:   requestBucketLabels[bucket],
		fallbackLabel: requestRollupRanks[winner.Status].label,
		Age:           humanizeSince(time.Since(g.root.CreatedAt)) + " ago",
		rank:          requestRollupRanks[winner.Status].rank,
		createdAt:     winner.CreatedAt,
	}
	if cta, ok := requestCTALabels[winner.Status]; ok {
		card.CTALabel = cta
	} else {
		card.CTALabel = "View"
	}
	if childCounts != nil {
		card.Subtasks = childCounts[rootID]
	}
	if g.root.ChatTurnID == nil || *g.root.ChatTurnID == "" {
		card.Origin = "created here"
	}
	return card
}

// finishStatusLines fills in every card whose StatusLine is still empty
// after attachExecutionDerivedFields (no narration line was found):
// FAILED falls back to the playbook's human-friendly message; every
// other status falls back to its rollup kind label (§5.5).
func finishStatusLines(cards []*requestCard, representative map[string]*persistence.Task) {
	for _, card := range cards {
		if card.StatusLine != "" {
			continue
		}
		rep := representative[card.RequestID]
		switch {
		case rep != nil && rep.Status == persistence.TaskStatusFailed:
			class := ""
			if rep.LastErrorClass != nil {
				class = *rep.LastErrorClass
			}
			card.StatusLine = playbook.Lookup(class).HumanFriendly()
		case rep != nil && rep.Status == persistence.TaskStatusCompleted && len(card.Deliverables) > 0:
			// Outcome summary (2026-07-12 mobile-usability pass): a done
			// card with files must say so, not just "Completed". Runs only
			// when no narration line exists — a narrated completion already
			// reads as an outcome.
			noun := "files"
			if len(card.Deliverables) == 1 {
				noun = "file"
			}
			card.StatusLine = fmt.Sprintf("Done — %d %s ready below", len(card.Deliverables), noun)
		default:
			card.StatusLine = card.fallbackLabel
		}
	}
}

// attachExecutionDerivedFields resolves each card's active execution —
// its rollup-winning member task's most recent execution — and, from
// it, the status line (execution_narration's latest line) and
// deliverable chips (OUTPUT artifacts). One ExecutionRepository.
// GetByTaskIDs call resolves every card's execution; narration +
// artifacts are then fetched once per DISTINCT execution ID (deduped;
// bounded by the card count, never by the raw descendant-task count).
func (s *Server) attachExecutionDerivedFields(ctx context.Context, cards []*requestCard, representative map[string]*persistence.Task) {
	if s.execRepo == nil || len(representative) == 0 {
		return
	}
	repIDs := make([]string, 0, len(representative))
	for _, t := range representative {
		repIDs = append(repIDs, t.ID)
	}
	execByTask, err := s.execRepo.GetByTaskIDs(ctx, repIDs)
	if err != nil {
		s.logger.Debug().Err(err).Msg("execution lookup failed; request-card narration/deliverables suppressed")
		return
	}

	narrationCache := map[string]string{}
	deliverableCache := map[string][]DeliverableCard{}

	for _, card := range cards {
		rep := representative[card.RequestID]
		if rep == nil {
			continue
		}
		exec := execByTask[rep.ID]
		if exec == nil {
			continue
		}

		if line, cached := narrationCache[exec.ID]; cached {
			card.StatusLine = line
		} else if s.narrationRepo != nil {
			rows, err := s.narrationRepo.ListByExecution(ctx, exec.ID)
			if err != nil {
				s.logger.Debug().Err(err).Str("execution_id", exec.ID).Msg("narration lookup failed; request-card status line falls back")
			} else if len(rows) > 0 {
				line := rows[len(rows)-1].Text
				narrationCache[exec.ID] = line
				card.StatusLine = line
			}
		}

		if chips, cached := deliverableCache[exec.ID]; cached {
			card.Deliverables = chips
		} else if s.artifactRepo != nil {
			execID := exec.ID
			arts, err := s.artifactRepo.List(ctx, persistence.ArtifactFilter{ExecutionID: &execID, PageSize: 100})
			if err != nil {
				s.logger.Debug().Err(err).Str("execution_id", execID).Msg("artifact lookup failed; request-card deliverable chips suppressed")
			} else {
				chips := buildDeliverableCards(arts)
				deliverableCache[exec.ID] = chips
				card.Deliverables = chips
			}
		}
	}
}

// attachOriginBadges resolves the batch-origin channel badge for every
// card whose request-root carries a ChatTurnID: one
// GetChatAuditsByTurnIDs round-trip for the whole card set, then
// chatorigin.DecodeChatID per resolved row. Cards without a ChatTurnID
// already got "created here" set at build time; a ChatTurnID that
// fails to resolve (no audit row, undecodable chat id) is left blank —
// the badge is informational, never load-bearing (§5.5).
func (s *Server) attachOriginBadges(ctx context.Context, cards []*requestCard, roots map[string]*persistence.Task) {
	batch, ok := s.chatAudit.(chatAuditBatchLookup)
	if !ok || len(cards) == 0 {
		return
	}

	turnIDByRoot := make(map[string]string, len(cards))
	seen := make(map[string]bool, len(cards))
	turnIDs := make([]string, 0, len(cards))
	for _, card := range cards {
		root := roots[card.RequestID]
		if root == nil || root.ChatTurnID == nil || *root.ChatTurnID == "" {
			continue
		}
		turnIDByRoot[card.RequestID] = *root.ChatTurnID
		if !seen[*root.ChatTurnID] {
			seen[*root.ChatTurnID] = true
			turnIDs = append(turnIDs, *root.ChatTurnID)
		}
	}
	if len(turnIDs) == 0 {
		return
	}

	entries, err := batch.GetChatAuditsByTurnIDs(ctx, turnIDs)
	if err != nil {
		s.logger.Debug().Err(err).Msg("chat-audit batch lookup failed; request-card origin badges suppressed")
		return
	}

	for _, card := range cards {
		turnID, hasTurn := turnIDByRoot[card.RequestID]
		if !hasTurn {
			continue
		}
		entry, found := entries[turnID]
		if !found {
			continue
		}
		channel, _ := chatorigin.DecodeChatID(entry.ChatID)
		if channel == "" {
			continue
		}
		card.Origin = "from " + humanChannelName(channel)
	}
}

// humanChannelName maps a chatorigin.DecodeChatID channel name to its
// card-facing display text (§5.5: "from Telegram" / "from web chat").
// Falls back to the raw channel name for anything not yet special-
// cased (Slack, email, a future channel) rather than hiding the badge.
func humanChannelName(channel string) string {
	switch channel {
	case "telegram":
		return "Telegram"
	case "web-chat":
		return "web chat"
	case "slack":
		return "Slack"
	case "email":
		return "email"
	default:
		return channel
	}
}
