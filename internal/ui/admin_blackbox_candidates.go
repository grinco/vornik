package ui

// /ui/admin/blackbox/candidates — Self-Healing Workflow Genome v1
// operator surface. Extends the existing /ui/admin/blackbox triggers
// panel (admin_blackbox.go) with the candidate → trial → promotion
// funnel:
//
//   GET  /ui/admin/blackbox/candidates             → list page
//   GET  /ui/admin/blackbox/candidates/{id}         → detail:
//        candidate diff + motivation + expected effect + risk banner,
//        evidence run links, the trial SCORECARD rendered
//        baseline-vs-candidate with replay-mode limitations shown,
//        and promote/reject forms.
//   POST /ui/admin/blackbox/candidates/{id}/run-trial → operator-
//        triggered trial (static | replay). NOT a background loop
//        (LLD non-negotiable #5).
//   POST /ui/admin/blackbox/candidates/{id}/promote   → manual
//        promotion. Server-side gated: the promoter REFUSES anything
//        not in trial_passed (LLD non-negotiable #1; nothing
//        auto-promotes).
//   POST /ui/admin/blackbox/candidates/{id}/reject    → flip to
//        rejected without touching production.
//
// The copy is biased toward "why should I trust this repair?" — every
// candidate leads with its evidence, its expected effect, and a trial
// scorecard whose replay-fidelity caveats are shown inline, NOT toward
// "an AI found a patch". Same admin gate + audit + nil-safe pattern as
// the rest of /ui/admin/* (the adminRouter wrapper enforces the gate).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// HealingTrialRunnerUI is the narrow seam the run-trial button drives.
// Production wires it to the same adapter the API uses over
// *workflowhealing.TrialRunner; tests supply a fake. Kept in the ui
// package so ui doesn't import internal/workflowhealing.
type HealingTrialRunnerUI interface {
	// RunTrial runs a trial of the candidate in the given mode
	// (static | replay) against the evidence set and persists the
	// result. The ui handler only needs the error (it re-reads the
	// candidate + trials for the redirect target), so the concrete
	// verdict is not part of this seam.
	RunTrial(ctx context.Context, candidateID, mode string, evidenceIDs []string) error
	// RunTrialAsync opens the trial and returns immediately; the
	// evaluation finishes in a detached goroutine and lands on the
	// candidate's trial history. Used for replay trials, whose real
	// replays run minutes past any request window.
	RunTrialAsync(ctx context.Context, candidateID, mode string, evidenceIDs []string) error
}

// HealingCandidatePromoterUI is the narrow seam the promote/reject
// buttons drive. Promotion runs the gate + the memetic apply path and
// REFUSES anything not in trial_passed.
type HealingCandidatePromoterUI interface {
	Promote(ctx context.Context, candidateID, promotedBy string) (*persistence.HealingCandidate, error)
	Reject(ctx context.Context, candidateID string) (*persistence.HealingCandidate, error)
}

// Sentinel errors the ui seams surface so the handler can map a refusal
// to a friendly banner instead of a 500. The service adapters translate
// the workflowhealing sentinels into these (same set as the api
// package). errors.Is matching keeps the mapping decoupled from the
// concrete runner/promoter.
var (
	ErrUICandidateNotFound      = errors.New("healing candidate not found")
	ErrUICandidateTerminal      = errors.New("healing candidate is terminal")
	ErrUICandidateNotPromotable = errors.New("healing candidate is not promotable (requires trial_passed)")
	ErrUITrialMode              = errors.New("unsupported trial mode")
	ErrUITrialRunning           = errors.New("a trial is already running for this candidate")
)

// CandidateRow is the pre-formatted shape the list template renders.
// Display strings are pre-computed so the template stays arithmetic-
// and decode-free.
type CandidateRow struct {
	ID             string
	ProjectID      string
	WorkflowID     string
	TriggerID      string
	ProposalID     string
	Class          string
	RiskLevel      string
	Status         string
	StatusBadge    string // tailwind ring/bg class set for the status pill
	RiskBadge      string // tailwind ring/bg class set for the risk pill
	MotivationLine string // first line of the motivation, truncated
	CreatedAt      string
	IsTrialPassed  bool
	IsTerminal     bool
}

// TrialRow is the pre-formatted shape the detail template renders for
// one trial in the candidate's history.
type TrialRow struct {
	ID                 string
	Mode               string
	Verdict            string
	VerdictBadge       string
	StartedAt          string
	FinishedAt         string
	EvidenceIDs        []string
	HasScorecard       bool
	Inconclusive       bool
	InconclusiveReason string
	// ScoreLines are the pre-formatted baseline-vs-candidate deltas the
	// scorecard table renders. Empty when the trial has no scorecard.
	ScoreLines []ScoreLine
	// Reasons are the scorecard's human-readable verdict reasons.
	Reasons []string
	// Baseline / Candidate are the per-arm summary rows (runs, success
	// rate, cost, etc.) shown side by side.
	Baseline  *TrialSummaryView
	Candidate *TrialSummaryView
}

// ScoreLine is one row of the baseline-vs-candidate scorecard table:
// a labelled metric with its signed delta and a direction hint.
type ScoreLine struct {
	Label    string
	Value    string // pre-formatted delta, e.g. "+12.5%" or "-0.20"
	Improved bool   // true when the delta is in the desired direction
	Worsened bool   // true when the delta is in the undesired direction
}

// TrialSummaryView is the pre-formatted per-arm (baseline OR candidate)
// summary the detail page renders.
type TrialSummaryView struct {
	Runs                string
	SuccessRate         string
	AvgCostUSD          string
	AvgDurationSeconds  string
	HallucinationRate   string
	VerifierFailureRate string
}

// BlackBoxCandidatesData backs /ui/admin/blackbox/candidates (list).
type BlackBoxCandidatesData struct {
	adminCommonData
	Available      bool
	Rows           []CandidateRow
	ActionError    string
	FilterStatus   string
	FilterWorkflow string
	StatusOptions  []string
}

// BlackBoxCandidateDetailData backs /ui/admin/blackbox/candidates/{id}.
type BlackBoxCandidateDetailData struct {
	adminCommonData
	Available  bool
	Candidate  CandidateRow
	ProposalID string
	// ProposalDiff / Motivation / ExpectedEffect are denormalised
	// copies sourced from the linked WorkflowProposal at generation
	// time — the trust-this-repair evidence the operator reads first.
	ProposalDiff   string
	Motivation     string
	ExpectedEffect string
	// GenomeBaseline / GenomeCandidate are the structural fingerprints
	// (registry.Workflow.Hash) of the on-disk workflow vs the candidate.
	GenomeBaseline  string
	GenomeCandidate string
	// Evidence is the execution IDs the trigger captured; each links to
	// /ui/executions/{id} so the operator can read the failing runs.
	Evidence []string
	Trials   []TrialRow
	// CanRunTrial / CanPromote / CanReject gate the forms. Promote is
	// ALSO gated server-side by the promoter (this only hides the
	// button when the candidate plainly isn't promotable).
	CanRunTrial bool
	CanPromote  bool
	CanReject   bool
	// HasRunner / HasPromoter hide the buttons entirely when the wiring
	// isn't present on this deployment (vs offering an action that 503s).
	HasRunner   bool
	HasPromoter bool
	ActionError string
}

// candidateStatusOptions powers the list filter + mirrors the
// persistence status enum. Kept in one place so adding a status only
// touches the constants and this helper.
func candidateStatusOptions() []string {
	return []string{
		string(persistence.HealingCandidateDraft),
		string(persistence.HealingCandidateTrialRunning),
		string(persistence.HealingCandidateTrialPassed),
		string(persistence.HealingCandidateTrialFailed),
		string(persistence.HealingCandidatePromoted),
		string(persistence.HealingCandidateRejected),
	}
}

// statusBadgeClass maps a candidate status to a tailwind pill class set.
// trial_passed is green (promotable), trial_failed/rejected rose,
// promoted brand, the rest neutral.
func statusBadgeClass(status string) string {
	switch status {
	case string(persistence.HealingCandidateTrialPassed):
		return "ring-emerald-700/50 bg-emerald-900/30 text-emerald-200"
	case string(persistence.HealingCandidatePromoted):
		return "ring-brand-700/50 bg-brand-900/30 text-brand-200"
	case string(persistence.HealingCandidateTrialFailed),
		string(persistence.HealingCandidateRejected):
		return "ring-rose-700/50 bg-rose-900/30 text-rose-200"
	case string(persistence.HealingCandidateTrialRunning):
		return "ring-amber-700/50 bg-amber-900/30 text-amber-200"
	default:
		return "ring-dark-600 bg-dark-700/40 text-gray-300"
	}
}

// riskBadgeClass maps a risk level to a tailwind pill class set. High
// risk is loud (rose) so the operator can't miss a wide blast radius.
func riskBadgeClass(risk string) string {
	switch risk {
	case string(persistence.HealingRiskHigh):
		return "ring-rose-700/60 bg-rose-900/40 text-rose-100"
	case string(persistence.HealingRiskMedium):
		return "ring-amber-700/50 bg-amber-900/30 text-amber-200"
	default:
		return "ring-emerald-700/40 bg-emerald-900/20 text-emerald-200"
	}
}

// verdictBadgeClass maps a trial verdict to a tailwind pill class set.
func verdictBadgeClass(verdict string) string {
	switch verdict {
	case string(persistence.HealingTrialPassed):
		return "ring-emerald-700/50 bg-emerald-900/30 text-emerald-200"
	case string(persistence.HealingTrialFailed),
		string(persistence.HealingTrialErrored):
		return "ring-rose-700/50 bg-rose-900/30 text-rose-200"
	case string(persistence.HealingTrialInconclusive):
		return "ring-amber-700/50 bg-amber-900/30 text-amber-200"
	default:
		return "ring-dark-600 bg-dark-700/40 text-gray-300"
	}
}

// firstLine returns the first non-blank line of s, trimmed to max
// runes. Used to keep list rows compact without dropping the motivation
// entirely.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if max > 0 && len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// healingCandidateToRow formats a persistence candidate for the list +
// detail templates.
func healingCandidateToRow(c *persistence.HealingCandidate) CandidateRow {
	status := string(c.Status)
	risk := string(c.RiskLevel)
	return CandidateRow{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		WorkflowID:     c.WorkflowID,
		TriggerID:      c.TriggerID,
		ProposalID:     c.ProposalID,
		Class:          string(c.CandidateClass),
		RiskLevel:      risk,
		Status:         status,
		StatusBadge:    statusBadgeClass(status),
		RiskBadge:      riskBadgeClass(risk),
		MotivationLine: firstLine(c.Motivation, 120),
		CreatedAt:      c.CreatedAt.Local().Format("2006-01-02 15:04 MST"),
		IsTrialPassed:  c.Status == persistence.HealingCandidateTrialPassed,
		IsTerminal:     c.Status.IsTerminal(),
	}
}

// fmtPct formats a [0,1] rate as a percentage string with one decimal.
func fmtPct(v float64) string {
	return strconv.FormatFloat(100.0*v, 'f', 1, 64) + "%"
}

// fmtSignedPct formats a signed ratio as e.g. "+12.5%".
func fmtSignedPct(v float64) string {
	s := strconv.FormatFloat(100.0*v, 'f', 1, 64) + "%"
	if v > 0 {
		return "+" + s
	}
	return s
}

// fmtSignedFloat formats a signed float with two decimals.
func fmtSignedFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if v > 0 {
		return "+" + s
	}
	return s
}

// trialSummaryView decodes a TrialSummary JSON blob into the pre-
// formatted per-arm view. Returns nil for an empty/invalid blob so the
// template can fall back gracefully.
func trialSummaryView(blob string) *TrialSummaryView {
	blob = strings.TrimSpace(blob)
	if blob == "" || blob == "{}" {
		return nil
	}
	var s struct {
		Runs                int     `json:"runs"`
		Successes           int     `json:"successes"`
		AvgCostUSD          float64 `json:"avg_cost_usd"`
		AvgDurationSeconds  float64 `json:"avg_duration_seconds"`
		HallucinationRate   float64 `json:"hallucination_rate"`
		VerifierFailureRate float64 `json:"verifier_failure_rate"`
	}
	if err := json.Unmarshal([]byte(blob), &s); err != nil {
		return nil
	}
	successRate := 0.0
	if s.Runs > 0 {
		successRate = float64(s.Successes) / float64(s.Runs)
	}
	return &TrialSummaryView{
		Runs:                strconv.Itoa(s.Runs),
		SuccessRate:         fmtPct(successRate),
		AvgCostUSD:          "$" + strconv.FormatFloat(s.AvgCostUSD, 'f', 4, 64),
		AvgDurationSeconds:  strconv.FormatFloat(s.AvgDurationSeconds, 'f', 1, 64) + "s",
		HallucinationRate:   fmtPct(s.HallucinationRate),
		VerifierFailureRate: fmtPct(s.VerifierFailureRate),
	}
}

// scoreLinesFromScorecard decodes the HealingScorecard JSON blob into
// the pre-formatted scorecard table rows + verdict reasons + the
// replay-fidelity caveat. The "improved" direction is metric-specific:
// success up = good; cost/latency/hallucination/verifier down = good.
func scoreLinesFromScorecard(blob string) (lines []ScoreLine, reasons []string, inconclusive bool, inconclusiveReason string) {
	blob = strings.TrimSpace(blob)
	if blob == "" || blob == "{}" {
		return nil, nil, false, ""
	}
	var sc struct {
		SuccessDelta       float64  `json:"success_delta"`
		CostDeltaPct       float64  `json:"cost_delta_pct"`
		LatencyDeltaPct    float64  `json:"latency_delta_pct"`
		HallucinationDelta float64  `json:"hallucination_delta"`
		VerifierDelta      float64  `json:"verifier_delta"`
		Reasons            []string `json:"reasons"`
		Inconclusive       bool     `json:"inconclusive"`
		InconclusiveReason string   `json:"inconclusive_reason"`
	}
	if err := json.Unmarshal([]byte(blob), &sc); err != nil {
		return nil, nil, false, ""
	}
	lines = []ScoreLine{
		{
			Label:    "Success rate",
			Value:    fmtSignedPct(sc.SuccessDelta),
			Improved: sc.SuccessDelta > 0,
			Worsened: sc.SuccessDelta < 0,
		},
		{
			Label:    "Cost",
			Value:    fmtSignedPct(sc.CostDeltaPct),
			Improved: sc.CostDeltaPct < 0,
			Worsened: sc.CostDeltaPct > 0,
		},
		{
			Label:    "Latency",
			Value:    fmtSignedPct(sc.LatencyDeltaPct),
			Improved: sc.LatencyDeltaPct < 0,
			Worsened: sc.LatencyDeltaPct > 0,
		},
		{
			Label:    "Hallucination rate",
			Value:    fmtSignedFloat(sc.HallucinationDelta),
			Improved: sc.HallucinationDelta < 0,
			Worsened: sc.HallucinationDelta > 0,
		},
		{
			Label:    "Verifier-failure rate",
			Value:    fmtSignedFloat(sc.VerifierDelta),
			Improved: sc.VerifierDelta < 0,
			Worsened: sc.VerifierDelta > 0,
		},
	}
	return lines, sc.Reasons, sc.Inconclusive, sc.InconclusiveReason
}

// healingTrialToRow formats a persistence trial for the detail template,
// decoding the summary + scorecard JSON blobs into pre-formatted views.
func healingTrialToRow(t *persistence.HealingTrial) TrialRow {
	row := TrialRow{
		ID:           t.ID,
		Mode:         string(t.Mode),
		Verdict:      string(t.Verdict),
		VerdictBadge: verdictBadgeClass(string(t.Verdict)),
		StartedAt:    t.StartedAt.Local().Format("2006-01-02 15:04 MST"),
		EvidenceIDs:  t.EvidenceExecutionIDs,
		Baseline:     trialSummaryView(t.BaselineSummary),
		Candidate:    trialSummaryView(t.CandidateSummary),
	}
	if t.FinishedAt != nil {
		row.FinishedAt = t.FinishedAt.Local().Format("2006-01-02 15:04 MST")
	}
	lines, reasons, inconclusive, inconclusiveReason := scoreLinesFromScorecard(t.Scorecard)
	row.ScoreLines = lines
	row.Reasons = reasons
	row.Inconclusive = inconclusive
	row.InconclusiveReason = inconclusiveReason
	row.HasScorecard = len(lines) > 0
	return row
}

// AdminBlackBoxCandidates renders /ui/admin/blackbox/candidates — the
// list of healing candidates with status/risk badges and a filter.
func (s *Server) AdminBlackBoxCandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := BlackBoxCandidatesData{
		adminCommonData: adminCommonData{
			Title:       "Healing candidates",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:      s.healingCandidateRepo != nil,
		ActionError:    strings.TrimSpace(q.Get("action_error")),
		FilterStatus:   strings.TrimSpace(q.Get("status")),
		FilterWorkflow: strings.TrimSpace(q.Get("workflow")),
		StatusOptions:  candidateStatusOptions(),
	}
	if !data.Available {
		s.render(w, "admin_blackbox_candidates.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	filter := persistence.HealingCandidateListFilter{
		WorkflowID: data.FilterWorkflow,
		PageSize:   200,
	}
	if data.FilterStatus != "" {
		filter.Status = persistence.HealingCandidateStatus(data.FilterStatus)
	}
	rows, err := s.healingCandidateRepo.List(ctx, filter)
	if err != nil {
		s.logger.Warn().Err(err).Msg("healing candidate list failed")
	} else {
		for _, c := range rows {
			data.Rows = append(data.Rows, healingCandidateToRow(c))
		}
	}
	s.render(w, "admin_blackbox_candidates.html", data)
}

// AdminBlackBoxCandidateDetail renders
// /ui/admin/blackbox/candidates/{id} — the trust-this-repair page:
// diff + motivation + expected effect + risk banner, evidence links,
// the trial scorecard, and promote/reject/run-trial forms. 404s on a
// missing candidate so the URL bar isn't cached as "exists".
func (s *Server) AdminBlackBoxCandidateDetail(w http.ResponseWriter, r *http.Request, id string) {
	data := BlackBoxCandidateDetailData{
		adminCommonData: adminCommonData{
			Title:       "Healing candidate",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:   s.healingCandidateRepo != nil,
		HasRunner:   s.healingTrialRunner != nil,
		HasPromoter: s.healingPromoter != nil,
	}
	if !data.Available {
		s.render(w, "admin_blackbox_candidate.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	c, err := s.healingCandidateRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("candidate_id", id).Msg("candidate detail: get failed")
		http.Error(w, "candidate lookup failed", http.StatusInternalServerError)
		return
	}
	data.Candidate = healingCandidateToRow(c)
	data.ProposalID = c.ProposalID
	data.ProposalDiff = c.ProposalDiff
	data.Motivation = c.Motivation
	data.ExpectedEffect = c.ExpectedEffect
	data.GenomeBaseline = c.BaselineGenomeHash
	data.GenomeCandidate = c.CandidateGenomeHash
	// Evidence comes from the trigger; a missing/failed trigger lookup
	// degrades gracefully (no evidence links, page still renders).
	if s.healingTriggerRepo != nil && c.TriggerID != "" {
		if trig, terr := s.healingTriggerRepo.Get(ctx, c.TriggerID); terr == nil && trig != nil {
			data.Evidence = trig.EvidenceExecutionIDs
		}
	}
	// Trial history — newest first. A nil repo or list error leaves the
	// section empty rather than failing the whole page.
	if s.healingTrialRepo != nil {
		trials, lerr := s.healingTrialRepo.ListByCandidate(ctx, id)
		if lerr != nil {
			s.logger.Warn().Err(lerr).Str("candidate_id", id).Msg("candidate detail: trial list failed")
		} else {
			for _, t := range trials {
				data.Trials = append(data.Trials, healingTrialToRow(t))
			}
		}
	}
	// Action gating. Run-trial is offered whenever the candidate is not
	// terminal and the runner is wired. Promote shows only for a
	// trial_passed candidate (the promoter ALSO enforces this server-
	// side). Reject is offered for any non-terminal candidate.
	data.CanRunTrial = data.HasRunner && !c.Status.IsTerminal()
	data.CanPromote = data.HasPromoter && c.Status == persistence.HealingCandidateTrialPassed
	data.CanReject = data.HasPromoter && !c.Status.IsTerminal()
	if msg := strings.TrimSpace(r.URL.Query().Get("action_error")); msg != "" {
		data.ActionError = msg
	}
	s.render(w, "admin_blackbox_candidate.html", data)
}

// candidateDetailURL composes the detail URL with an optional
// action_error banner.
func candidateDetailURL(id, actionErr string) string {
	base := "/ui/admin/blackbox/candidates/" + url.PathEscape(id)
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

// AdminBlackBoxCandidateRunTrial handles
// POST /ui/admin/blackbox/candidates/{id}/run-trial. Operator-triggered
// (no background loop). Reads the `mode` form field (static | replay;
// defaults to static), runs the trial synchronously, audits, redirects.
func (s *Server) AdminBlackBoxCandidateRunTrial(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingTrialRunner == nil {
		http.Error(w, "trial runner not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	if mode == "" {
		mode = string(persistence.HealingTrialModeStatic)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var err error
	if mode == string(persistence.HealingTrialModeReplay) {
		// Replay re-runs real evidence executions — minutes to tens of
		// minutes. Open the trial and return at once; the verdict lands
		// in the trial history below when the detached run finishes.
		err = s.healingTrialRunner.RunTrialAsync(ctx, id, mode, nil)
	} else {
		err = s.healingTrialRunner.RunTrial(ctx, id, mode, nil)
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrUICandidateNotFound):
			http.NotFound(w, r)
			return
		case errors.Is(err, ErrUICandidateTerminal):
			http.Redirect(w, r, candidateDetailURL(id, "candidate is terminal; cannot run a trial"), http.StatusSeeOther)
			return
		case errors.Is(err, ErrUITrialRunning):
			http.Redirect(w, r, candidateDetailURL(id, "a trial is already running; wait for its verdict before starting another"), http.StatusSeeOther)
			return
		case errors.Is(err, ErrUITrialMode):
			http.Redirect(w, r, candidateDetailURL(id, "unsupported trial mode "+strconv.Quote(mode)), http.StatusSeeOther)
			return
		default:
			s.logger.Warn().Err(err).Str("candidate_id", id).Msg("candidate run-trial failed")
			http.Redirect(w, r, candidateDetailURL(id, "trial run failed: "+err.Error()), http.StatusSeeOther)
			return
		}
	}
	s.auditCandidate(ctx, r, "blackbox-candidate.trial-run", id, "mode="+mode)
	http.Redirect(w, r, candidateDetailURL(id, ""), http.StatusSeeOther)
}

// AdminBlackBoxCandidatePromote handles
// POST /ui/admin/blackbox/candidates/{id}/promote. Server-side gated:
// the promoter REFUSES anything not in trial_passed (mapped to a 409-
// style banner). On success the candidate transitions to promoted and
// the linked proposal is applied via the memetic path.
func (s *Server) AdminBlackBoxCandidatePromote(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingPromoter == nil {
		http.Error(w, "promoter not wired", http.StatusServiceUnavailable)
		return
	}
	// Promotion writes WORKFLOW.md, git-commits, and hot-reloads — give
	// it room.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	promotedBy := adminPrincipal(r)
	cand, err := s.healingPromoter.Promote(ctx, id, promotedBy)
	if err != nil {
		switch {
		case errors.Is(err, ErrUICandidateNotFound):
			http.NotFound(w, r)
			return
		case errors.Is(err, ErrUICandidateNotPromotable):
			http.Redirect(w, r, candidateDetailURL(id, "candidate has not cleared a trial; promotion requires status trial_passed"), http.StatusSeeOther)
			return
		case errors.Is(err, ErrUICandidateTerminal):
			http.Redirect(w, r, candidateDetailURL(id, "candidate is already promoted or rejected"), http.StatusSeeOther)
			return
		default:
			s.logger.Warn().Err(err).Str("candidate_id", id).Msg("candidate promote failed")
			http.Redirect(w, r, candidateDetailURL(id, "promotion failed: "+err.Error()), http.StatusSeeOther)
			return
		}
	}
	detail := "workflow=" + cand.WorkflowID + " proposal=" + cand.ProposalID
	s.auditCandidate(ctx, r, "blackbox-candidate.promoted", id, detail)
	http.Redirect(w, r, candidateDetailURL(id, ""), http.StatusSeeOther)
}

// AdminBlackBoxCandidateReject handles
// POST /ui/admin/blackbox/candidates/{id}/reject. Flips a non-terminal
// candidate to rejected without touching production.
func (s *Server) AdminBlackBoxCandidateReject(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingPromoter == nil {
		http.Error(w, "promoter not wired", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cand, err := s.healingPromoter.Reject(ctx, id)
	if err != nil {
		switch {
		case errors.Is(err, ErrUICandidateNotFound):
			http.NotFound(w, r)
			return
		case errors.Is(err, ErrUICandidateTerminal):
			http.Redirect(w, r, candidateDetailURL(id, "candidate is already promoted or rejected"), http.StatusSeeOther)
			return
		default:
			s.logger.Warn().Err(err).Str("candidate_id", id).Msg("candidate reject failed")
			http.Redirect(w, r, candidateDetailURL(id, "reject failed: "+err.Error()), http.StatusSeeOther)
			return
		}
	}
	s.auditCandidate(ctx, r, "blackbox-candidate.rejected", id, "workflow="+cand.WorkflowID)
	http.Redirect(w, r, candidateDetailURL(id, ""), http.StatusSeeOther)
}

// auditCandidate writes an admin-audit row for a candidate action. Nil
// audit repo is a no-op; the write is best-effort and never blocks the
// operator action.
func (s *Server) auditCandidate(ctx context.Context, r *http.Request, action, candidateID, after string) {
	if s.adminAuditRepo == nil {
		return
	}
	_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Timestamp: time.Now().UTC(),
		Principal: adminPrincipal(r),
		Source:    "ui",
		Action:    action,
		Target:    candidateID,
		After:     after,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
}
