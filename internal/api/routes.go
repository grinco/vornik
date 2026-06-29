// Package api provides HTTP routing for the vornik data plane API.
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/ratelimit"
)

// parseGitRequest decodes a /api/v1/git/ smart-HTTP request path into its
// project ID, service suffix, and resolved gitService, applying the
// service-param gate. It is the SINGLE source of truth for git routing shared
// between registerGitRoutes (production) and gitRegisteredRouter (tests) so the
// two never drift (Task 1.4 carryover discipline).
//
// Returns ok=false when the request is not a routable git endpoint — the
// caller responds 404. When ok=true, svc is the service the request maps to
// (upload for reads, receive for pushes).
//
// Routable shapes:
//
//	info/refs?service=git-upload-pack   → upload (read advertisement)
//	info/refs?service=git-receive-pack  → receive (push advertisement)
//	git-upload-pack    (POST)           → upload (read pack negotiation)
//	git-receive-pack   (POST)           → receive (push pack transfer)
//
// Everything else — dumb protocol (info/refs with no/unknown service), object
// paths, HEAD — returns ok=false.
func parseGitRequest(urlPath, serviceParam string) (projectID string, svc gitService, ok bool) {
	const prefix = "/api/v1/git/"
	rest := strings.TrimPrefix(urlPath, prefix)

	// First path segment (e.g. "proj_foo.git"); strip the ".git" suffix.
	seg := rest
	if idx := strings.Index(rest, "/"); idx >= 0 {
		seg = rest[:idx]
	}
	projectID = strings.TrimSuffix(seg, ".git")

	suffix := ""
	if len(rest) > len(seg)+1 {
		suffix = rest[len(seg)+1:]
	}

	switch suffix {
	case "info/refs":
		switch serviceParam {
		case "git-upload-pack":
			return projectID, gitServiceUpload, true
		case "git-receive-pack":
			return projectID, gitServiceReceive, true
		default:
			// Dumb protocol (no/unknown service) is rejected at the Go
			// layer — defense in depth before reaching git-http-backend.
			return "", gitServiceUpload, false
		}
	case "git-upload-pack":
		return projectID, gitServiceUpload, true
	case "git-receive-pack":
		return projectID, gitServiceReceive, true
	default:
		return "", gitServiceUpload, false
	}
}

// registerGitRoutes mounts the git smart-HTTP catch-all under /api/v1/git/ on
// mux. It must be called inside the caps.ServeAPI block.
//
// Go's net/http 1.22 mux cannot match a wildcard segment with a literal dot
// (e.g. {projectID}.git), so we register a plain prefix catch-all and parse
// the project ID + service via parseGitRequest — mirroring what
// gitRegisteredRouter does in tests.
//
// Both the read path (git-upload-pack: clone/fetch) and the write path
// (git-receive-pack: push, gated by allow_push + serialized by the workspace
// lock) are served. Each request is wrapped by the auth middleware bound to
// the service parseGitRequest resolved, so the read/write capability gate runs
// against the right service.
func registerGitRoutes(mux *http.ServeMux, server *Server, authEnabled bool, caps config.NodeCapabilities) {
	if server == nil {
		return
	}

	// Emit a prominent warning when git routes are exposed without auth.
	// This is a one-time warning at registration, not per-request.
	if !authEnabled {
		server.logger.Warn().
			Msg("git-over-HTTPS: auth_enabled=false — git routes are unauthenticated; any client can clone AND PUSH to all workspaces")
	}

	// Multi-node precondition warning (design §4.7): the workspace lock is
	// process-local, so it only protects the workspace when git is served
	// from the SAME node that executes tasks. When this node serves the API
	// but does NOT run workers, the served repo is not the one tasks mutate
	// and the lock cannot serialize pushes against execution. Single-node is
	// the v1 precondition; the cross-node pg-advisory lock + shared storage
	// is the planned lift.
	if !caps.RunWorkers {
		server.logger.Warn().
			Msg("git-over-HTTPS: this node serves the API but does NOT run workers (RunWorkers=false) — the process-local workspace lock cannot protect pushes against task execution on another node; git-over-HTTPS assumes the git endpoint is co-located with execution + workspace (single-node precondition, design §4.7)")
	}

	// Build the auth middleware. When auth is disabled, gitHTTPAuth stamps
	// anonymous context and calls next; when enabled it validates the key
	// and (for the receive service) the allow_push capability.
	adminCheck := func(rawKey string) bool {
		if server.adminConfig.Enabled {
			return server.adminConfig.IsAdminKey(rawKey)
		}
		return false
	}
	authMW := gitHTTPAuth(server.apiKeyRepo, adminCheck, authEnabled)
	uploadHandler := authMW(gitServiceUpload, http.HandlerFunc(server.GitHTTPBackend))
	receiveHandler := authMW(gitServiceReceive, http.HandlerFunc(server.GitHTTPBackend))

	mux.HandleFunc("/api/v1/git/", func(w http.ResponseWriter, r *http.Request) {
		projectID, svc, ok := parseGitRequest(r.URL.Path, r.URL.Query().Get("service"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		r.SetPathValue("projectID", projectID)
		if svc == gitServiceReceive {
			receiveHandler.ServeHTTP(w, r)
			return
		}
		uploadHandler.ServeHTTP(w, r)
	})
}

// configHandlers holds the config API handlers.
// This is set via SetConfigHandlers before routing is set up.
var configHandlers *ConfigHandlers

// doctorHandlers holds the doctor API handlers.
var doctorHandlers *DoctorHandlers

// SetDoctorHandlers sets the doctor handlers for the API router.
func SetDoctorHandlers(handlers *DoctorHandlers) {
	doctorHandlers = handlers
}

// Router wraps the HTTP handler with all routes configured.
type Router struct {
	handler http.Handler
	server  *Server
}

// SetConfigHandlers sets the config handlers for the API router.
// This must be called before NewRouter if config endpoints are needed.
func SetConfigHandlers(handlers *ConfigHandlers) {
	configHandlers = handlers
}

// NewRouter creates a new API router with all handlers and middleware configured.
func NewRouter(server *Server, cfg *config.Config) *Router {
	// Create mux
	mux := http.NewServeMux()

	// Resolve node capabilities once — gates the five route-group regions below.
	// A nil or zero-value cfg resolves to "all" (every capability on), which is
	// today's default and preserves byte-for-byte backwards compatibility.
	caps := config.ResolveNodeProfile(config.NodeConfig{})
	if cfg != nil {
		caps = config.ResolveNodeProfile(cfg.Node)
	}

	// Health endpoints (unauthenticated) - Kubernetes probe conventions.
	// /livez and /readyz follow the k8s split: /livez is the liveness
	// probe (always 200 while the process is alive — used to decide
	// whether to kill -9 the container); /readyz is the readiness probe
	// (flips to 503 during drain so load balancers stop routing).
	// /healthz stays as a /livez alias for older operators + scripts.
	mux.HandleFunc("/livez", server.Livez)
	mux.HandleFunc("/healthz", server.Livez) // Liveness alias (was Healthz; same body now)
	mux.HandleFunc("/readyz", server.Readyz)
	mux.HandleFunc("/health/live", server.Livez)   // Liveness probe alias
	mux.HandleFunc("/health/ready", server.Readyz) // Readiness probe alias

	// Metrics endpoint - Prometheus metrics.
	//
	// A5 (audit 2026-06-10): /metrics is exempted from AuthMiddleware
	// and its Prometheus labels carry project IDs and api-key IDs, so
	// an open /metrics on the LAN-reachable API port lets an
	// unauthenticated scraper enumerate tenant structure. When
	// metrics.require_admin is set AND auth is enabled, gate the
	// main-port /metrics behind the admin key; the intended scrape
	// target then becomes the dedicated loopback metrics listener
	// (metrics.addr). Default OFF preserves the open scrape for
	// single-tenant / trusted-network deployments. /metrics is exempt
	// from AuthMiddleware, so the key is read from the request
	// directly via extractAPIKey rather than from context.
	var metricsHandler http.Handler
	if server.metricsRegistry != nil {
		metricsHandler = promhttp.HandlerFor(server.metricsRegistry, promhttp.HandlerOpts{})
	} else {
		metricsHandler = promhttp.Handler()
	}
	if server.config != nil && server.config.Metrics.RequireAdmin && server.config.API.AuthEnabled {
		inner := metricsHandler
		scrapeToken := server.config.Metrics.ScrapeToken
		metricsHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := extractAPIKey(r)
			// Accept the dedicated read-only scrape token (constant-time)
			// OR the admin key. The scrape token lets Prometheus
			// authenticate without holding admin.
			okToken := scrapeToken != "" && key != "" &&
				subtle.ConstantTimeCompare([]byte(key), []byte(scrapeToken)) == 1
			if !okToken && (key == "" || !server.adminConfig.IsAdminKey(key)) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="vornik-metrics"`)
				http.Error(w, "metrics authentication required (audit A5): present the scrape token or admin key", http.StatusUnauthorized)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	mux.Handle("/metrics", metricsHandler)

	// iOS Safari root-path icon probes (unauthenticated, unconditional).
	//
	// When an operator adds the vornik UI to an iOS home screen, Safari
	// probes the well-known root paths /apple-touch-icon.png,
	// /apple-touch-icon-precomposed.png, and /favicon.ico BEFORE following
	// any <link rel="apple-touch-icon"> tags. These paths are exempt from
	// AuthMiddleware (isPublicEndpoint returns true for them); the handlers
	// redirect to their /ui/static/ equivalents, which are already served
	// by the auth-exempt /ui/static/ prefix. Registered unconditionally
	// (outside caps gates) — icon probes happen regardless of which node
	// role is active, as long as the HTTP server is reachable.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/static/favicon.ico", http.StatusFound)
	})
	mux.HandleFunc("/apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/static/apple-touch-icon.png", http.StatusFound)
	})
	mux.HandleFunc("/apple-touch-icon-precomposed.png", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/static/apple-touch-icon.png", http.StatusFound)
	})

	// Region 1: data-plane / admin / control endpoints — only on ServeAPI nodes.
	if caps.ServeAPI {

		// Config management endpoints. These go through the auth middleware chain
		// applied at the bottom of this function (applyMiddleware wraps the mux),
		// so they require a valid API key when auth is enabled.
		if configHandlers != nil {
			mux.HandleFunc("/api/v1/config/reload-status", configHandlers.GetReloadStatus)
			mux.HandleFunc("/api/v1/config/reload", configHandlers.Reload)
		}
		// GET /api/v1/config — effective config dump with secrets redacted.
		// Registered only when the server carries a config reference (it
		// always does in service.Container, but direct-handler tests may not).
		mux.HandleFunc("/api/v1/config", server.GetConfig)

		// GET /api/v1/live/fleet — fleet "Now Running" SSE feed (UI F2).
		// Project-scoped event-driven refresh trigger for the live grid.
		mux.HandleFunc("/api/v1/live/fleet", server.FleetLive)

		// Doctor endpoint (authenticated)
		if doctorHandlers != nil {
			mux.HandleFunc("/api/v1/doctor", doctorHandlers.RunDoctor)
			// Feature-doctor read surface: list all features + per-feature diagnosis.
			// The exact-match route wins for the bare "/features" path; the
			// trailing-slash prefix route handles both "/features/{id}" lookups and
			// "/features/{id}/enable" calls — routeFeaturePath dispatches by suffix
			// (/enable → EnableFeature, otherwise → GetFeature).
			mux.HandleFunc("/api/v1/doctor/features", doctorHandlers.ListFeatures)
			mux.HandleFunc("/api/v1/doctor/features/", doctorHandlers.routeFeaturePath)
		}

		// Admin API surface — admin-ui-design.md slice 1. Today this
		// is just the audit-list endpoint that backs `vornikctl admin
		// audit`. The handler enforces both the admin.enabled gate
		// (404 when disabled) and the admin-key allowlist (403 when
		// auth is enabled but the key isn't admin).
		mux.HandleFunc("/api/v1/admin/audit", server.AdminAuditList)
		// Chat-audit list endpoint — CLI mirror of /ui/admin/chat-audit.
		// One row per dispatcher turn (system prompt hash, model, tool
		// calls, response excerpt, cost). Same admin gate matrix as
		// /admin/audit; surfaces operator-visible turns only.
		mux.HandleFunc("/api/v1/admin/chat-audit", server.AdminChatAuditList)
		// Workflow telemetry rollup — Slice 1 of the memetic-workflows
		// arc. Returns per-workflow execution-evidence the architect
		// agent (Slice 2) will consume. Daemon-wide rollup; same admin
		// gate matrix as /admin/audit.
		mux.HandleFunc("/api/v1/admin/workflow-stats", server.AdminWorkflowStats)
		// Workflow architect propose — Slice 2c of the memetic-workflows
		// arc. POST runs one architect turn; the inserted pending
		// proposal returns as JSON for the operator approval UI. Same
		// admin gate matrix as /admin/audit.
		mux.HandleFunc("/api/v1/admin/workflow-architect/propose", server.AdminWorkflowArchitectPropose)
		// Workflow proposals review — Slice 3a of the memetic-workflows
		// arc. List + show + decide (approve/reject). The exact-path
		// handler covers list; the trailing-slash handler routes
		// per-id GET and POST /decide.
		mux.HandleFunc("/api/v1/admin/workflow-proposals", server.AdminWorkflowProposalsList)
		mux.HandleFunc("/api/v1/admin/workflow-proposals/", server.AdminWorkflowProposalsItem)

		// Cross-project call admin surface — inter-project
		// orchestration Phase D follow-on. List + show + force-
		// resolve operator actions on the cross_project_calls
		// ledger. Same admin-key gate as /admin/audit.
		mux.HandleFunc("/api/v1/admin/cpc", server.AdminCPCList)
		mux.HandleFunc("/api/v1/admin/cpc/", server.adminCPCRouter)

		// Support-report bundle endpoint — admin-gated. Builds the
		// server-collectable, already-redacted core bundle and streams
		// it as tar.gz; vornikctl augments with host-only sections.
		// See https://docs.vornik.io
		mux.HandleFunc("/api/v1/support-report", server.SupportReport)

		// Autonomy Black Box trace endpoint — Phase A read-side
		// surface. Returns the chronologically-merged unified
		// trace for one task assembled from the nine audit tables.
		// Same admin-key gate as /admin/audit.
		mux.HandleFunc("/api/v1/admin/blackbox/traces/", server.AdminBlackBoxTraces)
		// Phase C counterfactual replay surface — engine + scorecard
		// + side-effect classifier. See internal/blackbox/counterfactual.go.
		mux.HandleFunc("/api/v1/admin/blackbox/replay", server.AdminBlackBoxReplay)
		mux.HandleFunc("/api/v1/admin/blackbox/scorecard/", server.AdminBlackBoxScorecard)
		mux.HandleFunc("/api/v1/admin/blackbox/sideeffects", server.AdminBlackBoxSideEffects)
		// Policy-Aware Memory Firewall admin surface — Phase C.
		mux.HandleFunc("/api/v1/admin/memory/policy/evaluations", server.AdminMemoryFirewallEvaluations)
		mux.HandleFunc("/api/v1/admin/memory/policy/evaluations.csv", server.AdminMemoryFirewallEvaluationsCSV)
		// Proof-verifier: all evaluations recorded under one policy digest.
		// Prefix-registered so {digest} is read from the path tail. The
		// exact-match "evaluations" route above wins for the bare path; the
		// "/digest/" suffix routes here (drift-mitigation §8.3 — was 404).
		mux.HandleFunc("/api/v1/admin/memory/policy/evaluations/digest/", server.AdminMemoryFirewallEvaluationsByDigest)
		mux.HandleFunc("/api/v1/admin/memory/policy/mode", server.AdminMemoryFirewallMode)
		mux.HandleFunc("/api/v1/admin/memory/policy/chunks/", server.AdminMemoryFirewallChunkPolicy)

		// Companion-plugin admin surface (LLD 21). Mints + lists
		// per-session bearer keys scoped to one project + an optional
		// workflow allowlist + an optional USD budget cap. Admin-gated;
		// revocation reuses the existing /api/v1/projects/{id}/keys/{kid}
		// DELETE handler since a companion key is just an api_keys row
		// with extra scope columns.
		mux.HandleFunc("/api/v1/admin/companion/grant", server.CompanionGrant)
		mux.HandleFunc("/api/v1/admin/companion/keys", server.CompanionKeysList)

		// Continuous-learning instinct layer — read/inspect/retire surfaces.
		// The list + per-id router go through the normal auth chain (no
		// admin scope required: instincts are advisory evidence, reading
		// them is a low-privilege operation). The recompute endpoint is
		// admin-gated (it re-derives every matching instinct's confidence).
		// All four are read/inspect/retire only — they NEVER mutate
		// behaviour. See continuous-learning-instinct-layer-design.md.
		mux.HandleFunc("/api/v1/instincts", server.ListInstincts)
		mux.HandleFunc("/api/v1/instincts/", server.instinctsRouter)
		mux.HandleFunc("/api/v1/admin/instincts/recompute", server.AdminInstinctsRecompute)

		// Companion MCP server (LLD 21). Speaks MCP JSON-RPC over HTTP;
		// the host-LLM plugin connects with the bearer key minted via
		// /api/v1/admin/companion/grant. The handler resolves the key
		// row inside each call so AllowedWorkflows / BudgetCapUSD enforce
		// even if AuthMiddleware's lighter check only verified the
		// shape + active state.
		mux.HandleFunc("/api/v1/mcp/companion", server.CompanionMCPHandler)

		// Workflow-healing triggers — Black Box Phase B. The
		// hourly detector writes triggers when a workflow's last-24h
		// roll-up regresses against the 7-day baseline. Operators
		// list + dismiss + feed evidence to the memetic architect.
		mux.HandleFunc("/api/v1/admin/workflow-healing/triggers", server.AdminHealingTriggersList)
		mux.HandleFunc("/api/v1/admin/workflow-healing/triggers/", server.adminHealingTriggersItem)
		// Override surface (Phase B operator-tuning, migration 81) —
		// per-(project, workflow, class) threshold + mute knobs the
		// detector consults. GET lists, POST upserts, POST .../delete
		// removes. The single trailing-slash route covers all three
		// because adminHealingOverridesRouter dispatches by suffix +
		// method.
		mux.HandleFunc("/api/v1/admin/workflow-healing/overrides", server.adminHealingOverridesRouter)
		mux.HandleFunc("/api/v1/admin/workflow-healing/overrides/delete", server.adminHealingOverridesRouter)
		// Healing candidates (Self-Healing Workflow Genome v1) — GET the
		// candidate + its trial scorecard history; POST run-trial (operator-
		// triggered, no background loop); POST promote (gate + memetic apply,
		// refuses without trial_passed); POST reject. adminHealingCandidatesItem
		// dispatches /{id} + /{id}/{action} by suffix.
		mux.HandleFunc("/api/v1/admin/workflow-healing/candidates/", server.adminHealingCandidatesItem)

		// Scheduled reminders surface — read + cancel only. Creation
		// is driven by the dispatcher's set_reminder tool, not the
		// REST API (the LLM-side contract is where reminders are
		// born in v1). See https://docs.vornik.io
		mux.HandleFunc("/api/v1/reminders", server.ListReminders)
		mux.HandleFunc("/api/v1/reminders/", server.remindersRouter)

		// Operator profiles surface — backs the `vornikctl operator`
		// CLI (list / show / set / forget) + future external
		// integrations. Same allow-list + rationale-required semantics
		// the dispatcher's update_operator_profile tool enforces.
		// Nil-repo deployments respond 503 from the handlers.
		mux.HandleFunc("/api/v1/operators", server.ListOperators)
		mux.HandleFunc("/api/v1/operators/", server.operatorsRouter)

		// API v1 routes - using path prefix matching (Go < 1.22 compatible)
		// Method checking is done in handlers or via methodHandler wrapper
		// Daemon-level MCP discovery. Mirrors the per-project /mcp/tools
		// endpoint but at the daemon scope — operators consume this to
		// inventory what MCP servers are installed system-wide without
		// hand-walking every project YAML. Read-only; NEVER grants a
		// project access to tools it hasn't explicitly declared.
		mux.HandleFunc("/api/v1/mcp/servers", server.ListMCPServers)
		// Daemon capabilities — discovery endpoint companion plugins
		// (Claude Code, Codex, opencode, etc.) call on first connect to
		// learn the daemon's version, transports, feature flags, and the
		// project/workflow scope of the calling key. See LLD 21.
		mux.HandleFunc("/api/v1/capabilities", server.GetCapabilities)
		mux.HandleFunc("/api/v1/memory/stats", server.MemoryStats)
		mux.HandleFunc("/api/v1/memory/cache-stats", server.MemoryCacheStats)
		mux.HandleFunc("/api/v1/projects/wizard/converse", server.ProjectWizardConverse)
		mux.HandleFunc("/api/v1/projects/wizard/", server.projectWizardRouter)
		mux.HandleFunc("/api/v1/setup/status", server.SetupStatus)
		mux.HandleFunc("/api/v1/setup/models", server.SetupModels)
		mux.HandleFunc("/api/v1/setup/session", server.SetupSessionCreate)
		mux.HandleFunc("/api/v1/setup/session/", server.setupSessionRouter)
		mux.HandleFunc("/api/v1/memory/backfill-titles", server.MemoryBackfillTitles)
		mux.HandleFunc("/api/v1/memory/reclassify-llm", server.MemoryReclassifyLLM)
		// KG re-flag — operator-driven backfill of isolated entities.
		// Project-scoped; flips needs_graph_extraction = TRUE on chunks
		// that produced zero edges so the KG worker reprocesses them.
		// See https://docs.vornik.io §"KG ingestion pipeline".
		mux.HandleFunc("/api/v1/memory/regraph", server.MemoryRegraph)
		mux.HandleFunc("/api/v1/executions/", server.apiV1ExecutionsHandler)
		mux.HandleFunc("/api/v1/projects/", server.apiV1ProjectsHandler)
		mux.HandleFunc("/api/v1/projects", server.ListProjects) // unscoped list (no trailing slash)
		// Project template gallery (2026.6.0 SaaS-readiness).
		//   GET  /api/v1/project-templates       — list available templates
		//   POST /api/v1/projects/from-template  — materialise a template
		// The POST path is registered as a SPECIFIC exact match BEFORE
		// the catch-all /api/v1/projects/ prefix above would otherwise
		// route "from-template" as a project ID. ServeMux's
		// longest-prefix-match resolves the exact match first.
		mux.HandleFunc("/api/v1/project-templates", server.ListProjectTemplates)
		mux.HandleFunc("/api/v1/projects/from-template", server.CreateProjectFromTemplate)
		mux.HandleFunc("/api/v1/swarms", server.ListSwarms)
		mux.HandleFunc("/api/v1/swarms/", server.apiV1SwarmsHandler)
		mux.HandleFunc("/api/v1/workflows", server.ListWorkflows)
		mux.HandleFunc("/api/v1/workflows/", server.apiV1WorkflowsHandler)
		// Fleet observability — slice C1. Read-only; no admin gate required
		// (cluster topology is non-sensitive operational data).
		mux.HandleFunc("/api/v1/cluster", server.ClusterStatus)

		// Git smart-HTTP read path (Slice 1). The catch-all prefix route
		// /api/v1/git/ is registered here; authentication and service-suffix
		// validation are handled inside registerGitRoutes.
		gitAuthEnabled := cfg != nil && cfg.API.AuthEnabled
		registerGitRoutes(mux, server, gitAuthEnabled, caps)

	} // end region 1: ServeAPI

	// Regions 2 & 4 (webhook ingress) mount on BOTH ServeAPI nodes (trusted
	// subnet, local ingest) and ServeWebhooks nodes (DMZ). A DMZ webhook node
	// has RunWorkers=false so it cannot execute task logic. Slice B adds the
	// mTLS relay so DMZ nodes forward verified events to the job tier instead
	// of writing to the DB directly. See cluster-topology-and-roles-design.md.

	// Region 2: public webhook ingress — on ServeAPI OR ServeWebhooks nodes.
	if caps.ServeAPI || caps.ServeWebhooks {
		mux.HandleFunc("/api/v1/webhooks/", server.IngestWebhook)
	} // end region 2: ServeAPI || ServeWebhooks

	// Region 3: A2A protocol surface — only on ServeAPI nodes.
	if caps.ServeAPI {

		// A2A protocol surface. Card endpoints under /.well-known/
		// are public (per spec — clients fetch before they have
		// credentials); task submit + SSE under /a2a/v1/agents/ go
		// through the same auth chain as the rest of /api. The
		// handler is nil unless WithA2AHandler was set, so an
		// operator who hasn't opted in shows nothing to scanners.
		if server.a2aHandler != nil {
			mux.HandleFunc("/.well-known/agent.json", server.a2aHandler.HandleWellKnown)
			mux.HandleFunc("/.well-known/agent.json/", server.a2aHandler.HandleWellKnown)
			mux.HandleFunc("/a2a/v1/agents/", server.a2aHandler.HandleAgentRoute)
		}

	} // end region 3: ServeAPI

	// Region 4: provider-webhook ingress (GitHub App, Slack) — on ServeAPI OR ServeWebhooks.
	if caps.ServeAPI || caps.ServeWebhooks {

		// GitHub App webhook — mounted only when an operator has
		// configured a github_app block on at least one project and
		// the service container has wired the handler via
		// WithGitHubAppWebhookHandler. Leaving the route unmounted
		// when unconfigured 404s deliveries, which is a clearer
		// failure mode than a 401 from a stub handler.
		if server.githubAppWebhook != nil {
			mux.HandleFunc("/api/v1/github-app/webhook", server.githubAppWebhook)
		}
		// Slack Events API webhook — mounted only when an operator has
		// configured a slack block on at least one project and the
		// service container has wired the handler via
		// WithSlackWebhookHandler. Same 404-when-unmounted posture as
		// the GitHub App route above.
		if server.slackWebhook != nil {
			mux.HandleFunc("/api/v1/slack/webhook", server.slackWebhook)
		}

	} // end region 4: ServeAPI || ServeWebhooks

	// Region 5: chat proxy, Ollama compat, playbook, internal agent endpoints — only on ServeAPI.
	if caps.ServeAPI {

		// Internal OpenAI-compatible chat completions proxy. Only registered
		// when a chat provider is configured; otherwise leaving the route
		// unmapped lets a stray call 404 instead of 503, which is the
		// clearer failure mode when the dispatcher is intentionally off.
		//
		// Both /api/v1/... (the daemon's canonical surface) and /v1/...
		// (what OpenAI SDKs default to) are wired. Third-party clients
		// configured with base_url = https://vornik.example/v1 reach
		// the same handlers, with the same Bearer-token auth and the
		// same OpenAI-shaped ChatRequest/ChatResponse envelopes the
		// agent containers already use. Streaming (stream:true) is
		// still out of scope for this slice — handler 400s the request.
		if server.chatProvider != nil {
			mux.HandleFunc("/api/v1/chat/completions", server.ChatCompletions)
			mux.HandleFunc("/v1/chat/completions", server.ChatCompletions)
			// Model discovery: aggregates ListModels across every chat
			// sub-provider that supports it. Lives next to the proxy so
			// "is this provider wired" is the same gate for both.
			mux.HandleFunc("/api/v1/models", server.ListModels)
			mux.HandleFunc("/v1/models", server.ListModels)

			// Ollama compat layer (2026-05-16). Translation handlers
			// covering the endpoints Ollama-native clients (Open WebUI,
			// LobeChat, Ollama CLI) reach by default. Routes are
			// specific enough that they don't shadow the canonical
			// /api/v1/... surface — Go's ServeMux uses longest-prefix
			// match. Streaming uses NDJSON framing (one JSON document
			// per line) which is Ollama's native shape.
			mux.HandleFunc("/api/tags", server.OllamaTags)
			mux.HandleFunc("/api/chat", server.OllamaChat)
			mux.HandleFunc("/api/generate", server.OllamaGenerate)
			mux.HandleFunc("/api/version", server.OllamaVersion)
			mux.HandleFunc("/api/show", server.OllamaShow)
			// Root handler: Ollama-native clients ping GET / before
			// any other call to confirm "Ollama is running". Returns
			// the banner on the exact "/" path and 404 for any other
			// path not claimed by a more-specific route, so it doubles
			// as the api mux's default-404 sink. The top-level mux
			// mounts /ui at higher priority via longest-prefix-match,
			// so the UI tree is unaffected.
			mux.HandleFunc("/", server.OllamaRoot)
		}
		// Failure-class playbook. Always available — the corpus is
		// daemon-version-pinned metadata, no chat dependency.
		mux.HandleFunc("/api/v1/playbook", server.Playbook)
		mux.HandleFunc("/api/v1/playbook/", server.Playbook)

		// Realtime tool-call audit ingest. Agents POST one row per
		// tool call as it completes, instead of waiting for the
		// post-step batch in result.json. Idempotent on audit_id so
		// the streaming + batch paths can both fire safely.
		mux.HandleFunc("/api/v1/internal/tool-audit", server.IngestToolAudit)
		// LLM usage streaming. Agent calls this after every iteration
		// with cumulative numbers; deterministic ID
		// (`tu_<task>_<step>_<role>`) makes successive calls upsert into
		// the same row instead of inserting duplicates. Gives cancelled
		// tasks a real cost summary even though step-finalize never
		// runs.
		mux.HandleFunc("/api/v1/internal/llm-usage", server.IngestLLMUsage)
		// Trading order audit stream. The broker MCP's AuditWriter
		// posts one of these per place_order / place_bracket_order
		// call (success or refused). Idempotent on (project_id,
		// idempotency_key) so the broker's retry-on-failure loop
		// can re-post the same row without producing duplicates.
		// See trading-broker-design.md → "Audit Channel".
		mux.HandleFunc("/api/v1/internal/trading-orders", server.IngestTradingOrder)
		// Trading safety event audit stream — kill-switch toggles,
		// breaker trips, cap refusals, idempotency replay hits.
		// Append-only (PRIMARY KEY on id keeps retries idempotent).
		mux.HandleFunc("/api/v1/internal/trading-safety-events", server.IngestTradingSafetyEvent)
		// Trading fill audit stream — Phase 3. The broker MCP's
		// poll loop posts one row per fill it observes. ON
		// CONFLICT (id) DO NOTHING so retries are silent no-ops.
		mux.HandleFunc("/api/v1/internal/trading-fills", server.IngestTradingFill)
		// Shadow fill audit stream — exec-reconcile shadow mode
		// (Task 14). Accepts the same wire shape as /trading-fills
		// but writes to trading_fills_shadow via RecordShadow for
		// side-by-side comparison without touching the live table.
		// Same auth/method guards as the live route.
		mux.HandleFunc("/api/v1/internal/trading-fills-shadow", server.IngestTradingFillShadow)
		// Trading state replay — broker MCP boot hydration. GET
		// returns today's UTC fills + last-hour orders so the
		// safety envelope's dayTurnover and orderTimes survive
		// broker-mcp recreates. Without this every env-var change
		// or image rebuild zeroes the day's turnover / rate-limit
		// state. Scoped to one project per call.
		mux.HandleFunc("/api/v1/internal/trading-state-replay", server.GetTradingStateReplay)

	} // end region 5: ServeAPI

	// Apply middleware chain
	handler := applyMiddleware(mux, cfg, server)

	// Wrap with API metrics middleware if Prometheus registry is available.
	if server.metricsRegistry != nil {
		apiMetrics := NewAPIMetrics(server.metricsRegistry)
		server.apiMetrics = apiMetrics
		handler = apiMetrics.Middleware(handler)
	}

	// Structured access log — emits one line per request with
	// method/path/query/remote/UA/status/bytes/duration. 5xx
	// promote to warn, 4xx to info, the rest stay at debug so
	// the happy path doesn't drown high-volume health probes.
	// Applied last (= outermost) so it sees the final response
	// status post-auth-middleware rewriting, not the inner
	// handler's pre-auth response.
	handler = AccessLogMiddleware(server.logger.With().Str("component", "http").Logger())(handler)

	return &Router{
		handler: handler,
		server:  server,
	}
}

// apiV1ExecutionsHandler routes requests under /api/v1/executions/{executionId}/
func (s *Server) apiV1ExecutionsHandler(w http.ResponseWriter, r *http.Request) {
	// Extract execution ID from path
	executionID := extractExecutionID(r)
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "executionId is required")
		return
	}

	path := r.URL.Path
	// Remove the /api/v1/executions/{executionId} prefix to get remaining path
	prefix := "/api/v1/executions/" + executionID
	remaining := strings.TrimPrefix(path, prefix)

	// Route based on remaining path and method
	switch remaining {
	case "", "/":
		// GET /executions/{executionId}
		if r.Method == http.MethodGet {
			s.GetExecution(w, r)
			return
		}
	case "/pause", "/pause/":
		// POST /executions/{executionId}/pause
		if r.Method == http.MethodPost {
			s.PauseExecution(w, r)
			return
		}
	case "/resume", "/resume/":
		// POST /executions/{executionId}/resume
		if r.Method == http.MethodPost {
			s.ResumeExecution(w, r)
			return
		}
	case "/retry-from-step", "/retry-from-step/":
		// POST /executions/{executionId}/retry-from-step
		if r.Method == http.MethodPost {
			s.RetryExecutionFromStep(w, r)
			return
		}
	case "/fork-from-step", "/fork-from-step/":
		// POST /executions/{executionId}/fork-from-step
		// Failure-forensics Feature #1 Phase B.
		if r.Method == http.MethodPost {
			s.ForkExecutionFromStep(w, r)
			return
		}
	case "/live", "/live/":
		// GET /executions/{executionId}/live — WebSocket upgrade
		// for the Feature #3 live observation surface.
		if r.Method == http.MethodGet {
			s.ExecutionLive(w, r, executionID)
			return
		}
	case "/hints", "/hints/":
		// POST /executions/{executionId}/hints — operator-
		// injected mid-execution hint (Feature #3 Phase C).
		// GET — list all hints for the live view's history pane.
		if r.Method == http.MethodPost {
			s.ExecutionHintCreate(w, r, executionID)
			return
		}
		if r.Method == http.MethodGet {
			s.ExecutionHintList(w, r, executionID)
			return
		}
	}

	http.NotFound(w, r)
}

// apiV1SwarmsHandler routes requests under /api/v1/swarms/{swarmId}. The
// list endpoint at /api/v1/swarms (no trailing slash) is registered
// separately on the mux. Right now the only subpath is the swarm detail
// GET; leaving it as a dedicated router makes adding future role-level
// endpoints (GET /api/v1/swarms/{id}/roles/{role}) a one-line change.
func (s *Server) apiV1SwarmsHandler(w http.ResponseWriter, r *http.Request) {
	id := extractPathSegmentAfter(r, "swarms")
	if id == "" {
		// /api/v1/swarms/ with nothing after — treat as the list.
		s.ListSwarms(w, r)
		return
	}
	s.GetSwarm(w, r)
}

// apiV1WorkflowsHandler mirrors apiV1SwarmsHandler for workflows.
func (s *Server) apiV1WorkflowsHandler(w http.ResponseWriter, r *http.Request) {
	id := extractPathSegmentAfter(r, "workflows")
	if id == "" {
		s.ListWorkflows(w, r)
		return
	}
	s.GetWorkflow(w, r)
}

// apiV1ProjectsHandler routes requests under /api/v1/projects/{projectId}/
func (s *Server) apiV1ProjectsHandler(w http.ResponseWriter, r *http.Request) {
	// Extract project ID from path
	projectID := extractProjectID(r)
	if projectID == "" {
		// /api/v1/projects/ with nothing after — delegate to the list
		// endpoint so the trailing-slash form and the no-slash form
		// behave the same.
		if r.Method == http.MethodGet {
			s.ListProjects(w, r)
			return
		}
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}

	path := r.URL.Path
	// Remove the /api/v1/projects/{projectId}/ prefix to get remaining path
	prefix := "/api/v1/projects/" + projectID
	remaining := strings.TrimPrefix(path, prefix)

	// Route based on remaining path and method
	if strings.HasPrefix(remaining, "/tasks/") {
		// Could be GET /tasks/{taskId}, GET /tasks/{taskId}/logs,
		// POST /tasks/{taskId}/cancel, POST /tasks/{taskId}/retry, or GET /tasks
		taskPart := strings.TrimSuffix(strings.TrimPrefix(remaining, "/tasks/"), "/")
		if taskPart == "" || taskPart == "list" {
			// GET /tasks - list tasks
			if r.Method == http.MethodGet {
				s.ListTasks(w, r)
				return
			}
		} else {
			// Conversational task lifecycle (Phase 24) — match
			// /messages and /messages/{id}/answer first; they share
			// the /tasks/{id}/messages prefix that the suffix
			// matchers below would otherwise miss.
			if strings.HasSuffix(taskPart, "/messages") {
				switch r.Method {
				case http.MethodGet:
					s.ListTaskMessages(w, r)
					return
				case http.MethodPost:
					s.PostTaskMessage(w, r)
					return
				}
			} else if strings.Contains(taskPart, "/messages/") && strings.HasSuffix(taskPart, "/answer") {
				if r.Method == http.MethodPost {
					s.AnswerCheckpoint(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/amend") {
				if r.Method == http.MethodPost {
					s.AmendBrief(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/pause") {
				if r.Method == http.MethodPost {
					s.PauseTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/resume") {
				if r.Method == http.MethodPost {
					s.ResumeTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/approve") {
				// POST /tasks/{taskId}/approve — autonomy manual-approval
				// gate: AWAITING_APPROVAL → QUEUED.
				if r.Method == http.MethodPost {
					s.ApproveTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/reject") {
				// POST /tasks/{taskId}/reject — AWAITING_APPROVAL → CANCELLED.
				if r.Method == http.MethodPost {
					s.RejectTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/close") {
				if r.Method == http.MethodPost {
					s.CloseTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/summarize") {
				// Phase 32 — lead calls this to compress older
				// messages into a `note`. Filtered out of the
				// prompt window on subsequent executions.
				if r.Method == http.MethodPost {
					s.SummarizeThread(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/cancel") {
				if r.Method == http.MethodPost {
					s.CancelTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/retry") {
				// POST /tasks/{taskId}/retry
				if r.Method == http.MethodPost {
					s.RetryTask(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/logs") {
				if r.Method == http.MethodGet {
					s.GetTaskLogs(w, r)
					return
				}
			} else if strings.HasSuffix(taskPart, "/explain") {
				// POST /tasks/{taskId}/explain — post-mortem
				// summary. GET also accepted as a convenience for
				// curl + UI usage; both produce the same output.
				taskID := strings.TrimSuffix(taskPart, "/explain")
				s.ExplainTask(w, r, projectID, taskID)
				return
			} else if strings.HasSuffix(taskPart, "/hints") || strings.HasSuffix(taskPart, "/hints/") {
				// POST /tasks/{taskId}/hints — task-scoped operator
				// steering. Survives across retries (new executions
				// inherit pending task-scoped hints). GET lists
				// pending task hints not yet consumed.
				taskID := strings.TrimSuffix(strings.TrimSuffix(taskPart, "/"), "/hints")
				switch r.Method {
				case http.MethodPost:
					s.TaskHintCreate(w, r, projectID, taskID)
					return
				case http.MethodGet:
					s.TaskHintList(w, r, projectID, taskID)
					return
				}
			} else {
				// GET /tasks/{taskId}
				if r.Method == http.MethodGet {
					s.GetTask(w, r)
					return
				}
			}
		}
	} else if remaining == "/tasks.csv" {
		// CSV export: GET /api/v1/projects/{p}/tasks.csv
		if r.Method == http.MethodGet {
			s.ExportTasksCSV(w, r)
			return
		}
	} else if remaining == "/audit.csv" {
		if r.Method == http.MethodGet {
			s.ExportAuditCSV(w, r)
			return
		}
	} else if remaining == "/spend.csv" {
		if r.Method == http.MethodGet {
			s.ExportSpendCSV(w, r)
			return
		}
	} else if remaining == "/tasks" || remaining == "/tasks/" {
		// POST /tasks - create task
		if r.Method == http.MethodPost {
			s.CreateTask(w, r)
			return
		}
		// GET /tasks - list tasks
		if r.Method == http.MethodGet {
			s.ListTasks(w, r)
			return
		}
	} else if strings.HasPrefix(remaining, "/executions") {
		// GET /executions - list executions for project
		if r.Method == http.MethodGet {
			s.ListExecutions(w, r)
			return
		}
	} else if remaining == "/config" || remaining == "/config/" {
		// GET /projects/{id}/config — full project definition from the
		// in-memory registry. No DB round-trip.
		if r.Method == http.MethodGet {
			s.GetProjectConfig(w, r)
			return
		}
	} else if remaining == "/archive" || remaining == "/archive/" {
		// POST /projects/{id}/archive — flips lifecycle.status to
		// archived + schedules deletion. Body {"grace":"7d",
		// "reason":"..."} optional.
		s.ProjectArchive(w, r, projectID)
		return
	} else if remaining == "/unarchive" || remaining == "/unarchive/" {
		// POST /projects/{id}/unarchive — clears the lifecycle
		// block, returning the project to active.
		s.ProjectUnarchive(w, r, projectID)
		return
	} else if remaining == "/delete-now" || remaining == "/delete-now/" {
		// POST /projects/{id}/delete-now — rewinds the scheduled
		// delete time so the sweeper picks the project up on the
		// next tick. Requires the project to be archived first.
		s.ProjectDeleteNow(w, r, projectID)
		return
	} else if remaining == "/autonomy/evaluations" || remaining == "/autonomy/evaluations/" {
		// GET /projects/{id}/autonomy/evaluations — list autonomy audit rows
		if r.Method == http.MethodGet {
			s.ListAutonomyEvaluations(w, r)
			return
		}
	} else if remaining == "/autonomy/summary" || remaining == "/autonomy/summary/" {
		// GET /projects/{id}/autonomy/summary — aggregated outcome counts
		if r.Method == http.MethodGet {
			s.GetAutonomyEvaluationSummary(w, r)
			return
		}
	} else if remaining == "/ratelimit-status" || remaining == "/ratelimit-status/" {
		// GET /projects/{id}/ratelimit-status — per-key + per-project
		// rate-limit headroom + recent warn/block counts. Drives the
		// homepage "approaching limit" banner and the operator panel.
		if r.Method == http.MethodGet {
			s.GetProjectRateLimitStatus(w, r)
			return
		}
	} else if remaining == "/webhooks/events" || remaining == "/webhooks/events/" {
		// GET /webhooks/events - list webhook ingress audit rows
		if r.Method == http.MethodGet {
			s.ListWebhookEvents(w, r)
			return
		}
	} else if remaining == "/memory/search" || remaining == "/memory/search/" {
		// GET /memory/search?q=<query>&limit=<n>
		if r.Method == http.MethodGet {
			s.MemorySearch(w, r)
			return
		}
	} else if remaining == "/memory/feedback" || remaining == "/memory/feedback/" {
		// GET /memory/feedback?days=<n>&sample=<n>
		if r.Method == http.MethodGet {
			s.MemoryFeedback(w, r, projectID)
			return
		}
	} else if remaining == "/memory/epochs" || remaining == "/memory/epochs/" {
		// GET /memory/epochs?limit=N — Phase 3
		if r.Method == http.MethodGet {
			s.MemoryEpochs(w, r, projectID)
			return
		}
	} else if remaining == "/memory/rollback" || remaining == "/memory/rollback/" {
		// POST /memory/rollback — Phase 3
		if r.Method == http.MethodPost {
			s.MemoryRollback(w, r, projectID)
			return
		}
	} else if remaining == "/memory/rollbacks" || remaining == "/memory/rollbacks/" {
		// GET /memory/rollbacks?limit=N — audit history
		if r.Method == http.MethodGet {
			s.MemoryRollbacks(w, r, projectID)
			return
		}
	} else if remaining == "/memory/quarantine" || remaining == "/memory/quarantine/" {
		// GET /memory/quarantine?limit=N — Phase 2
		if r.Method == http.MethodGet {
			s.MemoryQuarantineList(w, r, projectID)
			return
		}
	} else if strings.HasPrefix(remaining, "/memory/quarantine/") {
		// POST /memory/quarantine/<id>/release  or  /drop
		if r.Method == http.MethodPost {
			s.MemoryQuarantineAction(w, r, projectID, strings.TrimPrefix(remaining, "/memory/quarantine/"))
			return
		}
	} else if remaining == "/memory/health" || remaining == "/memory/health/" {
		// GET /memory/health — composite per-project status
		if r.Method == http.MethodGet {
			s.MemoryHealth(w, r, projectID)
			return
		}
	} else if remaining == "/mcp/tools" || remaining == "/mcp/tools/" {
		// GET /mcp/tools — list tools available to this project
		if r.Method == http.MethodGet {
			s.ListMCPTools(w, r)
			return
		}
	} else if remaining == "/mcp/tools/call" || remaining == "/mcp/tools/call/" {
		// POST /mcp/tools/call — invoke a tool by qualified name
		if r.Method == http.MethodPost {
			s.CallMCPTool(w, r)
			return
		}
	} else if remaining == "/gist" || remaining == "/gist/" {
		// GET /gist — return the periodic LLM-free term-frequency
		// summary for this project. 404 when the consolidate loop
		// hasn't populated a row yet.
		s.GetProjectGist(w, r, projectID)
		return
	} else if remaining == "/keys" || remaining == "/keys/" {
		// POST /keys — create a new DB-backed bearer token for the
		// project; GET /keys — list every key (active + revoked).
		switch r.Method {
		case http.MethodPost:
			s.CreateAPIKey(w, r)
			return
		case http.MethodGet:
			s.ListAPIKeys(w, r)
			return
		}
	} else if strings.HasPrefix(remaining, "/artifacts/") && strings.HasSuffix(remaining, "/extract") {
		// POST /api/v1/projects/{id}/artifacts/{artifactId}/extract.
		// Runs the registered extractor on the source artifact and
		// (when memory is wired) chunks the result into project
		// memory. See document-extraction-design.md.
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
			return
		}
		// remaining looks like "/artifacts/<id>/extract" — pull the
		// id segment, reject malformed shapes.
		mid := strings.TrimPrefix(remaining, "/artifacts/")
		artifactID := strings.TrimSuffix(mid, "/extract")
		if artifactID == "" || strings.Contains(artifactID, "/") {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "artifactId is required")
			return
		}
		// PathValue isn't set by this manual router, so stash the id
		// in the request context under the same key the handler
		// expects via r.PathValue. Go's stdlib http exposes
		// r.SetPathValue precisely for this case (added 1.22).
		r.SetPathValue("artifactId", artifactID)
		s.ExtractArtifact(w, r)
		return
	} else if strings.HasPrefix(remaining, "/keys/") {
		// /keys/{kid}/rotate (POST), /keys/{kid}/workflows (PUT), or
		// /keys/{kid} (DELETE / PATCH).
		keyID, action, ok := splitKeyActionPath(remaining)
		if !ok {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid keys path")
			return
		}
		switch action {
		case "rotate":
			s.RotateAPIKey(w, r, keyID)
			return
		case "allow-push":
			// PUT /keys/{kid}/allow-push flips the allow_push capability.
			// Body: {"allow_push": true|false}. Same IDOR guard as
			// workflows and revoke: project-scope checked inside the
			// handler; 404 on cross-project or missing key.
			if r.Method == http.MethodPut {
				s.UpdateAllowPushHandler(w, r, keyID)
				return
			}
		case "workflows":
			// PUT /keys/{kid}/workflows replaces the allowed_workflows
			// list wholesale. Body: {"allowed_workflows": [...]}.
			// Add / remove semantics live on the CLI side (the
			// `vornikctl key update --add-workflow` form fetches the
			// current list, mutates, and PUTs the new set), so the
			// HTTP surface stays simple PUT-only.
			if r.Method == http.MethodPut {
				s.UpdateAPIKeyAllowedWorkflows(w, r, keyID)
				return
			}
		case "":
			if r.Method == http.MethodDelete {
				s.RevokeAPIKey(w, r, keyID)
				return
			}
		}
	}

	http.NotFound(w, r)
}

// Handler returns the HTTP handler.
func (r *Router) Handler() http.Handler {
	return r.handler
}

// AuthConfigOption customises the AuthConfig BuildAuthConfig
// returns. Used by the api router + UI subtree to plug DB-backed
// API-key lookup into the same auth chain without duplicating the
// config parsing logic.
type AuthConfigOption func(*AuthConfig)

// WithAPIKeyLookup wires the DB-backed key lookup. Nil disables
// (static-keys map only). Both call sites — the api router and the
// UI subtree — should pass the same repo so the two surfaces honour
// the same DB rows.
func WithAPIKeyLookup(lookup APIKeyLookup) AuthConfigOption {
	return func(c *AuthConfig) {
		c.APIKeyLookup = lookup
	}
}

// WithAPIKeyToucher fires async last_used_at updates on every
// successful DB-backed auth. Same nil-safe contract as
// WithAPIKeyLookup.
func WithAPIKeyToucher(toucher APIKeyToucher) AuthConfigOption {
	return func(c *AuthConfig) {
		c.APIKeyToucher = toucher
	}
}

// WithAuthAPIKeyLimiter wires the per-key request-rate limiter
// onto an AuthConfig. The same `*ratelimit.APIKeyLimiter`
// instance MUST be passed to both the api router and the UI
// subtree so bucket state isn't double-counted across surfaces.
// Distinct name from the Server-level WithAPIKeyLimiter so the
// two option types don't collide at the call sites that build
// AuthConfig directly.
func WithAuthAPIKeyLimiter(l *ratelimit.APIKeyLimiter) AuthConfigOption {
	return func(c *AuthConfig) {
		c.APIKeyLimiter = l
	}
}

// WithAuthRateLimitMetrics wires per-key limiter metrics into
// AuthMiddleware. Nil is valid and leaves rate limiting functional.
func WithAuthRateLimitMetrics(m *ratelimit.Metrics) AuthConfigOption {
	return func(c *AuthConfig) {
		c.RateLimitMetrics = m
	}
}

// WithAuthPerIPLimiter wires the unauthenticated per-IP backstop
// (hardening sub-item 2) onto an AuthConfig. The same limiter
// instance MUST be shared between the API + UI subtrees so a
// flood targeting /ui/... can't sidestep the limit by also
// hitting /api/... under the same IP. nil is the legacy default
// — no per-IP enforcement.
func WithAuthPerIPLimiter(l *ratelimit.PerIPLimiter, rps, burst int) AuthConfigOption {
	return func(c *AuthConfig) {
		c.PerIPLimiter = l
		c.PerIPRateLimitRPS = rps
		c.PerIPRateLimitBurst = burst
	}
}

// WithSessionBackend wires the browser-session cookie backend into
// the auth chain (Phase 3 login). When non-nil, the backend is prepended
// to the chain (session → hmac → dbkeys → static). Nil — the default —
// leaves cookie auth off, and
// a stale vornik_session cookie never influences a request. Both
// AuthConfig-building call sites (the api router and the UI subtree)
// should pass the same backend instance so the two surfaces honour
// the same sessions.
func WithSessionBackend(b auth.Backend) AuthConfigOption {
	return func(c *AuthConfig) {
		c.SessionBackend = b
	}
}

// WithAuthDryRunMetrics wires the dry-run denial counter into
// AuthMiddleware. Nil is valid and leaves metric emission disabled;
// the dedup warn log still fires. Both the api router and the UI
// subtree should pass the same DryRunMetrics instance so denials
// from both surfaces are counted together.
func WithAuthDryRunMetrics(m *DryRunMetrics) AuthConfigOption {
	return func(c *AuthConfig) {
		c.DryRunMetrics = m
	}
}

// WithAuthChainMetrics wires the backend-verdict counter into
// AuthMiddleware. Nil is valid and disables recording. Same shared-
// instance rule as WithAuthDryRunMetrics: the api router and the UI
// subtree must pass the same AuthChainMetrics so one counter covers
// both surfaces.
func WithAuthChainMetrics(m *AuthChainMetrics) AuthConfigOption {
	return func(c *AuthConfig) {
		c.ChainMetrics = m
	}
}

// BuildAuthConfig translates the daemon's API config into an AuthConfig
// the middleware can consume. Exported so the UI subtree (mounted in
// the service container, outside the api router's middleware chain)
// can wrap itself with the same auth policy without re-implementing
// the key map.
func BuildAuthConfig(cfg *config.Config, opts ...AuthConfigOption) AuthConfig {
	keys := make(map[string][]string)
	if cfg != nil && len(cfg.API.APIKeys) > 0 {
		for _, k := range cfg.API.APIKeys {
			keys[k] = nil
		}
	}
	out := AuthConfig{
		Enabled:       cfg != nil && cfg.API.AuthEnabled,
		DryRun:        cfg != nil && cfg.API.AuthDryRun,
		StaticAPIKeys: keys,
	}
	// Brute-force lockout on auth failures — only meaningful when auth is
	// enforced. Defaults (15 failures / 5 min → 15 min lockout); the IP is
	// resolved via the wired PerIPLimiter, so the lockout is effective once
	// that limiter is present (production wiring).
	if out.Enabled {
		out.AuthFailures = newAuthFailureLimiter(0, 0, 0)
	}
	// Admin-class keys bypass project scoping at stamp time (2026-06-07
	// regression, bug-class recurrence #3 — see AuthConfig.AdminKeyChecker).
	// Wired here, not per call site, so the api router AND the UI subtree
	// (both build through this function) inherit one bypass rule.
	if cfg != nil && cfg.Admin.Enabled {
		out.AdminKeyChecker = cfg.Admin.IsAdminKey
	}
	for _, opt := range opts {
		opt(&out)
	}
	return out
}

// applyMiddleware applies the middleware chain to the handler.
// The supplied Server's apiKeyRepo (when non-nil) is wired into
// the auth config so DB-backed bearer tokens take precedence over
// the static-keys map.
func applyMiddleware(handler http.Handler, cfg *config.Config, server *Server) http.Handler {
	var opts []AuthConfigOption
	if server != nil && server.apiKeyRepo != nil {
		opts = append(opts,
			WithAPIKeyLookup(server.apiKeyRepo),
			WithAPIKeyToucher(server.apiKeyRepo),
		)
	}
	if server != nil && server.apiKeyLimiter != nil {
		opts = append(opts, WithAuthAPIKeyLimiter(server.apiKeyLimiter))
	}
	if server != nil && server.rateLimitMetrics != nil {
		opts = append(opts, WithAuthRateLimitMetrics(server.rateLimitMetrics))
	}
	if server != nil && server.dryRunMetrics != nil {
		opts = append(opts, WithAuthDryRunMetrics(server.dryRunMetrics))
	}
	if server != nil && server.chainMetrics != nil {
		opts = append(opts, WithAuthChainMetrics(server.chainMetrics))
	}
	if server != nil && server.perIPLimiter != nil {
		opts = append(opts, WithAuthPerIPLimiter(
			server.perIPLimiter,
			server.perIPRateLimitRPS,
			server.perIPRateLimitBurst,
		))
	}
	if server != nil && server.sessionBackend != nil {
		opts = append(opts, WithSessionBackend(server.sessionBackend))
	}
	authConfig := BuildAuthConfig(cfg, opts...)

	// Apply middleware in reverse order (last applied = first executed)
	// 1. Project authorization
	handler = ProjectAuthMiddleware()(handler)

	// 2. API key authentication
	handler = AuthMiddleware(authConfig)(handler)

	return handler
}

// SetupRoutes is a convenience function that creates a router and returns its handler.
func SetupRoutes(server *Server, cfg *config.Config) http.Handler {
	router := NewRouter(server, cfg)
	return router.Handler()
}
