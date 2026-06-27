// Code in this file owns the daemon-level admin surface — see
// https://docs.vornik.io Slice 1: read-only
// (with one idempotent POST for MCP refresh), gated by the
// `admin` package middleware.

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

// MCPInventorySource returns a snapshot of the daemon-level MCP
// catalog for the admin pages. Defined as an interface so the
// ui package doesn't pull internal/mcp (which transitively pulls
// chat + a JSON-RPC client) into its test image. Implementations
// live in service container wiring.
type MCPInventorySource interface {
	Snapshot() AdminMCPSnapshot
}

// AdminMCPSnapshot is the small, ui-facing shape the admin MCP
// page renders. ProjectCount / ServerCount mirror the manager's
// own counters; PerProject holds the actual per-project server
// list. Empty when no MCP wiring is active.
type AdminMCPSnapshot struct {
	ProjectCount int
	ServerCount  int
	PerProject   []AdminMCPProjectRow
}

// AdminMCPProjectRow describes one project's MCP catalog row.
type AdminMCPProjectRow struct {
	ProjectID string
	Servers   []AdminMCPServerRow
}

// AdminMCPServerRow is one server entry. ToolCount is the
// discovered-tools count after the initial handshake.
type AdminMCPServerRow struct {
	Name      string
	ToolCount int
	Connected bool
}

// MCPRefresher is the narrow surface the admin-mcp page needs to
// re-dial every MCP server. Implementations must be idempotent;
// the admin handler writes an audit row before calling it.
type MCPRefresher interface {
	RefreshAll(ctx context.Context) error
}

// MCPConfigSource returns the daemon-level MCP server inventory
// from config (the read-only side of the integrations page —
// edit is slice 3). Names are the per-project server names
// scoped by project id; the slice 1 implementation just lists
// what's configured without diffing it against what's connected.
type MCPConfigSource interface {
	ConfiguredMCPServers() []AdminMCPProjectRow
}

// ReadinessProvider runs the same checks /readyz exposes over HTTP
// but returns them in-process so the admin landing tile doesn't
// have to self-HTTP-call. Wired in container_http.go alongside the
// /readyz handler so they stay in lockstep.
type ReadinessProvider interface {
	ReadinessChecks(ctx context.Context) []AdminReadinessCheck
}

// AdminReadinessCheck mirrors the /readyz JSON shape — Name + a
// terse "ok" / "error" status + an opaque error message when
// failed. Same generic "check failed" string the public /readyz
// surface uses; admin-page operators see no extra detail leaked.
type AdminReadinessCheck struct {
	Name   string
	Status string
	Error  string
}

// LeaseAuditSource provides the data backing /ui/admin/health/leases.
// Implementations query the `tasks_lease_audit` view from migration
// v27. Behind an interface so the ui package's test fixtures don't
// need a running postgres.
type LeaseAuditSource interface {
	CountByStatus(ctx context.Context) (map[string]int64, error)
	Recent(ctx context.Context, limit int) ([]AdminLeaseAuditRow, error)
}

// AdminLeaseAuditRow is one audit row on the /ui/admin/health/leases
// page. Mirrors the tasks_lease_audit table from migration v27.
type AdminLeaseAuditRow struct {
	ID         int64
	TaskID     string
	ChangedAt  time.Time
	OldStatus  string
	NewStatus  string
	OldLeaseID string
	NewLeaseID string
	SQLSnippet string
}

// StuckExecutionSource backs the watchdog tab — we surface the
// most recent executions whose error_code starts with "watchdog"
// (the watchdog package's failure path tags failures with
// "watchdog/stuck" today). The data path's small and read-only;
// concrete impls live with the executions repo.
type StuckExecutionSource interface {
	RecentWatchdogFailures(ctx context.Context, limit int) ([]AdminStuckExecution, error)
}

// AdminStuckExecution is one row on the watchdog tab. Kept small
// so the page can stay terse — operators that want details click
// through to /ui/executions/{id}.
type AdminStuckExecution struct {
	ExecutionID string
	TaskID      string
	ProjectID   string
	WorkflowID  string
	StartedAt   time.Time
	UpdatedAt   time.Time
	ErrorCode   string
	ErrorMsg    string
}

// AdminAuditWriter narrows persistence.AdminAuditRepository to the
// single method the UI's mutating handlers need. Defined here so
// per-handler tests can stub the writer without dragging the full
// repository contract in.
type AdminAuditWriter interface {
	Insert(ctx context.Context, entry *persistence.AdminAuditEntry) error
}

// adminCommonData carries the fields every admin page shares —
// CurrentPage drives nav highlighting, IsAdmin keeps the nav link
// rendered, and Title is the browser tab title.
type adminCommonData struct {
	Title       string
	CurrentPage string
	IsAdmin     bool
}

// AdminLandingData backs /ui/admin/.
type AdminLandingData struct {
	adminCommonData
	Readiness       []AdminReadinessCheck
	RecentAudit     []*persistence.AdminAuditEntry
	AuditAvailable  bool
	HealthAvailable bool
	// AwaitingUsers is the count of logins awaiting approval, surfaced
	// as a callout so the operator is prompted to act. Zero (or no
	// identity core wired) hides the callout.
	AwaitingUsers int
	UsersWired    bool
}

// AdminAuditData backs /ui/admin/audit.
type AdminAuditData struct {
	adminCommonData
	Entries         []*persistence.AdminAuditEntry
	Limit           int
	LimitOptions    []int
	FilterAction    string
	FilterPrincipal string
	FilterTarget    string
	FilterSince     string
	Available       bool
}

// AdminHealthIndexData backs /ui/admin/health/.
type AdminHealthIndexData struct {
	adminCommonData
}

// AdminHealthLeasesData backs /ui/admin/health/leases.
type AdminHealthLeasesData struct {
	adminCommonData
	StatusCounts map[string]int64
	Rows         []AdminLeaseAuditRow
	Available    bool
	Error        string
}

// AdminHealthWatchdogData backs /ui/admin/health/watchdog.
type AdminHealthWatchdogData struct {
	adminCommonData
	Failures  []AdminStuckExecution
	Available bool
}

// AdminHealthMCPData backs /ui/admin/health/mcp.
type AdminHealthMCPData struct {
	adminCommonData
	Snapshot  AdminMCPSnapshot
	Available bool
	Refreshed bool
	Error     string
}

// VoiceProbeStatus is the per-probe shape for one of {STT binary,
// STT model, TTS binary, TTS model, ffmpeg}. Configured=false
// indicates the operator hasn't wired this provider at all (the
// surface still renders the row so the absence is visible). OK is
// the at-request stat result; Error carries the os.Stat error
// message when OK=false.
type VoiceProbeStatus struct {
	Label      string // human-readable row name ("Whisper binary", "Piper model", "ffmpeg")
	Configured bool
	Path       string // resolved path (config override or $PATH lookup)
	OK         bool
	Error      string
}

// VoiceRuntimeStatus aggregates the boot-time voice diagnostics
// from the service container. Mirrors the WARN-log surface in
// container_voice.go's probe* helpers but renders to a table
// instead of journald.
type VoiceRuntimeStatus struct {
	STTProvider string // e.g. "whisper-local" or "" when disabled
	TTSProvider string // e.g. "piper" or ""
	Probes      []VoiceProbeStatus
}

// StorageRuntimeStatus reports the active artifact FileBackend and
// its connectivity / writability state. Backend is the normalised
// name ("filesystem" or "s3") from Config.Storage; the per-kind
// fields are populated only for the active backend.
type StorageRuntimeStatus struct {
	Backend string // "filesystem" | "s3"

	// Filesystem-only fields. Path is the configured artifacts_path;
	// Writable is true when the daemon can stat AND write to it
	// (probed via a touch+remove at request time).
	FilesystemPath     string
	FilesystemWritable bool
	FilesystemError    string

	// S3-only fields. Endpoint may be empty for default AWS resolver.
	// Reachable=true when a HeadBucket call succeeds within the probe
	// timeout; Error carries the SDK error message otherwise.
	S3Endpoint     string
	S3Region       string
	S3Bucket       string
	S3Prefix       string
	S3UsePathStyle bool
	S3Reachable    bool
	S3Error        string
}

// AdminHealthRuntimeData backs /ui/admin/health/runtime.
type AdminHealthRuntimeData struct {
	adminCommonData
	Voice     VoiceRuntimeStatus
	Storage   StorageRuntimeStatus
	Available bool // true when a RuntimeReadinessSource is wired
}

// RuntimeReadinessSource powers /ui/admin/health/runtime. The
// service container provides a concrete impl that reads Config.Voice
// + Config.Storage at request time and probes the binary / model /
// bucket reachability.
type RuntimeReadinessSource interface {
	VoiceStatus(ctx context.Context) VoiceRuntimeStatus
	StorageStatus(ctx context.Context) StorageRuntimeStatus
}

// AdminIntegrationsMCPData backs /ui/admin/integrations/mcp.
type AdminIntegrationsMCPData struct {
	adminCommonData
	Configured []AdminMCPProjectRow
	Available  bool
}

// EmailChannelInventory exposes the live per-project email channel
// state to /ui/admin/integrations/email. One row per project that
// has a fully configured `email` block; the channel's in-memory
// session map is queried per-render so the page reflects current
// inbound activity since daemon start.
//
// Slice 1 read-only surface — no edits, no force-reconnect button.
// The session list is the only operationally interesting state
// (IMAP/SMTP wiring is settled at boot; live state is "which
// threads has the bot seen this run").
type EmailChannelInventory interface {
	EmailChannels(ctx context.Context) []AdminEmailChannelRow
}

// AdminEmailChannelRow describes one project's email channel: its
// wired endpoints + the sessions seen since daemon start. Sessions
// is empty when no inbound has arrived for that project yet.
type AdminEmailChannelRow struct {
	ProjectID          string
	IMAPHost           string
	IMAPPort           int
	IMAPMailbox        string
	OutboundConfigured bool
	SMTPHost           string
	FromAddress        string
	AllowlistSize      int
	AttachmentCapBytes int64
	VerifyInboundAuth  bool
	AuthPolicy         string
	// Sessions is the live ListSessions snapshot, newest first.
	// Empty list = no inbound on this channel yet this boot.
	Sessions []AdminEmailSessionRow
	// SessionsError captures the rare case where ListSessions
	// failed (today's implementation never errors but the
	// conversation.Channel contract permits it). Empty on the
	// happy path.
	SessionsError string
}

// AdminEmailSessionRow is one inbound thread the channel has seen.
// Mirrors the conversation.Session shape but with template-friendly
// formatted timestamps.
type AdminEmailSessionRow struct {
	ID               string
	Title            string
	LastActivity     time.Time
	ParticipantCount int
}

// AdminIntegrationsEmailData backs /ui/admin/integrations/email.
type AdminIntegrationsEmailData struct {
	adminCommonData
	Channels  []AdminEmailChannelRow
	Available bool
}

// DispatcherToolInventory powers /ui/admin/integrations/dispatcher-tools.
// One row per registered dispatcher tool (DispatcherTools()) with
// its backing service + current availability. The UI uses this to
// answer "why does the LLM say it can't email / search memory /
// store an artifact?" without trawling boot logs.
type DispatcherToolInventory interface {
	DispatcherTools() []AdminDispatcherToolRow
}

// AdminDispatcherToolRow describes one tool's operator-visible state.
// Mirrors dispatcher.ToolInfo (the bridge lives in the service
// container) so the UI package doesn't depend on internal/dispatcher.
type AdminDispatcherToolRow struct {
	Name           string
	Description    string
	BackingService string // short type label, "" for always-on
	Available      bool
}

// AdminIntegrationsDispatcherToolsData backs
// /ui/admin/integrations/dispatcher-tools.
type AdminIntegrationsDispatcherToolsData struct {
	adminCommonData
	Tools     []AdminDispatcherToolRow
	Available bool
}

// adminLimitOptions defines the page-size choices for the audit
// table. Mirrors the existing /ui/audit selector so operators have
// the same dropdown shape across surfaces.
var adminLimitOptions = []int{20, 50, 100, 200}

func adminClampLimit(raw string) int {
	if raw == "" {
		return 50
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 50
	}
	for _, opt := range adminLimitOptions {
		if n == opt {
			return n
		}
	}
	if n > 200 {
		return 200
	}
	return n
}

// buildAdminChatAuditURL composes the /ui/admin/chat-audit URL
// carrying the optional chat / project filter params. Built in Go
// rather than in the template because html/template's contextual
// escaper rejects URL fragments whose query-vs-path context
// depends on conditional output. url.Values.Encode handles RFC
// 3986 escaping automatically.
func buildAdminChatAuditURL(filterChat, filterProject string) string {
	const base = "/ui/admin/chat-audit"
	q := url.Values{}
	if filterChat != "" {
		q.Set("chat", filterChat)
	}
	if filterProject != "" {
		q.Set("project", filterProject)
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// AdminLanding renders /ui/admin/. Three tiles: readiness snapshot,
// recent audit (last 5), and a quick-links grid.
func (s *Server) AdminLanding(w http.ResponseWriter, r *http.Request) {
	data := AdminLandingData{
		adminCommonData: adminCommonData{
			Title:       "Admin",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		AuditAvailable:  s.adminAuditRepo != nil,
		HealthAvailable: s.adminReadiness != nil,
		UsersWired:      s.identityRepo != nil,
	}
	// Readiness probes (DB/MCP/storage pings) and the recent-audit query are
	// independent — run them concurrently so the most-hit admin page blocks on
	// max(readiness, audit) ≈ 3s rather than their sum.
	var wg sync.WaitGroup
	if s.adminReadiness != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			data.Readiness = s.adminReadiness.ReadinessChecks(ctx)
		}()
	}
	if s.adminAuditRepo != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			entries, err := s.adminAuditRepo.List(ctx, persistence.AdminAuditFilter{PageSize: 5})
			if err != nil {
				s.logger.Warn().Err(err).Msg("admin landing: failed to load recent audit")
				return
			}
			data.RecentAudit = entries
		}()
	}
	if s.identityRepo != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			views, err := s.identityRepo.ListUsers(ctx)
			if err != nil {
				s.logger.Warn().Err(err).Msg("admin landing: failed to count awaiting users")
				return
			}
			data.AwaitingUsers = countAwaiting(views)
		}()
	}
	wg.Wait()
	s.render(w, "admin_landing.html", data)
}

// AdminAudit renders /ui/admin/audit. Filter axes: action,
// principal, target prefix, since timestamp, limit.
func (s *Server) AdminAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := adminClampLimit(q.Get("limit"))
	data := AdminAuditData{
		adminCommonData: adminCommonData{
			Title:       "Admin Audit",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Limit:           limit,
		LimitOptions:    adminLimitOptions,
		FilterAction:    q.Get("action"),
		FilterPrincipal: q.Get("principal"),
		FilterTarget:    q.Get("target"),
		FilterSince:     q.Get("since"),
		Available:       s.adminAuditRepo != nil,
	}
	if s.adminAuditRepo == nil {
		s.render(w, "admin_audit.html", data)
		return
	}
	filter := persistence.AdminAuditFilter{
		Action:       data.FilterAction,
		Principal:    data.FilterPrincipal,
		TargetPrefix: data.FilterTarget,
		PageSize:     limit,
	}
	if data.FilterSince != "" {
		if t, err := time.Parse(time.RFC3339, data.FilterSince); err == nil {
			filter.Since = t
		} else if t, err := time.Parse("2006-01-02", data.FilterSince); err == nil {
			filter.Since = t
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	entries, err := s.adminAuditRepo.List(ctx, filter)
	if err != nil {
		s.logger.Warn().Err(err).Msg("admin audit list failed")
	} else {
		data.Entries = entries
	}
	s.render(w, "admin_audit.html", data)
}

// AdminChatAuditData backs /ui/admin/chat-audit.
type AdminChatAuditData struct {
	adminCommonData
	Available      bool
	Entries        []*persistence.ChatAuditEntry
	Limit          int
	LimitOptions   []int
	FilterChat     string
	FilterProject  string
	FilterSince    string
	SelectedDetail *persistence.ChatAuditEntry
	SelectedPrompt string
	// SelectedHallucinations is the parsed []hallucination.Signal
	// for the drill-down detail panel. Empty when the selected
	// row had no signals OR the JSON didn't decode. Populated
	// before render so the template stays declarative.
	SelectedHallucinations []HallucinationSignalRow
	// CloseURL is the pre-built "close drill-down" link target.
	// Built here instead of via nested {{if}} in the template
	// because html/template's contextual escaper rejects URLs
	// whose query-vs-path context depends on conditional output
	// (got: "{{.FilterProject}} appears in an ambiguous context
	// within a URL"). Building it in Go side-steps the issue
	// cleanly and url.Values handles escaping per RFC 3986.
	CloseURL string
}

// AdminChatAudit renders /ui/admin/chat-audit. Filter axes:
// chat_id, project_id, since timestamp. Optional ?id=<row-id>
// pins a row's detail panel + full system prompt below the list.
func (s *Server) AdminChatAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := adminClampLimit(q.Get("limit"))
	data := AdminChatAuditData{
		adminCommonData: adminCommonData{
			Title:       "Chat Audit",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Limit:         limit,
		LimitOptions:  adminLimitOptions,
		FilterChat:    q.Get("chat"),
		FilterProject: q.Get("project"),
		FilterSince:   q.Get("since"),
		Available:     s.adminChatAudit != nil,
	}
	data.CloseURL = buildAdminChatAuditURL(data.FilterChat, data.FilterProject)
	if s.adminChatAudit == nil {
		s.render(w, "admin_chat_audit.html", data)
		return
	}
	filter := persistence.ChatAuditFilter{
		ChatID:    data.FilterChat,
		ProjectID: data.FilterProject,
		PageSize:  limit,
	}
	if data.FilterSince != "" {
		if t, err := time.Parse(time.RFC3339, data.FilterSince); err == nil {
			filter.Since = t
		} else if t, err := time.Parse("2006-01-02", data.FilterSince); err == nil {
			filter.Since = t
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	entries, err := s.adminChatAudit.List(ctx, filter)
	if err != nil {
		s.logger.Warn().Err(err).Msg("admin chat-audit list failed")
	} else {
		data.Entries = entries
	}
	// Drill-down panel: when ?id=<row-id> is set, find the matching
	// row in the page and resolve its system prompt body.
	if rowID := q.Get("id"); rowID != "" {
		for _, e := range data.Entries {
			if e.ID == rowID {
				data.SelectedDetail = e
				if e.SystemPromptHash != "" {
					if body, perr := s.adminChatAudit.GetPrompt(ctx, e.SystemPromptHash); perr == nil {
						data.SelectedPrompt = body
					}
				}
				data.SelectedHallucinations = parseHallucinationSignals(e.HallucinationSignalsJSON)
				break
			}
		}
	}
	s.render(w, "admin_chat_audit.html", data)
}

// parseHallucinationSignals decodes the audit row's JSON into the
// ui-local row shape. Returns nil on empty input AND on decode
// errors — the drill-down panel just hides the section in either
// case (the operator can still see the raw JSON via the audit row
// itself). Pre-renders Severity → SeverityClass mapping so the
// template stays declarative.
func parseHallucinationSignals(jsonBlob string) []HallucinationSignalRow {
	if jsonBlob == "" {
		return nil
	}
	// Local decode shape — mirrors hallucination.Signal's exported
	// JSON tags without dragging the package import into ui.
	type signalDecode struct {
		Detector         string    `json:"detector"`
		Severity         string    `json:"severity"`
		ClaimType        string    `json:"claim_type"`
		ClaimValue       string    `json:"claim_value"`
		Sentence         string    `json:"sentence,omitempty"`
		EvidenceSearched string    `json:"evidence_searched,omitempty"`
		Detail           string    `json:"detail"`
		RecordedAt       time.Time `json:"recorded_at"`
	}
	var decoded []signalDecode
	if err := json.Unmarshal([]byte(jsonBlob), &decoded); err != nil {
		return nil
	}
	out := make([]HallucinationSignalRow, 0, len(decoded))
	for _, s := range decoded {
		row := HallucinationSignalRow{
			Detector:         s.Detector,
			Severity:         s.Severity,
			SeverityClass:    hallucinationSeverityBadgeClass(s.Severity),
			ClaimType:        s.ClaimType,
			ClaimValue:       s.ClaimValue,
			Sentence:         s.Sentence,
			EvidenceSearched: s.EvidenceSearched,
			Detail:           s.Detail,
		}
		if !s.RecordedAt.IsZero() {
			row.RecordedAt = s.RecordedAt.UTC().Format("2006-01-02 15:04:05")
		}
		out = append(out, row)
	}
	return out
}

// hallucinationSeverityBadgeClass maps a hallucination severity
// to the chat-audit drill-down template's tint class. Distinct
// from the trading-panel's severityClass (which returns CSS class
// names like "outcome-bad") because the hallucination badge uses
// Tailwind colour tokens directly. Centralised so future severity
// values (or a renamed "warn" → "medium") only need one place to
// change.
func hallucinationSeverityBadgeClass(sev string) string {
	switch sev {
	case "high":
		return "rose"
	case "warn":
		return "amber"
	default:
		return "gray"
	}
}

// AdminHealthIndex renders /ui/admin/health/ — a small landing
// page that lists the three sub-pages (leases, watchdog, mcp).
func (s *Server) AdminHealthIndex(w http.ResponseWriter, r *http.Request) {
	data := AdminHealthIndexData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
	}
	s.render(w, "admin_health_index.html", data)
}

// AdminHealthLeases renders /ui/admin/health/leases. Reads the
// tasks_lease_audit view from migration v27.
func (s *Server) AdminHealthLeases(w http.ResponseWriter, r *http.Request) {
	data := AdminHealthLeasesData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health — Leases",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.adminLeaseAudit != nil,
	}
	if s.adminLeaseAudit == nil {
		s.render(w, "admin_health_leases.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if counts, err := s.adminLeaseAudit.CountByStatus(ctx); err == nil {
		data.StatusCounts = counts
	} else {
		data.Error = err.Error()
		s.logger.Warn().Err(err).Msg("admin lease audit: count failed")
	}
	if rows, err := s.adminLeaseAudit.Recent(ctx, 50); err == nil {
		data.Rows = rows
	} else if data.Error == "" {
		data.Error = err.Error()
		s.logger.Warn().Err(err).Msg("admin lease audit: recent failed")
	}
	s.render(w, "admin_health_leases.html", data)
}

// AdminHealthWatchdog renders /ui/admin/health/watchdog. Surfaces
// the most recent watchdog-tagged execution failures.
func (s *Server) AdminHealthWatchdog(w http.ResponseWriter, r *http.Request) {
	data := AdminHealthWatchdogData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health — Watchdog",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.adminStuckExecs != nil,
	}
	if s.adminStuckExecs == nil {
		s.render(w, "admin_health_watchdog.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if rows, err := s.adminStuckExecs.RecentWatchdogFailures(ctx, 20); err == nil {
		data.Failures = rows
	} else {
		s.logger.Warn().Err(err).Msg("admin watchdog: failed to load failures")
	}
	s.render(w, "admin_health_watchdog.html", data)
}

// AdminHealthRuntime renders /ui/admin/health/runtime — voice
// (STT/TTS) provider health + storage (FileBackend) reachability.
// Each subsystem can be wired or not; the page renders even when
// no source is attached so the operator can confirm the page route
// itself works.
func (s *Server) AdminHealthRuntime(w http.ResponseWriter, r *http.Request) {
	data := AdminHealthRuntimeData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health — Runtime",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.runtimeReadiness != nil,
	}
	if s.runtimeReadiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		data.Voice = s.runtimeReadiness.VoiceStatus(ctx)
		data.Storage = s.runtimeReadiness.StorageStatus(ctx)
	}
	s.render(w, "admin_health_runtime.html", data)
}

// AdminHealthMCP renders /ui/admin/health/mcp on GET; on POST it
// triggers a daemon-wide MCP refresh and writes an audit row.
// The refresh is the lone slice-1 mutation — admin-ui-design.md
// §5.10 explicitly allows it because the action is idempotent
// and observable.
func (s *Server) AdminHealthMCP(w http.ResponseWriter, r *http.Request) {
	data := AdminHealthMCPData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health — MCP",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.adminMCPSource != nil,
	}

	if r.Method == http.MethodPost {
		if s.adminMCPRefresher == nil {
			http.Error(w, "MCP refresh not wired", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		err := s.adminMCPRefresher.RefreshAll(ctx)
		if err != nil {
			data.Error = err.Error()
			s.logger.Warn().Err(err).Msg("admin MCP refresh failed")
		} else {
			data.Refreshed = true
		}
		// Audit row regardless of outcome — operators want to see
		// failed refreshes too.
		if s.adminAuditRepo != nil {
			after := "ok"
			if err != nil {
				after = "error: " + err.Error()
			}
			_ = s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
				Principal: adminPrincipal(r),
				Source:    "ui",
				Action:    "mcp.refresh",
				After:     fmt.Sprintf("{%q:%q}", "result", after),
				IP:        clientIP(r),
				UserAgent: r.UserAgent(),
			})
		}
	}

	if s.adminMCPSource != nil {
		data.Snapshot = s.adminMCPSource.Snapshot()
	}
	s.render(w, "admin_health_mcp.html", data)
}

// AdminIntegrationsEmail renders /ui/admin/integrations/email. One
// row per project with a wired email channel, showing the IMAP/SMTP
// endpoints plus the live in-memory session table (threads inbound
// since daemon start). Read-only — operators wanting to nudge a
// thread go to the IMAP mailbox directly.
func (s *Server) AdminIntegrationsEmail(w http.ResponseWriter, r *http.Request) {
	data := AdminIntegrationsEmailData{
		adminCommonData: adminCommonData{
			Title:       "Admin Integrations — Email",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.emailChannelInventory != nil,
	}
	if s.emailChannelInventory != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		data.Channels = s.emailChannelInventory.EmailChannels(ctx)
	}
	s.render(w, "admin_integrations_email.html", data)
}

// AdminIntegrationsDispatcherTools renders the dispatcher-tool
// inventory at /ui/admin/integrations/dispatcher-tools. One row
// per registered tool with its backing service and current
// availability. Read-only — operators can't toggle wiring from the
// page (every gap is fixed by config + restart).
func (s *Server) AdminIntegrationsDispatcherTools(w http.ResponseWriter, r *http.Request) {
	data := AdminIntegrationsDispatcherToolsData{
		adminCommonData: adminCommonData{
			Title:       "Admin Integrations — Dispatcher tools",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.dispatcherToolInventory != nil,
	}
	if s.dispatcherToolInventory != nil {
		data.Tools = s.dispatcherToolInventory.DispatcherTools()
	}
	s.render(w, "admin_integrations_dispatcher_tools.html", data)
}

// AdminIntegrationsMCP renders /ui/admin/integrations/mcp — the
// read-only listing of the daemon-level mcp.servers block. Edit
// is slice 3.
func (s *Server) AdminIntegrationsMCP(w http.ResponseWriter, r *http.Request) {
	data := AdminIntegrationsMCPData{
		adminCommonData: adminCommonData{
			Title:       "Admin Integrations — MCP",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.adminMCPConfig != nil,
	}
	if s.adminMCPConfig != nil {
		data.Configured = s.adminMCPConfig.ConfiguredMCPServers()
	}
	s.render(w, "admin_integrations_mcp.html", data)
}

// adminRouter dispatches /admin/* requests. Mounted by Handler()
// when an admin wiring is present.
func (s *Server) adminRouter(w http.ResponseWriter, r *http.Request) {
	// D3 (audit 2026-06-10): defense-in-depth admin re-check. Every
	// handler dispatched below relies SOLELY on the admin.Middleware
	// wrap order being correct — which silently disengaged once
	// (incident b777ef4a). This single guard fails closed if a future
	// wrap-order regression lets a non-admin reach the router: when
	// auth is enabled and the request did NOT resolve to admin, 403.
	// The auth-off case is untouched — admin.Middleware stamps
	// IsAdmin=true for every caller when auth is disabled, and the
	// explicit IsAuthEnabledFromContext guard means even a regression
	// that drops that stamp still admits the single-tenant operator.
	if !admin.IsAdminFromContext(r.Context()) && api.IsAuthEnabledFromContext(r.Context()) {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	if path == "" {
		path = "/"
	}
	switch path {
	case "/", "":
		s.AdminLanding(w, r)
	case "/audit":
		s.AdminAudit(w, r)
	case "/chat-audit":
		s.AdminChatAudit(w, r)
	case "/memory-audit":
		s.AdminMemoryAudit(w, r)
	case "/health", "/health/":
		s.AdminHealthIndex(w, r)
	case "/health/leases":
		s.AdminHealthLeases(w, r)
	case "/health/watchdog":
		s.AdminHealthWatchdog(w, r)
	case "/health/mcp":
		s.AdminHealthMCP(w, r)
	case "/health/runtime":
		s.AdminHealthRuntime(w, r)
	case "/health/cluster":
		s.AdminHealthCluster(w, r)
	case "/integrations/mcp":
		s.AdminIntegrationsMCP(w, r)
	case "/integrations/email":
		s.AdminIntegrationsEmail(w, r)
	case "/integrations/dispatcher-tools":
		s.AdminIntegrationsDispatcherTools(w, r)
	case "/cpc", "/cpc/":
		s.AdminCPC(w, r)
	case "/instincts", "/instincts/":
		s.AdminInstincts(w, r)
	case "/blackbox", "/blackbox/":
		s.AdminBlackBox(w, r)
	case "/blackbox/overrides", "/blackbox/overrides/":
		s.AdminBlackBoxOverrides(w, r)
	case "/blackbox/overrides/save":
		s.AdminBlackBoxOverrideSave(w, r)
	case "/blackbox/overrides/delete":
		s.AdminBlackBoxOverrideDelete(w, r)
	case "/blackbox/candidates", "/blackbox/candidates/":
		s.AdminBlackBoxCandidates(w, r)
	case "/memory/firewall", "/memory/firewall/":
		s.AdminMemoryFirewall(w, r)
	case "/keys", "/keys/":
		s.AdminKeys(w, r)
	case "/users", "/users/":
		s.AdminUsers(w, r)
	case "/workflow-proposals", "/workflow-proposals/":
		s.AdminWorkflowProposals(w, r)
	default:
		// /users/{id}/<action> — POST surfaces from the Users page
		// (login approval). Admin gate already wraps this router.
		if strings.HasPrefix(path, "/users/") {
			if s.dispatchAdminUserRoute(w, r, strings.TrimPrefix(path, "/users/")) {
				return
			}
		}
		// /cpc/{id}/cancel — POST surface from the cpc admin page.
		// Pattern-match here so the handler stays out of the
		// shared mux registration (admin gate already wraps this
		// router).
		if strings.HasPrefix(path, "/cpc/") && strings.HasSuffix(path, "/cancel") {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/cpc/"), "/cancel")
			s.AdminCPCCancel(w, r, id)
			return
		}
		// /instincts/{id}/retire — POST surface from the instinct admin
		// page's per-row Retire button. Advisory only; same admin gate
		// already wraps this router.
		if strings.HasPrefix(path, "/instincts/") && strings.HasSuffix(path, "/retire") {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/instincts/"), "/retire")
			s.AdminInstinctRetire(w, r, id)
			return
		}
		// /blackbox/triggers/bulk-dismiss — POST surface from the
		// index page's batch-dismiss form. Single-segment after
		// triggers/, so it must match BEFORE the per-id dispatch
		// below (which expects /triggers/<id>).
		if path == "/blackbox/triggers/bulk-dismiss" {
			s.AdminBlackBoxTriggersBulkDismiss(w, r)
			return
		}
		// /blackbox/triggers/{id} (+ /dismiss, /generate-candidate
		// POST suffixes) — Phase B detail page + actions. Must
		// precede the bare /blackbox/{task_id} dispatch because
		// "triggers/<id>" has two slash segments.
		if strings.HasPrefix(path, "/blackbox/triggers/") {
			rest := strings.TrimPrefix(path, "/blackbox/triggers/")
			for action, handler := range map[string]func(http.ResponseWriter, *http.Request, string){
				"dismiss":            s.AdminBlackBoxTriggerDismiss,
				"generate-candidate": s.AdminBlackBoxTriggerGenerateCandidate,
			} {
				if strings.HasSuffix(rest, "/"+action) {
					id := strings.TrimSuffix(rest, "/"+action)
					if id != "" && !strings.Contains(id, "/") {
						handler(w, r, id)
						return
					}
				}
			}
			if rest != "" && !strings.Contains(rest, "/") {
				s.AdminBlackBoxTriggerDetail(w, r, rest)
				return
			}
		}
		// /blackbox/candidates/{id} (+ /run-trial, /promote, /reject
		// POST suffixes) — Self-Healing Workflow Genome v1 candidate
		// detail + actions. Must precede the bare /blackbox/{task_id}
		// dispatch because "candidates/<id>" has two slash segments.
		if strings.HasPrefix(path, "/blackbox/candidates/") {
			rest := strings.TrimPrefix(path, "/blackbox/candidates/")
			for action, handler := range map[string]func(http.ResponseWriter, *http.Request, string){
				"run-trial": s.AdminBlackBoxCandidateRunTrial,
				"promote":   s.AdminBlackBoxCandidatePromote,
				"reject":    s.AdminBlackBoxCandidateReject,
			} {
				if strings.HasSuffix(rest, "/"+action) {
					id := strings.TrimSuffix(rest, "/"+action)
					if id != "" && !strings.Contains(id, "/") {
						handler(w, r, id)
						return
					}
				}
			}
			if rest != "" && !strings.Contains(rest, "/") {
				s.AdminBlackBoxCandidateDetail(w, r, rest)
				return
			}
		}
		// /blackbox/compare/{a}/{b} — Phase C side-by-side trace
		// comparison. Pattern-match must precede the bare
		// /blackbox/{task_id} dispatch below since `compare/{a}/{b}`
		// has two slash segments.
		if strings.HasPrefix(path, "/blackbox/compare/") {
			rest := strings.TrimPrefix(path, "/blackbox/compare/")
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				s.AdminBlackBoxCompare(w, r, parts[0], parts[1])
				return
			}
		}
		// /blackbox/{task_id} — Autonomy Black Box trace view. The
		// task_id may contain underscores / hex; reject only paths
		// with a trailing segment that would mean a multi-level
		// route we don't define.
		if strings.HasPrefix(path, "/blackbox/") {
			taskID := strings.TrimPrefix(path, "/blackbox/")
			if taskID != "" && !strings.Contains(taskID, "/") {
				s.AdminBlackBoxTrace(w, r, taskID)
				return
			}
		}
		// /memory/firewall/chunks/{id} — per-chunk policy detail
		// view (2026.5.9 follow-on). Chunk IDs are hex hashes
		// without "/" so single-segment is the right filter.
		if strings.HasPrefix(path, "/memory/firewall/chunks/") {
			chunkID := strings.TrimPrefix(path, "/memory/firewall/chunks/")
			if chunkID != "" && !strings.Contains(chunkID, "/") {
				s.AdminMemoryFirewallChunk(w, r, chunkID)
				return
			}
		}
		// Workflow proposals — drill-down + POST {decide,apply,
		// rollback}. Pattern-match here so net/http's prefix mux
		// doesn't fight us.
		if strings.HasPrefix(path, "/workflow-proposals/") {
			rest := strings.TrimPrefix(path, "/workflow-proposals/")
			for action, handler := range map[string]func(http.ResponseWriter, *http.Request, string){
				"decide":   s.AdminWorkflowProposalDecide,
				"apply":    s.AdminWorkflowProposalApply,
				"rollback": s.AdminWorkflowProposalRollback,
				"modify":   s.AdminWorkflowProposalModify,
			} {
				if strings.HasSuffix(rest, "/"+action) {
					id := strings.TrimSuffix(rest, "/"+action)
					if id != "" && !strings.Contains(id, "/") {
						handler(w, r, id)
						return
					}
				}
			}
			if rest != "" && !strings.Contains(rest, "/") {
				s.AdminWorkflowProposalDetail(w, r, rest)
				return
			}
		}
		http.NotFound(w, r)
	}
}

// adminPrincipal pulls the matched API key off the context — the
// admin gate stashed it there. Falls back to "unknown" so the
// audit row always has a value.
func adminPrincipal(r *http.Request) string {
	if v := admin.PrincipalFromContext(r.Context()); v != "" {
		return v
	}
	return "unknown"
}

// clientIP returns the source IP for an admin audit row. It reads the
// centrally-resolved, trusted-proxy-aware client IP that realip.Middleware
// stored in the request context, falling back to RemoteAddr's host when
// unset. It NEVER reads X-Forwarded-For directly — behind the Cloudflare
// tunnel the leftmost hop is attacker-controlled.
// see LLD § https://docs.vornik.io
func clientIP(r *http.Request) string {
	if ip := realip.ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return realip.RemoteHost(r)
}

// Compile-time assertion that AdminAuditWriter is satisfied by the
// canonical persistence interface. Keeps the test stub honest.
var _ AdminAuditWriter = (persistence.AdminAuditRepository)(nil)
