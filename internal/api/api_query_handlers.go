package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/apiaccess"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taintlineage"
)

// Agent-facing third-party API endpoints (design
// https://docs.vornik.io §2, §5,
// §5c). A task agent calls query_api / list_apis for ITS OWN project with
// every agent-path control applied SERVER-SIDE — the agent cannot opt out:
//
//	POST /api/v1/projects/{projectId}/api/query      → AgentQueryAPI
//	GET  /api/v1/projects/{projectId}/api/providers  → AgentListAPIProviders
//
// Pipeline (design Architecture):
//
//	auth (key.project == PATH project; ProjectAuthMiddleware + defense-in-depth)
//	 → resolve task/role/execution from the authenticated key + headers
//	 → request-size cap (reject over-size body BEFORE apiaccess)
//	 → per-task budget check (call-count + cumulative bytes)
//	 → apiaccess.Query  (shared capability gate; agent read-only default)
//	 → outputguard ScanWithProvenance + Redact (agent has no dispatcher pass)
//	 → byte cap = agent_llm.tool_result_max_bytes + truncation marker
//	 → attribution audit {provider, method, path, query-hash+len, status,
//	   bytes, task_id, role, execution_id, kind}
//	 → respond
//
// SECURITY: the caller is an LLM whose args are prompt-injectable and which
// runs in a tool loop. The controls therefore never rely on LLM good
// behavior — they are the daemon-side allowlist, read-only-for-agents, the
// per-task budget, and the response redaction/cap. The credential never
// leaves the gateway; project scope comes from the PATH, never the body.

const (
	// maxAgentAPICallsPerTask bounds how many query_api + list_apis calls
	// one task may make (design §5 per-task budget). It caps the
	// loop-amplification a prompt-injected agent can be driven into — a
	// bound a human chat caller can't be pushed past.
	maxAgentAPICallsPerTask = 100
	// maxAgentAPIBytesPerTask bounds the cumulative response bytes one
	// task may pull back through the gateway (design §5 per-task budget).
	maxAgentAPIBytesPerTask int64 = 8 << 20 // 8 MiB
	// maxAgentQueryRequestBytes caps the query_api POST body — the
	// request-side mirror of the response byte cap (design §5). All
	// LLM-controlled params (provider/method/path/query/body) travel in
	// this body, so bounding it bounds the request-side exfil vector.
	maxAgentQueryRequestBytes int64 = 64 << 10 // 64 KiB
	// maxAgentListQueryLen caps the list_apis ?query= filter length (the
	// only LLM-controlled param on the discovery path).
	maxAgentListQueryLen = 1 << 10 // 1 KiB
	// defaultToolResultMaxBytes is the response byte cap when
	// agent_llm.tool_result_max_bytes is 0 (config default 256 KiB).
	defaultToolResultMaxBytes = 256 << 10
	// agentAPIBudgetSeedPageSize bounds the audit re-derivation query.
	// A task can never accrue more than maxAgentAPICallsPerTask agent API
	// rows (the budget refuses further calls), so this is a safe ceiling.
	agentAPIBudgetSeedPageSize = maxAgentAPICallsPerTask*2 + 16
	// maxTrackedAgentAPITasks bounds daemon-resident counters. Evicted tasks
	// safely reconstruct from persisted audit on their next call.
	maxTrackedAgentAPITasks = 10_000
	// maxAuditPathLen caps the raw request path persisted in the audit row
	// (F1). Unlike query/body (hashed — the primary exfil channel), the path
	// is kept verbatim for operator observability (it is the called route),
	// but capped so it cannot become an unbounded secondary exfil channel.
	// The residual is bounded by the request-size cap (design §5).
	maxAuditPathLen = 256
)

// Audit tool-name identities. These match the dispatcher's tool names so
// dashboards see one identity per capability across the chat + agent
// surfaces; agent rows are distinguished from chat rows by a non-empty
// TaskID (the chat path writes TaskID=""). Reusing the ToolAudit surface
// (not a parallel table) satisfies design §5's "extend, don't invent".
const (
	agentQueryToolName = "query_api"
	agentListToolName  = "list_apis"
)

// Audit status labels recorded in the attribution payload.
const (
	agentAPIStatusOK             = "ok"
	agentAPIStatusRefused        = "refused"
	agentAPIStatusBudgetExceeded = "budget_exceeded"
	agentAPIStatusOversize       = "oversize"
)

// agentAPIAuditPayload is the attribution blob stored in the audit row's
// ToolInput. The raw query is NEVER stored — only its hash + length (design
// §5: the query is the exfil channel). The credential never appears here.
type agentAPIAuditPayload struct {
	Kind      string `json:"kind"` // "query" | "list"
	Provider  string `json:"provider,omitempty"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	QueryHash string `json:"query_hash"`
	QueryLen  int    `json:"query_len"`
	Role      string `json:"role,omitempty"`
	Status    string `json:"status"`
	Bytes     int64  `json:"bytes"`
	// Agent-write policy trail (LLD 2026-07-22; write methods only). Mode is the
	// resolved gateway.agent_writes; WalkOutcome + RootTaskID + CreationSource
	// come from the origin walk (every mode records them — "not_walked"/nil/
	// "unknown" under off). Empty on reads.
	AgentWritesMode string `json:"agent_writes_mode,omitempty"`
	WalkOutcome     string `json:"walk_outcome,omitempty"`
	RootTaskID      string `json:"root_task_id,omitempty"`
	CreationSource  string `json:"creation_source,omitempty"`
}

// apiTaskSpend is the running per-task budget counter.
type apiTaskSpend struct {
	calls    int
	bytes    int64
	lastUsed time.Time
}

// apiBudgetTracker is the daemon-resident per-task budget (design §5, §5c).
// It is re-derivable from the audit rows: on first sight of a task the
// counter is seeded from the persisted audit (so a mid-task daemon restart
// does not launder the control open). The zero value is ready to use.
type apiBudgetTracker struct {
	mu   sync.Mutex
	seen map[string]*apiTaskSpend
}

// reserveCall seeds from audit on first sight, checks both ceilings, and —
// when under budget — records one additional call. Returns ("", true) when
// the call may proceed; (reason, false) when a ceiling is already reached.
//
// A refused capability/gateway call STILL consumes a call slot: an LLM can
// hammer the gateway with refused calls just as readily as accepted ones, so
// the call-count ceiling must bound both. Only response bytes (added via
// addBytes on success) count toward the byte ceiling.
func (b *apiBudgetTracker) reserveCall(taskID string, seed func() (apiTaskSpend, error)) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = make(map[string]*apiTaskSpend)
	}
	sp := b.seen[taskID]
	if sp == nil {
		s, err := seed()
		if err != nil {
			return "per-task API budget state unavailable; refusing the call until persisted usage can be verified.", false
		}
		if len(b.seen) >= maxTrackedAgentAPITasks {
			var oldestID string
			var oldest time.Time
			for id, candidate := range b.seen {
				if oldestID == "" || candidate.lastUsed.Before(oldest) {
					oldestID, oldest = id, candidate.lastUsed
				}
			}
			delete(b.seen, oldestID)
		}
		sp = &s
		b.seen[taskID] = sp
	}
	sp.lastUsed = time.Now()
	if sp.calls >= maxAgentAPICallsPerTask {
		return fmt.Sprintf(
			"per-task API call budget exhausted (%d calls); no further query_api/list_apis calls will run for this task.",
			maxAgentAPICallsPerTask,
		), false
	}
	if sp.bytes >= maxAgentAPIBytesPerTask {
		return fmt.Sprintf(
			"per-task API byte budget exhausted (%d bytes returned); no further query_api/list_apis calls will run for this task.",
			maxAgentAPIBytesPerTask,
		), false
	}
	sp.calls++
	return "", true
}

// addBytes adds n response bytes to a task's running byte total. No-op if the
// task was never reserved (defensive).
func (b *apiBudgetTracker) addBytes(taskID string, n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sp := b.seen[taskID]; sp != nil {
		sp.bytes += n
		sp.lastUsed = time.Now()
	}
}

// agentAPIContext carries the attribution resolved from the authenticated
// request. task_id is authoritative (parsed from the task-scoped key's
// name); execution_id comes from the X-Execution-ID header and is validated
// against the task; role is re-derived server-side from the task's current
// step (authoritative, not agent-supplied).
type agentAPIContext struct {
	taskID      string
	executionID string
	role        string
}

// resolveAgentAPIContext derives task/execution/role for the request. The
// task_id is taken from the authenticated key's name (agent:task_<id>) — the
// agent cannot forge it. Returns an error string (and false) on a task/
// execution binding mismatch, which the caller turns into a 403.
func (s *Server) resolveAgentAPIContext(r *http.Request) (agentAPIContext, string, bool) {
	var actx agentAPIContext
	ctx := r.Context()
	if id := IdentityFromContext(ctx); id != nil {
		if row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey); ok && row != nil {
			if boundTaskID, isTaskKey := persistence.TaskIDFromKeyName(row.Name); isTaskKey {
				actx.taskID = boundTaskID
			}
		}
	}
	// F2 (security): these endpoints are task-agent-only. A non-task key
	// (project/admin/session key) has no per-task budget and would bypass the
	// call/byte ceilings entirely — the exact loop-amplification control the
	// design relies on (§5). Reject it (fail-closed) rather than serve it
	// unbounded.
	if actx.taskID == "" {
		return agentAPIContext{}, "these endpoints require a task-scoped API key", false
	}
	actx.executionID = r.Header.Get("X-Execution-ID")
	if err := s.validateExecutionTaskBinding(ctx, actx.taskID, actx.executionID); err != nil {
		return agentAPIContext{}, "X-Execution-ID does not belong to the task-scoped API key", false
	}
	actx.role = s.resolveTaskRole(ctx, actx.taskID)
	return actx, "", true
}

// resolveTaskRole re-derives the calling role from the task's current
// execution step (task → execution.CurrentStepID → workflow step → role).
// Returns "" on any resolution gap. Mirrors the resolution roleAllowsMCPTool
// uses so the audited role is authoritative rather than agent-reported.
func (s *Server) resolveTaskRole(ctx context.Context, taskID string) string {
	if taskID == "" || s.executionRepo == nil || s.projectRegistry == nil {
		return ""
	}
	exec, err := s.executionRepo.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil || exec.CurrentStepID == nil || *exec.CurrentStepID == "" {
		return ""
	}
	_, workflow, err := s.projectRegistry.GetProjectWithWorkflow(exec.ProjectID)
	if err != nil || workflow == nil {
		return ""
	}
	step, ok := workflow.Steps[*exec.CurrentStepID]
	if !ok {
		return ""
	}
	return step.Role
}

// agentAPIAllowlist returns the per-project api_providers resolver injected
// into apiaccess.Service. It loads permissions.api_providers from the
// registry (nil/empty ⇒ all providers allowed — the empty-means-all
// convention). It never errors; a nil registry or absent project resolves to
// nil (empty-means-all). The wide-open posture is warned about once per
// project by warnEmptyAPIAllowlistOnce, not surfaced as an error.
func (s *Server) agentAPIProviders(projectID string) []string {
	if s.projectRegistry == nil {
		return nil
	}
	p := s.projectRegistry.GetProject(projectID)
	if p == nil {
		return nil
	}
	return p.Permissions.APIProviders
}

// warnEmptyAPIAllowlistOnce logs an operator warning the first time the agent
// API surface serves a project whose permissions.api_providers is empty
// (⇒ every registered provider is reachable by the untrusted-LLM path). Once
// per project per process (design §5c empty-allowlist visibility; mirrors
// registry.WarnUnknownAPIProviders).
func (s *Server) warnEmptyAPIAllowlistOnce(projectID string) {
	if len(s.agentAPIProviders(projectID)) != 0 {
		return
	}
	if _, loaded := s.apiAllowlistWarnedProjects.LoadOrStore(projectID, struct{}{}); loaded {
		return
	}
	s.logger.Warn().
		Str("project", projectID).
		Msg("agent query_api enabled on a project with empty permissions.api_providers — every registered provider is reachable by task agents; set an allowlist to narrow the untrusted-LLM path")
}

// toolResultMaxBytes returns the effective response byte cap. Reads
// agent_llm.tool_result_max_bytes directly (the endpoint has no agent env);
// 0 ⇒ the 256 KiB default. This is the same knob the container injects as
// VORNIK_TOOL_RESULT_MAX_BYTES, applied here for the query_api path (design
// §6b: one config knob, two enforcement loci, capped once).
func (s *Server) toolResultMaxBytes() int {
	if s.config != nil {
		if n := s.config.ResolvedAgentLLM().ToolResultMaxBytes; n > 0 {
			return n
		}
	}
	return defaultToolResultMaxBytes
}

// deriveTaskAPISpend re-derives a task's spent budget from the persisted
// audit rows (design §5c). A missing repository, empty task ID, or transient
// store error fails closed: caching a zero seed would let a restart or storage
// outage reset the task's call and byte ceilings. Only agent rows (our two tool
// names, TaskID matched) are summed; chat rows carry an empty TaskID and are
// excluded by the filter.
func (s *Server) deriveTaskAPISpend(ctx context.Context, taskID string) (apiTaskSpend, error) {
	if s.toolAuditRepo == nil || taskID == "" {
		return apiTaskSpend{}, errors.New("tool audit repository unavailable")
	}
	rows, err := s.toolAuditRepo.List(ctx, persistence.ToolAuditFilter{
		TaskID:   &taskID,
		PageSize: agentAPIBudgetSeedPageSize,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).
			Msg("agent api budget: audit re-derivation failed; refusing call")
		return apiTaskSpend{}, err
	}
	var spend apiTaskSpend
	for _, row := range rows {
		if row == nil {
			continue
		}
		if row.ToolName != agentQueryToolName && row.ToolName != agentListToolName {
			continue
		}
		var p agentAPIAuditPayload
		if json.Unmarshal([]byte(row.ToolInput), &p) != nil {
			// Unparseable payload: count it as a consumed call (fail-closed,
			// never launder a slot open) but attribute no bytes.
			spend.calls++
			continue
		}
		// Only rows that actually consumed a live call slot contribute to the
		// re-derived counter, so the seed matches the running counter's
		// accounting (F7). reserveCall increments calls BEFORE apiaccess, so a
		// gateway/policy refusal DID consume a slot (status ok + refused);
		// budget_exceeded (ceiling already hit, never incremented) and oversize
		// (rejected before reserveCall) did NOT — including them inflated the
		// seed and could refuse legitimate calls after a restart. Bytes are
		// added only for successful (ok) rows, matching addBytes.
		switch p.Status {
		case agentAPIStatusOK:
			spend.calls++
			spend.bytes += p.Bytes
		case agentAPIStatusRefused:
			spend.calls++
		default:
			// agentAPIStatusBudgetExceeded / agentAPIStatusOversize: no slot
			// consumed live, so excluded from the re-derived seed.
		}
	}
	return spend, nil
}

// hashAndLen returns the hex SHA-256 of b and its byte length. Used to record
// the query fingerprint + size without persisting the raw (LLM-controlled,
// potentially exfil-bearing) query.
func hashAndLen(b []byte) (string, int) {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), len(b)
}

// writeAgentAPIAudit persists one attribution row via the shared ToolAudit
// surface. Best-effort: an audit failure is logged but never fails the call
// path (same contract as the dispatcher's logAudit + the tool-audit ingest).
func (s *Server) writeAgentAPIAudit(ctx context.Context, projectID string, actx agentAPIContext, p agentAPIAuditPayload) {
	if s.toolAuditRepo == nil {
		return
	}
	toolName := agentQueryToolName
	if p.Kind == "list" {
		toolName = agentListToolName
	}
	input, err := json.Marshal(p)
	if err != nil {
		s.logger.Warn().Err(err).Msg("agent api audit: marshal failed")
		return
	}
	entry := &persistence.ToolAuditEntry{
		ID:          persistence.GenerateID("ta"),
		ProjectID:   projectID,
		TaskID:      actx.taskID,
		ExecutionID: actx.executionID,
		StepID:      "agent-api-endpoint",
		ToolName:    toolName,
		ToolInput:   string(input),
		ToolOutput:  p.Status,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.toolAuditRepo.Log(ctx, entry); err != nil {
		s.logger.Warn().Err(err).Str("tool", toolName).Str("task_id", actx.taskID).
			Msg("agent api audit: persist failed")
	}
}

// AgentQueryRequest is the LLM-facing shape for POST .../api/query. It never
// names a credential — provider + params only (design §3.1). projectId comes
// from the PATH, never this body.
type AgentQueryRequest struct {
	Provider string         `json:"provider"`
	Method   string         `json:"method"`
	Path     string         `json:"path"`
	Query    map[string]any `json:"query"`
	Body     map[string]any `json:"body"`
}

// AgentQueryResponse is the success shape. On a policy refusal the response
// instead carries {"refusal": "..."} with HTTP 200 so the agent's tool loop
// can surface the reason to the LLM for self-correction (mirroring the chat
// tool's refusal text; never a raw Go error).
type AgentQueryResponse struct {
	Body       string `json:"body,omitempty"`
	Provenance string `json:"provenance,omitempty"`
	Bytes      int    `json:"bytes,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Refusal    string `json:"refusal,omitempty"`
}

// agentAPIPreflight runs the checks shared by both agent API handlers:
// project extraction, PATH-only scope (defense in depth over
// ProjectAuthMiddleware — a project-A key must never reach project B's
// endpoint, design §2), gateway-configured, the once-per-project
// empty-allowlist warning, and task/role/execution resolution. On any
// failure it writes the HTTP error and returns ok=false.
func (s *Server) agentAPIPreflight(w http.ResponseWriter, r *http.Request) (string, agentAPIContext, bool) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return "", agentAPIContext{}, false
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "project not allowed")
		return "", agentAPIContext{}, false
	}
	if s.apiGatewayClient == nil {
		respondError(w, http.StatusServiceUnavailable, "API_GATEWAY_DISABLED",
			"third-party API gateway not configured on this daemon")
		return "", agentAPIContext{}, false
	}
	s.warnEmptyAPIAllowlistOnce(projectID)
	actx, bindErr, ok := s.resolveAgentAPIContext(r)
	if !ok {
		respondError(w, http.StatusForbidden, "FORBIDDEN", bindErr)
		return "", agentAPIContext{}, false
	}
	return projectID, actx, true
}

// newAgentAPIService builds the shared capability gate for the agent path.
// Allowlist is loaded internally from the project (no fail-open caller param).
// permitWrite is the pre-resolved gateway.agent_writes decision for THIS
// request (see resolveAgentWrite): the AgentWrites closure returns it verbatim,
// so the write policy is decided once, up front, per request — never a
// package-level singleton, re-resolved on every write attempt (LLD §5.2,
// review A1/A3). Read callers (ListProviders) pass false; reads never reach the
// write gate, so the value is immaterial there.
func (s *Server) newAgentAPIService(permitWrite bool) *apiaccess.Service {
	return &apiaccess.Service{
		Client: s.apiGatewayClient,
		Allowlist: func(p string) ([]string, error) {
			return s.agentAPIProviders(p), nil
		},
		AgentWrites: func(_, _ string) bool { return permitWrite },
	}
}

// reserveAgentBudget applies the per-task budget BEFORE apiaccess (design
// pipeline). Returns ("", true) when the call may proceed (including for a
// non-task key, which has no budget). A refused call still consumes a call
// slot; only success adds bytes (via apiBudget.addBytes at the call sites).
func (s *Server) reserveAgentBudget(r *http.Request, actx agentAPIContext) (string, bool) {
	if actx.taskID == "" {
		return "", true
	}
	return s.apiBudget.reserveCall(actx.taskID, func() (apiTaskSpend, error) {
		return s.deriveTaskAPISpend(r.Context(), actx.taskID)
	})
}

// AgentQueryAPI handles POST /api/v1/projects/{projectId}/api/query.
func (s *Server) AgentQueryAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported")
		return
	}
	projectID, actx, ok := s.agentAPIPreflight(w, r)
	if !ok {
		return
	}
	req, audit, method, ok := s.parseAgentQueryRequest(w, r, projectID, actx)
	if !ok {
		return
	}

	if reason, ok := s.reserveAgentBudget(r, actx); !ok {
		audit.Status = agentAPIStatusBudgetExceeded
		s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)
		respondJSON(w, http.StatusOK, AgentQueryResponse{Refusal: reason})
		return
	}

	// Agent-write policy (LLD 2026-07-22): resolve the origin ONCE for write
	// methods (reads never reach the gate, so never pay for a walk) and let the
	// resolved permit drive the per-request AgentWrites closure. The resolution
	// also seeds the audit row + the write counter, so every mode is
	// correlatable — including a refused write under off.
	isWrite := !apiaccess.IsReadMethod(method)
	var res writeResolution
	if isWrite {
		res = s.resolveAgentWrite(r.Context(), actx.taskID)
		audit.AgentWritesMode = res.mode
		audit.WalkOutcome = res.walkOutcome
		audit.RootTaskID = res.rootTaskID
		audit.CreationSource = res.creationSource

		// Taint-lineage gate (taint-lineage-tracking §4.4): after the WHO gate
		// (resolveAgentWrite) permits, also consult WHAT informed the write. In
		// enforce mode a tainted (or Unknown, or incomplete-walk) lineage
		// SYNCHRONOUSLY REFUSES the write — same control-flow shape as an
		// agent_writes refusal, no park (D5). advisory allows + flags + metric;
		// off is a no-op.
		taint := s.resolveTaintReview(r.Context(), projectID, actx.taskID)
		switch taint.mode {
		case taintlineage.ModeEnforce:
			if taint.requiresReview {
				audit.Status = agentAPIStatusRefused
				s.recordAgentWrite(res, false)
				s.recordTaintWrite(taint.mode, "refused")
				s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)
				respondJSON(w, http.StatusOK, AgentQueryResponse{
					Refusal: "This write was refused: it was derived from untrusted content and this project enforces taint-lineage review. An operator must review the untrusted sources before this API write can proceed.",
				})
				return
			}
			// Allowed under enforce (untainted / matching latch) → permitted (M6).
			s.recordTaintWrite(taint.mode, "permitted")
		case taintlineage.ModeAdvisory:
			if taint.tainted {
				s.recordTaintWrite(taint.mode, "flagged")
			} else {
				// Allowed + untainted → permitted, the §14 calibration denominator (M6).
				s.recordTaintWrite(taint.mode, "permitted")
			}
		}

		// EU AI Act Art 50(1) publication gate (G6 finding B): a write to a
		// provider whose readers are people must carry the disclosure. Runs
		// LAST of the write gates — after WHO (agent_writes) and WHAT (taint) —
		// because it is the only one that inspects the payload, and there is no
		// point vetting content for a write the policy gates already refused.
		if reason := s.agentPublicationRefusal(req.Provider, req.Path, req.Body); reason != "" {
			audit.Status = agentAPIStatusRefused
			s.recordAgentWrite(res, false)
			s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)
			respondJSON(w, http.StatusOK, AgentQueryResponse{Refusal: reason})
			return
		}
	}

	outcome := s.newAgentAPIService(res.permit).Query(r.Context(), projectID, actx.role, apigateway.Request{
		Provider: req.Provider,
		Method:   method,
		Path:     req.Path,
		Query:    req.Query,
		Body:     req.Body,
	})
	if outcome.Refusal != "" {
		audit.Status = agentAPIStatusRefused
		if isWrite {
			s.recordAgentWrite(res, false)
		}
		s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)
		respondJSON(w, http.StatusOK, AgentQueryResponse{Refusal: outcome.Refusal})
		return
	}
	if isWrite {
		s.recordAgentWrite(res, true)
	}

	// Redaction (agent has no dispatcher output_guard pass; design §2) then
	// byte cap + truncation marker (agent can't opt out; design §6b).
	scanned := s.redactAgentAPIBody(outcome.Body, outcome.Provenance)
	capped, truncated := capToolResultBytes(scanned, s.toolResultMaxBytes())

	audit.Status = agentAPIStatusOK
	audit.Bytes = int64(len(capped))
	if actx.taskID != "" {
		s.apiBudget.addBytes(actx.taskID, int64(len(capped)))
	}
	s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)

	respondJSON(w, http.StatusOK, AgentQueryResponse{
		Body:       capped,
		Provenance: "third_party",
		Bytes:      len(capped),
		Truncated:  truncated,
	})
}

// parseAgentQueryRequest enforces the request-size cap BEFORE apiaccess
// (design §5c placement — all LLM-controlled params travel in this body),
// decodes the request, computes the query fingerprint (hash + length; the
// raw query is never stored — it is the exfil channel, design §5), and seeds
// the attribution audit. On any failure it writes the HTTP error (auditing an
// over-size attempt for exfil visibility) and returns ok=false.
func (s *Server) parseAgentQueryRequest(w http.ResponseWriter, r *http.Request, projectID string, actx agentAPIContext) (AgentQueryRequest, agentAPIAuditPayload, string, bool) {
	body, err := readLimitedBody(w, r, maxAgentQueryRequestBytes)
	if err != nil {
		var tooLarge bodyTooLargeError
		if errors.As(err, &tooLarge) {
			s.writeAgentAPIAudit(r.Context(), projectID, actx, agentAPIAuditPayload{
				Kind: "query", Role: actx.role, Status: agentAPIStatusOversize,
			})
			respondError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE",
				"query_api request body exceeds the size cap")
			return AgentQueryRequest{}, agentAPIAuditPayload{}, "", false
		}
		respondError(w, http.StatusBadRequest, "READ_FAILED", "failed to read request body")
		return AgentQueryRequest{}, agentAPIAuditPayload{}, "", false
	}
	var req AgentQueryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body: "+err.Error())
		return AgentQueryRequest{}, agentAPIAuditPayload{}, "", false
	}

	fingerprint, _ := json.Marshal(struct {
		Path  string         `json:"path"`
		Query map[string]any `json:"query"`
		Body  map[string]any `json:"body"`
	}{Path: req.Path, Query: req.Query, Body: req.Body})
	queryHash, queryLen := hashAndLen(fingerprint)

	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = "GET"
	}
	audit := agentAPIAuditPayload{
		Kind:      "query",
		Provider:  req.Provider,
		Method:    strings.ToUpper(method),
		Path:      capAuditPath(req.Path),
		QueryHash: queryHash,
		QueryLen:  queryLen,
		Role:      actx.role,
	}
	return req, audit, method, true
}

// capAuditPath bounds the raw path persisted in the audit (F1) to
// maxAuditPathLen bytes, backing off to a UTF-8 rune boundary so the stored
// value stays valid text. The path is kept (not hashed) for operator
// observability; the cap keeps it from being an unbounded exfil channel.
func capAuditPath(p string) string {
	if len(p) <= maxAuditPathLen {
		return p
	}
	cut := maxAuditPathLen
	for cut > 0 && !utf8RuneStart(p[cut]) {
		cut--
	}
	return p[:cut]
}

// AgentProvidersResponse is the shape for GET .../api/providers.
type AgentProvidersResponse struct {
	Providers []apigateway.ProviderInfo `json:"providers"`
	Count     int                       `json:"count"`
	Refusal   string                    `json:"refusal,omitempty"`
}

// AgentListAPIProviders handles GET /api/v1/projects/{projectId}/api/providers.
// A discovery call is in-budget and audited (kind=list) — an LLM can hammer
// discovery too (design §5c).
func (s *Server) AgentListAPIProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	projectID, actx, ok := s.agentAPIPreflight(w, r)
	if !ok {
		return
	}

	query := r.URL.Query().Get("query")
	if len(query) > maxAgentListQueryLen {
		respondError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE",
			"list_apis query filter exceeds the size cap")
		return
	}
	queryHash, queryLen := hashAndLen([]byte(query))
	audit := agentAPIAuditPayload{
		Kind:      "list",
		QueryHash: queryHash,
		QueryLen:  queryLen,
		Role:      actx.role,
	}

	if reason, ok := s.reserveAgentBudget(r, actx); !ok {
		audit.Status = agentAPIStatusBudgetExceeded
		s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)
		respondJSON(w, http.StatusOK, AgentProvidersResponse{Refusal: reason})
		return
	}

	// list_apis is a read (provider discovery); it never reaches the write gate,
	// so the write permit is immaterial — pass false.
	providers, _, err := s.newAgentAPIService(false).ListProviders(r.Context(), projectID, query)
	if err != nil {
		audit.Status = agentAPIStatusRefused
		s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)
		respondJSON(w, http.StatusOK, AgentProvidersResponse{
			Refusal: fmt.Sprintf("could not list API providers for project %q.", projectID),
		})
		return
	}
	if providers == nil {
		providers = []apigateway.ProviderInfo{}
	}
	// F4: the discovery catalog gets NO apiaccess redaction pass — its free-text
	// Description/Examples reach the LLM verbatim. Run the secret-class scan
	// (first-party) so a credential-shaped token in a provider Description can't
	// leak through the discovery path.
	providers = s.redactProviderCatalog(providers)

	// The discovery payload is small; still bound the audit byte figure by
	// the serialized length so the budget accounting stays consistent.
	payload, _ := json.Marshal(providers)
	audit.Status = agentAPIStatusOK
	audit.Bytes = int64(len(payload))
	if actx.taskID != "" {
		s.apiBudget.addBytes(actx.taskID, int64(len(payload)))
	}
	s.writeAgentAPIAudit(r.Context(), projectID, actx, audit)

	respondJSON(w, http.StatusOK, AgentProvidersResponse{
		Providers: providers,
		Count:     len(providers),
	})
}

// agentAPIScan is the output-guard entry point, indirected through a package
// var so the fail-closed panic path (F3b) is unit-testable — production code
// always uses outputguard.ScanWithProvenance.
var agentAPIScan = outputguard.ScanWithProvenance

// agentContentScanError is the safe stub returned when the content scan panics
// (F3b, fail-closed): the raw, unscanned body is NEVER returned to the LLM.
const agentContentScanError = "[response withheld: content-scan error]"

// redactAgentAPIBody runs the output guard over a gateway response body and
// redacts findings in place — the agent-path equivalent of the dispatcher's
// applyOutputGuard (which the agent path does not traverse).
//
// F3a: the outputguard secret-class rules are NOT all HIGH — KindAdversarialURL
// (a credential-shaped token/api_key/password/secret/auth query param on a URL)
// fires at WARN, and KindEncodedPayload (long base64/hex blobs) at INFO. Only
// data:text/… adversarial URLs are HIGH. Redacting HIGH-only therefore leaked
// credential-in-URL findings straight to the LLM. We now redact any HIGH
// finding (injection or secret) AND any secret-class finding at WARN ("Medium")
// or above — the credential-leak cases. INFO-level encoded blobs are left
// (legit base64 images / hashes; below the Medium floor).
//
// F3b: on a guard panic we fail CLOSED — log and return a safe stub, never the
// raw unscanned body.
func (s *Server) redactAgentAPIBody(body string, prov outputguard.Provenance) (out string) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error().Interface("panic", rec).
				Msg("agent api: output-guard scan panicked; withholding response (fail-closed)")
			out = agentContentScanError
		}
	}()
	rep := agentAPIScan(body, prov)
	if !rep.HasFinding() {
		return body
	}
	return redactAgentAPIFindings(body, rep)
}

// redactAgentAPIFindings redacts the findings that qualify under the agent-path
// policy (see redactAgentAPIBody / F3a): every HIGH finding, plus every
// secret-class finding at WARN or above. Qualifying findings are promoted to
// HIGH so outputguard.Redact (which only splices HIGH spans) removes them,
// reusing the shared marker format.
func redactAgentAPIFindings(body string, rep outputguard.Report) string {
	promoted := make([]outputguard.Finding, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		redact := f.Severity == outputguard.SeverityHigh ||
			(isSecretClassKind(f.Kind) && f.Severity == outputguard.SeverityWarn)
		if !redact {
			continue
		}
		f.Severity = outputguard.SeverityHigh
		promoted = append(promoted, f)
	}
	if len(promoted) == 0 {
		return body
	}
	return outputguard.Redact(body, outputguard.Report{Findings: promoted})
}

// isSecretClassKind reports whether a finding Kind belongs to the outputguard
// secret class (credential leakage / encoded payloads) rather than the
// injection class. Finding does not carry the rule class, so we classify by
// Kind; mirrors outputguard's rule table (KindAdversarialURL / KindEncodedPayload
// are classSecret).
func isSecretClassKind(k outputguard.Kind) bool {
	switch k {
	case outputguard.KindAdversarialURL, outputguard.KindEncodedPayload:
		return true
	}
	return false
}

// redactProviderCatalog runs the secret-class scan over each provider's
// free-text fields (Description + Examples) before the discovery payload is
// returned to the LLM (F4). The catalog is first-party (operator-authored), so
// injection-class rules are skipped (ProvenanceFirstParty) but secret-class
// rules still run — a credential-shaped token in a Description is redacted.
// Reuses redactAgentAPIBody so the redaction policy stays single-sourced.
func (s *Server) redactProviderCatalog(providers []apigateway.ProviderInfo) []apigateway.ProviderInfo {
	out := make([]apigateway.ProviderInfo, len(providers))
	for i, p := range providers {
		p.Description = s.redactAgentAPIBody(p.Description, outputguard.ProvenanceFirstParty)
		if len(p.Examples) > 0 {
			ex := make([]string, len(p.Examples))
			for j, e := range p.Examples {
				ex[j] = s.redactAgentAPIBody(e, outputguard.ProvenanceFirstParty)
			}
			p.Examples = ex
		}
		out[i] = p
	}
	return out
}

// capToolResultBytes truncates s to at most maxBytes bytes and, when it
// truncated, appends a visible marker so the LLM knows the response was cut
// (design §6b byte cap + truncation marker). maxBytes <= 0 disables the cap.
// Truncation is on a UTF-8 rune boundary so the body stays valid text.
func capToolResultBytes(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	marker := fmt.Sprintf("\n\n[truncated: response exceeded %d bytes]", maxBytes)
	if maxBytes <= len(marker) {
		return marker[:maxBytes], true
	}
	cut := maxBytes - len(marker)
	// Back off to a rune boundary so we don't split a multibyte sequence.
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker, true
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not
// a 0b10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}
