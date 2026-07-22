package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
)

// Task 4.3 — the attention queue's inline actions (design §5.6).
//
// The state-transition logic already lives in task_conversation.go
// (uiApproveTask/uiRejectTask/uiSimpleFlip, uiAnswerCheckpoint) and
// task_actions.go (retryOne/TaskRetry) — this file adds ONLY the
// HTMX partial-render path those handlers call into after mutating
// state, so an inline approve/reject/answer/retry click re-renders
// its own attention-queue row instead of navigating away.

// renderInboxItemFragment re-renders one attention-queue row for the
// HTMX partial-swap path, after an inline action has already run. It
// deliberately does its own fresh persistence.Task load (never trusts
// the pre-mutation task the caller had in hand) so the fragment always
// reflects the row's CURRENT state — the §7 idempotency contract: a
// stale double-click's fragment shows what the row looks like now, not
// what the click intended.
//
// Writes an empty (200) body when the row no longer belongs in the
// attention queue — the task wasn't found, the repo isn't wired, or
// the task's status left the four attention categories (the action
// resolved it: approved→QUEUED, rejected→CANCELLED, answered→QUEUED,
// retried→QUEUED). Combined with the row's hx-swap="outerHTML" target,
// an empty body removes the row from the DOM — "the item resolves in
// place" from the design's §5.6 description. This also covers "task
// deleted under an open card" (§7): a failed re-fetch degrades to the
// same empty-body removal, no 500.
//
// Callers are responsible for having already scope-gated taskID on
// THIS request (TaskConversationAction's uiRequireProjectScope call,
// TaskRetry's own gate) before reaching this helper — it does not
// re-derive scope from taskID alone, only from the request the caller
// already validated.
func (s *Server) renderInboxItemFragment(ctx context.Context, w http.ResponseWriter, taskID string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if s.taskRepo == nil {
		return
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return
	}
	c, ok := byStatusCategory(task.Status)
	if !ok {
		// Resolved out of the attention set — row disappears.
		return
	}
	item, ok := newAttentionItem(task, c, time.Now().Add(-failedRecencyWindow))
	if !ok {
		// FAILED aged past the 24h window between the action and this
		// re-render (practically unreachable within one request, but
		// keeps this helper's classification identical to Inbox's).
		return
	}
	if item.Kind == inboxKindNeedsInput && s.taskMessageRepo != nil {
		if cp, cpErr := s.taskMessageRepo.GetOpenCheckpoint(ctx, task.ID); cpErr == nil && cp != nil {
			item.CheckpointID = cp.ID
			if cp.Metadata != nil {
				item.Checkpoint = parseCheckpointView(cp.Metadata)
			}
		}
	}
	if err := s.templates.ExecuteTemplate(w, "inboxItemRow", item); err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("renderInboxItemFragment: render failed")
	}
}

// byStatusCategory looks up the inboxCategory a task's live status maps
// to, if any. Small linear scan over the fixed 4-entry inboxCategories
// — not worth a package-level map for 4 items, and keeps this the
// single source of truth Inbox's byStatus map is built from.
func byStatusCategory(status persistence.TaskStatus) (inboxCategory, bool) {
	for _, c := range inboxCategories {
		if c.status == status {
			return c, true
		}
	}
	return inboxCategory{}, false
}

// ---------------------------------------------------------------------
// Supervised web-write approvals (supervised-web-write-actions Task 6)
// ---------------------------------------------------------------------

// webWriteCard is one pending web-write approval rendered as a "Needs
// approval" card in /inbox. It is a read-only display projection of a
// persistence.WebWriteAction row (status='pending'): the operator sees exactly
// what the site would submit — the filled-form screenshot, the full field
// table with provenance, the target host and the submission id — and either
// approves (mints the capability token) or rejects. The raw approval token is
// NEVER on this struct; it is minted in the approve handler and delivered only
// to the owning agent run.
type webWriteCard struct {
	SubmissionID string
	ProjectID    string
	TargetHost   string
	TargetURL    string
	Age          string
	// ScreenshotRef is the artifact ref for the preview (filled-form)
	// screenshot. See screenshotHref for how the template resolves it.
	ScreenshotRef string
	// Fields is the read-only enumerated field table (name + value +
	// provenance incl. volatile), decoded from field_table_json. Empty when
	// the row carries no (or unparseable) field table — the card then renders
	// the target/host summary alone.
	Fields    []webWriteFieldRow
	createdAt time.Time
}

// webWriteFieldRow mirrors the scraper's FieldRow (services/scraper/src/
// submit.ts) shape carried in field_table_json — the operator-facing per-field
// record. Provenance is one of agent-bound|page-default|hidden|volatile.
type webWriteFieldRow struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Value      string `json:"value"`
	Provenance string `json:"provenance"`
	Bound      bool   `json:"bound"`
}

// ScreenshotHref returns the operator-facing URL for the filled-form preview
// screenshot, or "" when there is no screenshot to show.
//
// TODO(web-write Task 8-10 screenshot wiring): ScreenshotRef is produced by the
// scraper preview (Task 8) and persisted to the artifact store; its exact
// artifact addressing (bare artifact id vs a store key) is not yet finalised in
// the daemon↔scraper client (Task 10). Until that lands the template renders a
// clearly-marked placeholder instead of an <img>; when the ref is a plain
// artifact id this hook already resolves it to the existing /ui/artifacts/
// download route. The field table + target host below are the load-bearing
// approval evidence and are always rendered.
func (c webWriteCard) ScreenshotHref() string {
	ref := strings.TrimSpace(c.ScreenshotRef)
	if ref == "" {
		return ""
	}
	return "/ui/artifacts/" + url.PathEscape(ref)
}

// loadPendingWebWrites lists the pending web-write approvals in the request's
// scope and folds them to display cards. Nil-safe on every seam: no repo, a
// repo without the query capability, a query error, or a scoped-out row all
// degrade to fewer (or zero) cards rather than failing the inbox render.
func (s *Server) loadPendingWebWrites(r *http.Request) []webWriteCard {
	if s.webWriteRepo == nil {
		return nil
	}
	lister, ok := s.webWriteRepo.(persistence.WebWritePendingLister)
	if !ok {
		// A write/CAS-only repo without the read query — nothing to list.
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := lister.ListPendingByProject(ctx, scopeQueryIDs(r))
	if err != nil {
		s.logger.Warn().Err(err).Msg("inbox: pending web-write list failed; approval cards suppressed")
		return nil
	}
	cards := make([]webWriteCard, 0, len(rows))
	for _, row := range rows {
		if row == nil || !api.RequestAllowsProject(r, row.ProjectID) {
			continue
		}
		cards = append(cards, newWebWriteCard(row))
	}
	// Oldest-first, mirroring the attention queue's within-category order — a
	// write that has waited longest for a decision surfaces first.
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].createdAt.Before(cards[j].createdAt)
	})
	return cards
}

// newWebWriteCard builds a display card from a pending row. The field table is
// best-effort decoded: a malformed field_table_json yields a card with no field
// rows (the target/host summary still renders) rather than dropping the card.
func newWebWriteCard(row *persistence.WebWriteAction) webWriteCard {
	card := webWriteCard{
		SubmissionID:  row.SubmissionID,
		ProjectID:     row.ProjectID,
		TargetHost:    row.TargetHost,
		TargetURL:     row.TargetURL,
		ScreenshotRef: row.ScreenshotRef,
		Age:           humanizeSince(time.Since(row.CreatedAt)) + " ago",
		createdAt:     row.CreatedAt,
	}
	if len(row.FieldTableJSON) > 0 {
		var fields []webWriteFieldRow
		if err := json.Unmarshal(row.FieldTableJSON, &fields); err == nil {
			card.Fields = fields
		}
	}
	return card
}

// webWriteInboxRouter dispatches the POST-only approve/reject actions on a
// pending web-write. Path (after the /ui prefix is stripped upstream):
// /inbox/web-write/{submission_id}/{approve|reject}.
func (s *Server) webWriteInboxRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ui")
	rest = strings.TrimPrefix(rest, "/inbox/web-write/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	submissionID, action := parts[0], parts[1]
	switch action {
	case "approve":
		s.WebWriteApprove(w, r, submissionID)
	case "reject":
		s.WebWriteReject(w, r, submissionID)
	default:
		http.NotFound(w, r)
	}
}

// WebWriteApprove is the authenticated, CSRF-protected POST that approves a
// pending web-write: it mints a fresh one-time capability token, stores its
// row-bound hash via WebWriteRepo.Approve, and delivers the RAW token to the
// owning agent run (the Task-10 resume seam). The token is never rendered in
// the UI — the operator only sees a confirmation. GET (or any non-POST) and a
// request lacking a same-origin signal are rejected before any mutation, so the
// approval can never be triggered by a deep link or a cross-site request.
func (s *Server) WebWriteApprove(w http.ResponseWriter, r *http.Request, submissionID string) {
	if s.webWriteRepo == nil {
		http.Error(w, "web-write approvals not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		// A minted capability MUST NOT be reachable via a GET deep link.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !inboxRequestSameOrigin(r) {
		http.Error(w, "CSRF: cross-site or unverifiable origin", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, err := s.webWriteRepo.Get(ctx, submissionID)
	if err != nil || row == nil {
		http.NotFound(w, r)
		return
	}
	// Multi-tenant scope gate: a project-scoped caller must not approve
	// another tenant's web-write (mirrors TaskConversationAction).
	if !s.uiRequireProjectScope(w, r, row.ProjectID) {
		return
	}

	// Mint a fresh random capability token and bind it to the whole persisted
	// row via the shared dispatcher hash — submit re-derives the same hash from
	// the presented token + the current row and requires an exact match.
	token, err := newWebWriteApprovalToken()
	if err != nil {
		s.logger.Error().Err(err).Str("submission_id", submissionID).Msg("web-write approve: token mint failed")
		http.Error(w, "token generation failed", http.StatusInternalServerError)
		return
	}
	tokenHash := dispatcher.WebWriteApprovalTokenHash(token, row)
	approver := s.operatorIDForRequest(r)

	if err := s.webWriteRepo.Approve(ctx, submissionID, tokenHash, approver); err != nil {
		if errors.Is(err, persistence.ErrNoTransition) {
			// Already decided (raced with another approve/reject, or already
			// approved) — no second token is minted.
			s.redirectInbox(w, r, "web-write-already-decided")
			return
		}
		s.logger.Error().Err(err).Str("submission_id", submissionID).Msg("web-write approve failed")
		http.Error(w, "approve failed", http.StatusInternalServerError)
		return
	}

	// Deliver the RAW token as a capability to the owning agent run. This is
	// the ONLY place the raw token leaves this handler — never to the UI.
	if s.webWriteApprovalDeliver != nil {
		if derr := s.webWriteApprovalDeliver(submissionID, row.AgentRunID, token); derr != nil {
			// The approval is persisted; a failed delivery can be retried via
			// the resume path, so this is logged, not rolled back.
			s.logger.Warn().Err(derr).
				Str("submission_id", submissionID).
				Str("agent_run_id", row.AgentRunID).
				Msg("web-write approve: capability delivery failed (approval persisted; deliverable via resume)")
		}
	} else {
		s.logger.Warn().
			Str("submission_id", submissionID).
			Str("agent_run_id", row.AgentRunID).
			Msg("TODO(web-write Task 10): no approval-delivery hook wired; approval persisted but capability token not routed to the agent run")
	}

	s.redirectInbox(w, r, "web-write-approved")
}

// WebWriteReject is the authenticated, CSRF-protected POST that declines a
// pending web-write (WebWriteRepo.Reject). Same POST-only + same-origin gates
// as approve; no token is ever minted.
func (s *Server) WebWriteReject(w http.ResponseWriter, r *http.Request, submissionID string) {
	if s.webWriteRepo == nil {
		http.Error(w, "web-write approvals not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !inboxRequestSameOrigin(r) {
		http.Error(w, "CSRF: cross-site or unverifiable origin", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, err := s.webWriteRepo.Get(ctx, submissionID)
	if err != nil || row == nil {
		http.NotFound(w, r)
		return
	}
	if !s.uiRequireProjectScope(w, r, row.ProjectID) {
		return
	}

	approver := s.operatorIDForRequest(r)
	if err := s.webWriteRepo.Reject(ctx, submissionID, approver); err != nil {
		if errors.Is(err, persistence.ErrNoTransition) {
			s.redirectInbox(w, r, "web-write-already-decided")
			return
		}
		s.logger.Error().Err(err).Str("submission_id", submissionID).Msg("web-write reject failed")
		http.Error(w, "reject failed", http.StatusInternalServerError)
		return
	}
	s.redirectInbox(w, r, "web-write-rejected")
}

// redirectInbox sends the operator back to /ui/inbox with a notice. HTMX
// callers (the inline card forms) get an empty 200 so the swapped-out card is
// simply removed from the DOM — matching the attention-row fragment convention.
func (s *Server) redirectInbox(w http.ResponseWriter, r *http.Request, notice string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	target := "/ui/inbox"
	if notice != "" {
		target += "?notice=" + notice
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// newWebWriteApprovalToken mints a fresh 256-bit random capability token, hex
// encoded. This is the raw secret bound (via WebWriteApprovalTokenHash) to the
// approved row and delivered to the owning agent run.
func newWebWriteApprovalToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("web-write token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// inboxRequestSameOrigin mirrors the daemon AuthMiddleware's CSRF same-origin
// decision (internal/api isCSRFSafe) applied defensively in-handler for this
// higher-stakes write (approving a web-write mints a capability). This codebase
// has no per-form CSRF token — mutating cookie/Basic requests are gated by the
// Sec-Fetch-Site / Origin same-origin signal — so this re-applies that exact
// ladder here rather than inventing a token the rest of the UI doesn't use.
// Fails closed when no trustworthy same-origin signal is present.
func inboxRequestSameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "same-site", "none":
		return true
	case "cross-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Sec-Fetch-Site and no Origin on a mutating request — no
		// same-origin signal to trust. Fail closed.
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Host == r.Host
}
