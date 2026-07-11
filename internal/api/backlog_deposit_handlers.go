package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"vornik.io/vornik/internal/backlogfile"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/textsim"
	"vornik.io/vornik/internal/textutil"
)

const (
	backlogDepositMaxTitleLen       = 140
	backlogDepositMaxDetailLen      = 2000
	backlogDepositMaxEvidenceLen    = 500
	backlogDepositRenderedDetailCap = 600
	// backlogDepositCooldown is how long after a matching [x]/[!] item
	// was deposited a regression-flagged re-deposit is still rejected
	// (design C1's spin-loop guard — Adopted item #3 in the risk audit).
	backlogDepositCooldown = 7 * 24 * time.Hour
)

// backlogDepositValidKinds enumerates the `kind` values the deposit
// endpoint accepts. Kept as a package-level set (rather than an
// exported registry enum) because this vocabulary is owned by the
// backlog-deposit contract, not the project schema.
var backlogDepositValidKinds = map[string]bool{
	"bug":          true,
	"optimisation": true,
	"inefficiency": true,
	"refactor":     true,
}

// backlogDepositRequest is the wire shape for
// POST /api/v1/internal/backlog-deposit. See https://docs.vornik.io
// 2026-07-09-autonomous-dev-loop-design.md (design C1) for the full
// pipeline this drives.
type backlogDepositRequest struct {
	ProjectID   string `json:"project_id"`
	TaskID      string `json:"task_id"`
	ExecutionID string `json:"execution_id"`
	StepID      string `json:"step_id"`
	Role        string `json:"role"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Evidence    string `json:"evidence"`
	Regression  bool   `json:"regression"`
}

// backlogDepositResponse is the wire shape of every 200 response.
// Rejected outcomes carry Reason; the accepted outcome carries Item
// (the exact rendered line appended to BACKLOG.md).
type backlogDepositResponse struct {
	Status string `json:"status"`
	Item   string `json:"item,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// writeBacklogDepositJSON writes a 200 response with the given
// status/item/reason. Agent-visible pipeline outcomes (accepted,
// duplicate, secret, cap, cooldown, regression_required) are always
// HTTP 200 so the calling tool gets a structured result instead of
// having to distinguish an HTTP error from a semantic rejection —
// auth/validation failures are the only non-200s (via respondError).
func writeBacklogDepositJSON(w http.ResponseWriter, resp backlogDepositResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// backlogDepositMetricOutcome maps a rejection reason to the metric's
// outcome label. regression_required folds into "duplicate" — both
// are the dedup path finding an existing matching item, just gated by
// the marker (open vs. closed) — so the metric doesn't grow a 7th
// outcome value beyond the documented set (accepted/duplicate/secret/
// cap/cooldown/invalid).
func backlogDepositMetricOutcome(reason string) string {
	if reason == "regression_required" {
		return "duplicate"
	}
	return reason
}

// BacklogDeposit handles POST /api/v1/internal/backlog-deposit. Agents
// call this (via the backlog_deposit tool) to propose a `- [?]` line in
// the project's BACKLOG.md. Pipeline order (design C1): auth/consistency
// guards -> field validation -> render the single line -> secret-scan
// the rendered line -> per-task rate cap -> dedup against existing
// items -> append.
func (s *Server) BacklogDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := readLimitedBody(w, r, 1<<20) // 1 MiB cap, mirrors IngestToolAudit
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req backlogDepositRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if req.ProjectID == "" || req.TaskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project_id and task_id are required")
		return
	}

	// Authorisation: same guard stack as IngestToolAudit, in the same
	// order — a project-scoped key must be authorised for the body's
	// project_id, a task-scoped key must match the body's task_id, and
	// (when we can check) the task must actually belong to the claimed
	// project, and the execution must belong to the task.
	if !s.backlogDepositGuardsPass(w, r, req) {
		return
	}

	project := s.backlogDepositResolveProject(w, req)
	if project == nil {
		return
	}

	title := flattenWhitespace(req.Title)
	// M2 fix (2026-07-09 final review): renderBacklogDepositLine embeds
	// the title verbatim, but both parsers that recover a title from a
	// rendered line — parseBacklogItemKindTitle below and
	// internal/autonomy/manager.go's backlogItemTitle — cut at the
	// FIRST " — " (em-dash) separator. A title containing that sequence
	// would render fine but come back truncated on parse, silently
	// weakening exact-title dedup. Swap the em-dash character itself
	// for a plain hyphen so the title can never introduce the
	// separator the renderer also uses — this only touches the dash
	// character, so surrounding spacing (or lack of it) is preserved.
	title = strings.ReplaceAll(title, "—", "-")
	if msg, ok := backlogDepositFieldError(req, title); !ok {
		s.recordBacklogDepositMetric(req.ProjectID, "invalid")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}

	abs, ok := s.backlogDepositResolvePath(w, req.ProjectID, project)
	if !ok {
		return
	}

	now := time.Now().UTC()
	line := renderBacklogDepositLine(req.Kind, title, req.Detail, req.Evidence, req.TaskID, now)

	// Secret-scan the RENDERED line (not the raw fields) — the trailer's
	// task_id/evidence and the assembled detail are exactly what lands in
	// BACKLOG.md, so that's what must be clean.
	line, rejected := s.backlogDepositSecretScan(w, req, line)
	if rejected {
		return
	}

	// Rate cap: read-only check against the in-memory per-task counter,
	// incremented only after a successful Append below. A deposit that
	// would be rejected as a duplicate/secret never consumes a cap slot.
	maxPerTask := int64(project.BacklogDeposits.ResolveMaxPerTask())
	if s.backlogDepositCount(req.TaskID) >= maxPerTask {
		s.recordBacklogDepositMetric(req.ProjectID, "cap")
		writeBacklogDepositJSON(w, backlogDepositResponse{Status: "rejected", Reason: "cap"})
		return
	}

	items, err := s.backlogStore.Items(abs, project.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	// Per design C1 (https://docs.vornik.io
	// design.md §C1 dedup step), a regression re-deposit requires
	// regression:true AND non-empty evidence — regression:true alone
	// (or with whitespace-only evidence) does not satisfy the flag and
	// is treated the same as regression:false for dedup purposes.
	regressionOK := req.Regression && flattenWhitespace(req.Evidence) != ""
	if reason, dup := backlogDepositDedupReason(items, req.Kind, title, regressionOK, now); dup {
		s.recordBacklogDepositMetric(req.ProjectID, backlogDepositMetricOutcome(reason))
		writeBacklogDepositJSON(w, backlogDepositResponse{Status: "rejected", Reason: reason})
		return
	}

	if err := s.backlogStore.Append(abs, project.ID, line); err != nil {
		respondError(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	s.incrementBacklogDepositCount(req.TaskID)
	s.recordBacklogDepositMetric(req.ProjectID, "accepted")
	writeBacklogDepositJSON(w, backlogDepositResponse{Status: "accepted", Item: line})
}

// backlogDepositGuardsPass runs the auth/consistency guard stack shared
// with IngestToolAudit (requestAllowsProject -> mismatchedTaskScopedKey
// -> task/project consistency -> execution/task binding). Returns false
// after already writing the 403 response when a guard rejects.
func (s *Server) backlogDepositGuardsPass(w http.ResponseWriter, r *http.Request, req backlogDepositRequest) bool {
	if !requestAllowsProject(r, req.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for project")
		return false
	}
	if mismatchedTaskScopedKey(r, req.TaskID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "task_id does not match the task-scoped API key")
		return false
	}
	if req.TaskID != "" && s.taskRepo != nil {
		task, err := s.taskRepo.Get(r.Context(), req.TaskID)
		if err == nil && task != nil && task.ProjectID != req.ProjectID {
			respondError(w, http.StatusForbidden, "FORBIDDEN", "task_id belongs to a different project than project_id")
			return false
		}
	}
	if err := s.validateExecutionTaskBinding(r.Context(), req.TaskID, req.ExecutionID); err != nil {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "execution_id does not belong to task_id")
		return false
	}
	return true
}

// backlogDepositResolveProject looks up req.ProjectID in the registry,
// writing a 503/404 response and returning nil when it can't resolve.
func (s *Server) backlogDepositResolveProject(w http.ResponseWriter, req backlogDepositRequest) *registry.Project {
	if s.projectRegistry == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "project registry not wired")
		return nil
	}
	project := s.projectRegistry.GetProject(req.ProjectID)
	if project == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "project not found")
		return nil
	}
	return project
}

// backlogDepositFieldError validates the request's user-supplied
// content fields (title/detail/evidence/kind), returning ok=true when
// every field is in bounds.
func backlogDepositFieldError(req backlogDepositRequest, title string) (msg string, ok bool) {
	switch {
	case title == "" || len(title) > backlogDepositMaxTitleLen:
		return fmt.Sprintf("title must be 1-%d characters", backlogDepositMaxTitleLen), false
	case len(req.Detail) > backlogDepositMaxDetailLen:
		return fmt.Sprintf("detail must be at most %d characters", backlogDepositMaxDetailLen), false
	case len(req.Evidence) > backlogDepositMaxEvidenceLen:
		return fmt.Sprintf("evidence must be at most %d characters", backlogDepositMaxEvidenceLen), false
	case !backlogDepositValidKinds[req.Kind]:
		return fmt.Sprintf("kind must be one of bug|optimisation|inefficiency|refactor, got %q", req.Kind), false
	default:
		return "", true
	}
}

// backlogDepositResolvePath resolves and safety-checks project's
// absolute BACKLOG.md path, writing a 503/400 response and returning
// ok=false when it can't be resolved.
func (s *Server) backlogDepositResolvePath(w http.ResponseWriter, projectID string, project *registry.Project) (abs string, ok bool) {
	if s.backlogStore == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "backlog store not wired")
		return "", false
	}
	if s.config == nil || s.config.Runtime.ProjectWorkspacePath == "" {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "workspace path not configured")
		return "", false
	}
	rel := project.ResolveBacklogFilePath()
	if rel == "" {
		s.recordBacklogDepositMetric(projectID, "invalid")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "backlogFilePath invalid")
		return "", false
	}
	// safepath.JoinUnder resolves symlinks in the deepest existing prefix
	// and asserts the candidate stays inside the workspace root — the
	// exact defense tickBacklog uses (internal/autonomy/manager.go), so a
	// backlogFilePath like "link" -> /etc/shadow can't be planted by an
	// in-container agent and used to read/overwrite arbitrary host files.
	abs, err := safepath.JoinUnder(s.config.Runtime.ProjectWorkspacePath, project.ID, rel)
	if err != nil {
		s.recordBacklogDepositMetric(projectID, "invalid")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "backlogFilePath escapes workspace")
		return "", false
	}
	return abs, true
}

// backlogDepositSecretScan scans the rendered line and applies the
// resolved secrets.Action. Returns the (possibly redacted) line and
// rejected=true once the Block action has already written the 200
// rejected response.
func (s *Server) backlogDepositSecretScan(w http.ResponseWriter, req backlogDepositRequest, line string) (out string, rejected bool) {
	if s.secretsDetector == nil {
		return line, false
	}
	findings := s.secretsDetector.Scan([]byte(line))
	if len(findings) == 0 {
		return line, false
	}
	action := secrets.ResolveAction(secrets.CheckpointBacklogDeposit, s.secretsActions)
	counts := secrets.CountByType(findings)
	logEvent := s.logger.Warn().
		Str("project_id", req.ProjectID).
		Str("task_id", req.TaskID).
		Str("checkpoint", secrets.CheckpointBacklogDeposit).
		Str("action", string(action)).
		Int("findings", len(findings)).
		Interface("by_type", counts)
	switch action {
	case secrets.ActionBlock:
		logEvent.Msg("backlog deposit blocked by secret-leak scan")
		s.recordBacklogDepositMetric(req.ProjectID, "secret")
		writeBacklogDepositJSON(w, backlogDepositResponse{Status: "rejected", Reason: "secret"})
		return line, true
	case secrets.ActionRedact:
		logEvent.Msg("backlog deposit scanned — redacting before append")
		return string(secrets.Redact([]byte(line), findings)), false
	default: // ActionDetect
		logEvent.Msg("backlog deposit scanned — detect-only")
		return line, false
	}
}

// recordBacklogDepositMetric is a nil-safe wrapper around
// s.apiMetrics.RecordBacklogDeposit so call sites don't need their own
// nil check (apiMetrics is only set when a Prometheus registry was
// configured — see routes.go).
func (s *Server) recordBacklogDepositMetric(project, outcome string) {
	if s == nil || s.apiMetrics == nil {
		return
	}
	s.apiMetrics.RecordBacklogDeposit(project, outcome)
}

// backlogDepositCount returns the current accepted-deposit count for
// taskID without mutating it (rate-cap read side).
func (s *Server) backlogDepositCount(taskID string) int64 {
	v, ok := s.backlogDepositCounts.Load(taskID)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(v.(*int64))
}

// incrementBacklogDepositCount bumps taskID's accepted-deposit counter.
// Called only after a successful Append.
func (s *Server) incrementBacklogDepositCount(taskID string) {
	v, _ := s.backlogDepositCounts.LoadOrStore(taskID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

// flattenWhitespace collapses any run of whitespace (including
// newlines) to a single space and trims the ends — used on title,
// detail, and evidence so the rendered BACKLOG.md line is always
// exactly one line (backlogfile.Store.Append rejects embedded
// newlines).
func flattenWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// renderBacklogDepositLine builds the exact BACKLOG.md item text
// (everything after "- [?] "): `**[<kind>]** <title> — <detail>
// (evidence: <evidence>; via <task_id>, <YYYY-MM-DD>)`. The evidence
// segment is omitted cleanly when empty. detail is whitespace-
// flattened and truncated to backlogDepositRenderedDetailCap chars.
func renderBacklogDepositLine(kind, title, detail, evidence, taskID string, now time.Time) string {
	detail = flattenWhitespace(detail)
	detail = textutil.TruncateBytes(detail, backlogDepositRenderedDetailCap)
	evidence = flattenWhitespace(evidence)
	date := now.UTC().Format("2006-01-02")

	var trailer string
	if evidence == "" {
		trailer = fmt.Sprintf("(via %s, %s)", taskID, date)
	} else {
		trailer = fmt.Sprintf("(evidence: %s; via %s, %s)", evidence, taskID, date)
	}
	return fmt.Sprintf("**[%s]** %s — %s %s", kind, title, detail, trailer)
}

// titlePunctRE strips punctuation for normalizeTitle — anything that
// isn't a letter, number, or whitespace.
var titlePunctRE = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)

// normalizeTitle lower-cases, strips punctuation, and collapses
// whitespace so trivially-reworded duplicate titles ("Fix the Cache!"
// vs "fix the cache") compare equal.
func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = titlePunctRE.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

// backlogItemKindTitleRE extracts the leading "**[kind]**" token our
// own renderer emits.
var backlogItemKindTitleRE = regexp.MustCompile(`^\*\*\[([a-z]+)\]\*\*\s*`)

// parseBacklogItemKindTitle splits a BACKLOG.md item's text (everything
// after the "- [x] " marker box) into its kind and title, per the
// dedup contract: kind is the leading "**[kind]**" token (empty when
// absent — e.g. an operator-authored line predating this grammar), and
// title is the text up to the first " — " separator (the whole
// remainder when there is no separator).
func parseBacklogItemKindTitle(text string) (kind, title string) {
	rest := text
	if m := backlogItemKindTitleRE.FindStringSubmatchIndex(text); m != nil {
		kind = text[m[2]:m[3]]
		rest = text[m[1]:]
	}
	if idx := strings.Index(rest, " — "); idx >= 0 {
		rest = rest[:idx]
	}
	return kind, strings.TrimSpace(rest)
}

// backlogItemDateRE finds the "via <task_id>, YYYY-MM-DD)" trailer our
// own renderer emits. Not anchored to the end of the string: MarkConsumed
// / MarkFailed append their own "(task: <id>)" / ", failed)" suffix
// AFTER our trailer, which would break an end-anchored match — and an
// unparseable date is deliberately treated as "old" (see
// backlogDepositDedupReason), so failing open here is intentional.
var backlogItemDateRE = regexp.MustCompile(`via\s+\S+,\s*(\d{4}-\d{2}-\d{2})\)`)

// parseBacklogItemDate extracts the deposit date from an item's
// trailer. ok is false when no trailer is found or the date fails to
// parse (e.g. an operator-authored line with no trailer at all).
func parseBacklogItemDate(text string) (t time.Time, ok bool) {
	m := backlogItemDateRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02", m[1])
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// backlogDepositDedupReason implements the design-C1 dedup pass against
// every existing BACKLOG.md item. "Match" is: an exact normalized-title
// match (any kind), OR textsim.Jaccard >= 0.85 between same-kind items,
// OR textsim.Jaccard >= 0.95 across kinds. What a match means depends on
// the matched item's marker:
//   - open ([ ] pending / [?] proposed): always "duplicate".
//   - closed ([x] done / [!] failed): "regression_required" unless
//     regressionOK is true; with regressionOK true, "cooldown" if the
//     matched item's trailer date is under 7 days old, otherwise
//     allowed (the loop keeps checking remaining items). Per design C1,
//     the caller is responsible for regressionOK meaning both
//     regression:true AND non-empty evidence were supplied — bare
//     regression:true with no evidence must NOT satisfy this flag.
//
// An unparseable/absent trailer date on a closed match is treated as
// old — i.e. it does not trigger cooldown — so a legacy operator-
// authored line without our trailer format never permanently blocks a
// legitimate regression re-deposit.
func backlogDepositDedupReason(items []backlogfile.Item, kind, title string, regressionOK bool, now time.Time) (reason string, dup bool) {
	normTitle := normalizeTitle(title)
	for _, it := range items {
		itemKind, itemTitle := parseBacklogItemKindTitle(it.Text)
		itemNorm := normalizeTitle(itemTitle)

		exactMatch := normTitle != "" && normTitle == itemNorm
		sim := textsim.Jaccard(normTitle, itemNorm)
		sameKind := kind == itemKind

		matched := exactMatch ||
			(sameKind && sim >= 0.85) ||
			(!sameKind && sim >= 0.95)
		if !matched {
			continue
		}

		switch it.Marker {
		case ' ', '?':
			return "duplicate", true
		case 'x', '!':
			if !regressionOK {
				return "regression_required", true
			}
			if d, ok := parseBacklogItemDate(it.Text); ok && now.Sub(d) < backlogDepositCooldown {
				return "cooldown", true
			}
			// Old enough (or unparseable, treated as old) — this
			// particular closed item doesn't block the deposit;
			// keep checking the rest.
		}
	}
	return "", false
}
