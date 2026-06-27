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

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/archiveutil"
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
		if logs, lerr := s.taskLogSource.TaskLogs(r.Context(), br.TaskID, containerLogTail); lerr == nil {
			builder.WriteContainerLogs(br, res, logs)
			builder.finalize(br, res)
		}
	}

	size, written, err := streamSupportBundle(w, res)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	// Metrics + structured log (LLD §10).
	if s.apiMetrics != nil {
		rawLabel := fmt.Sprintf("%t", res.manifest.Raw)
		s.apiMetrics.SupportReportGeneratedTotal.WithLabelValues(res.manifest.Mode, rawLabel).Inc()
		s.apiMetrics.SupportReportBytesTotal.WithLabelValues(res.manifest.Mode).Add(float64(size))
	}
	s.logger.Info().
		Str("component", "support_report").
		Str("mode", res.manifest.Mode).
		Str("task_id", br.TaskID).
		Bool("raw", res.manifest.Raw).
		Int64("bytes", written).
		Int("redactions", res.tally.total).
		Dur("duration", time.Since(start)).
		Msg("support report generated")
}

// resolveSupportRequest parses + validates the body and enforces authz
// (LLD §8), returning the resolved bundleRequest. On any failure it
// writes the error response and returns ok=false.
func (s *Server) resolveSupportRequest(w http.ResponseWriter, r *http.Request) (bundleRequest, bool) {
	var req supportReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body")
		return bundleRequest{}, false
	}
	hasTask := strings.TrimSpace(req.TaskID) != ""
	hasWindow := strings.TrimSpace(req.Since) != ""
	if hasTask == hasWindow {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "exactly one of task_id or since is required")
		return bundleRequest{}, false
	}
	maxSize := req.MaxSize
	if maxSize <= 0 {
		maxSize = defaultSupportMaxSize
	}
	br := bundleRequest{MaxSize: maxSize, IncludeRaw: req.IncludeRaw}

	if hasTask {
		return s.resolveTaskScope(w, r, req, br)
	}
	return s.resolveWindowScope(w, r, req, br)
}

// resolveTaskScope validates per-task authz: the task's ProjectID must
// be in the caller's scope, else 404 (no cross-project existence leak).
func (s *Server) resolveTaskScope(w http.ResponseWriter, r *http.Request, req supportReportRequest, br bundleRequest) (bundleRequest, bool) {
	if s.taskRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "task repository not wired")
		return bundleRequest{}, false
	}
	task, err := s.taskRepo.Get(r.Context(), req.TaskID)
	if err != nil || task == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return bundleRequest{}, false
	}
	if !requestAllowsProject(r, task.ProjectID) {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
		return bundleRequest{}, false
	}
	br.TaskID = req.TaskID
	return br, true
}

// resolveWindowScope enforces that window mode (all-projects) requires a
// GLOBAL admin key — a project-scoped key (one that stamped projectIDKey)
// is refused — then parses the window.
func (s *Server) resolveWindowScope(w http.ResponseWriter, r *http.Request, req supportReportRequest, br bundleRequest) (bundleRequest, bool) {
	if _, scoped := requestScopedProjectSet(r); scoped {
		respondError(w, http.StatusForbidden, "GLOBAL_ADMIN_REQUIRED",
			"window-mode support reports span all projects and require a global admin key")
		return bundleRequest{}, false
	}
	since, until, err := parseWindow(req.Since, req.Until)
	if err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return bundleRequest{}, false
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
func streamSupportBundle(w http.ResponseWriter, res *buildResult) (size, written int64, err error) {
	staging, err := os.MkdirTemp("", "vornik-support-build-*")
	if err != nil {
		return 0, 0, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	for name, content := range res.files {
		target := filepath.Join(staging, filepath.FromSlash(name))
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return 0, 0, fmt.Errorf("stage: %w", mkErr)
		}
		if wErr := os.WriteFile(target, content, 0o600); wErr != nil {
			return 0, 0, fmt.Errorf("stage: %w", wErr)
		}
	}

	tmpArchive := filepath.Join(staging, "..", "vornik-support-"+sanitizeForFilename(res.manifest.Mode)+".tar.gz")
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
	w.Header().Set("X-Vornik-Support-Mode", res.manifest.Mode)
	w.Header().Set("X-Vornik-Support-Raw", fmt.Sprintf("%t", res.manifest.Raw))
	w.WriteHeader(http.StatusOK)
	written, _ = io.Copy(w, f)
	return size, written, nil
}

// newBundleBuilder wires a builder from the Server's repositories +
// detector + config. Repos absent on this deployment are left nil; the
// builder degrades those sections gracefully (best-effort, LLD §7).
func (s *Server) newBundleBuilder() *bundleBuilder {
	b := &bundleBuilder{
		detector: s.secretsDetector,
		version:  version.Default,
	}
	b.repos = supportRepos{}
	if s.taskRepo != nil {
		b.repos.Tasks = s.taskRepo
	}
	if s.executionRepo != nil {
		b.repos.Executions = s.executionRepo
	}
	if s.stepOutcomeRepo != nil {
		b.repos.Outcomes = s.stepOutcomeRepo
	}
	if s.toolAuditRepo != nil {
		b.repos.ToolAudit = s.toolAuditRepo
	}
	if s.llmUsageRepo != nil {
		b.repos.LLMUsage = s.llmUsageRepo
	}
	if s.taskMessageRepo != nil {
		b.repos.Messages = s.taskMessageRepo
	}
	if s.adminAuditRepo != nil {
		b.repos.AdminAudit = s.adminAuditRepo
	}
	if s.artifactRepo != nil {
		b.repos.Artifacts = s.artifactRepo
	}
	if s.artifactOpener != nil {
		b.opener = supportArtifactOpenerAdapter{s.artifactOpener}
	}
	// Doctor / health / metrics + judge / postmortem are wired by the
	// service container when available via Set* hooks; nil → those
	// sections degrade gracefully.
	if s.supportDoctor != nil {
		b.doctor = s.supportDoctor
	}
	if s.supportHealth != nil {
		b.health = s.supportHealth
	}
	if s.supportMetrics != nil {
		b.metrics = s.supportMetrics
	}
	if s.supportJudgeRepo != nil {
		b.repos.JudgeVerdct = s.supportJudgeRepo
	}
	if s.supportPostMortemRepo != nil {
		b.repos.PostMortem = s.supportPostMortemRepo
	}

	// Config snapshot: field-name redaction (redactSecrets) → YAML.
	// The builder additionally runs internal/secrets value-pattern
	// redaction over the result (defense in depth).
	if s.config != nil {
		if yml, err := redactedConfigYAML(s.config); err == nil {
			b.configYAML = yml
		}
	}
	return b
}

// supportArtifactOpenerAdapter bridges the Server's ArtifactOpener
// (returns io.ReadCloser) to the builder's supportArtifactOpener
// (returns readCloser, a structural alias of io.ReadCloser).
type supportArtifactOpenerAdapter struct {
	o ArtifactOpener
}

func (a supportArtifactOpenerAdapter) Open(ctx context.Context, id string) (readCloser, error) {
	rc, err := a.o.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// redactedConfigYAML field-name-redacts the config then marshals to
// YAML so config.redacted.yaml is honestly YAML-shaped.
func redactedConfigYAML(cfg any) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", err
	}
	redacted := redactSecrets(generic)
	out, err := yaml.Marshal(redacted)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseWindow resolves since/until. since accepts an RFC3339 timestamp
// or a Go duration (interpreted as "now - duration"). until defaults to
// now.
func parseWindow(sinceStr, untilStr string) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	since, err := parseTimeOrDuration(sinceStr, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid since: %w", err)
	}
	until := now
	if strings.TrimSpace(untilStr) != "" {
		until, err = parseTimeOrDuration(untilStr, now)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid until: %w", err)
		}
	}
	if until.Before(since) {
		return time.Time{}, time.Time{}, fmt.Errorf("until is before since")
	}
	return since, until, nil
}

// parseTimeOrDuration accepts an RFC3339 timestamp OR a Go duration
// (relative to ref, subtracted: "2h" → ref-2h).
func parseTimeOrDuration(s string, ref time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return ref.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("not an RFC3339 timestamp or Go duration: %q", s)
}

func sanitizeForFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		return "bundle"
	}
	return s
}
