package ui

// Form-driven project config editor — Phase 1B v1 of the web-
// authoring UX work. See https://docs.vornik.io
//
// Scope for v1 (deliberately narrow):
//   - Identity: displayName, description
//   - Autonomy: enabled, mode, goal, maxTasksPerHour, pollInterval,
//     requireApproval, allowedTaskTypes
//   - Permissions: secrets, allowedTools
//
// Every other field stays in the raw YAML editor at
// /ui/projects/{id}/config — that route is preserved verbatim
// and shown as "Advanced YAML" in the form view. This narrow
// scope keeps the patcher's exposed paths small while addressing
// the most-common non-technical-operator edits.
//
// Save flow: form values → []yamlPatch → applyYAMLPatches on the
// existing file content → validateProjectConfigEdit (sandbox
// registry load) → writeProjectConfigAtomic → configReloader.
// Comments, commented-out scaffolds, and unrelated keys survive
// because the patcher is yaml.Node-based, not unmarshal/marshal.

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/fieldguard"
	"vornik.io/vornik/internal/registry"
)

// commonTaskTypeSuggestions seeds the chip row above the Allowed
// Task Types textarea. Operators free-form-curate this list per
// project; the chips just save typing common values. Pulled from
// the in-tree configs (dev-project, ibkr-trader, news-feed).
var commonTaskTypeSuggestions = []string{
	"research",
	"feature",
	"bug-fix",
	"refactor",
	"test-coverage",
	"trading",
	"roadmap-revision",
	"chore",
}

// commonAllowedToolSuggestions is the project-level agent runtime
// tool catalog surfaced in the permissions.allowedTools checkbox
// grid. These are the tools the executor passes into worker
// containers, not the Telegram/dispatcher chat tools.
var commonAllowedToolSuggestions = []string{
	"current_time",
	"file_read",
	"read_many_files",
	"file_write",
	"file_edit",
	"run_shell",
	"grep",
	"glob",
	"git_status",
	"git_diff",
	"git_log",
	"git_show",
	"test_run",
	"lint_run",
	"typecheck_run",
	"memory_search",
}

// ProjectConfigFormData backs the form-driven editor template.
// Mirrors the field set the form exposes plus the Brief preview
// card and the YAML escape-hatch link.
type ProjectConfigFormData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	ConfigPath  string
	Error       string
	Success     string
	BackupPath  string

	// HasBrief reports whether a PROJECT.md companion exists.
	// Drives the "View / Create brief" badge on the Identity
	// section. The brief editor itself is a follow-up; v1 only
	// surfaces the existence + goal preview.
	HasBrief      bool
	BriefGoalPrev string

	// Identity
	DisplayName string
	Description string

	// Routing — which swarm + workflow this project uses, the
	// adaptive-router candidate menu, and the concurrency caps.
	// SwarmID and DefaultWorkflowID render as dropdowns populated
	// from the registry; SwarmOptions / WorkflowOptions carry the
	// ID lists into the template.
	SwarmID                    string
	DefaultWorkflowID          string
	AdaptiveCandidateWorkflows string
	DefaultPriority            int
	MaxConcurrentTasks         int
	SwarmOptions               []string
	WorkflowOptions            []string

	// Autonomy
	AutonomyEnabled         bool
	AutonomyMode            string
	AutonomyGoal            string
	AutonomyMaxTasksPerHour int
	AutonomyPollInterval    string
	AutonomyRequireApproval bool
	// AllowedTaskTypes / allowedTools / secrets are rendered as
	// textareas with one item per line. Easier to paste from a
	// list than comma-separated, and the diff stays line-oriented.
	AutonomyAllowedTaskTypes string

	// Permissions
	PermissionsSecrets string
	// PermissionsAllowedTools is the FULL final list of tool names
	// the project permits — saved into the YAML as
	// permissions.allowedTools. The form composes it from two
	// inputs: BuiltinTools (canonical checkbox grid) plus
	// CustomAllowedTools (free-form textarea for MCP /
	// project-specific names).
	PermissionsAllowedTools string

	// BuiltinTools is the curated agent runtime tool catalog the form
	// renders as a checkbox grid above the Allowed Tools textarea.
	// The saved value is still permissions.allowedTools, which the
	// executor passes to worker containers.
	BuiltinTools []BuiltinToolOption

	// CustomAllowedTools is the textarea-bound subset of
	// allowedTools — everything that's NOT a canonical built-in
	// (typically MCP tool names). Kept separate so a
	// re-render-on-error preserves the operator's custom entries
	// without smearing them across the checkbox grid.
	CustomAllowedTools string

	// TaskTypeSuggestions seeds the chip row above the Allowed
	// Task Types textarea. Open-ended (operators curate the list),
	// so the chips are recommendations, not a closed enum.
	TaskTypeSuggestions []string

	// Budget — USD caps and the timezone the daily/monthly
	// windows reset on. Floats so 5.00 / 19.95 round-trip
	// without precision surprises. Zero values mean "no cap"
	// per the existing project loader semantics.
	BudgetDailySoftUSD   float64
	BudgetDailyHardUSD   float64
	BudgetMonthlySoftUSD float64
	BudgetMonthlyHardUSD float64
	BudgetTimezone       string

	// RateLimit — task-creation frequency caps.
	RateLimitTasksPerMinute int
	RateLimitTasksPerHour   int

	// Retention — per-project pruning thresholds in days.
	// Empty / zero inherits the daemon default.
	RetentionTaskLLMUsageDays int
	RetentionToolAuditDays    int
	RetentionTasksDays        int
	RetentionExecutionsDays   int
	RetentionArtifactsDays    int

	// Chat — system prompt prefix prepended on every chat
	// session in this project.
	ChatSystemPrefix string

	// HallucinationJudge — Phase 3 LLM-as-judge config.
	JudgeEnabled bool
	JudgeModel   string
	JudgePrompt  string

	// Trading — broker policy scalars editable from the form.
	// Caps + the per-symbol watchlist details stay on disk for
	// now; this v1 covers the highest-touch fields (mode,
	// killSwitch, watchlist tickers, notify chat id).
	TradingMode              string
	TradingKillSwitch        bool
	TradingWatchlist         string
	TradingNotifyFillsChatID int64

	// GitHubApp — full scalar surface for the per-project
	// GitHub App channel. Arrays are rendered as newline-
	// separated textareas (same chip-list convention as
	// permissions / autonomy allowedTaskTypes).
	GitHubAppAppID            int64
	GitHubAppPrivateKeyPath   string
	GitHubAppInstallationID   int64
	GitHubAppAPIBaseURL       string
	GitHubAppWebhookSecretEnv string
	GitHubAppRepoAllowlist    string
	GitHubAppTaskLabels       string
	GitHubAppPRReviewLabels   string
	GitHubAppSenderAllowlist  string

	// AutonomyModes is the dropdown's option list. Kept here
	// rather than inline in the template so the template stays
	// rendering-only and the canonical list of modes lives in
	// Go code.
	AutonomyModes []string
	// ModelOptions feeds model dropdowns only when a live model
	// catalog is wired. Pricing is not a reachability catalog, so
	// the form deliberately leaves this empty and falls back to a
	// free-text input until the UI can consume /api/v1/models.
	ModelOptions []string
	// TimezoneOptions feeds the budget timezone dropdown.
	// Hardcoded daemon-side so operators picking a budget
	// reset zone don't have to remember IANA spelling.
	TimezoneOptions []string

	// MCPRegistryRows feeds the "MCP servers" section's checkbox
	// list. One row per daemon-known server; each row carries the
	// project's current subscribe state + per-tool selections.
	MCPRegistryRows []MCPRegistryRow
	// MCPCustomRows are project-only MCP server entries that
	// aren't in the daemon registry. Editable rows below the
	// registry-driven rows. Empty when the project only uses
	// registry-known servers.
	MCPCustomRows []MCPCustomRow
	// MCPRegistryUnavailable is true when the daemon-level MCP
	// registry source returned an error or wasn't configured. The
	// template surfaces a banner in that case so operators know
	// why the registry row list is empty.
	MCPRegistryUnavailable bool
	// MCPRegistryError carries the human-readable reason behind
	// MCPRegistryUnavailable. Surfaced inside the banner so
	// operators can act on it (e.g. wire WithMCPRegistrySource).
	MCPRegistryError string
	// MCPNextCustomIndex is the index the template should use for
	// a new "Add custom server" row. Always equals the current
	// MCPCustomRows length; templates can't compute len() on
	// their own.
	MCPNextCustomIndex int
}

// BuiltinToolOption pairs a canonical tool name with whether the
// project currently allows it. Drives the checkbox grid above the
// Allowed Tools textarea.
type BuiltinToolOption struct {
	Name    string
	Allowed bool
}

// commonTimezones is the curated IANA list rendered in the
// budget timezone dropdown. ~25 zones covering every UTC offset
// operators are likely to want; staying static avoids the
// ~600-entry tzdata dump that would dwarf the form itself.
var commonTimezones = []string{
	"UTC",
	"America/Los_Angeles",
	"America/Denver",
	"America/Chicago",
	"America/New_York",
	"America/Sao_Paulo",
	"Europe/London",
	"Europe/Paris",
	"Europe/Berlin",
	"Europe/Prague",
	"Europe/Helsinki",
	"Europe/Moscow",
	"Africa/Cairo",
	"Africa/Johannesburg",
	"Asia/Dubai",
	"Asia/Karachi",
	"Asia/Kolkata",
	"Asia/Bangkok",
	"Asia/Singapore",
	"Asia/Hong_Kong",
	"Asia/Shanghai",
	"Asia/Tokyo",
	"Asia/Seoul",
	"Australia/Sydney",
	"Pacific/Auckland",
}

// ProjectConfigFormEdit renders the form-driven project config editor.
func (s *Server) ProjectConfigFormEdit(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.projectConfigFormData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "project_config_form.html", data)
}

// ProjectConfigFormSave applies form values to the underlying
// YAML via the surgical patcher, validates the result, writes
// atomically, and reloads the registry. On any failure the
// rendered form re-shows the operator's input plus an inline
// error.
func (s *Server) ProjectConfigFormSave(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.projectConfigFormData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "project_config_form.html", data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config_form.html", data)
		return
	}
	if err := validateProjectConfigFormNumbers(r); err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config_form.html", data)
		return
	}

	overlayFormValuesOntoData(&data, r)
	// MCP overlay walks the registry-driven rows (already
	// populated by projectConfigFormData) so the post-validation
	// fallback render reflects the operator's most recent picks.
	overlayMCPSection(&data, r)
	data.MCPNextCustomIndex = len(data.MCPCustomRows)

	existing, err := os.ReadFile(data.ConfigPath)
	if err != nil {
		data.Error = "Failed to read existing config: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "project_config_form.html", data)
		return
	}

	patches := buildFormPatches(&data)
	patches = append(patches, buildMCPPatches(&data)...)

	// Field-allowlist guard: the form may only touch the top-level
	// project keys it owns. A patch targeting anything else (most
	// importantly projectId — the project's identity, which would
	// orphan/rename the config file — but also any audit/lifecycle
	// key) is refused before we write, so a typo or an
	// accidentally-added patch can't silently corrupt a protected
	// field. The guard keys on the top-level segment of each patch
	// path.
	if err := projectConfigFormGuard.Check(topLevelPatchKeys(patches)); err != nil {
		data.Error = "Refused: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config_form.html", data)
		return
	}

	patched, err := applyYAMLPatches(existing, patches)
	if err != nil {
		data.Error = "Failed to apply form edits: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config_form.html", data)
		return
	}

	if err := validateProjectConfigEdit(s.configDir(), projectID, patched); err != nil {
		data.Error = "Validation failed: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config_form.html", data)
		return
	}

	backupPath, err := writeProjectConfigAtomic(data.ConfigPath, patched)
	if err != nil {
		data.Error = "Failed to write config: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "project_config_form.html", data)
		return
	}
	data.BackupPath = backupPath

	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			data.Error = "Saved, but reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "project_config_form.html", data)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			data.Error = "Saved, but registry reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "project_config_form.html", data)
			return
		}
	}

	data.Success = "Project config saved and reloaded."
	if backupPath != "" {
		data.Success += " Backup: " + backupPath
	}
	// Refresh form state from the reloaded project so any
	// normalisation (e.g. trimmed whitespace) the validator
	// applied is visible to the operator.
	if s.projectReg != nil {
		if proj := s.projectReg.GetProject(projectID); proj != nil {
			populateFormFromProject(&data, proj)
			data.TimezoneOptions = appendOptionIfMissing(data.TimezoneOptions, data.BudgetTimezone)
			// Reset + repopulate MCP section so post-save toggles
			// reflect the on-disk state (e.g. operator unsubscribed
			// → row goes back to Subscribed=false on the rerender).
			data.MCPRegistryRows = nil
			data.MCPCustomRows = nil
			populateMCPSection(&data, s.mcpRegistrySource, proj)
			data.MCPNextCustomIndex = len(data.MCPCustomRows)
		}
	}
	s.render(w, "project_config_form.html", data)
}

// projectConfigFormData builds the initial render-state for the
// form editor. Mirrors the YAML editor's path-handling so the
// two views agree on validity. Returns a data struct with .Error
// set when the project / config dir is invalid; callers should
// short-circuit rendering in that case.
func (s *Server) projectConfigFormData(projectID string) ProjectConfigFormData {
	data := ProjectConfigFormData{
		Title:               "Project Config (form): " + projectID,
		CurrentPage:         "projects",
		ProjectID:           projectID,
		AutonomyModes:       []string{"llm", "cron", "backlog"},
		TimezoneOptions:     commonTimezones,
		TaskTypeSuggestions: commonTaskTypeSuggestions,
	}
	if projectID == "" || strings.Contains(projectID, "/") || strings.Contains(projectID, string(os.PathSeparator)) {
		data.Error = "Invalid project id"
		return data
	}
	configDir := s.configDir()
	if configDir == "" {
		data.Error = "Registry config directory is not configured"
		return data
	}
	data.ConfigPath = configDir + "/projects/" + projectID + ".yaml"
	if _, err := os.Stat(data.ConfigPath); err != nil {
		data.Error = "Project config not found: " + err.Error()
		return data
	}
	if s.projectReg != nil {
		if proj := s.projectReg.GetProject(projectID); proj != nil {
			populateFormFromProject(&data, proj)
			data.TimezoneOptions = appendOptionIfMissing(data.TimezoneOptions, data.BudgetTimezone)
			populateMCPSection(&data, s.mcpRegistrySource, proj)
		} else {
			// Project not in registry — still render an empty MCP
			// section with the registry banner state. (Hits when
			// the YAML is on disk but registry load skipped it.)
			populateMCPSection(&data, s.mcpRegistrySource, nil)
		}
		// Dropdown options regardless of whether the project
		// loaded — empty registry = empty list.
		for _, sw := range s.projectReg.ListSwarms() {
			data.SwarmOptions = append(data.SwarmOptions, sw.ID)
		}
		for _, wf := range s.projectReg.ListWorkflows() {
			data.WorkflowOptions = append(data.WorkflowOptions, wf.ID)
		}
	}
	data.MCPNextCustomIndex = len(data.MCPCustomRows)
	return data
}

// populateFormFromProject copies field values out of a parsed
// *registry.Project into the form data struct. Used on both
// initial GET render and after a successful save so the form
// reflects the canonical post-reload state.
func populateFormFromProject(data *ProjectConfigFormData, proj *registry.Project) {
	data.DisplayName = proj.DisplayName
	data.Description = proj.Description
	data.SwarmID = proj.SwarmID
	data.DefaultWorkflowID = proj.DefaultWorkflowID
	data.AdaptiveCandidateWorkflows = strings.Join(proj.AdaptiveCandidateWorkflows, "\n")
	data.DefaultPriority = proj.DefaultPriority
	data.MaxConcurrentTasks = proj.MaxConcurrentTasks
	data.AutonomyEnabled = proj.Autonomy.Enabled
	data.AutonomyMode = proj.Autonomy.Mode
	data.AutonomyGoal = proj.Autonomy.Goal
	data.AutonomyMaxTasksPerHour = proj.Autonomy.MaxTasksPerHour
	data.AutonomyPollInterval = proj.Autonomy.PollInterval
	data.AutonomyRequireApproval = proj.Autonomy.RequireApproval
	data.AutonomyAllowedTaskTypes = strings.Join(proj.Autonomy.AllowedTaskTypes, "\n")
	data.PermissionsSecrets = strings.Join(proj.Permissions.Secrets, "\n")
	// Save-side value is the full, joined list — that's what the
	// patcher writes back into permissions.allowedTools.
	data.PermissionsAllowedTools = strings.Join(proj.Permissions.AllowedTools, "\n")
	// Split for the UI: builtin checkbox states + customs textarea.
	// Operators tick common tools without remembering spelling
	// while keeping a free-form override for MCP / project names.
	builtinSet := map[string]bool{}
	for _, n := range commonAllowedToolSuggestions {
		builtinSet[n] = false
	}
	var customs []string
	for _, name := range proj.Permissions.AllowedTools {
		if _, isBuiltin := builtinSet[name]; isBuiltin {
			builtinSet[name] = true
			continue
		}
		customs = append(customs, name)
	}
	data.BuiltinTools = make([]BuiltinToolOption, 0, len(builtinSet))
	for _, n := range commonAllowedToolSuggestions {
		data.BuiltinTools = append(data.BuiltinTools, BuiltinToolOption{Name: n, Allowed: builtinSet[n]})
	}
	data.CustomAllowedTools = strings.Join(customs, "\n")
	data.BudgetDailySoftUSD = proj.Budget.DailySoftUSD
	data.BudgetDailyHardUSD = proj.Budget.DailyHardUSD
	data.BudgetMonthlySoftUSD = proj.Budget.MonthlySoftUSD
	data.BudgetMonthlyHardUSD = proj.Budget.MonthlyHardUSD
	data.BudgetTimezone = proj.Budget.Timezone
	data.RateLimitTasksPerMinute = proj.RateLimit.TasksPerMinute
	data.RateLimitTasksPerHour = proj.RateLimit.TasksPerHour
	data.RetentionTaskLLMUsageDays = proj.Retention.TaskLLMUsageDays
	data.RetentionToolAuditDays = proj.Retention.ToolAuditDays
	data.RetentionTasksDays = proj.Retention.TasksDays
	data.RetentionExecutionsDays = proj.Retention.ExecutionsDays
	data.RetentionArtifactsDays = proj.Retention.ArtifactsDays
	data.ChatSystemPrefix = proj.Chat.SystemPrefix
	data.JudgeEnabled = proj.HallucinationJudge.Enabled
	data.JudgeModel = proj.HallucinationJudge.Model
	data.JudgePrompt = proj.HallucinationJudge.Prompt
	data.TradingMode = proj.Trading.Mode
	data.TradingKillSwitch = proj.Trading.KillSwitch
	data.TradingWatchlist = strings.Join(proj.Trading.Watchlist, "\n")
	data.TradingNotifyFillsChatID = proj.Trading.NotifyFillsChatID
	data.GitHubAppAppID = proj.GitHubApp.AppID
	data.GitHubAppPrivateKeyPath = proj.GitHubApp.PrivateKeyPath
	data.GitHubAppInstallationID = proj.GitHubApp.InstallationID
	data.GitHubAppAPIBaseURL = proj.GitHubApp.APIBaseURL
	data.GitHubAppWebhookSecretEnv = proj.GitHubApp.WebhookSecretEnv
	data.GitHubAppRepoAllowlist = strings.Join(proj.GitHubApp.RepoAllowlist, "\n")
	data.GitHubAppTaskLabels = strings.Join(proj.GitHubApp.TaskLabels, "\n")
	data.GitHubAppPRReviewLabels = strings.Join(proj.GitHubApp.PRReviewLabels, "\n")
	data.GitHubAppSenderAllowlist = strings.Join(proj.GitHubApp.SenderAllowlist, "\n")
	if proj.Brief != nil {
		data.HasBrief = true
		data.BriefGoalPrev = firstNonEmptyLine(proj.Brief.Goal)
	}
}

func appendOptionIfMissing(options []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return options
	}
	for _, option := range options {
		if option == value {
			return options
		}
	}
	return append(options, value)
}

// firstNonEmptyLine returns the first non-blank line of s,
// truncated to a single short preview. Used for the Brief card's
// goal teaser on the Identity section.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			if len(t) > 120 {
				return t[:120] + "…"
			}
			return t
		}
	}
	return ""
}

// overlayFormValuesOntoData copies posted form values onto the
// data struct. Separate from buildFormPatches so the rendering
// fallback (validation failure) shows the operator's most recent
// input, even when we never made it to the patcher.
func overlayFormValuesOntoData(data *ProjectConfigFormData, r *http.Request) {
	data.DisplayName = strings.TrimSpace(r.FormValue("displayName"))
	data.Description = r.FormValue("description")
	data.SwarmID = strings.TrimSpace(r.FormValue("swarmId"))
	data.DefaultWorkflowID = strings.TrimSpace(r.FormValue("defaultWorkflowId"))
	data.AdaptiveCandidateWorkflows = r.FormValue("adaptiveCandidateWorkflows")
	data.DefaultPriority = parseFormInt(r.FormValue("defaultPriority"))
	data.MaxConcurrentTasks = parseFormInt(r.FormValue("maxConcurrentTasks"))
	data.AutonomyEnabled = parseFormBool(r.FormValue("autonomy_enabled"))
	data.AutonomyMode = strings.TrimSpace(r.FormValue("autonomy_mode"))
	data.AutonomyGoal = r.FormValue("autonomy_goal")
	data.AutonomyMaxTasksPerHour = parseFormInt(r.FormValue("autonomy_maxTasksPerHour"))
	data.AutonomyPollInterval = strings.TrimSpace(r.FormValue("autonomy_pollInterval"))
	data.AutonomyRequireApproval = parseFormBool(r.FormValue("autonomy_requireApproval"))
	data.AutonomyAllowedTaskTypes = r.FormValue("autonomy_allowedTaskTypes")
	data.PermissionsSecrets = r.FormValue("permissions_secrets")
	// Re-assemble allowedTools from the checkbox grid (common agent
	// runtime tools) plus the custom textarea (MCP /
	// project-specific names). Suggested tools first, then
	// customs in operator order — stable diffs and grouped
	// entries in the saved YAML.
	data.CustomAllowedTools = r.FormValue("permissions_allowedTools_custom")
	checkedBuiltins := r.Form["permissions_allowedTools_builtin"]
	checkedSet := map[string]bool{}
	for _, c := range checkedBuiltins {
		checkedSet[c] = true
	}
	var combined []string
	for _, b := range commonAllowedToolSuggestions {
		if checkedSet[b] {
			combined = append(combined, b)
		}
	}
	if custom := strings.TrimSpace(data.CustomAllowedTools); custom != "" {
		combined = append(combined, custom)
	} else if legacy := strings.TrimSpace(r.FormValue("permissions_allowedTools")); legacy != "" {
		// Compatibility for pre-checkbox callers and older tests
		// that still post the former textarea field.
		combined = append(combined, legacy)
		data.CustomAllowedTools = legacy
	}
	data.PermissionsAllowedTools = strings.Join(combined, "\n")
	// Reflect the checkbox state back into BuiltinTools so a
	// re-render-on-error preserves what the operator just ticked.
	data.BuiltinTools = make([]BuiltinToolOption, 0, len(commonAllowedToolSuggestions))
	for _, b := range commonAllowedToolSuggestions {
		data.BuiltinTools = append(data.BuiltinTools, BuiltinToolOption{Name: b, Allowed: checkedSet[b]})
	}
	// Restore task-type suggestions on the re-render.
	data.TaskTypeSuggestions = commonTaskTypeSuggestions
	data.BudgetDailySoftUSD = parseFormFloat(r.FormValue("budget_dailySoftUsd"))
	data.BudgetDailyHardUSD = parseFormFloat(r.FormValue("budget_dailyHardUsd"))
	data.BudgetMonthlySoftUSD = parseFormFloat(r.FormValue("budget_monthlySoftUsd"))
	data.BudgetMonthlyHardUSD = parseFormFloat(r.FormValue("budget_monthlyHardUsd"))
	data.BudgetTimezone = strings.TrimSpace(r.FormValue("budget_timezone"))
	data.RateLimitTasksPerMinute = parseFormInt(r.FormValue("rateLimit_tasksPerMinute"))
	data.RateLimitTasksPerHour = parseFormInt(r.FormValue("rateLimit_tasksPerHour"))
	data.RetentionTaskLLMUsageDays = parseFormInt(r.FormValue("retention_taskLLMUsageDays"))
	data.RetentionToolAuditDays = parseFormInt(r.FormValue("retention_toolAuditDays"))
	data.RetentionTasksDays = parseFormInt(r.FormValue("retention_tasksDays"))
	data.RetentionExecutionsDays = parseFormInt(r.FormValue("retention_executionsDays"))
	data.RetentionArtifactsDays = parseFormInt(r.FormValue("retention_artifactsDays"))
	data.ChatSystemPrefix = r.FormValue("chat_systemPrefix")
	data.JudgeEnabled = parseFormBool(r.FormValue("judge_enabled"))
	data.JudgeModel = strings.TrimSpace(r.FormValue("judge_model"))
	data.JudgePrompt = r.FormValue("judge_prompt")
	data.TradingMode = strings.TrimSpace(r.FormValue("trading_mode"))
	data.TradingKillSwitch = parseFormBool(r.FormValue("trading_killSwitch"))
	data.TradingWatchlist = r.FormValue("trading_watchlist")
	data.TradingNotifyFillsChatID = parseFormInt64(r.FormValue("trading_notifyFillsChatID"))
	data.GitHubAppAppID = parseFormInt64(r.FormValue("githubApp_appID"))
	data.GitHubAppPrivateKeyPath = strings.TrimSpace(r.FormValue("githubApp_privateKeyPath"))
	data.GitHubAppInstallationID = parseFormInt64(r.FormValue("githubApp_installationID"))
	data.GitHubAppAPIBaseURL = strings.TrimSpace(r.FormValue("githubApp_apiBaseURL"))
	data.GitHubAppWebhookSecretEnv = strings.TrimSpace(r.FormValue("githubApp_webhookSecretEnv"))
	data.GitHubAppRepoAllowlist = r.FormValue("githubApp_repoAllowlist")
	data.GitHubAppTaskLabels = r.FormValue("githubApp_taskLabels")
	data.GitHubAppPRReviewLabels = r.FormValue("githubApp_prReviewLabels")
	data.GitHubAppSenderAllowlist = r.FormValue("githubApp_senderAllowlist")
}

// buildFormPatches assembles the []yamlPatch corresponding to
// the current form state. Order matters when the patcher creates
// intermediate mappings — autonomy / permissions parent maps are
// created on the first nested patch that needs them.
//
// Strings with RemoveIfEmpty deliberately delete their key when
// the operator clears the field so we don't litter the YAML with
// `displayName: ""` or `mode: ""`.
// projectConfigFormGuard is the allowlist of top-level project-YAML
// keys the config form is permitted to write. It is declared
// independently of buildFormPatches on purpose: if a future edit adds
// a patch under a new top-level key (or a typo'd one), the save fails
// loudly until this list is updated, rather than silently writing it.
// projectId is deliberately absent — the form must never mutate the
// project's identity.
var projectConfigFormGuard = fieldguard.Allowlist(
	"displayName",
	"description",
	"swarmId",
	"defaultWorkflowId",
	"adaptiveCandidateWorkflows",
	"defaultPriority",
	"maxConcurrentTasks",
	"autonomy",
	"permissions",
	"budget",
	"rate_limit",
	"retention",
	"chat",
	"hallucinationJudge",
	"trading",
	"github_app",
	"mcp",
)

func buildFormPatches(data *ProjectConfigFormData) []yamlPatch {
	return []yamlPatch{
		{Path: []string{"displayName"}, Value: data.DisplayName, RemoveIfEmpty: true},
		{Path: []string{"description"}, Value: data.Description, RemoveIfEmpty: true},
		{Path: []string{"swarmId"}, Value: data.SwarmID, RemoveIfEmpty: true},
		{Path: []string{"defaultWorkflowId"}, Value: data.DefaultWorkflowID, RemoveIfEmpty: true},
		{Path: []string{"adaptiveCandidateWorkflows"}, Value: splitChipList(data.AdaptiveCandidateWorkflows), RemoveIfEmpty: true},
		{Path: []string{"defaultPriority"}, Value: data.DefaultPriority, RemoveIfEmpty: true},
		{Path: []string{"maxConcurrentTasks"}, Value: data.MaxConcurrentTasks, RemoveIfEmpty: true},
		{Path: []string{"autonomy", "enabled"}, Value: data.AutonomyEnabled},
		{Path: []string{"autonomy", "mode"}, Value: data.AutonomyMode, RemoveIfEmpty: true},
		{Path: []string{"autonomy", "goal"}, Value: data.AutonomyGoal, RemoveIfEmpty: true},
		{Path: []string{"autonomy", "maxTasksPerHour"}, Value: data.AutonomyMaxTasksPerHour, RemoveIfEmpty: true},
		{Path: []string{"autonomy", "pollInterval"}, Value: data.AutonomyPollInterval, RemoveIfEmpty: true},
		{Path: []string{"autonomy", "requireApproval"}, Value: data.AutonomyRequireApproval, RemoveIfEmpty: true},
		{Path: []string{"autonomy", "allowedTaskTypes"}, Value: splitChipList(data.AutonomyAllowedTaskTypes), RemoveIfEmpty: true},
		{Path: []string{"permissions", "secrets"}, Value: splitChipList(data.PermissionsSecrets), RemoveIfEmpty: true},
		{Path: []string{"permissions", "allowedTools"}, Value: splitChipList(data.PermissionsAllowedTools), RemoveIfEmpty: true},
		{Path: []string{"budget", "daily_soft_usd"}, Value: data.BudgetDailySoftUSD, RemoveIfEmpty: true},
		{Path: []string{"budget", "daily_hard_usd"}, Value: data.BudgetDailyHardUSD, RemoveIfEmpty: true},
		{Path: []string{"budget", "monthly_soft_usd"}, Value: data.BudgetMonthlySoftUSD, RemoveIfEmpty: true},
		{Path: []string{"budget", "monthly_hard_usd"}, Value: data.BudgetMonthlyHardUSD, RemoveIfEmpty: true},
		{Path: []string{"budget", "timezone"}, Value: data.BudgetTimezone, RemoveIfEmpty: true},
		{Path: []string{"rate_limit", "tasks_per_minute"}, Value: data.RateLimitTasksPerMinute, RemoveIfEmpty: true},
		{Path: []string{"rate_limit", "tasks_per_hour"}, Value: data.RateLimitTasksPerHour, RemoveIfEmpty: true},
		{Path: []string{"retention", "task_llm_usage_days"}, Value: data.RetentionTaskLLMUsageDays, RemoveIfEmpty: true},
		{Path: []string{"retention", "tool_audit_days"}, Value: data.RetentionToolAuditDays, RemoveIfEmpty: true},
		{Path: []string{"retention", "tasks_days"}, Value: data.RetentionTasksDays, RemoveIfEmpty: true},
		{Path: []string{"retention", "executions_days"}, Value: data.RetentionExecutionsDays, RemoveIfEmpty: true},
		{Path: []string{"retention", "artifacts_days"}, Value: data.RetentionArtifactsDays, RemoveIfEmpty: true},
		{Path: []string{"chat", "system_prefix"}, Value: data.ChatSystemPrefix, RemoveIfEmpty: true},
		{Path: []string{"hallucinationJudge", "enabled"}, Value: data.JudgeEnabled},
		{Path: []string{"hallucinationJudge", "model"}, Value: data.JudgeModel, RemoveIfEmpty: true},
		{Path: []string{"hallucinationJudge", "prompt"}, Value: data.JudgePrompt, RemoveIfEmpty: true},
		// Trading scalars. Mode + killSwitch + notify_fills_chat_id
		// are the highest-touch fields; Caps and per-symbol
		// trading-config detail stay in Advanced YAML.
		{Path: []string{"trading", "mode"}, Value: data.TradingMode, RemoveIfEmpty: true},
		{Path: []string{"trading", "killSwitch"}, Value: data.TradingKillSwitch, RemoveIfEmpty: true},
		{Path: []string{"trading", "watchlist"}, Value: splitChipList(data.TradingWatchlist), RemoveIfEmpty: true},
		{Path: []string{"trading", "notify_fills_chat_id"}, Value: data.TradingNotifyFillsChatID, RemoveIfEmpty: true},
		// GitHub App. Note repo_allowlist + sender_allowlist must
		// each have valid contents or the channel rejects every
		// inbound delivery — the loader's Validate catches that
		// at registry-load time.
		{Path: []string{"github_app", "app_id"}, Value: data.GitHubAppAppID, RemoveIfEmpty: true},
		{Path: []string{"github_app", "private_key_path"}, Value: data.GitHubAppPrivateKeyPath, RemoveIfEmpty: true},
		{Path: []string{"github_app", "installation_id"}, Value: data.GitHubAppInstallationID, RemoveIfEmpty: true},
		{Path: []string{"github_app", "api_base_url"}, Value: data.GitHubAppAPIBaseURL, RemoveIfEmpty: true},
		{Path: []string{"github_app", "webhook_secret_env"}, Value: data.GitHubAppWebhookSecretEnv, RemoveIfEmpty: true},
		{Path: []string{"github_app", "repo_allowlist"}, Value: splitChipList(data.GitHubAppRepoAllowlist), RemoveIfEmpty: true},
		{Path: []string{"github_app", "task_labels"}, Value: splitChipList(data.GitHubAppTaskLabels), RemoveIfEmpty: true},
		{Path: []string{"github_app", "pr_review_labels"}, Value: splitChipList(data.GitHubAppPRReviewLabels), RemoveIfEmpty: true},
		{Path: []string{"github_app", "sender_allowlist"}, Value: splitChipList(data.GitHubAppSenderAllowlist), RemoveIfEmpty: true},
	}
}

// splitChipList turns a newline-separated (or comma-separated)
// textarea value into a deduplicated []string. Newlines are the
// primary separator so operators can paste copy-pasted lists,
// but commas work too because chip-list UIs everywhere expect
// commas to behave.
func splitChipList(raw string) []string {
	raw = strings.ReplaceAll(raw, ",", "\n")
	seen := map[string]struct{}{}
	out := []string{}
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// parseFormBool handles both the `<select>` "true"/"false" form
// and HTML checkboxes' "on"/missing convention. Anything truthy
// returns true; anything else returns false. Centralised so the
// handler doesn't sprinkle string comparisons across the file.
func parseFormBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "on", "1", "yes":
		return true
	default:
		return false
	}
}

// parseFormInt converts a form integer field to int, defaulting
// to 0 on blank / parse failure. Negative values pass through —
// the project validator catches them downstream where it has the
// full context to give a useful error.
func parseFormInt(raw string) int {
	t := strings.TrimSpace(raw)
	if t == "" {
		return 0
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0
	}
	return n
}

// parseFormInt64 converts a form numeric field to int64.
// Used for the GitHub App identifiers + the Telegram chat id
// in Trading.NotifyFillsChatID — both exceed the int32 range
// in practice (Telegram chat ids for super-groups are 64-bit
// negatives).
func parseFormInt64(raw string) int64 {
	t := strings.TrimSpace(raw)
	if t == "" {
		return 0
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseFormFloat converts a form numeric field to float64,
// defaulting to 0 on blank / parse failure. Used by the budget
// section's USD-amount inputs. Negative values pass through so
// the project validator can surface a meaningful error.
func parseFormFloat(raw string) float64 {
	t := strings.TrimSpace(raw)
	if t == "" {
		return 0
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0
	}
	return f
}

func validateProjectConfigFormNumbers(r *http.Request) error {
	for _, field := range []string{
		"defaultPriority",
		"maxConcurrentTasks",
		"autonomy_maxTasksPerHour",
		"rateLimit_tasksPerMinute",
		"rateLimit_tasksPerHour",
		"retention_taskLLMUsageDays",
		"retention_toolAuditDays",
		"retention_tasksDays",
		"retention_executionsDays",
		"retention_artifactsDays",
	} {
		if err := validateOptionalFormInt(r, field); err != nil {
			return err
		}
	}
	for _, field := range []string{
		"trading_notifyFillsChatID",
		"githubApp_appID",
		"githubApp_installationID",
	} {
		if err := validateOptionalFormInt64(r, field); err != nil {
			return err
		}
	}
	for _, field := range []string{
		"budget_dailySoftUsd",
		"budget_dailyHardUsd",
		"budget_monthlySoftUsd",
		"budget_monthlyHardUsd",
	} {
		if err := validateOptionalFormFloat(r, field); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkflowFormNumbers(r *http.Request) error {
	for _, field := range []string{"maxStepVisits", "maxIterations"} {
		if err := validateOptionalFormInt(r, field); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalFormInt(r *http.Request, field string) error {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return nil
	}
	if _, err := strconv.Atoi(raw); err != nil {
		return fmt.Errorf("%s must be an integer", field)
	}
	return nil
}

func validateOptionalFormInt64(r *http.Request, field string) error {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return nil
	}
	if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
		return fmt.Errorf("%s must be a 64-bit integer", field)
	}
	return nil
}

func validateOptionalFormFloat(r *http.Request, field string) error {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return nil
	}
	if _, err := strconv.ParseFloat(raw, 64); err != nil {
		return fmt.Errorf("%s must be a number", field)
	}
	return nil
}
