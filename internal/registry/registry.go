// Package registry provides in-memory registries for projects, swarms, and workflows.
package registry

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Registry holds all loaded definitions and provides thread-safe read access.
// It serves as the central in-memory cache for configuration loaded from YAML files.
type Registry struct {
	mu        sync.RWMutex
	active    *ConfigSet
	staged    *ConfigSet
	projects  map[string]*Project
	swarms    map[string]*Swarm
	workflows map[string]*Workflow
	// transient holds runtime-registered workflows that are NOT part
	// of the loaded config — e.g. the self-healing trial runner's
	// candidate genomes, registered under a "<wf>-candidate-<hash>"
	// id for the duration of a replay trial. Kept separate from
	// `workflows` so a config reload (which replaces `workflows`
	// wholesale) cannot drop a candidate mid-trial. GetWorkflow falls
	// back to this map. Lazily initialised by RegisterTransient.
	transient map[string]*Workflow
	configDir string
}

// ConfigSet represents a fully loaded, validated registry snapshot.
type ConfigSet struct {
	projects  map[string]*Project
	swarms    map[string]*Swarm
	workflows map[string]*Workflow
	configDir string
}

// ConfigDiff describes changes between the active and staged registry snapshots.
type ConfigDiff struct {
	ChangedProjects  []string
	DeletedProjects  []string
	ChangedSwarms    []string
	DeletedSwarms    []string
	ChangedWorkflows []string
	DeletedWorkflows []string
}

// HasChanges reports whether the diff contains any changes.
func (d ConfigDiff) HasChanges() bool {
	return len(d.ChangedProjects) > 0 ||
		len(d.DeletedProjects) > 0 ||
		len(d.ChangedSwarms) > 0 ||
		len(d.DeletedSwarms) > 0 ||
		len(d.ChangedWorkflows) > 0 ||
		len(d.DeletedWorkflows) > 0
}

// ValidationError aggregates all validation errors found during a reload
type ValidationError struct {
	Errors []error
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "validation failed"
	}
	msg := fmt.Sprintf("validation failed with %d error(s):", len(e.Errors))
	for _, err := range e.Errors {
		msg += fmt.Sprintf("\n  - %s", err.Error())
	}
	return msg
}

// New creates a new empty Registry.
func New() *Registry {
	active := &ConfigSet{
		projects:  make(map[string]*Project),
		swarms:    make(map[string]*Swarm),
		workflows: make(map[string]*Workflow),
	}
	return &Registry{
		active:    active,
		projects:  active.projects,
		swarms:    active.swarms,
		workflows: active.workflows,
	}
}

// Load loads all configurations from the specified directory.
// Projects with invalid cross-references are stripped with a returned
// *ValidationError (non-fatal); the remaining projects load normally.
// Fatal errors (I/O, parse) are returned as plain errors.
func (r *Registry) Load(configDir string) error {
	if err := r.Stage(configDir); err != nil {
		return err
	}
	warnings := r.StripInvalidFromStaged()
	if err := r.ActivateStaged(); err != nil {
		return err
	}
	if warnings != nil {
		return warnings
	}
	return nil
}

// Reload re-reads all configuration files from disk and validates them.
// On success, the in-memory cache is updated atomically.
// On failure, the existing configuration is preserved.
func (r *Registry) Reload() error {
	if r.configDir == "" {
		return fmt.Errorf("no config directory configured")
	}
	return r.Load(r.configDir)
}

// ConfigDir returns the directory the registry currently reads
// from on Reload. For multi-path loads (LoadFromPaths) this is
// the last (most-specific) layer — the one operators actively
// edit. Exposed for the 2026.7.0 F14 layered-config tests + the
// docs page.
func (r *Registry) ConfigDir() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.configDir
}

// Stage loads configs into a staged snapshot without touching the active state.
func (r *Registry) Stage(configDir string) error {
	staged, err := loadConfigSet(configDir)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.staged = staged
	return nil
}

// ValidateStaged validates the currently staged snapshot.
func (r *Registry) ValidateStaged() error {
	r.mu.RLock()
	staged := r.staged
	r.mu.RUnlock()

	if staged == nil {
		return fmt.Errorf("no staged configuration to validate")
	}

	return validateConfigSet(staged)
}

// StripInvalidFromStaged removes projects with broken cross-references from
// the staged snapshot in-place. Other entities are unaffected. Returns a
// *ValidationError listing every stripped project, or nil if all are valid.
func (r *Registry) StripInvalidFromStaged() *ValidationError {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.staged == nil {
		return nil
	}
	return stripInvalidProjects(r.staged)
}

// stripInvalidProjects removes projects whose cross-references are broken from
// cfg.projects and returns the collected errors, or nil if all are valid.
func stripInvalidProjects(cfg *ConfigSet) *ValidationError {
	var errs []error
	for projectID, project := range cfg.projects {
		swarm, swarmExists := cfg.swarms[project.SwarmID]
		if !swarmExists {
			errs = append(errs, fmt.Errorf("project '%s' references non-existent swarm '%s'", projectID, project.SwarmID))
			delete(cfg.projects, projectID)
			continue
		}
		workflow, workflowExists := cfg.workflows[project.DefaultWorkflowID]
		if !workflowExists {
			errs = append(errs, fmt.Errorf("project '%s' references non-existent workflow '%s'", projectID, project.DefaultWorkflowID))
			delete(cfg.projects, projectID)
			continue
		}
		// Validate every adaptive candidate exists. Operators
		// configure adaptive routing per project; an unknown entry
		// here would surface as a runtime delegation failure on a
		// real task, which is a worse failure mode than refusing
		// the project at registry-load.
		var candidateMissing bool
		for _, cand := range project.AdaptiveCandidateWorkflows {
			if _, ok := cfg.workflows[cand]; !ok {
				errs = append(errs, fmt.Errorf("project '%s' adaptiveCandidateWorkflows references non-existent workflow '%s'", projectID, cand))
				candidateMissing = true
			}
		}
		if candidateMissing {
			delete(cfg.projects, projectID)
			continue
		}
		// Validate the GitHub-App reply workflow override, when set.
		// Same reasoning as the adaptive candidates: a typo'd
		// reply_workflow_id should fail loudly at load, not silently
		// route GitHub tasks to a missing workflow at runtime.
		if id := strings.TrimSpace(project.GitHubApp.ReplyWorkflowID); id != "" {
			if _, ok := cfg.workflows[id]; !ok {
				errs = append(errs, fmt.Errorf("project '%s' github_app.reply_workflow_id references non-existent workflow '%s'", projectID, id))
				delete(cfg.projects, projectID)
				continue
			}
		}
		roleNames := make(map[string]struct{}, len(swarm.Roles))
		for _, role := range swarm.Roles {
			roleNames[role.Name] = struct{}{}
		}
		// Build a role-lookup map so the gate-schema check below can
		// reach the SwarmRole's outputSchema in O(1) per step.
		roleByName := make(map[string]*SwarmRole, len(swarm.Roles))
		for i := range swarm.Roles {
			roleByName[swarm.Roles[i].Name] = &swarm.Roles[i]
		}
		var projectErrs []error
		for stepID, step := range workflow.Steps {
			if step.Type != "agent" || step.Role == "" {
				continue
			}
			if _, ok := roleNames[step.Role]; !ok {
				projectErrs = append(projectErrs, fmt.Errorf(
					"project '%s' workflow '%s' step '%s' references role '%s' not present in swarm '%s'",
					projectID, workflow.ID, stepID, step.Role, swarm.ID,
				))
				continue
			}
			// Workflow-gate schema compat check (item 11 of
			// https://docs.vornik.io). When the
			// step gates on a path the role's schema can't produce,
			// the runtime gate evaluation silently goes to "no
			// match" — the workflow stalls or falls through to a
			// default branch, depending on operator config. Refuse
			// the project at load time instead.
			//
			// Only runs against roles that have migrated to
			// outputSchema. Legacy roles (no schema declared) skip
			// this check; their gate paths are operator-vouched and
			// failures still surface at runtime.
			role := roleByName[step.Role]
			if role == nil || role.OutputSchema == nil {
				continue
			}
			for gateIdx, gate := range step.Gates {
				for _, path := range gateConditionPaths(gate.Condition) {
					if !role.OutputSchema.DeclaresPath(path) {
						projectErrs = append(projectErrs, fmt.Errorf(
							"project '%s' workflow '%s' step '%s' gate[%d] condition %q references path '%s' not declared in role '%s' outputSchema (role schema must list this path under properties so the gate can evaluate it)",
							projectID, workflow.ID, stepID, gateIdx, gate.Condition, path, step.Role,
						))
					}
				}
			}
		}
		// Autonomy mode validation: empty string defaults to "llm",
		// canonical values pass, anything else is operator typo and
		// refused — silent fallthrough would land work on the wrong
		// engine. Only enforced when autonomy is enabled; disabled
		// projects can carry a stale/invalid mode without blocking
		// the rest of the registry.
		if project.Autonomy.Enabled {
			rawMode := strings.ToLower(strings.TrimSpace(project.Autonomy.Mode))
			switch rawMode {
			case "", AutonomyModeLLM, AutonomyModeCron, AutonomyModeBacklog:
				// ok
			default:
				projectErrs = append(projectErrs, fmt.Errorf(
					"project '%s' autonomy.mode '%s' is not one of (llm, cron, backlog)",
					projectID, project.Autonomy.Mode,
				))
			}
			// Cron-mode requires a non-empty Goal — it's the prompt
			// fired every tick. Without it the path has nothing to
			// hand the agent.
			if rawMode == AutonomyModeCron && strings.TrimSpace(project.Autonomy.Goal) == "" {
				projectErrs = append(projectErrs, fmt.Errorf(
					"project '%s' autonomy.mode 'cron' requires a non-empty autonomy.goal (used as the per-tick prompt)",
					projectID,
				))
			}
			// Backlog-mode: validate the file-path safety up-front so
			// an operator typo (absolute path, `..` traversal) is
			// caught at load instead of swallowed at the first tick.
			if rawMode == AutonomyModeBacklog {
				if project.ResolveBacklogFilePath() == "" {
					projectErrs = append(projectErrs, fmt.Errorf(
						"project '%s' autonomy.backlogFilePath '%s' is invalid (absolute paths and '..' segments rejected)",
						projectID, project.Autonomy.BacklogFilePath,
					))
				}
			}
		}

		if len(projectErrs) > 0 {
			errs = append(errs, projectErrs...)
			delete(cfg.projects, projectID)
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// ActivateStaged atomically promotes the staged snapshot to active.
func (r *Registry) ActivateStaged() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.staged == nil {
		return fmt.Errorf("no staged configuration to activate")
	}

	r.applyActiveLocked(r.staged)
	r.staged = nil
	return nil
}

// DiffStaged reports the change set between the active and staged snapshots.
func (r *Registry) DiffStaged() (ConfigDiff, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.staged == nil {
		return ConfigDiff{}, fmt.Errorf("no staged configuration available")
	}

	return ConfigDiff{
		ChangedProjects:  changedIDs(r.active.projects, r.staged.projects),
		DeletedProjects:  deletedIDs(r.active.projects, r.staged.projects),
		ChangedSwarms:    changedIDs(r.active.swarms, r.staged.swarms),
		DeletedSwarms:    deletedIDs(r.active.swarms, r.staged.swarms),
		ChangedWorkflows: changedIDs(r.active.workflows, r.staged.workflows),
		DeletedWorkflows: deletedIDs(r.active.workflows, r.staged.workflows),
	}, nil
}

func (r *Registry) applyActiveLocked(cfg *ConfigSet) {
	r.active = cfg
	r.projects = cfg.projects
	r.swarms = cfg.swarms
	r.workflows = cfg.workflows
	r.configDir = cfg.configDir
}

func loadConfigSet(configDir string) (*ConfigSet, error) {
	projects, err := LoadProjects(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load projects: %w", err)
	}

	swarms, err := LoadSwarms(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load swarms: %w", err)
	}

	workflows, err := LoadWorkflows(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load workflows: %w", err)
	}

	cfg := &ConfigSet{
		projects:  projects,
		swarms:    swarms,
		workflows: workflows,
		configDir: configDir,
	}

	return cfg, nil
}

// LoadFromPaths loads workflows / swarms / projects from
// multiple directories in order. Later paths override earlier
// paths on a per-entity-ID basis — so the conventional layout
// is to pass the shared catalog first and the user-personal
// overrides last:
//
//	registry.LoadFromPaths(
//	    "/etc/vornik/catalog",      // org-shared
//	    "/var/lib/vornik/configs",  // project workspace
//	    "~/.config/vornik/configs", // user-personal
//	)
//
// Lays groundwork for multi-tenant catalog isolation in
// 2026.8.0: the org-shared layer becomes per-tenant, and
// individual users layer their own overrides without
// touching the shared file. Empty paths slice falls back to
// "no config" (registry stays at its zero state). Missing
// directories are tolerated — they fall through to the
// next path — so a deployment can ship layered defaults
// without requiring every path to exist on every host.
//
// configDir is set to the LAST (most-specific) path so
// Reload still has a target; reload semantics today only
// re-read the last path, which matches "the most-specific
// layer is the one operators actively edit."
//
// 2026.7.0 F14.
func (r *Registry) LoadFromPaths(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	if len(paths) == 1 {
		// Single-path → identical to the legacy Load so the
		// daemon's init code can call LoadFromPaths uniformly
		// without special-casing.
		return r.Load(paths[0])
	}
	merged := &ConfigSet{
		projects:  make(map[string]*Project),
		swarms:    make(map[string]*Swarm),
		workflows: make(map[string]*Workflow),
	}
	for _, p := range paths {
		// Skip missing paths — layered config tolerates
		// optional layers. A malformed YAML, by contrast,
		// is still fatal (operators want to see the syntax
		// error rather than silently dropping the layer).
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat config path %q: %w", p, err)
		}
		layer, err := loadConfigSet(p)
		if err != nil {
			return fmt.Errorf("load layer %q: %w", p, err)
		}
		// Later wins: overwrite same-ID entries from the
		// previous layer. Distinct IDs from earlier layers
		// remain visible — that's the "inheritance" the
		// roadmap calls out.
		for id, v := range layer.projects {
			merged.projects[id] = v
		}
		for id, v := range layer.swarms {
			merged.swarms[id] = v
		}
		for id, v := range layer.workflows {
			merged.workflows[id] = v
		}
		merged.configDir = p
	}
	r.mu.Lock()
	r.staged = merged
	r.mu.Unlock()
	warnings := r.StripInvalidFromStaged()
	if err := r.ActivateStaged(); err != nil {
		return err
	}
	if warnings != nil {
		return warnings
	}
	return nil
}

func validateConfigSet(cfg *ConfigSet) error {
	var errs []error

	for projectID, project := range cfg.projects {
		swarm, swarmExists := cfg.swarms[project.SwarmID]
		if !swarmExists {
			errs = append(errs, fmt.Errorf(
				"project '%s' references non-existent swarm '%s'",
				projectID, project.SwarmID,
			))
		}

		workflow, workflowExists := cfg.workflows[project.DefaultWorkflowID]
		if !workflowExists {
			errs = append(errs, fmt.Errorf(
				"project '%s' references non-existent workflow '%s'",
				projectID, project.DefaultWorkflowID,
			))
		}

		if swarmExists && workflowExists {
			roleNames := make(map[string]struct{}, len(swarm.Roles))
			for _, role := range swarm.Roles {
				roleNames[role.Name] = struct{}{}
			}
			for stepID, step := range workflow.Steps {
				if step.Type != "agent" || step.Role == "" {
					continue
				}
				if _, ok := roleNames[step.Role]; !ok {
					errs = append(errs, fmt.Errorf(
						"project '%s' workflow '%s' step '%s' references role '%s' not present in swarm '%s'",
						projectID, workflow.ID, stepID, step.Role, swarm.ID,
					))
				}
			}
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}

	return nil
}

func changedIDs[T any](active, staged map[string]*T) []string {
	ids := make([]string, 0)
	for id, activeVal := range active {
		stagedVal, ok := staged[id]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(activeVal, stagedVal) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func deletedIDs[T any](active, staged map[string]*T) []string {
	ids := make([]string, 0)
	for id := range active {
		if _, ok := staged[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// GetConfigDir returns the current configuration directory.
func (r *Registry) GetConfigDir() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.configDir
}

// GetProject returns a project by ID. Returns nil if not found.
func (r *Registry) GetProject(id string) *Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projects[id]
}

// GetSwarm returns a swarm by ID. Returns nil if not found.
func (r *Registry) GetSwarm(id string) *Swarm {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.swarms[id]
}

// GetWorkflow returns a workflow by ID. Returns nil if not found.
// Loaded config workflows win; runtime-registered transient
// workflows (RegisterTransient) are a fallback so the dispatcher can
// resolve a candidate genome during a replay trial.
func (r *Registry) GetWorkflow(id string) *Workflow {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if wf := r.workflows[id]; wf != nil {
		return wf
	}
	if r.transient != nil {
		return r.transient[id]
	}
	return nil
}

// RegisterTransient installs a runtime workflow under id, visible to
// GetWorkflow until DeregisterTransient removes it. Used by the
// self-healing trial runner to route a candidate-genome replay at a
// "<wf>-candidate-<hash>" id without touching the loaded config (so a
// config reload can't drop it mid-trial). The workflow's own ID field
// is set to id for self-consistency. Re-registering the same id
// overwrites. Returns an error only on empty id / nil workflow.
func (r *Registry) RegisterTransient(id string, wf *Workflow) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("registry: RegisterTransient requires a non-empty id")
	}
	if wf == nil {
		return fmt.Errorf("registry: RegisterTransient requires a non-nil workflow (id %q)", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transient == nil {
		r.transient = make(map[string]*Workflow)
	}
	wf.ID = id
	r.transient[id] = wf
	return nil
}

// DeregisterTransient removes a transient workflow. No-op if absent
// (safe to defer unconditionally). It only ever touches the transient
// map, never loaded config.
func (r *Registry) DeregisterTransient(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.transient, id)
}

// ListProjects returns all loaded projects sorted by ID,
// archived included. Callers that should skip archived rows
// (scheduler, dispatcher, task-creation) use ListActiveProjects
// instead.
func (r *Registry) ListProjects() []*Project {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Project, 0, len(r.projects))
	for _, p := range r.projects {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result
}

// ListActiveProjects returns the subset of ListProjects whose
// lifecycle status is not "archived". Scheduling, dispatch,
// autonomy, and task-creation surfaces filter through this so a
// project pending deletion stops attracting new work the moment
// it's archived (the sweeper still needs to see the archived
// rows on its tick — use ListProjects there).
func (r *Registry) ListActiveProjects() []*Project {
	all := r.ListProjects()
	out := make([]*Project, 0, len(all))
	for _, p := range all {
		if p == nil || p.IsArchived() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ListArchivedProjects returns the subset of ListProjects that
// are archived. Drives the UI's archived-projects section and
// the sweeper's deletion-due query.
func (r *Registry) ListArchivedProjects() []*Project {
	all := r.ListProjects()
	out := make([]*Project, 0)
	for _, p := range all {
		if p != nil && p.IsArchived() {
			out = append(out, p)
		}
	}
	return out
}

// ListSwarms returns all loaded swarms.
func (r *Registry) ListSwarms() []*Swarm {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Swarm, 0, len(r.swarms))
	for _, s := range r.swarms {
		result = append(result, s)
	}
	return result
}

// ListWorkflows returns all loaded workflows.
func (r *Registry) ListWorkflows() []*Workflow {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Workflow, 0, len(r.workflows))
	for _, w := range r.workflows {
		result = append(result, w)
	}
	return result
}

// GetProjectWithSwarm returns a project along with its resolved swarm.
// Returns an error if the project or swarm is not found.
func (r *Registry) GetProjectWithSwarm(projectID string) (*Project, *Swarm, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	project, exists := r.projects[projectID]
	if !exists {
		return nil, nil, fmt.Errorf("project '%s' not found", projectID)
	}

	swarm, exists := r.swarms[project.SwarmID]
	if !exists {
		return project, nil, fmt.Errorf("swarm '%s' referenced by project '%s' not found",
			project.SwarmID, projectID)
	}

	return project, swarm, nil
}

// GetProjectWithWorkflow returns a project along with its resolved default workflow.
// Returns an error if the project or workflow is not found.
func (r *Registry) GetProjectWithWorkflow(projectID string) (*Project, *Workflow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	project, exists := r.projects[projectID]
	if !exists {
		return nil, nil, fmt.Errorf("project '%s' not found", projectID)
	}

	workflow, exists := r.workflows[project.DefaultWorkflowID]
	if !exists {
		return project, nil, fmt.Errorf("workflow '%s' referenced by project '%s' not found",
			project.DefaultWorkflowID, projectID)
	}

	return project, workflow, nil
}

// ResolveProjectConfig returns the full configuration for a project:
// the project definition, its swarm, and its default workflow.
// Returns an error if any component is not found.
func (r *Registry) ResolveProjectConfig(projectID string) (*Project, *Swarm, *Workflow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	project, exists := r.projects[projectID]
	if !exists {
		return nil, nil, nil, fmt.Errorf("project '%s' not found", projectID)
	}

	swarm, exists := r.swarms[project.SwarmID]
	if !exists {
		return nil, nil, nil, fmt.Errorf("swarm '%s' referenced by project '%s' not found",
			project.SwarmID, projectID)
	}

	workflow, exists := r.workflows[project.DefaultWorkflowID]
	if !exists {
		return nil, nil, nil, fmt.Errorf("workflow '%s' referenced by project '%s' not found",
			project.DefaultWorkflowID, projectID)
	}

	return project, swarm, workflow, nil
}

// Stats returns statistics about the loaded configurations.
type Stats struct {
	ProjectCount  int
	SwarmCount    int
	WorkflowCount int
	ConfigDir     string
}

// GetStats returns statistics about the loaded configurations.
func (r *Registry) GetStats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return Stats{
		ProjectCount:  len(r.projects),
		SwarmCount:    len(r.swarms),
		WorkflowCount: len(r.workflows),
		ConfigDir:     r.configDir,
	}
}

// ProjectConcurrencyLimits returns a map of project ID to maxConcurrentTasks
// for all projects that have a limit configured (> 0). Projects with 0 or
// unset limits are omitted (no per-project enforcement).
func (r *Registry) ProjectConcurrencyLimits() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	limits := make(map[string]int)
	for id, p := range r.projects {
		if p.MaxConcurrentTasks > 0 {
			limits[id] = p.MaxConcurrentTasks
		}
	}
	return limits
}

// ArchivedProjectIDs returns the set of project IDs whose
// lifecycle status is "archived". Used by the scheduler's lease
// query to skip queued work on archived projects so dispatch
// stops the moment the archive flag flips.
func (r *Registry) ArchivedProjectIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for id, p := range r.projects {
		if p != nil && p.IsArchived() {
			out = append(out, id)
		}
	}
	return out
}

// ProjectPriorities returns a map of project ID to DefaultPriority
// for every loaded project. Used by the scheduler's lease query as
// the primary sort key — higher-priority projects (lower numeric
// values) drain before lower-priority ones (LLD §4.2). Projects
// with unset DefaultPriority appear with priority 0 here, which the
// scheduler's caller can treat however it wishes; the SQL-side
// COALESCE applies a default for projects MISSING from the map
// (vs. present with priority 0). The distinction matters because
// "explicit priority 0" is the highest-urgency setting.
func (r *Registry) ProjectPriorities() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	priorities := make(map[string]int)
	for id, p := range r.projects {
		priorities[id] = p.DefaultPriority
	}
	return priorities
}
