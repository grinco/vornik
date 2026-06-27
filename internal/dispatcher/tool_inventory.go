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
	te := a.toolExecutor

	gating := map[string]struct {
		backingService string
		available      bool
	}{
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
		"read_artifact":   {"ArtifactRepository", te != nil && te.artifactRepo != nil},

		// Option-gated: nil reflects missing wiring. These are
		// the ones admin ops actually care about — every gap
		// here is a "the bot can't do X" report waiting to happen.
		"send_email":     {"EmailSender", a.emailSender != nil},
		"memory_search":  {"MemorySearcher", a.memory != nil},
		"memory_correct": {"MemoryCorrector", a.memoryCorrector != nil},

		// Always available — no external wiring.
		"switch_project": {"", true},
		"tool_search":    {"", true},
	}

	tools := DispatcherTools()
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
			// Unknown tool — register a row with Available=false
			// so the UI surfaces the gap (means a tool was added
			// in DispatcherTools but not declared here).
			info.Available = false
		}
		out = append(out, info)
	}
	return out
}
