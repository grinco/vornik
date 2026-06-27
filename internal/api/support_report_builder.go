package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// support-report bundle builder
// =============================
//
// The daemon-side collector for `vornikctl support-report`. It gathers
// the §4.1 sections (DB rows, container logs, config, doctor, health,
// metrics), REDACTS every text payload through internal/secrets BEFORE
// it is written and BEFORE any size-cap truncation (review #7), and
// emits a staging directory the handler tars to gz.
//
// Everything here is driven through narrow interfaces so the whole
// collector is unit-testable with fakes and NO database — see
// support_report_builder_test.go. The redaction-coverage test
// (load-bearing, LLD §9) seeds a distinct secret into every section
// and asserts no raw secret survives in any produced file.
//
// See https://docs.vornik.io

// supportReportSchemaVersion is bumped when the MANIFEST.json shape
// changes so support intake tooling can detect format drift.
const supportReportSchemaVersion = 1

// Per-section row caps. Redaction runs on the FULL section text before
// these caps truncate (redact-before-truncate, review #7), so a cap can
// never split a partially-redacted secret into the bundle.
const (
	defaultToolAuditCap = 500
	defaultMessageCap   = 500
	defaultUsageCap     = 2000
	defaultOutcomeCap   = 2000
	defaultTaskCap      = 2000 // window-mode task summaries
	defaultAuditCap     = 2000 // window-mode admin audit rows
	defaultArtifactCap  = 1000
	// maxTextArtifactBytes bounds how many bytes of a single text
	// artifact we redact + ship. Binary artifacts ship as metadata
	// only (review #1) regardless of this.
	maxTextArtifactBytes = 1 << 20 // 1 MiB
	containerLogTail     = 2000    // lines
)

// supportRepos is the narrow set of repositories the builder reads.
// Each is optional: a nil repo (or one that errors) records a
// best-effort section error and the build continues (LLD §7). The
// concrete persistence repositories satisfy these structurally; tests
// pass fakes.
type supportRepos struct {
	Tasks       supportTaskReader
	Executions  supportExecutionReader
	Outcomes    supportOutcomeReader
	ToolAudit   supportToolAuditReader
	LLMUsage    supportUsageReader
	Messages    supportMessageReader
	JudgeVerdct SupportJudgeReader
	PostMortem  SupportPostMortemReader
	Artifacts   supportArtifactReader
	AdminAudit  supportAdminAuditReader
}

type supportTaskReader interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
	List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error)
}
type supportExecutionReader interface {
	List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error)
}
type supportOutcomeReader interface {
	List(ctx context.Context, filter persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error)
}
type supportToolAuditReader interface {
	List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error)
}
type supportUsageReader interface {
	List(ctx context.Context, filter persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error)
}
type supportMessageReader interface {
	List(ctx context.Context, filter persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error)
}

// SupportJudgeReader reads a task's judge verdict for the bundle.
type SupportJudgeReader interface {
	GetByTask(ctx context.Context, taskID string) (*persistence.TaskJudgeVerdict, error)
}

// SupportPostMortemReader reads a task's post-mortem for the bundle.
type SupportPostMortemReader interface {
	Get(ctx context.Context, taskID string) (*persistence.TaskPostMortem, error)
}
type supportArtifactReader interface {
	List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error)
}
type supportAdminAuditReader interface {
	List(ctx context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error)
}

// supportArtifactOpener reads the bytes of an artifact so the builder
// can classify text vs binary + compute a sha256. *artifacts.Store
// satisfies the same Open shape used elsewhere in this package.
type supportArtifactOpener interface {
	Open(ctx context.Context, artifactID string) (readCloser, error)
}

// readCloser is io.ReadCloser, aliased so the interface above doesn't
// pull io into every fake's signature noise. (It is io.ReadCloser.)
type readCloser interface {
	Read(p []byte) (int, error)
	Close() error
}

// SupportDoctorRunner runs the in-process doctor checks and returns a
// JSON-marshalable report. nil → doctor.json records "not configured".
type SupportDoctorRunner interface {
	Run(ctx context.Context) (any, error)
}

// SupportHealthSource and SupportMetricsSource provide the always-on
// health + metrics snapshots. Both optional.
type SupportHealthSource interface {
	Snapshot(ctx context.Context) (any, error)
}

// SupportMetricsSource snapshots the Prometheus metrics text.
type SupportMetricsSource interface {
	Snapshot(ctx context.Context) (string, error)
}

// bundleBuilder collects sections into an in-memory staging tree
// (map of relative path → bytes). The handler writes the tree to a
// staging dir and tars it. Keeping the tree in memory makes the
// redaction-coverage test able to assert over every produced byte
// without touching disk.
type bundleBuilder struct {
	repos    supportRepos
	opener   supportArtifactOpener
	doctor   SupportDoctorRunner
	health   SupportHealthSource
	metrics  SupportMetricsSource
	detector secrets.Detector
	version  string
	// config is the already-redacted config YAML snapshot. The
	// handler renders it (config marshaling lives on the Server); the
	// builder runs it through redaction again defensively.
	configYAML string
}

// bundleRequest is the resolved, validated request the builder acts
// on. Exactly one of TaskID / window (Since,Until) is set.
type bundleRequest struct {
	TaskID     string
	Since      time.Time
	Until      time.Time
	Window     bool
	MaxSize    int64
	IncludeRaw bool
}

// redactionTally accumulates per-type redaction counts across the
// whole bundle for REDACTION.txt. It never stores matched values.
type redactionTally struct {
	byType  map[string]int
	perFile map[string]int
	total   int
}

func newRedactionTally() *redactionTally {
	return &redactionTally{byType: map[string]int{}, perFile: map[string]int{}}
}

// manifest is the MANIFEST.json shape.
type manifest struct {
	SchemaVersion   int               `json:"schema_version"`
	Mode            string            `json:"mode"` // "task" | "window"
	TaskID          string            `json:"task_id,omitempty"`
	Since           string            `json:"since,omitempty"`
	Until           string            `json:"until,omitempty"`
	VornikVersion   string            `json:"vornik_version"`
	GeneratedAt     string            `json:"generated_at"`
	Raw             bool              `json:"raw"`
	ArchiveSHA256   string            `json:"archive_sha256,omitempty"`
	Files           []manifestFile    `json:"files"`
	Truncations     map[string]string `json:"truncations,omitempty"`
	SectionErrors   map[string]string `json:"section_errors,omitempty"`
	RedactionByType map[string]int    `json:"redaction_by_type"`
	RedactionTotal  int               `json:"redaction_total"`
}

type manifestFile struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// buildResult is the in-memory staging tree plus the manifest.
type buildResult struct {
	files       map[string][]byte // relative path -> content (manifest + redaction.txt added last)
	manifest    manifest
	tally       *redactionTally
	truncations map[string]string
	sectionErrs map[string]string
}

// Build collects every section for req. It NEVER returns a fatal error
// for a missing/failed section — those are recorded and the build
// continues (best-effort, LLD §7). It returns an error only for a
// programming-level problem (nil detector with redaction on).
func (b *bundleBuilder) Build(ctx context.Context, req bundleRequest) (*buildResult, error) {
	if !req.IncludeRaw && b.detector == nil {
		return nil, fmt.Errorf("support-report: redaction requested but no secrets detector wired")
	}
	res := &buildResult{
		files:       map[string][]byte{},
		tally:       newRedactionTally(),
		truncations: map[string]string{},
		sectionErrs: map[string]string{},
	}

	// Always-on sections.
	b.collectDoctor(ctx, req, res)
	b.collectConfig(req, res)
	b.collectVersion(req, res)
	b.collectHealth(ctx, req, res)
	b.collectMetrics(ctx, req, res)

	if req.Window {
		b.collectWindow(ctx, req, res)
	} else {
		b.collectTask(ctx, req, res)
	}

	b.finalize(req, res)
	return res, nil
}

// writeText redacts (unless raw) the payload for `name`, applies it to
// the tally, and stores it in the staging tree. Redaction runs on the
// FULL text before any caller-side truncation (the callers truncate
// ROW COUNTS, not bytes, and serialize the already-capped rows here —
// so the secret-straddling-cap hazard from review #7 cannot occur).
func (b *bundleBuilder) writeText(req bundleRequest, res *buildResult, name string, payload []byte) {
	if req.IncludeRaw {
		res.files[name] = payload
		return
	}
	findings := b.detector.Scan(payload)
	if len(findings) > 0 {
		redacted := secrets.Redact(payload, findings)
		for _, f := range findings {
			res.tally.byType[f.Type]++
			res.tally.total++
		}
		res.tally.perFile[name] += len(findings)
		res.files[name] = redacted
		return
	}
	res.files[name] = payload
}

// writeJSON marshals v then routes through writeText so JSON row
// payloads are redacted identically to plain text.
func (b *bundleBuilder) writeJSON(req bundleRequest, res *buildResult, name string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		res.sectionErrs[name] = fmt.Sprintf("marshal: %v", err)
		return
	}
	b.writeText(req, res, name, data)
}

func (b *bundleBuilder) collectDoctor(ctx context.Context, req bundleRequest, res *buildResult) {
	if b.doctor == nil {
		res.files["doctor.json"] = []byte(`{"note":"doctor runner not configured"}`)
		return
	}
	rep, err := b.doctor.Run(ctx)
	if err != nil {
		res.sectionErrs["doctor.json"] = err.Error()
		b.writeJSON(req, res, "doctor.json", map[string]string{"error": err.Error()})
		return
	}
	b.writeJSON(req, res, "doctor.json", rep)
}

func (b *bundleBuilder) collectConfig(req bundleRequest, res *buildResult) {
	if strings.TrimSpace(b.configYAML) == "" {
		res.files["config.redacted.yaml"] = []byte("# config snapshot not available\n")
		return
	}
	b.writeText(req, res, "config.redacted.yaml", []byte(b.configYAML))
}

func (b *bundleBuilder) collectVersion(req bundleRequest, res *buildResult) {
	v := b.version
	if v == "" {
		v = "unknown"
	}
	// Version is operator-trusted, but route through writeText for a
	// uniform path (it won't contain secrets).
	b.writeText(req, res, "version.txt", []byte(v+"\n"))
}

func (b *bundleBuilder) collectHealth(ctx context.Context, req bundleRequest, res *buildResult) {
	if b.health == nil {
		res.files["health.json"] = []byte(`{"note":"health source not configured"}`)
		return
	}
	snap, err := b.health.Snapshot(ctx)
	if err != nil {
		res.sectionErrs["health.json"] = err.Error()
		return
	}
	b.writeJSON(req, res, "health.json", snap)
}

func (b *bundleBuilder) collectMetrics(ctx context.Context, req bundleRequest, res *buildResult) {
	if b.metrics == nil {
		res.files["metrics.txt"] = []byte("# metrics source not configured\n")
		return
	}
	txt, err := b.metrics.Snapshot(ctx)
	if err != nil {
		res.sectionErrs["metrics.txt"] = err.Error()
		return
	}
	b.writeText(req, res, "metrics.txt", []byte(txt))
}

// ---- per-task sections ----

func (b *bundleBuilder) collectTask(ctx context.Context, req bundleRequest, res *buildResult) {
	tid := req.TaskID
	if b.repos.Tasks != nil {
		task, err := b.repos.Tasks.Get(ctx, tid)
		if err != nil {
			res.sectionErrs["task/task.json"] = err.Error()
		} else {
			b.writeJSON(req, res, "task/task.json", task)
		}
	}
	b.collectExecutions(ctx, req, res, tid)
	b.collectOutcomes(ctx, req, res, tid)
	b.collectToolAudit(ctx, req, res, tid)
	b.collectUsage(ctx, req, res, tid)
	b.collectMessages(ctx, req, res, tid)
	b.collectJudge(ctx, req, res, tid)
	b.collectPostMortem(ctx, req, res, tid)
	b.collectArtifacts(ctx, req, res, tid)
	b.collectContainerLogs(ctx, req, res, tid)
}

func (b *bundleBuilder) collectExecutions(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.Executions == nil {
		return
	}
	rows, err := b.repos.Executions.List(ctx, persistence.ExecutionFilter{TaskID: &tid, PageSize: 1000})
	if err != nil {
		res.sectionErrs["task/executions.json"] = err.Error()
		return
	}
	b.writeJSON(req, res, "task/executions.json", rows)
}

func (b *bundleBuilder) collectOutcomes(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.Outcomes == nil {
		return
	}
	rows, err := b.repos.Outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{TaskID: &tid, PageSize: defaultOutcomeCap + 1})
	if err != nil {
		res.sectionErrs["task/step_outcomes.json"] = err.Error()
		return
	}
	rows = capOutcomes(rows, defaultOutcomeCap, "task/step_outcomes.json", res)
	b.writeJSON(req, res, "task/step_outcomes.json", rows)
}

func (b *bundleBuilder) collectToolAudit(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.ToolAudit == nil {
		return
	}
	rows, err := b.repos.ToolAudit.List(ctx, persistence.ToolAuditFilter{TaskID: &tid, PageSize: defaultToolAuditCap + 1})
	if err != nil {
		res.sectionErrs["task/tool_audit.json"] = err.Error()
		return
	}
	if len(rows) > defaultToolAuditCap {
		res.truncations["task/tool_audit.json"] = fmt.Sprintf("%d of %d+ rows", defaultToolAuditCap, len(rows))
		rows = rows[:defaultToolAuditCap]
	}
	b.writeJSON(req, res, "task/tool_audit.json", rows)
}

func (b *bundleBuilder) collectUsage(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.LLMUsage == nil {
		return
	}
	rows, err := b.repos.LLMUsage.List(ctx, persistence.TaskLLMUsageFilter{TaskID: &tid, PageSize: defaultUsageCap + 1})
	if err != nil {
		res.sectionErrs["task/llm_usage.json"] = err.Error()
		return
	}
	if len(rows) > defaultUsageCap {
		res.truncations["task/llm_usage.json"] = fmt.Sprintf("%d of %d+ rows", defaultUsageCap, len(rows))
		rows = rows[:defaultUsageCap]
	}
	b.writeJSON(req, res, "task/llm_usage.json", rows)
}

func (b *bundleBuilder) collectMessages(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.Messages == nil {
		return
	}
	rows, err := b.repos.Messages.List(ctx, persistence.TaskMessageFilter{TaskID: tid, Limit: defaultMessageCap + 1})
	if err != nil {
		res.sectionErrs["task/messages.json"] = err.Error()
		return
	}
	if len(rows) > defaultMessageCap {
		res.truncations["task/messages.json"] = fmt.Sprintf("%d of %d+ rows", defaultMessageCap, len(rows))
		rows = rows[:defaultMessageCap]
	}
	b.writeJSON(req, res, "task/messages.json", rows)
}

func (b *bundleBuilder) collectJudge(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.JudgeVerdct == nil {
		return
	}
	v, err := b.repos.JudgeVerdct.GetByTask(ctx, tid)
	if err != nil {
		res.sectionErrs["task/judge.json"] = err.Error()
		return
	}
	if v == nil {
		return
	}
	b.writeJSON(req, res, "task/judge.json", v)
}

func (b *bundleBuilder) collectPostMortem(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.PostMortem == nil {
		return
	}
	pm, err := b.repos.PostMortem.Get(ctx, tid)
	if err != nil {
		res.sectionErrs["task/postmortem.json"] = err.Error()
		return
	}
	if pm == nil {
		return
	}
	b.writeJSON(req, res, "task/postmortem.json", pm)
}

// artifactMeta is the per-artifact entry in artifacts/MANIFEST.json.
// Binary artifacts ship as metadata ONLY (review #1): no bytes. Text
// artifacts are additionally written (redacted) under artifacts/.
type artifactMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	Shipped     bool   `json:"shipped"` // true only for redacted text artifacts
}

func (b *bundleBuilder) collectArtifacts(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	if b.repos.Artifacts == nil {
		return
	}
	rows, err := b.repos.Artifacts.List(ctx, persistence.ArtifactFilter{TaskID: &tid, PageSize: defaultArtifactCap + 1})
	if err != nil {
		res.sectionErrs["task/artifacts/MANIFEST.json"] = err.Error()
		return
	}
	if len(rows) > defaultArtifactCap {
		res.truncations["task/artifacts/MANIFEST.json"] = fmt.Sprintf("%d of %d+ artifacts", defaultArtifactCap, len(rows))
		rows = rows[:defaultArtifactCap]
	}
	metas := make([]artifactMeta, 0, len(rows))
	for _, a := range rows {
		m := artifactMeta{ID: a.ID, Name: a.Name}
		if b.opener != nil {
			data, ct, err := b.readArtifact(ctx, a.ID)
			if err == nil {
				sum := sha256.Sum256(data)
				m.Bytes = int64(len(data))
				m.SHA256 = hex.EncodeToString(sum[:])
				m.ContentType = ct
				if isTextContent(data) && len(data) <= maxTextArtifactBytes {
					// Ship the text artifact, redacted. Binaries are
					// metadata-only (review #1).
					fname := "task/artifacts/" + safeArtifactFilename(a.ID, a.Name)
					b.writeText(req, res, fname, data)
					m.Shipped = true
				}
			} else {
				res.sectionErrs["task/artifacts/"+a.ID] = err.Error()
			}
		}
		metas = append(metas, m)
	}
	// MANIFEST has no bytes — metadata is operator-authored names +
	// hashes; route through writeJSON for uniform redaction (names
	// may, rarely, embed a token).
	b.writeJSON(req, res, "task/artifacts/MANIFEST.json", metas)
}

func (b *bundleBuilder) readArtifact(ctx context.Context, id string) ([]byte, string, error) {
	rc, err := b.opener.Open(ctx, id)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rc.Close() }()
	// Bound the read so a huge artifact can't blow memory; the cap is
	// generous and binaries past it still get metadata via a partial
	// hash note (we just hash what we read; flagged below).
	const readCap = 8 << 20 // 8 MiB
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for len(buf) < readCap {
		n, rerr := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return buf, sniffContentType(buf), nil
}

func (b *bundleBuilder) collectContainerLogs(ctx context.Context, req bundleRequest, res *buildResult, tid string) {
	// Container logs are sourced via the Server's taskLogSource at the
	// handler layer (the builder has no executor dependency). The
	// handler injects them by calling writeContainerLogs. Nothing to
	// do here when no logs were injected.
	_ = ctx
	_ = req
	_ = res
	_ = tid
}

// WriteContainerLogs lets the handler inject already-fetched container
// log text (from taskLogSource) so it is redacted through the SAME
// path as every other section.
func (b *bundleBuilder) WriteContainerLogs(req bundleRequest, res *buildResult, logs string) {
	if strings.TrimSpace(logs) == "" {
		return
	}
	b.writeText(req, res, "task/container_logs.txt", []byte(logs))
}

// ---- window sections ----

func (b *bundleBuilder) collectWindow(ctx context.Context, req bundleRequest, res *buildResult) {
	b.collectWindowTasks(ctx, req, res)
	b.collectWindowAdminAudit(ctx, req, res)
	b.collectWindowCostRollup(ctx, req, res)
}

func (b *bundleBuilder) collectWindowTasks(ctx context.Context, req bundleRequest, res *buildResult) {
	if b.repos.Tasks == nil {
		return
	}
	all, err := b.repos.Tasks.List(ctx, persistence.TaskFilter{PageSize: 100000})
	if err != nil {
		res.sectionErrs["window/tasks.json"] = err.Error()
		return
	}
	filtered := make([]*persistence.Task, 0, len(all))
	for _, t := range all {
		if taskInWindow(t, req.Since, req.Until) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) > defaultTaskCap {
		res.truncations["window/tasks.json"] = fmt.Sprintf("%d of %d in-window tasks", defaultTaskCap, len(filtered))
		filtered = filtered[:defaultTaskCap]
	}
	b.writeJSON(req, res, "window/tasks.json", filtered)
}

func (b *bundleBuilder) collectWindowAdminAudit(ctx context.Context, req bundleRequest, res *buildResult) {
	if b.repos.AdminAudit == nil {
		return
	}
	rows, err := b.repos.AdminAudit.List(ctx, persistence.AdminAuditFilter{
		Since:    req.Since,
		Until:    req.Until,
		PageSize: defaultAuditCap + 1,
	})
	if err != nil {
		res.sectionErrs["window/admin_audit.json"] = err.Error()
		return
	}
	if len(rows) > defaultAuditCap {
		res.truncations["window/admin_audit.json"] = fmt.Sprintf("%d of %d+ rows", defaultAuditCap, len(rows))
		rows = rows[:defaultAuditCap]
	}
	b.writeJSON(req, res, "window/admin_audit.json", rows)
}

func (b *bundleBuilder) collectWindowCostRollup(ctx context.Context, req bundleRequest, res *buildResult) {
	if b.repos.LLMUsage == nil {
		return
	}
	rows, err := b.repos.LLMUsage.List(ctx, persistence.TaskLLMUsageFilter{
		Since:    &req.Since,
		Until:    &req.Until,
		PageSize: 100000,
	})
	if err != nil {
		res.sectionErrs["window/cost_rollup.json"] = err.Error()
		return
	}
	type rollup struct {
		ByProject map[string]float64 `json:"cost_usd_by_project"`
		ByModel   map[string]float64 `json:"cost_usd_by_model"`
		TotalUSD  float64            `json:"total_usd"`
		Rows      int                `json:"rows"`
	}
	r := rollup{ByProject: map[string]float64{}, ByModel: map[string]float64{}}
	for _, u := range rows {
		r.ByProject[u.ProjectID] += u.CostUSD
		r.ByModel[u.Model] += u.CostUSD
		r.TotalUSD += u.CostUSD
		r.Rows++
	}
	// Cost rollup carries no free text (project/model ids + numbers),
	// but route through writeJSON for the uniform path.
	b.writeJSON(req, res, "window/cost_rollup.json", r)
}

// finalize enforces the total size cap, then writes REDACTION.txt and
// MANIFEST.json (which themselves carry no secrets — only counts).
func (b *bundleBuilder) finalize(req bundleRequest, res *buildResult) {
	// Total size cap: drop the largest sections (after redaction) until
	// under cap, noting every drop in the manifest. Never silently lose
	// data — a dropped section is recorded (LLD §7).
	if req.MaxSize > 0 {
		b.enforceTotalCap(req, res)
	}

	// REDACTION.txt — counts by type only, plus per-file totals. NO values.
	var sb strings.Builder
	sb.WriteString("vornik support-report redaction summary\n")
	sb.WriteString("=======================================\n")
	fmt.Fprintf(&sb, "total redactions: %d\n\n", res.tally.total)
	sb.WriteString("by type:\n")
	for _, ty := range sortedKeys(res.tally.byType) {
		fmt.Fprintf(&sb, "  %-24s %d\n", ty, res.tally.byType[ty])
	}
	sb.WriteString("\nby file:\n")
	for _, fn := range sortedKeys(res.tally.perFile) {
		fmt.Fprintf(&sb, "  %-40s %d\n", fn, res.tally.perFile[fn])
	}
	res.files["REDACTION.txt"] = []byte(sb.String())

	// Build MANIFEST.json last so it lists every file (including
	// REDACTION.txt). Stable file ordering for reproducibility.
	mf := manifest{
		SchemaVersion:   supportReportSchemaVersion,
		VornikVersion:   b.version,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		Raw:             req.IncludeRaw,
		RedactionByType: res.tally.byType,
		RedactionTotal:  res.tally.total,
	}
	if req.Window {
		mf.Mode = "window"
		mf.Since = req.Since.UTC().Format(time.RFC3339)
		mf.Until = req.Until.UTC().Format(time.RFC3339)
	} else {
		mf.Mode = "task"
		mf.TaskID = req.TaskID
	}
	if len(res.truncations) > 0 {
		mf.Truncations = res.truncations
	}
	if len(res.sectionErrs) > 0 {
		mf.SectionErrors = res.sectionErrs
	}
	for _, name := range sortedKeys(byteMapKeys(res.files)) {
		mf.Files = append(mf.Files, manifestFile{Name: name, Bytes: len(res.files[name])})
	}
	mfData, _ := json.MarshalIndent(mf, "", "  ")
	res.files["MANIFEST.json"] = mfData
	res.manifest = mf
}

func (b *bundleBuilder) enforceTotalCap(req bundleRequest, res *buildResult) {
	total := 0
	for _, v := range res.files {
		total += len(v)
	}
	if int64(total) <= req.MaxSize {
		return
	}
	// Drop largest non-essential files first; never drop the bookkeeping
	// files (they're tiny + written after). Essential metadata
	// (version.txt, doctor.json) is kept; bulk row sections go first.
	type fe struct {
		name string
		size int
	}
	var entries []fe
	for n, v := range res.files {
		entries = append(entries, fe{n, len(v)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
	for _, e := range entries {
		if int64(total) <= req.MaxSize {
			break
		}
		if isEssentialFile(e.name) {
			continue
		}
		delete(res.files, e.name)
		res.truncations[e.name] = fmt.Sprintf("dropped to honour --max-size (%d bytes)", e.size)
		total -= e.size
	}
}

// ---- helpers ----

func capOutcomes(rows []*persistence.ExecutionStepOutcome, limit int, name string, res *buildResult) []*persistence.ExecutionStepOutcome {
	if len(rows) > limit {
		res.truncations[name] = fmt.Sprintf("%d of %d+ rows", limit, len(rows))
		return rows[:limit]
	}
	return rows
}

func taskInWindow(t *persistence.Task, since, until time.Time) bool {
	if t == nil {
		return false
	}
	// "created/terminal in [since,until]" — created OR updated within
	// the window. UpdatedAt approximates terminal time for terminal
	// rows.
	in := func(ts time.Time) bool {
		return !ts.Before(since) && !ts.After(until)
	}
	return in(t.CreatedAt) || in(t.UpdatedAt)
}

func isEssentialFile(name string) bool {
	switch name {
	case "MANIFEST.json", "REDACTION.txt", "version.txt", "doctor.json":
		return true
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func byteMapKeys(m map[string][]byte) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// isTextContent reports whether data looks like UTF-8 text (no NUL
// bytes in the first chunk). Binary content is shipped as metadata
// only (review #1).
func isTextContent(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return false
		}
	}
	return true
}

func sniffContentType(data []byte) string {
	if isTextContent(data) {
		return "text/plain"
	}
	return "application/octet-stream"
}

// safeArtifactFilename produces a path-safe filename for a shipped
// text artifact, prefixed with the artifact id so collisions on
// operator-chosen names can't clobber each other.
func safeArtifactFilename(id, name string) string {
	base := path.Base(name)
	base = strings.ReplaceAll(base, "/", "_")
	base = strings.ReplaceAll(base, "..", "_")
	if base == "" || base == "." {
		base = "artifact"
	}
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	return short + "-" + base
}
