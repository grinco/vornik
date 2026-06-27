package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

const maxWebhookBodyBytes = 2 << 20
const maxWebhookRejectedAuditEventsPerMinute = 60

// IngestWebhook handles POST /api/v1/webhooks/{projectId}/{source}.
func (s *Server) IngestWebhook(w http.ResponseWriter, r *http.Request) {
	projectID, sourceName := extractWebhookPath(r)
	if projectID == "" || sourceName == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project and source are required")
		return
	}
	// Ingestion is configured when we can either create tasks locally
	// (taskRepo, on a DB-bearing node) OR relay them to the job tier
	// (webhookRelay, on a DMZ RelayMode node — which has NO taskRepo). The
	// earlier `taskRepo == nil` guard 503'd every delivery on a DMZ relay node
	// before the relay branch below could run (incident 2026-06-12: GitHub
	// deliveries to a relay node returned 503 WEBHOOK_NOT_CONFIGURED).
	if s.projectRegistry == nil || (s.taskRepo == nil && s.webhookRelay == nil) {
		respondError(w, http.StatusServiceUnavailable, "WEBHOOK_NOT_CONFIGURED", "webhook ingestion is not configured")
		return
	}
	project := s.projectRegistry.GetProject(projectID)
	if project == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
		return
	}
	source, ok := findWebhookSource(project, sourceName)
	if !ok {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Webhook source not found")
		return
	}

	// http.MaxBytesReader streams the body with a hard cap;
	// over-limit reads short-circuit with an error rather than
	// buffering the full payload in memory. Pre-2026-05-29 we used
	// io.LimitReader(maxWebhookBodyBytes+1) which buffered up to
	// 2 MiB + 1 byte per concurrent request before rejecting — a
	// DoS amplifier when concurrent unauthenticated probes hit the
	// generic /api/v1/webhooks/ path.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		s.recordWebhookEvent(r.Context(), projectID, sourceName, "", body, persistence.WebhookEventStatusRejected, nil, "invalid_body", "Invalid webhook body")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid webhook body")
		return
	}
	if err := verifyWebhookSignature(r, body, source); err != nil {
		eventID := webhookEventIDFromBodyOrHeader(body, source.EventIDPath, r.Header.Get("X-GitHub-Delivery"))
		// Don't store err.Error() in the audit row — its text
		// distinguishes "missing signature" from "signature
		// mismatch", giving any reader of the per-project
		// webhook events list (e.g. via a valid project API
		// key) a binary-search oracle on the HMAC secret.
		// Detail stays in the operator logger below.
		s.recordWebhookEvent(r.Context(), projectID, sourceName, eventID, body, persistence.WebhookEventStatusRejected, nil, "invalid_signature", "")
		s.logger.Warn().Err(err).Str("project_id", projectID).Str("source", sourceName).Msg("webhook rejected")
		respondError(w, http.StatusUnauthorized, "INVALID_SIGNATURE", "Invalid webhook signature")
		return
	}

	if s.webhookRelay != nil {
		// DMZ no-DB invariant: a relay/DMZ node has no webhookEventRepo (nil),
		// so recordWebhookEvent is a no-op here. The success path performs NO
		// DB write — it simply mirrors the job-tier status back to the provider.
		// The invariant is enforced at container construction; this comment
		// records why the relay path is intentionally DB-free.
		deliveryID := r.Header.Get("X-GitHub-Delivery")
		status, err := s.webhookRelay.Forward(r.Context(), projectID, sourceName, deliveryID, body)
		if err != nil {
			s.recordWebhookEvent(r.Context(), projectID, sourceName, webhookEventIDFromBodyOrHeader(body, source.EventIDPath, deliveryID), body, persistence.WebhookEventStatusRejected, nil, "relay_failed", "")
			s.logger.Error().Err(err).
				Str("project_id", projectID).
				Str("source", sourceName).
				Str("delivery_id", deliveryID).
				Msg("webhook relay forward failed")
			// 502 → the provider (GitHub/Slack) retries; the DMZ keeps no spool.
			respondError(w, http.StatusBadGateway, "RELAY_FAILED", "failed to relay webhook to job tier")
			return
		}
		s.logger.Info().
			Str("project_id", projectID).
			Str("source", sourceName).
			Str("delivery_id", deliveryID).
			Int("status", status).
			Msg("webhook relayed to job tier")
		// Mirror the job tier's status to the provider. Audit on the DMZ
		// side is best-effort and DB-less, so skip recordWebhookEvent here
		// (the job tier records the authoritative audit row).
		w.WriteHeader(status)
		return
	}
	s.enqueueVerifiedWebhook(r.Context(), w, project, source, body, r.Header.Get("X-GitHub-Delivery"))
}

// enqueueVerifiedWebhook runs the post-signature-verification pipeline:
// secret-scan → JSON parse → task-type template → idempotency/dedup →
// admission (rate-limit + budget) → task create → audit → response. It is
// shared by the in-process IngestWebhook path AND the job-tier relay-ingress
// endpoint (RelayWebhook), which authenticates the caller via mTLS and so
// does not re-run HMAC verification. `deliveryID` is the provider delivery
// header (e.g. X-GitHub-Delivery) used for event-id derivation.
func (s *Server) enqueueVerifiedWebhook(ctx context.Context, w http.ResponseWriter, project *registry.Project, source registry.ProjectWebhookSource, body []byte, deliveryID string) {
	projectID := project.ID
	sourceName := source.Name

	// Phase 2 secret-leak scan. Default action for the webhook
	// checkpoint is Block: an authenticated inbound payload that
	// contains credentials almost always means upstream
	// misconfiguration the operator wants to fix at the source.
	// Per-source opt-out via ProjectWebhookSource.AllowSecrets for
	// signed payload formats that legitimately carry long
	// high-entropy tokens (delivery IDs etc.).
	if s.secretsDetector != nil && !source.AllowSecrets {
		if findings := s.secretsDetector.Scan(body); len(findings) > 0 {
			action := secrets.ResolveAction(secrets.CheckpointWebhook, s.secretsActions)
			counts := secrets.CountByType(findings)
			eventID := webhookEventIDFromBodyOrHeader(body, source.EventIDPath, deliveryID)
			logEvent := s.logger.Warn().
				Str("project_id", projectID).
				Str("source", sourceName).
				Str("event_id", eventID).
				Str("checkpoint", secrets.CheckpointWebhook).
				Str("action", string(action)).
				Int("findings", len(findings)).
				Interface("by_type", counts)
			switch action {
			case secrets.ActionBlock:
				logEvent.Msg("webhook payload blocked by secret-leak scan")
				reason := fmt.Sprintf("payload contains %d secret-shaped value(s); fix the source or set webhooks.<source>.allow_secrets: true", len(findings))
				s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusRejected, nil, "secret_leak", reason)
				respondError(w, http.StatusBadRequest, "SECRET_LEAK", reason)
				return
			case secrets.ActionRedact:
				// Redact-mode opt-in: rewrite the body before JSON
				// parse so downstream task creation never sees the
				// raw value. The signature is already verified above
				// (against the original body), so rewriting here is
				// safe.
				logEvent.Msg("webhook payload scanned — redacting before task creation")
				body = secrets.Redact(body, findings)
			default: // ActionDetect
				logEvent.Msg("webhook payload scanned — detect-only")
			}
		}
	}

	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		s.recordWebhookEvent(ctx, projectID, sourceName, hashWebhookBody(body), body, persistence.WebhookEventStatusRejected, nil, "invalid_json", err.Error())
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Webhook body must be a JSON object")
		return
	}
	// Source-level filter: drop deliveries the operator doesn't want to act on
	// (e.g. pull_request.synchronize) before any task is created. Acknowledged
	// 200 so the provider doesn't retry; recorded as "filtered" for visibility.
	if !matchesWebhookFilter(source.Filter, event) {
		eventID := webhookEventID(event, source.EventIDPath, deliveryID, body)
		s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusFiltered, nil, "filtered", "")
		respondJSON(w, http.StatusOK, map[string]string{"status": "filtered"})
		return
	}
	taskType, err := renderWebhookTemplate(source.TaskTypeTemplate, event)
	if err != nil || strings.TrimSpace(taskType) == "" {
		eventID := webhookEventID(event, source.EventIDPath, deliveryID, body)
		s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusRejected, nil, "template_error", "failed to render task template")
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "failed to render task template")
		return
	}

	eventID := webhookEventID(event, source.EventIDPath, deliveryID, body)
	idempotencyKey := "webhook:" + sourceName + ":" + eventID

	// forward_payload: hand the agent the verified event body so it can act on
	// the specific PR/issue. Agents read Context.prompt via extractPrompt.
	var taskContext json.RawMessage
	if source.ForwardPayload {
		taskContext, _ = json.Marshal(map[string]string{"prompt": string(body)})
	}

	// Deterministic forge classification: stamp a provider-neutral forge_job on
	// the task so the forge.* system steps (open_change_request / post_review /
	// fetch_diff) run without parsing the event. No-op when the project has no
	// forge configured or the event isn't a forge action.
	var forgeJob json.RawMessage
	workflowOverride := ""
	if s.forgeClassifier != nil {
		if fj, prompt, ok := s.forgeClassifier.ClassifyWebhook(ctx, project.ID, body); ok {
			forgeJob = fj
			// Hand the agent a clean spec (issue/CR title+body) instead of the
			// raw webhook JSON, when forward_payload requested a prompt.
			if prompt != "" && source.ForwardPayload {
				taskContext, _ = json.Marshal(map[string]string{"prompt": prompt})
			}
			// Route change requests (opened PR/MR) to the review workflow when
			// configured; issues keep the source's default workflow.
			if source.ChangeRequestWorkflowID != "" {
				var f struct {
					IsChangeRequest bool `json:"is_change_request"`
				}
				if json.Unmarshal(fj, &f) == nil && f.IsChangeRequest {
					workflowOverride = source.ChangeRequestWorkflowID
				}
			}
		}
	}

	// require_forge_event: a source that must act only on actionable forge events
	// drops anything the classifier didn't recognise (issues.closed, PR
	// synchronize, an unlabeled issue) rather than creating a forge_job-less task.
	if source.RequireForgeEvent && len(forgeJob) == 0 {
		eventID := webhookEventID(event, source.EventIDPath, deliveryID, body)
		s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusFiltered, nil, "not_a_forge_event", "")
		respondJSON(w, http.StatusOK, map[string]string{"status": "filtered"})
		return
	}

	existing, duplicate := s.existingWebhookTask(ctx, project.ID, idempotencyKey)
	if duplicate {
		s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusDuplicate, &existing.ID, "", "")
		respondJSON(w, http.StatusAccepted, buildCreateTaskResponse(existing))
		return
	}

	if admitted := s.admitWebhookTask(ctx, project); !admitted.ok {
		s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusRejected, nil, admitted.code, admitted.reason)
		respondError(w, admitted.status, admitted.code, admitted.reason)
		return
	}
	task, err := s.createWebhookTask(ctx, project, source, taskType, idempotencyKey, taskContext, forgeJob, workflowOverride)
	if err != nil {
		s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusRejected, nil, "create_task_failed", err.Error())
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to create task")
		return
	}
	s.recordWebhookEvent(ctx, projectID, sourceName, eventID, body, persistence.WebhookEventStatusAccepted, &task.ID, "", "")
	s.logger.Info().
		Str("project_id", projectID).
		Str("source", sourceName).
		Str("event_id", eventID).
		Str("task_id", task.ID).
		Msg("webhook accepted")
	respondJSON(w, http.StatusAccepted, buildCreateTaskResponse(task))
}

type webhookAdmission struct {
	ok     bool
	status int
	code   string
	reason string
}

func (s *Server) admitWebhookTask(ctx context.Context, project *registry.Project) webhookAdmission {
	if s.rateLimiter != nil {
		d := s.rateLimiter.Check(project, time.Now())
		if s.rateLimitMetrics != nil {
			s.rateLimitMetrics.ObserveProject(project.ID, d)
		}
		if d.Blocked {
			return webhookAdmission{status: http.StatusTooManyRequests, code: "RATE_LIMITED", reason: d.Reason}
		}
	}
	if s.llmUsageRepo != nil {
		decision, err := budget.Check(ctx, s.llmUsageRepo, project, time.Now().UTC())
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", project.ID).Msg("webhook: budget check failed — proceeding")
		} else if decision.Blocked {
			if s.budgetNotifier != nil {
				period, level := decision.Period()
				s.budgetNotifier.NotifyBudgetBreach(ctx, project.ID, level, period, decision)
			}
			return webhookAdmission{status: http.StatusTooManyRequests, code: "BUDGET_EXCEEDED", reason: decision.Reason}
		} else if decision.SoftBreached {
			s.logger.Warn().
				Str("project_id", project.ID).
				Str("reason", decision.Reason).
				Msg("webhook: create-task proceeding despite soft budget breach")
			if s.budgetNotifier != nil {
				period, level := decision.Period()
				s.budgetNotifier.NotifyBudgetBreach(ctx, project.ID, level, period, decision)
			}
		}
	}
	return webhookAdmission{ok: true}
}

// ListWebhookEvents handles GET /api/v1/projects/{projectId}/webhooks/events.
func (s *Server) ListWebhookEvents(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.webhookEventRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "WEBHOOK_AUDIT_NOT_CONFIGURED", "webhook audit is not configured")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	filter := persistence.WebhookEventFilter{ProjectID: &projectID, PageSize: limit}
	if source := strings.TrimSpace(r.URL.Query().Get("source")); source != "" {
		filter.Source = &source
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		filter.Status = &status
	}
	events, err := s.webhookEventRepo.List(r.Context(), filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list webhook events")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"events": events, "total": len(events)})
}

func (s *Server) createWebhookTask(ctx context.Context, project *registry.Project, source registry.ProjectWebhookSource, taskType, idempotencyKey string, taskContext, forgeJob json.RawMessage, workflowOverride string) (*persistence.Task, error) {
	if existing, err := s.taskRepo.GetByIdempotencyKey(ctx, project.ID, idempotencyKey); err == nil && existing != nil {
		return existing, nil
	}
	workflowID := source.WorkflowID
	if workflowOverride != "" {
		workflowID = workflowOverride
	}
	if workflowID == "" {
		workflowID = project.DefaultWorkflowID
	}
	priority := source.Priority
	if priority == 0 {
		priority = project.DefaultPriority
	}
	payload, err := marshalTaskPayload(CreateTaskRequest{
		TaskType:       taskType,
		Priority:       priority,
		WorkflowID:     workflowID,
		IdempotencyKey: idempotencyKey,
		Context:        taskContext,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal webhook task payload: %w", err)
	}
	// Merge the classified forge_job in at the payload top level (the forge.*
	// handlers read it there) without disturbing the context object shape.
	if len(forgeJob) > 0 {
		payload, err = mergeTopLevelJSON(payload, "forge_job", forgeJob)
		if err != nil {
			return nil, fmt.Errorf("merge forge_job into webhook task payload: %w", err)
		}
	}
	now := time.Now()
	task := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      project.ID,
		WorkflowID:     strPtr(workflowID),
		IdempotencyKey: &idempotencyKey,
		CreationSource: persistence.TaskCreationSourceUser,
		Status:         persistence.TaskStatusQueued,
		Priority:       priority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// Atomic hard-cap reservation (trading-hardening §1): the read-only
	// budget Check in admitWebhookTask is best-effort; this claims the
	// estimate against the cap before insert so concurrent webhooks can't
	// overshoot. FAIL OPEN on a ledger error; a Blocked decision refuses.
	if s.reservRepo != nil && s.llmUsageRepo != nil {
		decision, rerr := budget.Reserve(ctx, s.reservRepo, s.llmUsageRepo, project, task.ID, now.UTC())
		if rerr != nil {
			s.logger.Warn().Err(rerr).Str("project_id", project.ID).Msg("webhook: budget reserve failed — proceeding")
		} else if decision.Blocked {
			if s.budgetNotifier != nil {
				period, level := decision.Period()
				s.budgetNotifier.NotifyBudgetBreach(ctx, project.ID, level, period, decision)
			}
			return nil, fmt.Errorf("budget exceeded: %s", decision.Reason)
		}
	}
	if err := s.taskRepo.Create(ctx, task); err != nil {
		if existing, getErr := s.taskRepo.GetByIdempotencyKey(ctx, project.ID, idempotencyKey); getErr == nil && existing != nil {
			return existing, nil
		}
		return nil, err
	}
	if s.queue != nil {
		if err := s.queue.Enqueue(task.ID, project.ID, priority); err != nil {
			return nil, err
		}
	}
	if s.rateLimiter != nil {
		s.rateLimiter.Record(project.ID, time.Now())
	}
	return task, nil
}

func (s *Server) existingWebhookTask(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, bool) {
	if s.taskRepo == nil {
		return nil, false
	}
	task, err := s.taskRepo.GetByIdempotencyKey(ctx, projectID, idempotencyKey)
	return task, err == nil && task != nil
}

func (s *Server) recordWebhookEvent(ctx context.Context, projectID, sourceName, eventID string, body []byte, status string, taskID *string, code, message string) {
	if s.webhookEventRepo == nil {
		return
	}
	if status == persistence.WebhookEventStatusRejected && !s.allowWebhookRejectedAudit(projectID, sourceName, time.Now()) {
		s.logger.Warn().Str("project_id", projectID).Str("source", sourceName).Msg("dropping webhook rejected audit event due to rate limit")
		return
	}
	if eventID == "" && len(body) > 0 {
		eventID = hashWebhookBody(body)
	}
	eventID = truncateWebhookAuditField(eventID, 512)
	event := &persistence.WebhookEvent{
		ID:           persistence.GenerateID("webhook_evt"),
		ProjectID:    projectID,
		Source:       sourceName,
		EventID:      eventID,
		PayloadHash:  fullWebhookBodyHash(body),
		Status:       status,
		TaskID:       taskID,
		ErrorCode:    code,
		ErrorMessage: truncateWebhookAuditField(message, 2048),
		CreatedAt:    time.Now(),
	}
	if err := s.webhookEventRepo.Record(ctx, event); err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).Str("source", sourceName).Msg("failed to record webhook audit event")
	}
}

// webhookRejectGCStride amortizes the OTHER-key sweep across this many
// calls. The map is bounded in practice by the configured (project,
// source) pair count — IngestWebhook 404s before recording for unknown
// project/source — so the dominant cost is mutex hold time, not
// total work. Stride 64 keeps each call O(1) while still draining
// stale keys regularly.
const webhookRejectGCStride = 64

func (s *Server) allowWebhookRejectedAudit(projectID, sourceName string, now time.Time) bool {
	key := projectID + "/" + sourceName
	s.webhookRejectMu.Lock()
	defer s.webhookRejectMu.Unlock()
	if s.webhookRejectLog == nil {
		s.webhookRejectLog = make(map[string][]time.Time)
	}
	cutoff := now.Add(-1 * time.Minute)

	// Amortized GC: every Nth call sweeps OTHER keys whose entries
	// have all aged out. Pre-fix this sweep ran on every rejection
	// while holding the mutex — fine for tiny deployments, but on
	// hosts with many configured webhook sources every rejection
	// blocks every other rejection on the same lock for the duration
	// of a full map walk. Striding the sweep keeps the per-call cost
	// O(1) without letting stale keys live forever.
	s.webhookRejectGCCounter++
	if s.webhookRejectGCCounter%webhookRejectGCStride == 0 {
		for k, ts := range s.webhookRejectLog {
			if k == key {
				continue
			}
			fresh := 0
			for fresh < len(ts) && ts[fresh].Before(cutoff) {
				fresh++
			}
			if fresh >= len(ts) {
				delete(s.webhookRejectLog, k)
			}
		}
	}

	events := s.webhookRejectLog[key]
	first := 0
	for first < len(events) && events[first].Before(cutoff) {
		first++
	}
	events = events[first:]
	if len(events) >= maxWebhookRejectedAuditEventsPerMinute {
		s.webhookRejectLog[key] = events
		return false
	}
	events = append(events, now)
	s.webhookRejectLog[key] = events
	return true
}

func truncateWebhookAuditField(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func extractWebhookPath(r *http.Request) (string, string) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "api" && parts[i+1] == "v1" && parts[i+2] == "webhooks" {
			return parts[i+3], pathPart(parts, i+4)
		}
	}
	return "", ""
}

func pathPart(parts []string, idx int) string {
	if idx >= 0 && idx < len(parts) {
		return parts[idx]
	}
	return ""
}

func findWebhookSource(project *registry.Project, name string) (registry.ProjectWebhookSource, bool) {
	for _, source := range project.Webhooks.Sources {
		if source.Name == name {
			return source, true
		}
	}
	return registry.ProjectWebhookSource{}, false
}

func verifyWebhookSignature(r *http.Request, body []byte, source registry.ProjectWebhookSource) error {
	secret := source.Secret
	if secret == "" && source.SecretEnv != "" {
		secret = os.Getenv(source.SecretEnv)
	}
	if secret == "" {
		return fmt.Errorf("webhook secret is not configured")
	}
	sig := r.Header.Get("X-Vornik-Signature")
	if sig == "" {
		sig = r.Header.Get("X-Hub-Signature-256")
	}
	sig = strings.TrimPrefix(strings.TrimSpace(sig), "sha256=")
	if sig == "" {
		return fmt.Errorf("missing signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	sigBytes, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(sigBytes, want) {
		return fmt.Errorf("signature mismatch")
	}
	// Finding B5: optional replay/timestamp window for the generic
	// webhook path. The Slack channel already enforces a 5-minute
	// window; this brings parity to operator-configured generic
	// sources without breaking existing ones. Opt-in and backward-
	// compatible — unconfigured sources keep body-HMAC-only behavior.
	if err := verifyWebhookTimestampWindow(r, source); err != nil {
		return err
	}
	return nil
}

// verifyWebhookTimestampWindow enforces an OPTIONAL timestamp window for
// a generic webhook source (Finding B5). It is opt-in via two env vars
// keyed by the upper-cased source name (mirroring how SecretEnv already
// pulls webhook config from the environment, so no config-schema change
// is required):
//
//   - VORNIK_WEBHOOK_TS_HEADER_<SOURCE>    request header carrying a
//     Unix timestamp (seconds).
//   - VORNIK_WEBHOOK_TS_TOLERANCE_<SOURCE> Go duration (e.g. "5m").
//
// When EITHER is unset the check is a no-op (preserves current behavior
// for every existing webhook). When configured, the request's timestamp
// header must be present, parseable, and within ±tolerance of now —
// otherwise the request is rejected as a stale/replayed (or
// future-skewed) delivery. The HMAC already covers the body; this closes
// the capture-and-replay gap for sources that send a signed timestamp.
func verifyWebhookTimestampWindow(r *http.Request, source registry.ProjectWebhookSource) error {
	if r == nil {
		return nil
	}
	envKey := webhookSourceEnvSuffix(source.Name)
	headerName := os.Getenv("VORNIK_WEBHOOK_TS_HEADER_" + envKey)
	toleranceRaw := os.Getenv("VORNIK_WEBHOOK_TS_TOLERANCE_" + envKey)
	if headerName == "" || toleranceRaw == "" {
		return nil // not configured — preserve existing behavior
	}
	tolerance, err := time.ParseDuration(toleranceRaw)
	if err != nil || tolerance <= 0 {
		// Misconfiguration: fail closed for an opted-in source rather
		// than silently disabling the protection.
		return fmt.Errorf("webhook timestamp tolerance misconfigured")
	}
	tsRaw := strings.TrimSpace(r.Header.Get(headerName))
	if tsRaw == "" {
		return fmt.Errorf("missing webhook timestamp header")
	}
	tsSec, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid webhook timestamp header")
	}
	delta := time.Since(time.Unix(tsSec, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		return fmt.Errorf("webhook timestamp outside tolerance window")
	}
	return nil
}

// webhookSourceEnvSuffix upper-cases a source name and replaces any
// non-alphanumeric byte with '_' so it can be appended to an env-var
// prefix (env var names are conventionally [A-Z0-9_]).
func webhookSourceEnvSuffix(name string) string {
	var b strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteRune(c - 32)
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// webhookTemplateRefRE matches ${dotted.path} references in a
// task-type template. The capturing group is the path; valueAtPath
// resolves it against the JSON-decoded webhook body. Anything that
// isn't a valid identifier path expands to the empty string.
var webhookTemplateRefRE = regexp.MustCompile(`\$\{([^}]+)\}`)

// renderWebhookTemplate substitutes ${path.to.field} references
// against the webhook body. Replaces the previous text/template
// implementation, per the 2026-05-03 audit:
//
//   - text/template supports method calls on arbitrary values; if
//     a future caller passes anything richer than the JSON map
//     used today, the template language could be coerced into
//     calling those methods.
//   - missingkey=zero silently coerced missing keys to empty
//     strings, producing partially-garbled task types instead of
//     surfacing the config bug.
//
// The new shape is restricted variable substitution: only
// ${name.path} references resolve, and missing paths yield "" so
// the caller's existing emptiness check still catches malformed
// configs at validation time. Operator-friendly format ports as
// `{{ index .issue "title" }}` → `${issue.title}`.
func renderWebhookTemplate(tmpl string, event map[string]any) (string, error) {
	if tmpl == "" {
		return "webhook event", nil
	}
	out := webhookTemplateRefRE.ReplaceAllStringFunc(tmpl, func(match string) string {
		path := strings.TrimSpace(match[2 : len(match)-1])
		return valueAtPath(event, path)
	})
	return strings.TrimSpace(out), nil
}

// mergeTopLevelJSON sets key=value at the top level of a JSON object, preserving
// all existing keys. Used to attach the classified forge_job to a task payload
// without re-modeling the payload struct or touching the context object.
func mergeTopLevelJSON(payload []byte, key string, value json.RawMessage) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	obj[key] = value
	return json.Marshal(obj)
}

// matchesWebhookFilter reports whether the event satisfies a source's filter.
// Empty filter → true (no filtering). Forms:
//
//	${path}         → true when valueAtPath is non-empty
//	${path}=a,b,c   → true when valueAtPath equals one of the comma-listed values
//
// A malformed filter (no ${...}; rejected at config-load) conservatively
// returns false so a misconfigured source drops rather than floods.
func matchesWebhookFilter(filter string, event map[string]any) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	ref, want, hasEq := strings.Cut(filter, "=")
	got := resolveFilterRef(strings.TrimSpace(ref), event)
	if !hasEq {
		return got != ""
	}
	for _, v := range strings.Split(want, ",") {
		if got == strings.TrimSpace(v) {
			return true
		}
	}
	return false
}

// resolveFilterRef resolves a "${path}" reference against the event, or returns
// "" for anything that isn't a well-formed reference.
func resolveFilterRef(ref string, event map[string]any) string {
	if strings.HasPrefix(ref, "${") && strings.HasSuffix(ref, "}") {
		return valueAtPath(event, strings.TrimSpace(ref[2:len(ref)-1]))
	}
	return ""
}

func valueAtPath(m map[string]any, path string) string {
	if path == "" {
		return ""
	}
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[part]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}

func webhookEventID(event map[string]any, path, headerID string, body []byte) string {
	eventID := strings.TrimSpace(valueAtPath(event, path))
	if eventID != "" {
		return eventID
	}
	if headerID != "" {
		return headerID
	}
	return hashWebhookBody(body)
}

func webhookEventIDFromBodyOrHeader(body []byte, path, headerID string) string {
	if len(body) > 0 {
		var event map[string]any
		if err := json.Unmarshal(body, &event); err == nil {
			if id := webhookEventID(event, path, headerID, body); id != "" {
				return id
			}
		}
	}
	if headerID != "" {
		return headerID
	}
	return hashWebhookBody(body)
}

func hashWebhookBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

func fullWebhookBodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
