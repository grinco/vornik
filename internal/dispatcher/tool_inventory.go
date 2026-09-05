package dispatcher

// ToolInfo describes one dispatcher tool's operator-visible state:
// what it does, what backing service it needs, and whether that
// service is currently wired. The admin UI renders one row per
// info struct so operators can answer "why does the bot say it
// can't email/remember/store artifacts?" without spelunking the
// agent's option list.
//
// BackingService is a short label (capitalised type name) — not
// a programmatic identifier. Empty means "no external dependency,
// always available."
type ToolInfo struct {
	Name           string
	Description    string
	BackingService string
	Available      bool
}

// InventoryTools returns one row per registered dispatcher tool
// with its current availability. Reflects the agent's actual
// option-bag state — so a deployment that omits memory wiring
// shows memory_search as Available=false, even though the tool
// is registered in DispatcherTools(). Nil receiver returns nil
// (defensive for early-boot admin probes).
//
// The "always available" set (switch_project, tool_search) has
// BackingService="" so the UI renders them differently from
// the dependency-gated ones.
func (a *Agent) InventoryTools() []ToolInfo {
	if a == nil {
		return nil
	}
	// Build the availability map keyed by tool name. The map style
	// — instead of a switch in a loop — keeps the per-tool wiring
	// declaration co-located with the tool's metadata above so a
	// future tool addition lands in one place.
	// te is the ToolExecutor that holds the actual wiring; query it
	// for repo-presence checks instead of duplicating fields on
	// Agent. nil-safe: tests that build Agent directly without
	// going through NewAgent will see te==nil and every wired-repo
	// check returns false.
	gating := a.inventoryBacking()

	tools := RegisteredDispatcherTools()
	out := make([]ToolInfo, 0, len(tools))
	for _, t := range tools {
		info := ToolInfo{
			Name:        t.Function.Name,
			Description: t.Function.Description,
		}
		if g, ok := gating[t.Function.Name]; ok {
			info.BackingService = g.backingService
			info.Available = g.available
		} else {
			// Unreachable when TestInventoryTools_BackingIsDeclaredForExactlyTheRegisteredTools
			// passes; kept so a build that skipped tests still renders the gap
			// as unavailable rather than as a crash.
			info.Available = false
		}
		out = append(out, info)
	}
	return out
}

// toolBacking is one tool's dependency declaration: the service label the
// admin UI shows, and whether that service is wired on this Agent.
type toolBacking struct {
	backingService string
	available      bool
}

// inventoryBacking is the ONE declaration of what each dispatcher tool needs.
// Its key set is held equal to DispatcherTools() in both directions by
// TestInventoryTools_BackingIsDeclaredForExactlyTheRegisteredTools, so a tool
// is never registered without saying what it depends on, and no entry
// outlives its tool.
func (a *Agent) inventoryBacking() map[string]toolBacking {
	te := a.toolExecutor
	return map[string]toolBacking{
		// Repo-backed: real deployment always supplies these (or
		// the daemon would crash on first task lookup). We still
		// report the wiring for completeness so an operator sees
		// the full mapping.
		"list_projects":   {"Registry", te != nil && te.registry != nil},
		"list_tasks":      {"TaskRepository", te != nil && te.taskRepo != nil},
		"create_task":     {"TaskRepository", te != nil && te.taskRepo != nil},
		"get_task_status": {"TaskRepository", te != nil && te.taskRepo != nil},
		"wait_for_task":   {"TaskRepository", te != nil && te.taskRepo != nil && a.watchFunc != nil},
		"cancel_task":     {"TaskRepository", te != nil && te.taskRepo != nil},
		"retry_task":      {"TaskRepository", te != nil && te.taskRepo != nil},
		"list_executions": {"ExecutionRepository", te != nil && te.execRepo != nil},
		"list_artifacts":  {"ArtifactRepository", te != nil && te.artifactRepo != nil},
		"send_artifact":   {"ArtifactRepository", te != nil && te.artifactRepo != nil},
		"render_document": {"", true},
		// report_problem needs the problem-report seam; without it the tool
		// answers that reporting is not configured. Declared 2026-09-05, when
		// the two-way inventory test first named it as missing.
		"report_problem": {"ProblemReports", te != nil && te.problemReports != nil},
		"read_artifact":  {"ArtifactRepository", te != nil && te.artifactRepo != nil},

		// Option-gated: nil reflects missing wiring. These are
		// the ones admin ops actually care about — every gap
		// here is a "the bot can't do X" report waiting to happen.
		"send_email": {"EmailSender", a.emailSender != nil},
		// web_submit is a nil-wiring HARD gate: it needs BOTH the scraper
		// write client and the pending-write store. Reflects true
		// availability so operators see "the bot can't submit forms" when
		// either half is missing.
		"web_submit": {"ScraperWriteClient", te != nil && te.scraperWriteClient != nil && te.webWriteRepo != nil},
		"query_api":  {"APIClient", te != nil && te.apiClient != nil},
		// list_apis is stricter than query_api: it's advertised only
		// when the wired client also satisfies the optional
		// ProviderLister capability (design §5.5), so an operator
		// sees the true "discovery available" state rather than a
		// tool that always answers "not available on this daemon."
		"list_apis":     {"APIClient", te != nil && te.apiClient != nil && implementsProviderLister(te.apiClient)},
		"memory_search": {"MemorySearcher", a.memory != nil},
		"remember":      {"MemoryWrite", te != nil && te.memoryWrite != nil},
		// get_channel_thread reads the lead's own channel thread through the
		// channelThreads seam. It was registered without a backing entry
		// until 2026-09-05 and so rendered as unavailable in the admin
		// inventory whatever was wired — the gap the two-way test now names.
		getChannelThreadName: {"ChannelThreads", te != nil && te.channelThreads != nil},
		"memory_correct":     {"MemoryCorrector", a.memoryCorrector != nil},
		"memory_forget":      {"MemoryCorrector", a.memoryCorrector != nil},
		"set_reminder":       {"ReminderRepository", te != nil && te.reminderRepo != nil},
		"cancel_reminder": {
			"ReminderRepository",
			te != nil && te.reminderRepo != nil,
		},
		"update_reminder": {
			"ReminderRepository",
			te != nil && te.reminderRepo != nil,
		},
		"pause_reminder": {
			"ReminderRepository",
			te != nil && te.reminderRepo != nil,
		},
		"resume_reminder": {
			"ReminderRepository",
			te != nil && te.reminderRepo != nil,
		},
		"update_operator_profile": {
			"OperatorProfileRepository",
			te != nil && te.operatorProfiles != nil,
		},
		// Doubly gated (task 1.4): the bridge must be wired AND
		// composer.enabled must be true (default false during the
		// Phase 3 soak) — a wired-but-disabled bridge still reports
		// Available=false here so operators see "soak, not broken."
		"compose_automation": {"ComposerBridge", a.composer != nil && a.composerEnabled},

		// Always available — no external wiring.
		"switch_project": {"", true},
		"tool_search":    {"", true},
	}
}
