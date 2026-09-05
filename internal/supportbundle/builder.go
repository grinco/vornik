// Package supportbundle collects the diagnostics archive that
// `vornikctl support-report` produces. It lived in internal/api until
// 2026-09-04, when it moved here so the CLI could drive the SAME collector for
// Community-Edition local collection without importing the HTTP server — one
// collector, because the thing a second one would duplicate is the redaction
// path (support-bundle-in-CE design §3).
package supportbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/version"
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
	ContainerLogTail     = 2000    // lines
)

// Repos is the narrow set of repositories the builder reads.
// Each is optional: a nil repo (or one that errors) records a
// best-effort section error and the build continues (LLD §7). The
// concrete persistence repositories satisfy these structurally; tests
// pass fakes.
type Repos struct {
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

// ArtifactOpener reads the bytes of an artifact so the builder
// can classify text vs binary + compute a sha256. *artifacts.Store
// satisfies the same Open shape used elsewhere in this package.
type ArtifactOpener interface {
	Open(ctx context.Context, artifactID string) (ReadCloser, error)
}

// ReadCloser is io.ReadCloser, aliased so the interface above doesn't
// pull io into every fake's signature noise. (It is io.ReadCloser.)
type ReadCloser interface {
	Read(p []byte) (int, error)
	Close() error
}

// DoctorRunner runs the in-process doctor checks and returns a
// JSON-marshalable report. nil → doctor.json records "not configured".
type DoctorRunner interface {
	Run(ctx context.Context) (any, error)
}

// HealthSource and MetricsSource provide the always-on
// health + metrics snapshots. Both optional.
type HealthSource interface {
	Snapshot(ctx context.Context) (any, error)
}

// MetricsSource snapshots the Prometheus metrics text.
type MetricsSource interface {
	Snapshot(ctx context.Context) (string, error)
}

// Builder collects sections into an in-memory staging tree
// (map of relative path → bytes). The handler writes the tree to a
// staging dir and tars it. Keeping the tree in memory makes the
// redaction-coverage test able to assert over every produced byte
// without touching disk.
type Builder struct {
	Repos    Repos
	Opener   ArtifactOpener
	Doctor   DoctorRunner
	Health   HealthSource
	Metrics  MetricsSource
	Detector secrets.Detector
	Version  string
	// edition is the build's edition, normalized. It is NOT decoration: half
	// this bundle's diagnostic surface exists in one edition and not the other
	// (the admin endpoints, the blackbox trace, the EE providers), so an
	// ABSENT section means "not built into this edition" on Community and
	// "broken" on Enterprise — opposite diagnoses from identical evidence. A
	// support engineer who has to infer the edition from what is missing is
	// being asked to guess the thing that decides how to read the rest.
	Edition string
	// blackbox is the EE trace seam (nil on Community, or when the deployment
	// isn't Postgres-backed). The assembled trace is the derived timeline that
	// explains WHY a task went the way it did, and it was the one per-task
	// evidence the bundle never collected — so a problem report about a task
	// could not carry it (operator request 2026-08-03).
	Blackbox TraceService
	// config is the already-redacted config YAML snapshot. The
	// handler renders it (config marshaling lives on the Server); the
	// builder runs it through redaction again defensively.
	ConfigYAML string
	// registry is the DEPLOYED workflow / swarm / project set, prompts
	// included. Added 2026-09-04 because its absence was half a diagnosis: the
	// customer-reported forge-review bug turned on the `github-review` prompt
	// asserting "the previous step provided the diff" while the executor was
	// sending nothing, and a bundle carrying every execution row but no prompt
	// cannot show that. nil degrades the section away like every other repo.
	Registry RegistryReader
	// webhooks is the ingress audit — status and error_code only, never
	// payloads. An inbound-triggered task that never started leaves its only
	// trace here.
	Webhooks WebhookReader
}

// TraceService is the EE trace seam, narrowed to what the bundle needs. It
// travels with the collector rather than staying in internal/api: this package
// must not import the HTTP server, and the value is opaque here anyway — the
// enterprise adapter owns the concrete type and this code never asserts it,
// which is what let the interface move at all.
type TraceService interface {
	AssembleCached(ctx context.Context, taskID string) (trace any, cached bool, err error)
}

// RegistryReader is the deployed registry, narrowed to what the bundle
// reads. Satisfied by *registry.Registry.
type RegistryReader interface {
	ListWorkflows() []*registry.Workflow
	ListSwarms() []*registry.Swarm
	ListProjects() []*registry.Project
}

// WebhookReader is the webhook ingress audit, narrowed to List.
type WebhookReader interface {
	List(ctx context.Context, filter persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error)
}

// Request is the resolved, validated request the builder acts
// on. Exactly one of TaskID / window (Since,Until) is set.
type Request struct {
	TaskID     string
	Since      time.Time
	Until      time.Time
	Window     bool
	MaxSize    int64
	IncludeRaw bool
}

// RedactionTally accumulates per-type redaction counts across the
// whole bundle for REDACTION.txt. It never stores matched values.
type RedactionTally struct {
	ByType  map[string]int
	PerFile map[string]int
	Total   int
}

// NewRedactionTally returns an empty tally with its maps ready.
func NewRedactionTally() *RedactionTally {
	return &RedactionTally{ByType: map[string]int{}, PerFile: map[string]int{}}
}

// Manifest is the MANIFEST.json shape.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Mode          string `json:"mode"` // "task" | "window"
	TaskID        string `json:"task_id,omitempty"`
	Since         string `json:"since,omitempty"`
	Until         string `json:"until,omitempty"`
	VornikVersion string `json:"vornik_version"`
	// VornikEdition is "community" or "enterprise". Stated in the Manifest as
	// well as version.txt because the Manifest is what a tool reads, and a
	// section missing from Files means opposite things in the two editions.
	VornikEdition   string            `json:"vornik_edition"`
	GeneratedAt     string            `json:"generated_at"`
	Raw             bool              `json:"raw"`
	ArchiveSHA256   string            `json:"archive_sha256,omitempty"`
	Files           []ManifestFile    `json:"files"`
	Truncations     map[string]string `json:"truncations,omitempty"`
	SectionErrors   map[string]string `json:"section_errors,omitempty"`
	RedactionByType map[string]int    `json:"redaction_by_type"`
	RedactionTotal  int               `json:"redaction_total"`
}

// ManifestFile is one entry of the Manifest's file list.
type ManifestFile struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// Result is the in-memory staging tree plus the Manifest.
type Result struct {
	Files       map[string][]byte // relative path -> content (Manifest + redaction.txt added last)
	Manifest    Manifest
	Tally       *RedactionTally
	Truncations map[string]string
	SectionErrs map[string]string
}

// Build collects every section for req. It NEVER returns a fatal error
// for a missing/failed section — those are recorded and the build
// continues (best-effort, LLD §7). It returns an error only for a
// programming-level problem (nil detector with redaction on).
func (b *Builder) Build(ctx context.Context, req Request) (*Result, error) {
	if !req.IncludeRaw && b.Detector == nil {
		return nil, fmt.Errorf("support-report: redaction requested but no secrets detector wired")
	}
	res := &Result{
		Files:       map[string][]byte{},
		Tally:       NewRedactionTally(),
		Truncations: map[string]string{},
		SectionErrs: map[string]string{},
	}

	// Always-on sections.
	b.collectDoctor(ctx, req, res)
	b.collectConfig(req, res)
	b.collectVersion(req, res)
	b.collectHealth(ctx, req, res)
	b.collectMetrics(ctx, req, res)
	b.collectRegistry(req, res)
	b.collectWebhookEvents(ctx, req, res)

	if req.Window {
		b.collectWindow(ctx, req, res)
	} else {
		b.collectTask(ctx, req, res)
	}

	b.Finalize(req, res)
	return res, nil
}

// writeText redacts (unless raw) the payload for `name`, applies it to
// the tally, and stores it in the staging tree. Redaction runs on the
// FULL text before any caller-side truncation (the callers truncate
// ROW COUNTS, not bytes, and serialize the already-capped rows here —
// so the secret-straddling-cap hazard from review #7 cannot occur).
func (b *Builder) writeText(req Request, res *Result, name string, payload []byte) {
	if req.IncludeRaw {
		res.Files[name] = payload
		return
	}
	findings := b.Detector.Scan(payload)
	if len(findings) > 0 {
		redacted := secrets.Redact(payload, findings)
		for _, f := range findings {
			res.Tally.ByType[f.Type]++
			res.Tally.Total++
		}
		res.Tally.PerFile[name] += len(findings)
		res.Files[name] = redacted
		return
	}
	res.Files[name] = payload
}

// writeJSON marshals v then routes through writeText so JSON row
// payloads are redacted identically to plain text.
func (b *Builder) writeJSON(req Request, res *Result, name string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		res.SectionErrs[name] = fmt.Sprintf("marshal: %v", err)
		return
	}
	b.writeText(req, res, name, data)
}

func (b *Builder) collectDoctor(ctx context.Context, req Request, res *Result) {
	if b.Doctor == nil {
		res.Files["doctor.json"] = []byte(`{"note":"doctor runner not configured"}`)
		return
	}
	rep, err := b.Doctor.Run(ctx)
	if err != nil {
		res.SectionErrs["doctor.json"] = err.Error()
		b.writeJSON(req, res, "doctor.json", map[string]string{"error": err.Error()})
		return
	}
	b.writeJSON(req, res, "doctor.json", rep)
}

func (b *Builder) collectConfig(req Request, res *Result) {
	if strings.TrimSpace(b.ConfigYAML) == "" {
		res.Files["config.redacted.yaml"] = []byte("# config snapshot not available\n")
		return
	}
	b.writeText(req, res, "config.redacted.yaml", []byte(b.ConfigYAML))
}

func (b *Builder) collectVersion(req Request, res *Result) {
	v := b.Version
	if v == "" {
		v = "unknown"
	}
	// EDITION IS STATED, never inferred. It carried the bare version string
	// until 2026-09-04, which left the reader of a bundle to work out from
	// absent sections whether this was Community or Enterprise — and those
	// sections are absent for opposite reasons in the two editions.
	//
	// NormalizeEdition treats anything that is not exactly "enterprise" as
	// Community, so an unstamped build reports the less-privileged edition
	// rather than claiming one it may not be.
	edition := version.NormalizeEdition(b.Edition)
	body := fmt.Sprintf("version: %s\nedition: %s\n", v, edition)
	// Version is operator-trusted, but route through writeText for a
	// uniform path (it won't contain secrets).
	b.writeText(req, res, "version.txt", []byte(body))
}

func (b *Builder) collectHealth(ctx context.Context, req Request, res *Result) {
	if b.Health == nil {
		res.Files["health.json"] = []byte(`{"note":"health source not configured"}`)
		return
	}
	snap, err := b.Health.Snapshot(ctx)
	if err != nil {
		res.SectionErrs["health.json"] = err.Error()
		return
	}
	b.writeJSON(req, res, "health.json", snap)
}

func (b *Builder) collectMetrics(ctx context.Context, req Request, res *Result) {
	if b.Metrics == nil {
		res.Files["metrics.txt"] = []byte("# metrics source not configured\n")
		return
	}
	txt, err := b.Metrics.Snapshot(ctx)
	if err != nil {
		res.SectionErrs["metrics.txt"] = err.Error()
		return
	}
	b.writeText(req, res, "metrics.txt", []byte(txt))
}

// ---- per-task sections ----

func (b *Builder) collectTask(ctx context.Context, req Request, res *Result) {
	tid := req.TaskID
	if b.Repos.Tasks != nil {
		task, err := b.Repos.Tasks.Get(ctx, tid)
		if err != nil {
			res.SectionErrs["task/task.json"] = err.Error()
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
	b.collectBlackBoxTrace(ctx, req, res, tid)
}

// collectBlackBoxTrace writes the task's assembled Black Box trace.
//
// Three absences, one failure: an unwired seam (Community build, or Black Box
// disabled) and a task with no audit data are ABSENCES — no file, no section
// error, because "this edition doesn't have it" is not a collection failure. A
// genuine assembly error is recorded and the build continues (best-effort, LLD
// §7). The trace is opaque `any` from EE and goes through writeJSON, so it is
// redacted like every other section (the coverage gate asserts this).
func (b *Builder) collectBlackBoxTrace(ctx context.Context, req Request, res *Result, tid string) {
	if b.Blackbox == nil {
		return
	}
	trace, _, err := b.Blackbox.AssembleCached(ctx, tid)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			return
		}
		res.SectionErrs["task/blackbox_trace.json"] = err.Error()
		return
	}
	if trace == nil {
		return
	}
	b.writeJSON(req, res, "task/blackbox_trace.json", trace)
}

func (b *Builder) collectExecutions(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.Executions == nil {
		return
	}
	rows, err := b.Repos.Executions.List(ctx, persistence.ExecutionFilter{TaskID: &tid, PageSize: 1000})
	if err != nil {
		res.SectionErrs["task/executions.json"] = err.Error()
		return
	}
	b.writeJSON(req, res, "task/executions.json", rows)
}

func (b *Builder) collectOutcomes(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.Outcomes == nil {
		return
	}
	rows, err := b.Repos.Outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{TaskID: &tid, PageSize: defaultOutcomeCap + 1})
	if err != nil {
		res.SectionErrs["task/step_outcomes.json"] = err.Error()
		return
	}
	rows = capOutcomes(rows, defaultOutcomeCap, "task/step_outcomes.json", res)
	b.writeJSON(req, res, "task/step_outcomes.json", rows)
}

func (b *Builder) collectToolAudit(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.ToolAudit == nil {
		return
	}
	rows, err := b.Repos.ToolAudit.List(ctx, persistence.ToolAuditFilter{TaskID: &tid, PageSize: defaultToolAuditCap + 1})
	if err != nil {
		res.SectionErrs["task/tool_audit.json"] = err.Error()
		return
	}
	if len(rows) > defaultToolAuditCap {
		res.Truncations["task/tool_audit.json"] = fmt.Sprintf("%d of %d+ rows", defaultToolAuditCap, len(rows))
		rows = rows[:defaultToolAuditCap]
	}
	b.writeJSON(req, res, "task/tool_audit.json", rows)
}

func (b *Builder) collectUsage(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.LLMUsage == nil {
		return
	}
	rows, err := b.Repos.LLMUsage.List(ctx, persistence.TaskLLMUsageFilter{TaskID: &tid, PageSize: defaultUsageCap + 1})
	if err != nil {
		res.SectionErrs["task/llm_usage.json"] = err.Error()
		return
	}
	if len(rows) > defaultUsageCap {
		res.Truncations["task/llm_usage.json"] = fmt.Sprintf("%d of %d+ rows", defaultUsageCap, len(rows))
		rows = rows[:defaultUsageCap]
	}
	b.writeJSON(req, res, "task/llm_usage.json", rows)
}

func (b *Builder) collectMessages(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.Messages == nil {
		return
	}
	rows, err := b.Repos.Messages.List(ctx, persistence.TaskMessageFilter{TaskID: tid, Limit: defaultMessageCap + 1})
	if err != nil {
		res.SectionErrs["task/messages.json"] = err.Error()
		return
	}
	if len(rows) > defaultMessageCap {
		res.Truncations["task/messages.json"] = fmt.Sprintf("%d of %d+ rows", defaultMessageCap, len(rows))
		rows = rows[:defaultMessageCap]
	}
	b.writeJSON(req, res, "task/messages.json", rows)
}

func (b *Builder) collectJudge(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.JudgeVerdct == nil {
		return
	}
	v, err := b.Repos.JudgeVerdct.GetByTask(ctx, tid)
	if err != nil {
		res.SectionErrs["task/judge.json"] = err.Error()
		return
	}
	if v == nil {
		return
	}
	b.writeJSON(req, res, "task/judge.json", v)
}

func (b *Builder) collectPostMortem(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.PostMortem == nil {
		return
	}
	pm, err := b.Repos.PostMortem.Get(ctx, tid)
	if err != nil {
		res.SectionErrs["task/postmortem.json"] = err.Error()
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

func (b *Builder) collectArtifacts(ctx context.Context, req Request, res *Result, tid string) {
	if b.Repos.Artifacts == nil {
		return
	}
	rows, err := b.Repos.Artifacts.List(ctx, persistence.ArtifactFilter{TaskID: &tid, PageSize: defaultArtifactCap + 1})
	if err != nil {
		res.SectionErrs["task/artifacts/MANIFEST.json"] = err.Error()
		return
	}
	if len(rows) > defaultArtifactCap {
		res.Truncations["task/artifacts/MANIFEST.json"] = fmt.Sprintf("%d of %d+ artifacts", defaultArtifactCap, len(rows))
		rows = rows[:defaultArtifactCap]
	}
	metas := make([]artifactMeta, 0, len(rows))
	for _, a := range rows {
		m := artifactMeta{ID: a.ID, Name: a.Name}
		if b.Opener != nil {
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
				res.SectionErrs["task/artifacts/"+a.ID] = err.Error()
			}
		}
		metas = append(metas, m)
	}
	// MANIFEST has no bytes — metadata is operator-authored names +
	// hashes; route through writeJSON for uniform redaction (names
	// may, rarely, embed a token).
	b.writeJSON(req, res, "task/artifacts/MANIFEST.json", metas)
}

func (b *Builder) readArtifact(ctx context.Context, id string) ([]byte, string, error) {
	rc, err := b.Opener.Open(ctx, id)
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

func (b *Builder) collectContainerLogs(ctx context.Context, req Request, res *Result, tid string) {
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
func (b *Builder) WriteContainerLogs(req Request, res *Result, logs string) {
	if strings.TrimSpace(logs) == "" {
		return
	}
	b.writeText(req, res, "task/container_logs.txt", []byte(logs))
}

// ---- window sections ----

// collectRegistry writes the DEPLOYED workflow, swarm and project definitions —
// prompts included.
//
// Prompts are the point. The bundle already carried every execution row for a
// task and still could not explain the customer-reported forge-review failure,
// because the mechanism was a `github-review` prompt telling the agent "the
// previous step provided the diff" while the executor sent nothing. Rows show
// WHAT happened; the prompt shows what the agent was told to believe.
//
// Everything here goes through the same detector as every other section, so a
// prompt carrying a pasted credential is redacted like anything else.
func (b *Builder) collectRegistry(req Request, res *Result) {
	if b.Registry == nil {
		// A deployment with no loaded registry omits the section rather than
		// failing the bundle — the same degradation every repo gets.
		return
	}
	// Capped like every other list section. The registry is small on the
	// reference deployment (24 workflows, 8 projects), but "small here" is not
	// a bound, and an uncapped section is how a bundle becomes a database dump
	// with extra steps. The truncation is RECORDED, so a support engineer
	// reading a capped file knows it is capped (review 2026-09-04, finding f).
	// Sorted by ID, because the registry's List* methods walk a map and so
	// return a different order on every call. Two bundles collected from the
	// SAME deployment differed only by that ordering, which makes them
	// undiffable — and it is what the structural-parity test caught first
	// (2026-09-04): the two drivers agreed on every byte except the order.
	registrySection(req, res, b, "registry/workflows.json",
		toAny(sortedByID(b.Registry.ListWorkflows(), func(w *registry.Workflow) string {
			if w == nil {
				return ""
			}
			return w.ID
		})))
	registrySection(req, res, b, "registry/swarms.json",
		toAny(sortedByID(b.Registry.ListSwarms(), func(s *registry.Swarm) string {
			if s == nil {
				return ""
			}
			return s.ID
		})))
	registrySection(req, res, b, "registry/projects.json",
		toAny(sortedByID(b.Registry.ListProjects(), func(p *registry.Project) string {
			if p == nil {
				return ""
			}
			return p.ID
		})))
}

// defaultRegistryCap bounds each registry section. Generous: a deployment with
// more than this many workflows has a different problem, and the operator still
// gets the first N plus a note saying how many there were.
const defaultRegistryCap = 500

// sortedByID copies and orders a registry listing so the section is stable
// across collections. A nil entry sorts first rather than panicking: the
// bundle degrades, it does not fail.
func sortedByID[T any](in []T, id func(T) string) []T {
	out := make([]T, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return id(out[i]) < id(out[j]) })
	return out
}

func toAny[T any](in []T) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func registrySection(req Request, res *Result, b *Builder, name string, rows []any) {
	if len(rows) > defaultRegistryCap {
		res.Truncations[name] = fmt.Sprintf("%d of %d entries", defaultRegistryCap, len(rows))
		rows = rows[:defaultRegistryCap]
	}
	b.writeJSON(req, res, name, rows)
}

// supportWebhookPageSize bounds the ingress audit. The section is context for a
// report, not an export: newest-first, capped, and it never carries payloads.
const supportWebhookPageSize = 200

// collectWebhookEvents writes recent webhook ingress rows — status and error
// code, never the payload.
//
// The payload is deliberately absent rather than redacted: it is third-party
// content of unbounded shape, and a bundle that leaves the operator's trust
// boundary should not carry it on the strength of a scan. `payload_hash` is
// enough to correlate two reports of the same event; the status and error_code
// are what say whether the ingress accepted, rejected or dropped it.
//
// This is the only trace an inbound-triggered task leaves when it never became
// a task at all, which is exactly the report that arrives as "the webhook did
// nothing".
func (b *Builder) collectWebhookEvents(ctx context.Context, req Request, res *Result) {
	if b.Webhooks == nil {
		return
	}
	events, err := b.Webhooks.List(ctx, persistence.WebhookEventFilter{PageSize: supportWebhookPageSize})
	if err != nil {
		res.SectionErrs["webhook_events.json"] = err.Error()
		return
	}
	type webhookRow struct {
		ID          string    `json:"id"`
		ProjectID   string    `json:"project_id"`
		Source      string    `json:"source"`
		EventID     string    `json:"event_id"`
		PayloadHash string    `json:"payload_hash"`
		Status      string    `json:"status"`
		TaskID      *string   `json:"task_id,omitempty"`
		ErrorCode   string    `json:"error_code,omitempty"`
		CreatedAt   time.Time `json:"created_at"`
	}
	rows := make([]webhookRow, 0, len(events))
	for _, e := range events {
		if e == nil {
			continue
		}
		// ErrorMessage is dropped with the payload, and this was MEASURED
		// rather than assumed (review 2026-09-04, finding e, which argued the
		// messages are template-driven and could be carried safely). Of the
		// sixteen recordWebhookEvent call sites, thirteen pass a literal and
		// three pass a dynamic value: err.Error() for invalid_json and
		// create_task_failed, and — the decisive one — the `reason` on the
		// secret_leak path, whose whole subject is content that must not
		// leave. Carrying the field would mean trusting the detector to
		// re-detect a secret inside a description OF that secret.
		rows = append(rows, webhookRow{
			ID: e.ID, ProjectID: e.ProjectID, Source: e.Source, EventID: e.EventID,
			PayloadHash: e.PayloadHash, Status: e.Status, TaskID: e.TaskID,
			ErrorCode: e.ErrorCode, CreatedAt: e.CreatedAt,
		})
	}
	b.writeJSON(req, res, "webhook_events.json", rows)
}

func (b *Builder) collectWindow(ctx context.Context, req Request, res *Result) {
	b.collectWindowTasks(ctx, req, res)
	b.collectWindowAdminAudit(ctx, req, res)
	b.collectWindowCostRollup(ctx, req, res)
}

func (b *Builder) collectWindowTasks(ctx context.Context, req Request, res *Result) {
	if b.Repos.Tasks == nil {
		return
	}
	all, err := b.Repos.Tasks.List(ctx, persistence.TaskFilter{PageSize: 100000})
	if err != nil {
		res.SectionErrs["window/tasks.json"] = err.Error()
		return
	}
	filtered := make([]*persistence.Task, 0, len(all))
	for _, t := range all {
		if taskInWindow(t, req.Since, req.Until) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) > defaultTaskCap {
		res.Truncations["window/tasks.json"] = fmt.Sprintf("%d of %d in-window tasks", defaultTaskCap, len(filtered))
		filtered = filtered[:defaultTaskCap]
	}
	b.writeJSON(req, res, "window/tasks.json", filtered)
}

func (b *Builder) collectWindowAdminAudit(ctx context.Context, req Request, res *Result) {
	if b.Repos.AdminAudit == nil {
		return
	}
	rows, err := b.Repos.AdminAudit.List(ctx, persistence.AdminAuditFilter{
		Since:    req.Since,
		Until:    req.Until,
		PageSize: defaultAuditCap + 1,
	})
	if err != nil {
		res.SectionErrs["window/admin_audit.json"] = err.Error()
		return
	}
	if len(rows) > defaultAuditCap {
		res.Truncations["window/admin_audit.json"] = fmt.Sprintf("%d of %d+ rows", defaultAuditCap, len(rows))
		rows = rows[:defaultAuditCap]
	}
	b.writeJSON(req, res, "window/admin_audit.json", rows)
}

func (b *Builder) collectWindowCostRollup(ctx context.Context, req Request, res *Result) {
	if b.Repos.LLMUsage == nil {
		return
	}
	rows, err := b.Repos.LLMUsage.List(ctx, persistence.TaskLLMUsageFilter{
		Since:    &req.Since,
		Until:    &req.Until,
		PageSize: 100000,
	})
	if err != nil {
		res.SectionErrs["window/cost_rollup.json"] = err.Error()
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

// Finalize enforces the total size cap, then writes REDACTION.txt and
// MANIFEST.json (which themselves carry no secrets — only counts).
func (b *Builder) Finalize(req Request, res *Result) {
	// Total size cap: drop the largest sections (after redaction) until
	// under cap, noting every drop in the Manifest. Never silently lose
	// data — a dropped section is recorded (LLD §7).
	if req.MaxSize > 0 {
		b.enforceTotalCap(req, res)
	}

	// REDACTION.txt — counts by type only, plus per-file totals. NO values.
	var sb strings.Builder
	sb.WriteString("vornik support-report redaction summary\n")
	sb.WriteString("=======================================\n")
	fmt.Fprintf(&sb, "total redactions: %d\n\n", res.Tally.Total)
	sb.WriteString("by type:\n")
	for _, ty := range sortedKeys(res.Tally.ByType) {
		fmt.Fprintf(&sb, "  %-24s %d\n", ty, res.Tally.ByType[ty])
	}
	sb.WriteString("\nby file:\n")
	for _, fn := range sortedKeys(res.Tally.PerFile) {
		fmt.Fprintf(&sb, "  %-40s %d\n", fn, res.Tally.PerFile[fn])
	}
	// WHAT THE COUNTS ABOVE DO NOT COVER. A number with no scope reads as a
	// guarantee. The detector finds SECRETS — keys, tokens, credentials — and
	// nothing in it recognises a customer name, an internal hostname, a Jira
	// key or a repository URL. Those are harmless to a credential scanner and
	// are exactly what an operator must look for before handing the archive to
	// a third party, so the bundle says so where the counts are read rather
	// than only in a design document (review 2026-09-04, finding d).
	sb.WriteString(`
scope of these counts
---------------------
Redaction finds SECRETS: keys, tokens, credentials, high-entropy strings.
It does NOT remove business identifiers. Prompts, workflow and project
definitions (registry/) are carried verbatim apart from secrets, and can
contain customer names, internal hostnames, repository URLs and ticket keys.
Review registry/ and any prompt text before sending this archive outside your
organisation.
`)
	res.Files["REDACTION.txt"] = []byte(sb.String())

	// Build MANIFEST.json last so it lists every file (including
	// REDACTION.txt). Stable file ordering for reproducibility.
	mf := Manifest{
		SchemaVersion:   supportReportSchemaVersion,
		VornikVersion:   b.Version,
		VornikEdition:   version.NormalizeEdition(b.Edition),
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		Raw:             req.IncludeRaw,
		RedactionByType: res.Tally.ByType,
		RedactionTotal:  res.Tally.Total,
	}
	if req.Window {
		mf.Mode = "window"
		mf.Since = req.Since.UTC().Format(time.RFC3339)
		mf.Until = req.Until.UTC().Format(time.RFC3339)
	} else {
		mf.Mode = "task"
		mf.TaskID = req.TaskID
	}
	if len(res.Truncations) > 0 {
		mf.Truncations = res.Truncations
	}
	if len(res.SectionErrs) > 0 {
		mf.SectionErrors = res.SectionErrs
	}
	for _, name := range sortedKeys(byteMapKeys(res.Files)) {
		mf.Files = append(mf.Files, ManifestFile{Name: name, Bytes: len(res.Files[name])})
	}
	mfData, _ := json.MarshalIndent(mf, "", "  ")
	res.Files["MANIFEST.json"] = mfData
	res.Manifest = mf
}

func (b *Builder) enforceTotalCap(req Request, res *Result) {
	total := 0
	for _, v := range res.Files {
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
	for n, v := range res.Files {
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
		delete(res.Files, e.name)
		res.Truncations[e.name] = fmt.Sprintf("dropped to honour --max-size (%d bytes)", e.size)
		total -= e.size
	}
}

// ---- helpers ----

func capOutcomes(rows []*persistence.ExecutionStepOutcome, limit int, name string, res *Result) []*persistence.ExecutionStepOutcome {
	if len(rows) > limit {
		res.Truncations[name] = fmt.Sprintf("%d of %d+ rows", limit, len(rows))
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

// RedactedConfigYAML field-name-redacts a config struct and marshals it to
// YAML, which is what config.redacted.yaml carries.
//
// It is exported because BOTH drivers render this section — the daemon from
// the Server's config, the CLI from the file it loaded — and a second
// implementation of a redaction step is a second thing that can leak. The
// builder additionally runs the result through value-pattern redaction, so a
// secret in a field whose NAME does not look secret is still caught.
func RedactedConfigYAML(cfg any) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", err
	}
	out, err := yaml.Marshal(secrets.RedactConfig(generic))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ParseWindow resolves the since/until pair into an absolute window. Each
// accepts an RFC3339 timestamp or a Go duration, which is read as
// "now - duration"; until defaults to now.
//
// Exported for the same reason RedactedConfigYAML is: both drivers turn the
// operator's --since into a Request, and a window that means one thing over
// HTTP and another locally would produce two bundles that disagree about what
// they cover.
func ParseWindow(sinceStr, untilStr string) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	since, err := ParseTimeOrDuration(sinceStr, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid since: %w", err)
	}
	until := now
	if strings.TrimSpace(untilStr) != "" {
		until, err = ParseTimeOrDuration(untilStr, now)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid until: %w", err)
		}
	}
	if until.Before(since) {
		return time.Time{}, time.Time{}, fmt.Errorf("until is before since")
	}
	return since, until, nil
}

// ParseTimeOrDuration reads an RFC3339 timestamp, or a Go duration relative to
// ref (going backwards, as "--since 2h" means).
func ParseTimeOrDuration(s string, ref time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return ref.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("not an RFC3339 timestamp or Go duration: %q", s)
}
