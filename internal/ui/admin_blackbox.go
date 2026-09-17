package ui

// /ui/admin/blackbox — Autonomy Black Box trace surface (Phase A).
// Two views:
//   - GET /ui/admin/blackbox            → task_id search box +
//     "trace service not configured" notice when unwired
//   - GET /ui/admin/blackbox/<task_id>  → assembled trace
//                                         (chronological event log)
//
// Same admin gate as the rest of /ui/admin/* — the adminRouter
// wrapper already enforces it.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
)

// blackboxTriggerDetailURL composes the detail page URL with an
// optional action_error query param. Pulled out so the POST
// handlers and the test fixtures stay in sync.
func blackboxTriggerDetailURL(id, actionErr string) string {
	base := "/ui/admin/blackbox/triggers/" + url.PathEscape(id)
	if actionErr == "" {
		return base
	}
	if len(actionErr) > 200 {
		actionErr = actionErr[:200]
	}
	q := url.Values{}
	q.Set("action_error", actionErr)
	return base + "?" + q.Encode()
}

// BlackBoxIndexData backs /ui/admin/blackbox (the landing page).
// Pre-fills the search box from ?task_id= so an operator coming
// in via a deep-link sees the field populated.
type BlackBoxIndexData struct {
	adminCommonData
	Available     bool
	PrefilledTask string
	Error         string
	// Phase B — recent workflow-healing triggers. Nil when the
	// repo isn't wired or when the list call errored; the
	// template renders an empty state in both cases.
	Triggers          []HealingTriggerRow
	TriggersAvailable bool
	// TriggersActionError surfaces a bulk-dismiss outcome
	// summary as a banner. Populated from ?action_error= on
	// the redirect back from the bulk handler.
	TriggersActionError string
}

// HealingTriggerRow is the pre-formatted shape the index
// template renders for each row.
type HealingTriggerRow struct {
	ID              string
	ProjectID       string
	WorkflowID      string
	Class           string
	Status          string
	MetricName      string
	BaselineValue   float64
	ComparisonValue float64
	ThresholdValue  float64
	DeltaPct        float64 // pre-computed relative delta as %
	CreatedAt       string
	IsOpen          bool
}

// BlackBoxTriggerDetailData backs /ui/admin/blackbox/triggers/{id}.
// Carries the full trigger row (so the template can render baseline /
// comparison windows, evidence IDs, and any prior proposal link) plus
// the action-availability flags the form chooses on.
type BlackBoxTriggerDetailData struct {
	adminCommonData
	Available bool // true when healingTriggerRepo is wired
	Trigger   HealingTriggerRow
	// Evidence is the list of execution IDs the detector
	// captured. Each ID links through to /ui/executions/{id}.
	Evidence []string
	// BaselineWindow / ComparisonWindow are the pre-formatted
	// time ranges the template renders without arithmetic.
	BaselineWindow   string
	ComparisonWindow string
	// ProposalID is populated when status=generated_candidate;
	// the template links it through to /ui/admin/workflow-proposals.
	ProposalID string
	// ResolvedAt is the formatted resolution timestamp for the
	// metadata block. Empty when still open.
	ResolvedAt string
	// CanDismiss is true when the trigger is open AND the repo
	// is wired. The template gates the Dismiss form on this.
	CanDismiss bool
	// CanGenerate is true when the trigger is open AND both repo
	// + architect are wired. Hides the Generate-candidate button
	// when the architect isn't available on this deployment.
	CanGenerate bool
	// HasArchitect reports whether the architect wiring is
	// present; the template uses this to render a "wired but
	// trigger already terminal" hint vs "architect not configured".
	HasArchitect bool
	// ActionError surfaces the latest dismiss / generate-candidate
	// failure as an inline banner — pattern lifted from the
	// workflow-proposals detail page's DecideError round-trip.
	ActionError string
	// History is the list of past terminal triggers for the same
	// (project, workflow), newest first, EXCLUDING the current row.
	// Drives the "has this regressed before?" triage panel below
	// the evidence block.
	History []HealingTriggerRow
}

// BlackBoxTraceData backs /ui/admin/blackbox/{task_id}. The
// Trace is an opaque value from the EE adapter (*blackbox.Trace
// from the enterprise side); the template reads through it
// directly for the chronological event list.
type BlackBoxTraceData struct {
	adminCommonData
	Available bool
	TaskID    string
	Trace     any
	NotFound  bool
	Error     string
}

// AdminBlackBox renders the index page (search box + service-
// availability notice). The adminRouter calls this for /blackbox
// and /blackbox/.
func (s *Server) AdminBlackBox(w http.ResponseWriter, r *http.Request) {
	data := BlackBoxIndexData{
		adminCommonData: adminCommonData{
			Title:       "Black Box",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:           s.blackboxService != nil,
		PrefilledTask:       strings.TrimSpace(r.URL.Query().Get("task_id")),
		TriggersAvailable:   s.healingTriggerRepo != nil,
		TriggersActionError: strings.TrimSpace(r.URL.Query().Get("action_error")),
	}
	if s.healingTriggerRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		rows, err := s.healingTriggerRepo.List(ctx, persistence.HealingTriggerListFilter{PageSize: 50})
		if err == nil {
			for _, t := range rows {
				data.Triggers = append(data.Triggers, healingTriggerToRow(t))
			}
		}
	}
	s.render(w, "admin_blackbox.html", data)
}

// healingTriggerToRow formats a persistence row for the template.
// Pre-computes the relative-delta percentage so the template
// stays arithmetic-free.
func healingTriggerToRow(t *persistence.HealingTrigger) HealingTriggerRow {
	delta := 0.0
	if t.BaselineValue > 0 {
		delta = 100.0 * (t.ComparisonValue - t.BaselineValue) / t.BaselineValue
	}
	return HealingTriggerRow{
		ID:              t.ID,
		ProjectID:       t.ProjectID,
		WorkflowID:      t.WorkflowID,
		Class:           string(t.TriggerClass),
		Status:          string(t.Status),
		MetricName:      t.MetricName,
		BaselineValue:   t.BaselineValue,
		ComparisonValue: t.ComparisonValue,
		ThresholdValue:  t.ThresholdValue,
		DeltaPct:        delta,
		CreatedAt:       t.CreatedAt.Local().Format("2006-01-02 15:04 MST"),
		IsOpen:          t.Status == persistence.HealingTriggerStatusOpen,
	}
}

// AdminBlackBoxTrace renders the trace view for one task. The
// adminRouter dispatches /blackbox/{task_id} here.
func (s *Server) AdminBlackBoxTrace(w http.ResponseWriter, r *http.Request, taskID string) {
	data := BlackBoxTraceData{
		adminCommonData: adminCommonData{
			Title:       "Black Box · " + taskID,
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.blackboxService != nil,
		TaskID:    taskID,
	}
	if !data.Available {
		s.render(w, "admin_blackbox_trace.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tr, _, err := s.blackboxService.AssembleCached(ctx, taskID)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			data.NotFound = true
		} else {
			data.Error = err.Error()
		}
		s.render(w, "admin_blackbox_trace.html", data)
		return
	}
	data.Trace = tr
	s.render(w, "admin_blackbox_trace.html", data)
}

// BlackBoxCompareData backs /ui/admin/blackbox/compare/{a}/{b}.
// Renders the two traces side-by-side + the Scorecard's findings.
// Phase C of the Autonomy Black Box arc. Trace and Score fields
// are opaque values from the EE adapter (*blackbox.Trace /
// *blackbox.Scorecard on the enterprise side); the template reads
// through them directly.
type BlackBoxCompareData struct {
	adminCommonData
	Available bool
	TaskA     string
	TaskB     string
	NotFound  string // populated with the missing ID when one trace can't be loaded
	Error     string
	TraceA    any
	TraceB    any
	Score     any
}

// AdminBlackBoxCompare renders the side-by-side compare view for
// two traces (typically: an original + its counterfactual replay).
// The adminRouter dispatches /blackbox/compare/{a}/{b} here.
//
// Caveat: this view was added 2026-05-29 along with the Phase C
// backend. Browser-verification deferred to the next session
// where the operator can pick a real counterfactual pair.
func (s *Server) AdminBlackBoxCompare(w http.ResponseWriter, r *http.Request, taskA, taskB string) {
	data := BlackBoxCompareData{
		adminCommonData: adminCommonData{
			Title:       "Black Box Compare · " + taskA + " ⇄ " + taskB,
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.blackboxService != nil,
		TaskA:     taskA,
		TaskB:     taskB,
	}
	if !data.Available {
		s.render(w, "admin_blackbox_compare.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	trA, _, err := s.blackboxService.AssembleCached(ctx, taskA)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			data.NotFound = taskA
		} else {
			data.Error = "trace A: " + err.Error()
		}
		s.render(w, "admin_blackbox_compare.html", data)
		return
	}
	trB, _, err := s.blackboxService.AssembleCached(ctx, taskB)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			data.NotFound = taskB
		} else {
			data.Error = "trace B: " + err.Error()
		}
		s.render(w, "admin_blackbox_compare.html", data)
		return
	}
	score, _ := s.blackboxService.Compare(trA, trB)
	data.TraceA = trA
	data.TraceB = trB
	data.Score = score
	s.render(w, "admin_blackbox_compare.html", data)
}

// AdminBlackBoxTriggerDetail renders /ui/admin/blackbox/triggers/{id}.
// Full metadata + evidence + Dismiss / Generate-candidate forms when
// the row is still open. 404s when the trigger ID doesn't exist so
// the URL bar doesn't get cached as "this id exists" when it doesn't.
func (s *Server) AdminBlackBoxTriggerDetail(w http.ResponseWriter, r *http.Request, id string) {
	data := BlackBoxTriggerDetailData{
		adminCommonData: adminCommonData{
			Title:       "Workflow-healing trigger",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:    s.healingTriggerRepo != nil,
		HasArchitect: s.healingGenerator != nil,
	}
	if !data.Available {
		s.render(w, "admin_blackbox_trigger.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	t, err := s.healingTriggerRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("trigger_id", id).Msg("trigger detail: get failed")
		http.Error(w, "trigger lookup failed", http.StatusInternalServerError)
		return
	}
	data.Trigger = healingTriggerToRow(t)
	data.Evidence = t.EvidenceExecutionIDs
	data.BaselineWindow = t.BaselineStart.Local().Format("2006-01-02 15:04") +
		" → " + t.BaselineEnd.Local().Format("2006-01-02 15:04 MST")
	data.ComparisonWindow = t.ComparisonStart.Local().Format("2006-01-02 15:04") +
		" → " + t.ComparisonEnd.Local().Format("2006-01-02 15:04 MST")
	data.ProposalID = t.ProposalID
	if t.ResolvedAt != nil {
		data.ResolvedAt = t.ResolvedAt.Local().Format("2006-01-02 15:04 MST")
	}
	data.CanDismiss = t.Status == persistence.HealingTriggerStatusOpen
	data.CanGenerate = data.CanDismiss && data.HasArchitect
	if msg := strings.TrimSpace(r.URL.Query().Get("action_error")); msg != "" {
		data.ActionError = msg
	}
	// Firing history — past triggers for the same (project, workflow),
	// EXCLUDING the current row. List returns newest-first so the
	// panel reads as a regression timeline. A list-call failure
	// degrades gracefully: the panel renders empty rather than 500.
	historyRows, herr := s.healingTriggerRepo.List(ctx, persistence.HealingTriggerListFilter{
		ProjectID:  t.ProjectID,
		WorkflowID: t.WorkflowID,
		PageSize:   20,
	})
	if herr != nil {
		s.logger.Warn().Err(herr).Str("trigger_id", id).Msg("trigger detail: history list failed")
	} else {
		for _, h := range historyRows {
			if h.ID == t.ID {
				continue
			}
			data.History = append(data.History, healingTriggerToRow(h))
		}
	}
	s.render(w, "admin_blackbox_trigger.html", data)
}

// AdminBlackBoxTriggerDismiss handles
// POST /ui/admin/blackbox/triggers/{id}/dismiss. Flips the row to
// dismissed + stamps the admin audit log, then 303-redirects back
// to the detail page. Failures round-trip via ?action_error= the
// same way the workflow-proposals decide form does.
func (s *Server) AdminBlackBoxTriggerDismiss(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingTriggerRepo == nil {
		http.Error(w, "healing-trigger repo not wired", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	principal := adminPrincipal(r)
	err := s.healingTriggerRepo.Dismiss(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			// Either missing OR already terminal — repo collapses
			// both into ErrNotFound. Send the operator back with
			// a friendly message rather than a bare 404 page.
			http.Redirect(w, r,
				blackboxTriggerDetailURL(id, "trigger already resolved or not found"),
				http.StatusSeeOther)
			return
		}
		s.logger.Warn().Err(err).Str("trigger_id", id).Msg("trigger dismiss failed")
		http.Redirect(w, r, blackboxTriggerDetailURL(id, err.Error()), http.StatusSeeOther)
		return
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: principal,
			Source:    "ui",
			Action:    "blackbox-trigger.dismissed",
			Target:    id,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	if cpHubReturnRequested(r) {
		http.Redirect(w, r, cpHubOverviewURL("trigger-dismissed", ""), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, blackboxTriggerDetailURL(id, ""), http.StatusSeeOther)
}

// AdminBlackBoxTriggersBulkDismiss handles
// POST /ui/admin/blackbox/triggers/bulk-dismiss. Reads a form-encoded
// `ids` field (comma-separated OR repeated `ids` values), Dismisses
// each in order, writes one admin-audit row per success, and
// redirects to the index page with a summary in the action_error
// param (used here as a status banner — same banner element either
// way). Best-effort: per-ID failures are aggregated rather than
// aborting the batch, so the operator sees what worked vs what didn't.
func (s *Server) AdminBlackBoxTriggersBulkDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingTriggerRepo == nil {
		http.Error(w, "healing-trigger repo not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Accept both shapes: repeated `ids` checkboxes from the table
	// form AND a single comma-separated `ids` value (for curl /
	// scripted use).
	ids := uniqueNonEmpty(append(r.Form["ids"], strings.Split(r.FormValue("ids_csv"), ",")...))
	if len(ids) == 0 {
		http.Redirect(w, r, "/ui/admin/blackbox?action_error=no+triggers+selected", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	principal := adminPrincipal(r)
	var dismissed, failed int
	var firstErr string
	for _, id := range ids {
		err := s.healingTriggerRepo.Dismiss(ctx, id)
		if err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		dismissed++
		if s.adminAuditRepo != nil {
			_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
				Timestamp: time.Now().UTC(),
				Principal: principal,
				Source:    "ui",
				Action:    "blackbox-trigger.dismissed",
				Target:    id,
				Before:    "bulk",
				IP:        clientIP(r),
				UserAgent: r.UserAgent(),
			})
		}
	}
	summary := "/ui/admin/blackbox"
	if failed > 0 {
		msg := "dismissed " + strconv.Itoa(dismissed) + " of " + strconv.Itoa(len(ids)) +
			"; first error: " + firstErr
		if len(msg) > 200 {
			msg = msg[:200]
		}
		q := url.Values{}
		q.Set("action_error", msg)
		summary = "/ui/admin/blackbox?" + q.Encode()
	}
	http.Redirect(w, r, summary, http.StatusSeeOther)
}

// uniqueNonEmpty trims + dedupes a slice of strings, dropping
// blanks. Order-preserving so the audit log reads in the same
// order the operator selected.
func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// HealingCandidateGeneratorUI is the narrow seam over the ONE healing
// candidate producer (api.Server.GenerateHealingCandidateForTrigger). The UI
// used to carry its own copy of that orchestration, which is why the
// 2026-09-16 cutover repointed the API at the config assistant and left this
// page wired to an architect that no longer existed.
//
// Generate takes a trigger id and NOTHING ELSE. Everything the producer needs
// — project, workflow, evidence set, and the intent rendered from the metric
// and its thresholds — comes off the trigger row. This is the operator's
// requirement stated as a type: a candidate is one button, and there is no
// prompt field to type into.
type HealingCandidateGeneratorUI interface {
	// Generate returns the filed proposal's id, and whether a deterministic
	// recipe produced it rather than the assistant.
	Generate(ctx context.Context, triggerID string) (proposalID string, byRecipe bool, err error)
}

// Sentinels the service adapter maps the producer's errors onto, so this
// package renders outcomes without importing api.
var (
	// ErrUITriggerNotFound — no such trigger.
	ErrUITriggerNotFound = errors.New("no such healing trigger")
	// ErrUITriggerNotOpen — already dismissed or already generated.
	ErrUITriggerNotOpen = errors.New("only open triggers can generate candidates")
	// ErrUIProducerUnavailable — nothing wired to produce a candidate.
	ErrUIProducerUnavailable = errors.New("no healing candidate producer is wired on this deployment")
)

// TriggerStampError carries the proposal id through the one partial failure
// worth distinguishing: the proposal exists, the trigger stamp did not land.
type TriggerStampError struct {
	ProposalID string
	Err        error
}

func (e *TriggerStampError) Error() string { return e.Err.Error() }
func (e *TriggerStampError) Unwrap() error { return e.Err }

// AdminBlackBoxTriggerGenerateCandidate handles
// POST /ui/admin/blackbox/triggers/{id}/generate-candidate.
//
// A PRESENTATION of the shared producer: it decides how to render an outcome
// and nothing about what the outcome is. The trigger load, the open-status
// check, the recipe-then-assistant order, the trigger stamp and the candidate
// row all live in the producer, so this surface and the admin API cannot drift
// again.
func (s *Server) AdminBlackBoxTriggerGenerateCandidate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingGenerator == nil {
		http.Error(w, "no healing candidate producer wired", http.StatusServiceUnavailable)
		return
	}
	// Generous timeout — the assistant makes one synchronous model call.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	principal := adminPrincipal(r)

	proposalID, byRecipe, err := s.healingGenerator.Generate(ctx, id)
	if err != nil {
		s.renderGenerateCandidateError(w, r, id, err)
		return
	}

	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: principal,
			Source:    "ui",
			Action:    "blackbox-trigger.generated_candidate",
			Target:    id,
			After:     proposalID,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	s.logger.Info().Str("trigger_id", id).Str("proposal_id", proposalID).
		Bool("by_recipe", byRecipe).Msg("healing candidate generated from the UI")

	if cpHubReturnRequested(r) {
		// Folded-hub flow: the generated proposal is now visible in the hub
		// Proposals inbox, so keep the operator there rather than bouncing
		// to the standalone workflow-proposal page.
		http.Redirect(w, r, cpHubProposalsURL("candidate-generated", ""), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/admin/workflow-proposals/"+url.PathEscape(proposalID), http.StatusSeeOther)
}

// renderGenerateCandidateError puts the operator where their next action is.
// The distinctions matter: "nothing is wired", "this trigger is already
// resolved" and "the producer ran and declined" need different responses, and
// a stamp failure must still point at the proposal that DOES exist.
func (s *Server) renderGenerateCandidateError(w http.ResponseWriter, r *http.Request, id string, err error) {
	var stamp *TriggerStampError
	switch {
	case errors.Is(err, ErrUITriggerNotFound):
		http.NotFound(w, r)
	case errors.Is(err, ErrUITriggerNotOpen):
		http.Redirect(w, r, blackboxTriggerDetailURL(id,
			"trigger already resolved; only open triggers can generate candidates"), http.StatusSeeOther)
	case errors.As(err, &stamp):
		// The proposal exists — send the operator to it rather than losing
		// the producer's work to a bookkeeping failure.
		q := url.Values{}
		q.Set("decide_error", "trigger stamp failed: "+stamp.Error())
		http.Redirect(w, r, "/ui/admin/workflow-proposals/"+url.PathEscape(stamp.ProposalID)+"?"+q.Encode(),
			http.StatusSeeOther)
	case errors.Is(err, ErrUIProducerUnavailable):
		http.Redirect(w, r, blackboxTriggerDetailURL(id,
			"no healing candidate producer is wired on this deployment"), http.StatusSeeOther)
	default:
		s.logger.Warn().Err(err).Str("trigger_id", id).Msg("trigger generate-candidate failed")
		http.Redirect(w, r, blackboxTriggerDetailURL(id, err.Error()), http.StatusSeeOther)
	}
}
