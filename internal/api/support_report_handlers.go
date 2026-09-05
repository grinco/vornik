package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/archiveutil"
	"vornik.io/vornik/internal/supportbundle"
	"vornik.io/vornik/internal/version"
)

// support-report daemon endpoint
// ==============================
//
// POST /api/v1/support-report — admin-gated. Builds the server-
// collectable, already-redacted core of a support bundle and streams
// it back as a tar.gz. vornikctl augments it with host-only sections
// (journald, podman/systemctl versions) on the client side.
//
// Authorization (LLD §8):
//   - non-admin caller → 403 (requireAdminGate).
//   - --task mode → the task's ProjectID is validated against the
//     caller's authorized projects (requestAllowsProject). A project-
//     scoped key pulling another project's task gets 404 (not-found
//     semantics; no cross-project existence leak).
//   - --since window mode → spans all projects, so a GLOBAL admin key
//     is required: a project-scoped key (one that stamped projectIDKey)
//     is refused with 403.
//
// See https://docs.vornik.io

// supportReportRequest is the POST body.
type supportReportRequest struct {
	TaskID     string `json:"task_id,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	MaxSize    int64  `json:"max_size,omitempty"`
	IncludeRaw bool   `json:"include_raw,omitempty"`
}

// defaultSupportMaxSize is the daemon-side ceiling when the client
// doesn't supply one (mirrors the CLI default — 200 MiB).
const defaultSupportMaxSize = 200 << 20

// SupportReport handles POST /api/v1/support-report.
func (s *Server) SupportReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}

	br, ok := s.resolveSupportRequest(w, r)
	if !ok {
		return // resolveSupportRequest already wrote the error
	}

	start := time.Now()
	builder := s.newBundleBuilder()
	res, err := builder.Build(r.Context(), br)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	// Container logs come from the Server's taskLogSource (the builder
	// has no executor dependency). Fetch + inject so they redact through
	// the same path, then re-finalize so MANIFEST/REDACTION reflect them.
	if br.TaskID != "" && s.taskLogSource != nil {
		if logs, lerr := s.taskLogSource.TaskLogs(r.Context(), br.TaskID, supportbundle.ContainerLogTail); lerr == nil {
			builder.WriteContainerLogs(br, res, logs)
			builder.Finalize(br, res)
		}
	}

	size, written, err := streamSupportBundle(w, res)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	// Metrics + structured log (LLD §10).
	if s.apiMetrics != nil {
		rawLabel := fmt.Sprintf("%t", res.Manifest.Raw)
		s.apiMetrics.SupportReportGeneratedTotal.WithLabelValues(res.Manifest.Mode, rawLabel).Inc()
		s.apiMetrics.SupportReportBytesTotal.WithLabelValues(res.Manifest.Mode).Add(float64(size))
	}
	s.logger.Info().
		Str("component", "support_report").
		Str("mode", res.Manifest.Mode).
		Str("task_id", br.TaskID).
		Bool("raw", res.Manifest.Raw).
		Int64("bytes", written).
		Int("redactions", res.Tally.Total).
		Dur("duration", time.Since(start)).
		Msg("support report generated")
}

// resolveSupportRequest parses + validates the body and enforces authz
// (LLD §8), returning the resolved supportbundle.Request. On any failure it
// writes the error response and returns ok=false.
func (s *Server) resolveSupportRequest(w http.ResponseWriter, r *http.Request) (supportbundle.Request, bool) {
	var req supportReportRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return supportbundle.Request{}, false
	}
	hasTask := strings.TrimSpace(req.TaskID) != ""
	hasWindow := strings.TrimSpace(req.Since) != ""
	if hasTask == hasWindow {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "exactly one of task_id or since is required")
		return supportbundle.Request{}, false
	}
	maxSize := req.MaxSize
	if maxSize <= 0 {
		maxSize = defaultSupportMaxSize
	}
	br := supportbundle.Request{MaxSize: maxSize, IncludeRaw: req.IncludeRaw}

	if hasTask {
		return s.resolveTaskScope(w, r, req, br)
	}
	return s.resolveWindowScope(w, r, req, br)
}

// resolveTaskScope validates per-task authz: the task's ProjectID must
// be in the caller's scope, else 404 (no cross-project existence leak).
func (s *Server) resolveTaskScope(w http.ResponseWriter, r *http.Request, req supportReportRequest, br supportbundle.Request) (supportbundle.Request, bool) {
	if s.taskRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "task repository not wired")
		return supportbundle.Request{}, false
	}
	task, err := s.taskRepo.Get(r.Context(), req.TaskID)
	if err != nil || task == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return supportbundle.Request{}, false
	}
	if !requestAllowsProject(r, task.ProjectID) {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return supportbundle.Request{}, false
	}
	br.TaskID = req.TaskID
	return br, true
}

// resolveWindowScope enforces that window mode (all-projects) requires a
// GLOBAL admin key — a project-scoped key (one that stamped projectIDKey)
// is refused — then parses the window.
func (s *Server) resolveWindowScope(w http.ResponseWriter, r *http.Request, req supportReportRequest, br supportbundle.Request) (supportbundle.Request, bool) {
	if _, scoped := requestScopedProjectSet(r); scoped {
		respondError(w, http.StatusForbidden, "GLOBAL_ADMIN_REQUIRED",
			"window-mode support reports span all projects and require a global admin key")
		return supportbundle.Request{}, false
	}
	since, until, err := parseWindow(req.Since, req.Until)
	if err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return supportbundle.Request{}, false
	}
	br.Window = true
	br.Since = since
	br.Until = until
	return br, true
}

// streamSupportBundle stages the in-memory bundle to a temp dir, tars it
// (reusing archiveutil's TarGzDir + safe-path helpers — same as
// backup.go), and streams it to the client. Returns the archive size +
// bytes written.
func streamSupportBundle(w http.ResponseWriter, res *supportbundle.Result) (size, written int64, err error) {
	staging, err := os.MkdirTemp("", "vornik-support-build-*")
	if err != nil {
		return 0, 0, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	for name, content := range res.Files {
		target := filepath.Join(staging, filepath.FromSlash(name))
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return 0, 0, fmt.Errorf("stage: %w", mkErr)
		}
		if wErr := os.WriteFile(target, content, 0o600); wErr != nil {
			return 0, 0, fmt.Errorf("stage: %w", wErr)
		}
	}

	tmpArchive := filepath.Join(staging, "..", "vornik-support-"+sanitizeForFilename(res.Manifest.Mode)+".tar.gz")
	if tErr := archiveutil.TarGzDir(staging, tmpArchive); tErr != nil {
		return 0, 0, fmt.Errorf("archive: %w", tErr)
	}
	defer func() { _ = os.Remove(tmpArchive) }()

	f, err := os.Open(tmpArchive) //nolint:gosec // path is daemon-constructed in our own tmp dir
	if err != nil {
		return 0, 0, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	size = archiveutil.FileSize(tmpArchive)

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-Vornik-Support-Mode", res.Manifest.Mode)
	w.Header().Set("X-Vornik-Support-Raw", fmt.Sprintf("%t", res.Manifest.Raw))
	w.WriteHeader(http.StatusOK)
	written, _ = io.Copy(w, f)
	return size, written, nil
}

// newBundleBuilder wires a builder from the Server's repositories +
// detector + config. Repos absent on this deployment are left nil; the
// builder degrades those sections gracefully (best-effort, LLD §7).
func (s *Server) newBundleBuilder() *supportbundle.Builder {
	b := &supportbundle.Builder{
		Detector: s.secretsDetector,
		// s.BuildVersion(), NOT version.Default — which is the literal string
		// "unstamped". Every bundle built before 2026-09-04 reported that as
		// its version, which is exactly the failure api.go documents against
		// this same constant: "GetCapabilities reported the version.Default
		// CONSTANT to every client because the only version on the Server
		// looked like it belonged to telemetry. It does not — it is the build
		// version, and it is the right answer for any surface that needs one."
		// A support bundle whose version field says "unstamped" cannot answer
		// the first question a support engineer asks.
		Version: bundleVersion(s.BuildVersion()),
		// The edition the reader needs in order to interpret an absent
		// section. adminSurfacePresent IS the edition signal on the Server —
		// the service container sets it only inside its Enterprise providers
		// gate — so it is the honest source here rather than a second flag
		// that could disagree with the surface actually being served.
		Edition: editionFromAdminSurface(s.adminSurfacePresent),
		// EE-only; nil on Community, which omits the section rather than erroring.
		Blackbox: s.blackboxService,
	}
	// The deployed registry and the webhook ingress audit. Both degrade to an
	// omitted section when absent (support-report design §5, 2026-09-04).
	if s.projectRegistry != nil {
		b.Registry = s.projectRegistry
	}
	if s.webhookEventRepo != nil {
		b.Webhooks = s.webhookEventRepo
	}
	b.Repos = supportbundle.Repos{}
	if s.taskRepo != nil {
		b.Repos.Tasks = s.taskRepo
	}
	if s.executionRepo != nil {
		b.Repos.Executions = s.executionRepo
	}
	if s.stepOutcomeRepo != nil {
		b.Repos.Outcomes = s.stepOutcomeRepo
	}
	if s.toolAuditRepo != nil {
		b.Repos.ToolAudit = s.toolAuditRepo
	}
	if s.llmUsageRepo != nil {
		b.Repos.LLMUsage = s.llmUsageRepo
	}
	if s.taskMessageRepo != nil {
		b.Repos.Messages = s.taskMessageRepo
	}
	if s.adminAuditRepo != nil {
		b.Repos.AdminAudit = s.adminAuditRepo
	}
	if s.artifactRepo != nil {
		b.Repos.Artifacts = s.artifactRepo
	}
	if s.artifactOpener != nil {
		b.Opener = supportArtifactOpenerAdapter{s.artifactOpener}
	}
	// Doctor / health / metrics + judge / postmortem are wired by the
	// service container when available via Set* hooks; nil → those
	// sections degrade gracefully.
	if s.supportDoctor != nil {
		b.Doctor = s.supportDoctor
	}
	if s.supportHealth != nil {
		b.Health = s.supportHealth
	}
	if s.supportMetrics != nil {
		b.Metrics = s.supportMetrics
	}
	if s.supportJudgeRepo != nil {
		b.Repos.JudgeVerdct = s.supportJudgeRepo
	}
	if s.supportPostMortemRepo != nil {
		b.Repos.PostMortem = s.supportPostMortemRepo
	}

	// Config snapshot: field-name redaction (redactSecrets) → YAML.
	// The builder additionally runs internal/secrets value-pattern
	// redaction over the result (defense in depth).
	if s.config != nil {
		if yml, err := redactedConfigYAML(s.config); err == nil {
			b.ConfigYAML = yml
		}
	}
	return b
}

// supportArtifactOpenerAdapter bridges the Server's ArtifactOpener
// (returns io.ReadCloser) to the builder's supportArtifactOpener
// (returns supportbundle.ReadCloser, a structural alias of io.ReadCloser).
type supportArtifactOpenerAdapter struct {
	o ArtifactOpener
}

func (a supportArtifactOpenerAdapter) Open(ctx context.Context, id string) (supportbundle.ReadCloser, error) {
	rc, err := a.o.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// redactedConfigYAML field-name-redacts the config then marshals to
// YAML so config.redacted.yaml is honestly YAML-shaped.
//
// The implementation lives with the collector: the CLI's local driver renders
// the same section from the same config struct, and the design's one rule is
// that the redaction path has exactly one implementation
// (support-bundle-in-CE design §3).
func redactedConfigYAML(cfg any) (string, error) {
	return supportbundle.RedactedConfigYAML(cfg)
}

// parseWindow resolves since/until. It delegates to the collector package:
// the CLI's local driver resolves the SAME flags into the same Request, and a
// window that means one thing over HTTP and another locally would produce two
// bundles that disagree about what they cover.
func parseWindow(sinceStr, untilStr string) (time.Time, time.Time, error) {
	return supportbundle.ParseWindow(sinceStr, untilStr)
}

func sanitizeForFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		return "bundle"
	}
	return s
}

// editionFromAdminSurface maps the Server's edition signal to a version
// edition string. adminSurfacePresent is set only inside the service
// container's Enterprise providers gate, so it cannot claim an edition whose
// surface is not actually wired.
func editionFromAdminSurface(present bool) string {
	if present {
		return version.EditionEnterprise
	}
	return version.EditionCommunity
}

// bundleVersion falls back to the compiled-in default when the daemon's build
// version was never wired, so the field is never empty — but it prefers the
// real build every time it has one.
func bundleVersion(build string) string {
	if b := strings.TrimSpace(build); b != "" {
		return b
	}
	return version.Default
}
