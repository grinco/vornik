// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"cmp"
	"context"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
	"vornik.io/vornik/internal/registry"
)

type TaskDetailData struct {
	Title              string
	CurrentPage        string
	Task               *persistence.Task
	Project            *registry.Project
	Execution          *persistence.Execution   // most recent execution
	Executions         []*persistence.Execution // all executions (newest first)
	Artifacts          []*persistence.Artifact
	ExecutionArtifacts []*persistence.Artifact // artifacts from the latest execution
	ChangelogContent   string                  // rendered CHANGELOG.md content (if present)
	// Cost breakdown for this task. Empty Rows + zero TotalUSD means "no
	// usage data recorded" — either the task predates 2026.4.11 or the
	// LLM usage repo isn't wired.
	Cost TaskCostBreakdown
	// CostSort/CostDir/CostBaseURL drive the sortable header on the
	// per-step cost breakdown table.
	CostSort    string
	CostDir     string
	CostBaseURL string
	// JudgeVerdict carries the Phase 3 LLM-as-judge verdict for
	// this task (nil when the project hasn't enabled judging or
	// the verdict hasn't landed yet — judges run async).
	JudgeVerdict *TaskJudgeVerdictRow
	// PostMortem is the cached failure explainer (one paragraph
	// LLM summary). Nil for non-failed tasks, or for failed
	// tasks the operator hasn't generated a post-mortem for
	// yet. The template gates on this and on whether the
	// daemon has the explainer wired to decide what to render.
	PostMortem *persistence.TaskPostMortem
	// Phase 26+27 — conversational task lifecycle. Empty/disabled
	// when taskMessageRepo isn't wired.
	Conversation TaskConversationView
	// Notice carries the toast banner text from the action redirect
	// (e.g. "answer-sent"). Empty unless the URL has ?notice=...
	Notice string
	// Sibling navigation — IDs of the prev/next task in the same
	// project ordered by created_at. Empty when at the boundaries.
	// Phase 79.
	PrevTaskID string
	NextTaskID string
	// PostMortemAvailable is true when the daemon has the
	// explainer wired (chat client + repo). The template uses
	// this to decide whether to show the "Explain this
	// failure" button on failed tasks that don't yet have a
	// cached post-mortem.
	PostMortemAvailable bool
	// PostMortemError is set after a synchronous Generate call
	// that failed (LLM timeout, missing chat config, etc).
	// Surfaced as an inline hint on the panel so the operator
	// sees what went wrong instead of a silent re-render.
	PostMortemError string
	// Playbook is the rule-based remediation list for the
	// task's last_error_class. Populated only on failed tasks
	// where LastErrorClass is set; nil otherwise. Renders
	// alongside the post-mortem panel — the post-mortem says
	// "what went wrong on this specific task", the playbook
	// says "what historically resolves this class of failure".
	Playbook *playbook.Entry
	// LearnedRemediations is the (advisory) continuous-learning overlay
	// rendered beside the static Playbook — Consumer A, slice 3. It
	// lists worker-mined recovery instincts that resolved THIS failure
	// class in THIS project before ("similar failures here resolved
	// by …"). Populated ONLY when the instinct.consumers.failure_playbooks
	// gate is on AND the instinct repo is wired AND matching instincts
	// exist; empty otherwise, so the template skips the panel and the
	// page is byte-for-byte unchanged with the gate off. ADVISORY: it is
	// evidence the operator weighs, not an action the daemon takes.
	LearnedRemediations []playbook.LearnedRemediation
	// RecoveryActions is the class-aware one-click next-steps
	// list rendered below the Playbook. Populated only for
	// FAILED tasks; empty for non-failed (the template skips
	// the section). See internal/ui/recovery_actions.go for
	// the class → action mapping. Each action carries Label /
	// URL / Method / Variant so the template stays presentation-
	// only.
	RecoveryActions []RecoveryAction
	// SteerPrefill is a failure-derived starter hint the recovery
	// card's steering textarea is seeded with (when empty) so the
	// operator edits a class-appropriate template instead of staring
	// at a blank box. Empty for classes where a generic prefill would
	// be noise. See SteerPrefillFor. (2026-05-29 LLD-drift audit §8.6.)
	SteerPrefill string
	// Ancestors are the parent chain ordered root-first, *excluding*
	// the task itself. Empty for root tasks. Powers the breadcrumb
	// above the parent block on the detail page. Capped at 10 deep
	// to guard against self-referential cycles in bad data.
	Ancestors []*persistence.Task
	// Children are the direct descendants ordered by created_at ASC.
	// Empty for leaf tasks. Powers the "Subtasks" panel below the
	// parent block.
	Children []*persistence.Task
}

// TaskJudgeVerdictRow projects a persistence.TaskJudgeVerdict
// for the template. Pre-formatted strings keep the template
// simple; the JSONB signals are decoded into the same row
// shape Phase 1 uses so the UI's signal renderer covers both.
type TaskJudgeVerdictRow struct {
	Decision   string
	Confidence float64
	Summary    string
	Model      string
	Role       string
	RecordedAt time.Time
	// VerdictClass drives the pill colour: pass → ok, fail →
	// bad, abstain → pending.
	VerdictClass string
	Signals      []HallucinationSignalRow
}

// TaskCostBreakdown is the per-task cost panel's payload.
type TaskCostBreakdown struct {
	TotalUSD           float64
	TotalPromptTok     int64
	TotalCompletionTok int64
	Rows               []TaskCostStepRow
}

// TaskCostStepRow is one step's row in the per-task breakdown. One row per
// agent-step record. Default sort is CostUSD desc so the biggest spenders
// surface first; the user can re-sort by clicking column headers.
type TaskCostStepRow struct {
	StepID           string
	Role             string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	Iterations       int
	CostUSD          float64
	RecordedAt       time.Time
}

// taskCostColumns maps each sort key to a comparator on TaskCostStepRow.
// Default key is "cost" — biggest spenders first.
var taskCostColumns = map[string]func(a, b TaskCostStepRow) int{
	"step":  func(a, b TaskCostStepRow) int { return cmp.Compare(a.StepID, b.StepID) },
	"role":  func(a, b TaskCostStepRow) int { return cmp.Compare(a.Role, b.Role) },
	"model": func(a, b TaskCostStepRow) int { return cmp.Compare(a.Model, b.Model) },
	"in":    func(a, b TaskCostStepRow) int { return cmp.Compare(a.PromptTokens, b.PromptTokens) },
	"out":   func(a, b TaskCostStepRow) int { return cmp.Compare(a.CompletionTokens, b.CompletionTokens) },
	"iters": func(a, b TaskCostStepRow) int { return cmp.Compare(a.Iterations, b.Iterations) },
	"cost":  func(a, b TaskCostStepRow) int { return cmp.Compare(a.CostUSD, b.CostUSD) },
	"time":  func(a, b TaskCostStepRow) int { return a.RecordedAt.Compare(b.RecordedAt) },
}

// loadTaskSiblings finds the prev/next task IDs in the same
// project ordered by created_at. Phase 79 — powers the arrow
// nav in the sticky context bar so operators can swipe through
// adjacent tasks without bouncing back to the list.
//
// Best-effort: a DB error or missing repo just returns empty
// strings, the arrows hide, page renders without nav.
func (s *Server) loadTaskSiblings(ctx context.Context, task *persistence.Task) (prev, next string) {
	if s.taskRepo == nil || task == nil {
		return "", ""
	}
	// Pull a window of nearby tasks ordered by created_at desc.
	// PageSize=11 (5 before, current, 5 after) gives us neighbours
	// without a full table scan. Two list calls would be more
	// precise but the current List API doesn't expose
	// before/after cursors; this is fine for the UX we need.
	tasks, err := s.taskRepo.List(ctx, persistence.TaskFilter{
		ProjectID: &task.ProjectID,
		PageSize:  500,
	})
	if err != nil || len(tasks) == 0 {
		return "", ""
	}
	for i, t := range tasks {
		if t.ID != task.ID {
			continue
		}
		// Newest first → prev = newer task (i-1), next = older task (i+1).
		if i > 0 {
			prev = tasks[i-1].ID
		}
		if i+1 < len(tasks) {
			next = tasks[i+1].ID
		}
		break
	}
	return prev, next
}

// loadAncestors walks ParentTaskID up to the root, returning the
// chain ordered root → parent (excluding the task itself). Depth is
// capped at 10 — any chain deeper than that is almost certainly a
// data bug, so we stop walking rather than risk an unbounded fetch.
// A cycle is detected via the visited set and treated as "stop
// here". Returns nil on error / missing repo.
func (s *Server) loadAncestors(ctx context.Context, task *persistence.Task) []*persistence.Task {
	if s.taskRepo == nil || task == nil {
		return nil
	}
	const maxDepth = 10
	var rev []*persistence.Task
	seen := map[string]bool{task.ID: true}
	cur := task
	for cur.ParentTaskID != nil && *cur.ParentTaskID != "" && len(rev) < maxDepth {
		pid := *cur.ParentTaskID
		if seen[pid] {
			break
		}
		seen[pid] = true
		parent, err := s.taskRepo.Get(ctx, pid)
		if err != nil || parent == nil {
			break
		}
		rev = append(rev, parent)
		cur = parent
	}
	// Reverse so the slice runs root → parent.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// TaskDetail renders a single task detail page.
func (s *Server) TaskDetail(w http.ResponseWriter, r *http.Request) {
	// Extract task ID from path
	taskID := r.URL.Path[len("/tasks/"):]
	s.logger.Debug().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("task_id", taskID).
		Msg("rendering task detail")
	if taskID == "" {
		s.logger.Warn().Msg("task detail requested without task id")
		http.NotFound(w, r)
		return
	}

	costSort, costDir := sortParams(r, []string{"step", "role", "model", "in", "out", "iters", "cost", "time"}, "cost", "desc")
	data := TaskDetailData{
		CurrentPage: "tasks",
		CostSort:    costSort,
		CostDir:     costDir,
		CostBaseURL: sortBaseURL(r),
	}

	// Get task
	if s.taskRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		task, err := s.taskRepo.Get(ctx, taskID)
		if err != nil {
			s.logger.Warn().Err(err).Str("task_id", taskID).Msg("task not found for UI")
			http.NotFound(w, r)
			return
		}
		// Project-scope check: a scoped key for project A must not
		// read project B's task by guessing/probing the ID. Surface
		// as 404 (not 403) so existence isn't leaked. Legacy rows
		// with empty ProjectID (no project assigned) skip the check
		// per the in-tree convention — they're visible to whoever
		// can reach the page at all, which on auth-off deployments
		// is everyone, and on auth-on deployments is admin-only.
		if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
			http.NotFound(w, r)
			return
		}
		data.Task = task
		data.Title = "Task: " + taskID
		if s.projectReg != nil {
			data.Project = s.projectReg.GetProject(task.ProjectID)
		}

		// Sibling nav — find the prev/next task within the same
		// project ordered by created_at. Cheap query (LIMIT 1 each).
		// Populates the sticky context bar's arrow links.
		data.PrevTaskID, data.NextTaskID = s.loadTaskSiblings(ctx, task)

		// Hierarchy: ancestors (root → parent) and direct children.
		// Best-effort — read failure renders an empty breadcrumb /
		// children section rather than failing the whole page.
		data.Ancestors = s.loadAncestors(ctx, task)
		if children, err := s.taskRepo.GetChildren(ctx, task.ID); err == nil {
			data.Children = children
		} else {
			s.logger.Debug().Err(err).Str("task_id", task.ID).Msg("GetChildren failed; subtasks section suppressed")
		}

		// Get all executions for this task (newest first).
		if s.execRepo != nil {
			execs, err := s.execRepo.List(ctx, persistence.ExecutionFilter{
				TaskID:   &taskID,
				PageSize: 50,
			})
			if err == nil && len(execs) > 0 {
				data.Executions = execs
				data.Execution = execs[0] // most recent
			} else if err != nil {
				s.logger.Warn().Err(err).Str("task_id", taskID).Msg("failed to load task executions for UI")
			}
		}
		// Phase 3: load the LLM-as-judge verdict for this task,
		// if a verdict has been recorded. Verdicts arrive
		// asynchronously after task termination, so a fresh task
		// view may render without one even when judging is
		// enabled — that's expected; refresh later.
		if s.judgeVerdictRepo != nil {
			if v, err := s.judgeVerdictRepo.GetByTask(ctx, taskID); err == nil && v != nil {
				row := &TaskJudgeVerdictRow{
					Decision:     v.Verdict,
					Confidence:   v.Confidence,
					Summary:      v.Summary,
					Model:        v.Model,
					Role:         displayRole(v.Role),
					RecordedAt:   v.RecordedAt,
					VerdictClass: judgeVerdictCSSClass(v.Verdict),
					Signals:      parseHallucinationSignalsForUI(v.Signals),
				}
				data.JudgeVerdict = row
			}
		}

		// Phase 26+27 — conversation thread + scratchpad + open
		// checkpoint card. Best-effort: a read failure leaves the
		// section empty rather than failing the page.
		data.Conversation = s.loadConversationView(ctx, task)
		data.Notice = strings.TrimSpace(r.URL.Query().Get("notice"))

		// Post-mortem: load the cached explainer, if any. Showing
		// the button requires both the repo (to render the cache)
		// AND the explainer (to actually generate one) — the
		// template gates accordingly.
		data.PostMortemAvailable = s.postMortemRepo != nil && s.postMortemExplainer != nil
		if s.postMortemRepo != nil {
			if pm, err := s.postMortemRepo.Get(ctx, taskID); err == nil && pm != nil {
				data.PostMortem = pm
			}
		}

		// Failure-class playbook: rule-based remediation list for
		// the task's recorded last_error_class. Always renders for
		// FAILED tasks with a class set — the corpus is shipped
		// alongside the binary, no per-deployment wiring needed.
		// Lookup never returns nil; an unrecognised class falls
		// back to a generic "investigate via vornikctl task explain"
		// entry so the operator always has a starting point.
		if task.Status == persistence.TaskStatusFailed && task.LastErrorClass != nil && *task.LastErrorClass != "" {
			entry := playbook.Lookup(*task.LastErrorClass)
			data.Playbook = &entry
		}
		// Continuous-learning overlay (Consumer A, slice 3): worker-mined
		// recovery instincts that resolved this failure's error class(es)
		// in this project before. Double-gated (instinct.enabled AND
		// instinct.consumers.failure_playbooks) via instinctPlaybooks;
		// nil-safe; read-only. With the gate off the panel never renders.
		if task.Status == persistence.TaskStatusFailed {
			data.LearnedRemediations = s.learnedRemediationsForTask(ctx, task)
		}
		// Recovery actions card (Upgrade #1, 2026-05-26): class-aware
		// one-click next-steps the operator can take. See
		// internal/ui/recovery_actions.go for the class → action map.
		// Renders only for FAILED tasks with a class — unclassified
		// failures get the universal "Retry / Close" suffix via the
		// helper's default branch.
		if task.Status == persistence.TaskStatusFailed {
			class := ""
			if task.LastErrorClass != nil {
				class = *task.LastErrorClass
			}
			data.RecoveryActions = RecoveryActionsFor(class, task.ID)
			data.SteerPrefill = SteerPrefillFor(class)
		}
		// Surface a recent generate error from the redirect URL
		// (handler stashes it as ?post_mortem_error=…). One-shot:
		// the next page navigation drops it.
		if errMsg := r.URL.Query().Get("post_mortem_error"); errMsg != "" {
			data.PostMortemError = errMsg
		}

		// Get artifacts for this task.
		if s.artifactRepo != nil {
			artifacts, err := s.artifactRepo.List(ctx, persistence.ArtifactFilter{
				TaskID:   &taskID,
				PageSize: 100,
			})
			if err == nil {
				data.Artifacts = artifacts
			}

			// For completed/failed tasks, load artifacts from the latest
			// execution and look for CHANGELOG.md to render inline.
			if data.Execution != nil && (data.Task.Status == persistence.TaskStatusCompleted || data.Task.Status == persistence.TaskStatusFailed) {
				execID := data.Execution.ID
				execArtifacts, err := s.artifactRepo.List(ctx, persistence.ArtifactFilter{
					ExecutionID: &execID,
					PageSize:    100,
				})
				if err == nil {
					data.ExecutionArtifacts = execArtifacts
					for _, a := range execArtifacts {
						if isChangelogArtifact(a.Name) && a.StoragePath != "" {
							// Route through the backend-aware Store so this
							// works on S3 (and any future driver) without
							// special-casing the read path here.
							var content []byte
							var rerr error
							if s.artifactReader != nil {
								content, rerr = s.artifactReader.Retrieve(ctx, a.ID)
							} else {
								content, rerr = os.ReadFile(a.StoragePath)
							}
							if rerr == nil {
								data.ChangelogContent = string(content)
							}
						}
					}
				}
			}
		}
	} else {
		s.logger.Warn().Msg("task repository is not configured for UI")
		http.NotFound(w, r)
		return
	}

	// Per-task cost breakdown. Silent degrade to empty breakdown if the repo
	// isn't wired or the task has no recorded usage (e.g. pre-2026.4.11).
	if s.llmUsageRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rows, err := s.llmUsageRepo.List(ctx, persistence.TaskLLMUsageFilter{
			TaskID:   &taskID,
			PageSize: 200,
		})
		if err != nil {
			s.logger.Warn().Err(err).Str("task_id", taskID).Msg("failed to load task llm usage for UI")
		} else if len(rows) > 0 {
			bk := TaskCostBreakdown{Rows: make([]TaskCostStepRow, 0, len(rows))}
			for _, r := range rows {
				bk.TotalUSD += r.CostUSD
				bk.TotalPromptTok += r.PromptTokens
				bk.TotalCompletionTok += r.CompletionTokens
				bk.Rows = append(bk.Rows, TaskCostStepRow{
					StepID:           r.StepID,
					Role:             displayRole(r.Role),
					Model:            r.Model,
					PromptTokens:     r.PromptTokens,
					CompletionTokens: r.CompletionTokens,
					Iterations:       r.Iterations,
					CostUSD:          r.CostUSD,
					RecordedAt:       r.RecordedAt,
				})
			}
			sortBy(bk.Rows, taskCostColumns, costSort, costDir, "cost")
			data.Cost = bk
		}
	}

	// CSV / JSON export of the per-step cost breakdown. ?format=csv|json
	// on the task detail URL — same convention as the project page.
	switch exportFormat(r) {
	case "csv":
		header := []string{"step_id", "role", "model", "prompt_tokens", "completion_tokens", "iterations", "cost_usd", "recorded_at"}
		out := [][]string{header}
		for _, row := range data.Cost.Rows {
			out = append(out, []string{
				row.StepID, row.Role, row.Model,
				strconv.FormatInt(row.PromptTokens, 10),
				strconv.FormatInt(row.CompletionTokens, 10),
				strconv.Itoa(row.Iterations),
				strconv.FormatFloat(row.CostUSD, 'f', 6, 64),
				row.RecordedAt.UTC().Format(time.RFC3339),
			})
		}
		writeCSV(w, "cost-"+taskID+".csv", out)
		return
	case "json":
		writeJSON(w, "cost-"+taskID+".json", map[string]any{
			"task_id":           taskID,
			"total_usd":         data.Cost.TotalUSD,
			"prompt_tokens":     data.Cost.TotalPromptTok,
			"completion_tokens": data.Cost.TotalCompletionTok,
			"rows":              data.Cost.Rows,
		})
		return
	}

	s.render(w, "task_detail.html", data)
}

// learnedRemediationsForTask returns the (advisory) continuous-learning
// recovery overlay for a FAILED task — Consumer A, slice 3.
//
// Gating + safety:
//   - Returns nil immediately when the failure_playbooks gate is off
//     (s.instinctPlaybooks==false) or no instinct repo is wired, so with
//     the gate off the failed-task page is byte-for-byte unchanged.
//   - Read-only: it only queries the instinct + step-outcome repos. It
//     records NO application rows (unlike the executor surface) — a page
//     view is not an application of the instinct.
//   - Fail-soft: any repo error is logged and swallowed; a degraded
//     instinct store never blocks the failed-task page from rendering.
//
// It collects the distinct stepoutcome ErrorClass values recorded for
// the task's steps (that is the value recovery instincts key on — the
// task's coarser LastErrorClass won't match the trigger), then merges
// the per-class learned remediations, de-duplicated by instinct ID and
// ordered highest-confidence first.
func (s *Server) learnedRemediationsForTask(ctx context.Context, task *persistence.Task) []playbook.LearnedRemediation {
	if !s.instinctPlaybooks || s.instinctRepo == nil || task == nil || task.ProjectID == "" {
		return nil
	}
	if s.outcomeRepo == nil {
		return nil
	}

	taskID := task.ID
	rows, err := s.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		TaskID:   &taskID,
		PageSize: 200,
	})
	if err != nil {
		s.logger.Debug().Err(err).Str("task_id", taskID).
			Msg("learned-remediation overlay: step-outcome lookup failed; skipping panel")
		return nil
	}

	// Distinct (error_class, role) pairs to look up, preserving a stable
	// order so the merged output is deterministic.
	type cr struct{ class, role string }
	seenPair := map[cr]bool{}
	var pairs []cr
	for _, o := range rows {
		if o == nil || o.ErrorClass == "" {
			continue
		}
		p := cr{class: o.ErrorClass, role: o.Role}
		if seenPair[p] {
			continue
		}
		seenPair[p] = true
		pairs = append(pairs, p)
	}

	var merged []playbook.LearnedRemediation
	seenInstinct := map[string]bool{}
	for _, p := range pairs {
		rems, lerr := playbook.LearnedRemediations(ctx, s.instinctRepo, p.class, task.ProjectID, p.role, 3)
		if lerr != nil {
			s.logger.Debug().Err(lerr).Str("task_id", taskID).Str("error_class", p.class).
				Msg("learned-remediation overlay: instinct lookup failed; skipping class")
			continue
		}
		for _, r := range rems {
			if seenInstinct[r.InstinctID] {
				continue
			}
			seenInstinct[r.InstinctID] = true
			merged = append(merged, r)
		}
	}

	sortLearnedRemediations(merged)
	if len(merged) > 5 {
		merged = merged[:5]
	}
	return merged
}

// sortLearnedRemediations orders remediations highest-confidence first
// with a stable instinct-ID tiebreak so the panel is deterministic.
func sortLearnedRemediations(rems []playbook.LearnedRemediation) {
	sort.SliceStable(rems, func(i, j int) bool {
		if rems[i].Confidence != rems[j].Confidence {
			return rems[i].Confidence > rems[j].Confidence
		}
		return rems[i].InstinctID < rems[j].InstinctID
	})
}

// isChangelogArtifact recognises a CHANGELOG.md artifact in both
// the legacy un-disambig'd form ("CHANGELOG.md") and the new
// disambig'd form the executor's persistArtifacts now emits
// ("CHANGELOG-YYYYMMDD-XXXX.md"). Case-insensitive on the stem
// so operator-supplied variants like changelog.md still match.
//
// Matching strategy: case-fold the lowercase stem comparison;
// keep the date+id suffix strict (8 digits + 4 hex chars) so
// operator-named files like "CHANGELOG-2026-Q2.md" don't get
// pulled into the inline-render path by accident.
func isChangelogArtifact(name string) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	if lower == "changelog.md" {
		return true
	}
	// Disambig'd form: changelog-YYYYMMDD-XXXX.md
	const prefix = "changelog-"
	const suffix = ".md"
	const dateLen, idLen = 8, 4
	wantLen := len(prefix) + dateLen + 1 + idLen + len(suffix) // "changelog-DDDDDDDD-XXXX.md"
	if len(lower) != wantLen {
		return false
	}
	if !strings.HasPrefix(lower, prefix) || !strings.HasSuffix(lower, suffix) {
		return false
	}
	body := lower[len(prefix) : len(lower)-len(suffix)] // "DDDDDDDD-XXXX"
	if len(body) != dateLen+1+idLen || body[dateLen] != '-' {
		return false
	}
	for i := 0; i < dateLen; i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	for i := dateLen + 1; i < len(body); i++ {
		c := body[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
