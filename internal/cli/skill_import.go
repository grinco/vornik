package cli

// `vornikctl skill import <file>` — materialise a portable
// SWARM-SKILL.md into the deployed configs tree.
//
// Writes:
//   1. `<configs>/workflows/<workflowID>.md` — fresh WORKFLOW.md.
//   2. `<configs>/swarms/<swarmID>.md` — target swarm rewritten
//      with the imported roles appended.
//
// Conflict surface is reported up front (all problems in one pass):
//   - workflow file already exists in <configs>/workflows;
//   - role name collides with an existing role in the target swarm;
//   - target swarm file is missing (without --as-swarm to create it).
//
// Operator escape hatches:
//   --rename-workflow <id>     change the imported workflow ID
//   --rename-role old=new,...  rewrite imported role names + every
//                              workflow step's `role:` reference in
//                              lock-step
//   --into-swarm <id>          target a swarm other than the
//                              project's default
//   --as-swarm <id>            create a brand-new swarm
//   --project <id>             resolve the target swarm from project
//   --dry-run                  print the would-be writes, exit 0
//
// The CLI does not call the daemon — it parses + validates + writes
// the configs tree, then prompts the operator to reload via the
// usual `vornikctl config reload` path. Avoiding the API keeps import
// usable on an offline operator workstation that's preparing a
// deployment.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

var skillConfigIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

var (
	skillImportConfigsDir     string
	skillImportProject        string
	skillImportIntoSwarm      string
	skillImportAsSwarm        string
	skillImportRenameWorkflow string
	skillImportRenameRoles    []string
	skillImportDryRun         bool

	skillImportCmd = &cobra.Command{
		Use:   "import <file>",
		Short: "Materialise a SWARM-SKILL.md into the deployed configs tree",
		Long: `Import a SWARM-SKILL.md, writing a fresh WORKFLOW.md plus the
merged target swarm.

A target swarm is required — either the project's swarm (via
--project), an explicit swarm (--into-swarm), or a brand-new
swarm to create (--as-swarm).

Conflict detection is up-front: workflow IDs that already exist or
role names that collide are surfaced together before any write.

Use --dry-run to preview the writes without touching disk.`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillImport,
	}
)

func init() {
	skillImportCmd.Flags().StringVar(&skillImportConfigsDir, "configs-dir", "", "Configs directory (default: VORNIK_CONFIGS_DIR or ~/.config/vornik/configs)")
	skillImportCmd.Flags().StringVar(&skillImportProject, "project", "", "Resolve target swarm from this project's config")
	skillImportCmd.Flags().StringVar(&skillImportIntoSwarm, "into-swarm", "", "Merge the imported roles into this existing swarm (overrides --project)")
	skillImportCmd.Flags().StringVar(&skillImportAsSwarm, "as-swarm", "", "Create a new swarm with the imported roles (overrides --into-swarm)")
	skillImportCmd.Flags().StringVar(&skillImportRenameWorkflow, "rename-workflow", "", "Change the imported workflow's ID")
	skillImportCmd.Flags().StringSliceVar(&skillImportRenameRoles, "rename-role", nil, "Rewrite imported role names; repeat for multiple, format old=new")
	skillImportCmd.Flags().BoolVar(&skillImportDryRun, "dry-run", false, "Print the would-be writes without touching disk")
	skillCmd.AddCommand(skillImportCmd)
}

func runSkillImport(cmd *cobra.Command, args []string) error {
	// Bound the read before slurping the whole file into memory — a DoS
	// guard symmetric with the install resolver's size cap. The registry
	// parser re-checks, but stat-first avoids reading a multi-GB file at all.
	if fi, statErr := os.Stat(args[0]); statErr == nil && fi.Size() > registry.MaxSwarmSkillBytes {
		return fmt.Errorf("%s: file is %d bytes; max is %d", args[0], fi.Size(), registry.MaxSwarmSkillBytes)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read %s: %w", args[0], err)
	}

	report := registry.ValidateSwarmSkillMarkdown(data, filepath.Base(args[0]))
	if report.HasErrors() {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: validation failed\n", args[0])
		for _, f := range report.Findings {
			if f.Severity == registry.SeverityError {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", f)
			}
		}
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("refusing to import — fix validation errors first")
	}

	skill, err := registry.ParseSwarmSkill(data, filepath.Base(args[0]))
	if err != nil {
		return err
	}
	if skill.Workflow == nil || len(skill.Roles) == 0 {
		return fmt.Errorf("skill has no vornik payload (workflow + roles); was the source exported with --standard?")
	}

	if err := applyImportRenames(skill); err != nil {
		return err
	}

	configsDir := skillImportConfigsDir
	if configsDir == "" {
		configsDir = resolveConfigsDir("")
	}
	if configsDir == "" {
		return fmt.Errorf("could not locate configs/ directory (set --configs-dir or VORNIK_CONFIGS_DIR)")
	}

	plan, err := planSkillImport(skill, configsDir)
	if err != nil {
		return err
	}

	if conflicts := plan.conflicts(); len(conflicts) > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "import would conflict:\n")
		for _, c := range conflicts {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  - %s\n", c)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\nre-run with --rename-workflow / --rename-role to resolve.\n")
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("import conflict")
	}

	if skillImportDryRun {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "would write %s\n", plan.WorkflowPath)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "would update %s\n", plan.SwarmPath)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  roles added: %s\n", strings.Join(roleNames(skill.Roles), ", "))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(plan.WorkflowPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plan.WorkflowPath, plan.WorkflowBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.WorkflowPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.SwarmPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plan.SwarmPath, plan.SwarmBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.SwarmPath, err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", plan.WorkflowPath)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", plan.SwarmPath)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "next: vornikctl config reload\n")
	return nil
}

// applyImportRenames mutates skill in place to honour
// --rename-workflow / --rename-role flags. Role renames also
// rewrite every workflow step's `role:` reference so the
// imported bundle stays internally consistent.
func applyImportRenames(skill *registry.SwarmSkill) error {
	if skillImportRenameWorkflow != "" {
		skill.Workflow.ID = skillImportRenameWorkflow
	}
	if len(skillImportRenameRoles) == 0 {
		return nil
	}
	rename := make(map[string]string, len(skillImportRenameRoles))
	for _, pair := range skillImportRenameRoles {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("--rename-role %q must be old=new", pair)
		}
		rename[parts[0]] = parts[1]
	}
	for i := range skill.Roles {
		if newName, ok := rename[skill.Roles[i].Name]; ok {
			skill.Roles[i].Name = newName
		}
	}
	for id, step := range skill.Workflow.Steps {
		if newName, ok := rename[step.Role]; ok {
			step.Role = newName
			skill.Workflow.Steps[id] = step
		}
	}
	return nil
}

// importPlan packages every decision the import path makes BEFORE
// any disk write so the dry-run and the real run share one code
// path. conflicts() returns the collated reasons the plan can't
// proceed; an empty list means "go".
type importPlan struct {
	WorkflowPath        string
	WorkflowBytes       []byte
	SwarmPath           string
	SwarmBytes          []byte
	ExistingWorkflowMsg string
	ConflictRoles       []string
	MissingSwarmMsg     string
}

func (p *importPlan) conflicts() []string {
	var out []string
	if p.ExistingWorkflowMsg != "" {
		out = append(out, p.ExistingWorkflowMsg)
	}
	if p.MissingSwarmMsg != "" {
		out = append(out, p.MissingSwarmMsg)
	}
	for _, r := range p.ConflictRoles {
		out = append(out, fmt.Sprintf("role %q already exists in target swarm", r))
	}
	return out
}

// planSkillImport assembles the importPlan from the skill + the
// caller-resolved configsDir. Pure function — no disk writes.
//
// Callers that supply a non-empty explicitSwarmID skip the
// --project / --into-swarm / --as-swarm resolver — used by
// `vornikctl skill install`, which always namespaces the swarm.
func planSkillImport(skill *registry.SwarmSkill, configsDir string) (*importPlan, error) {
	return planSkillImportWithSwarm(skill, configsDir, "")
}

func planSkillImportWithSwarm(skill *registry.SwarmSkill, configsDir, explicitSwarmID string) (*importPlan, error) {
	if err := validateSkillConfigID("workflowId", skill.Workflow.ID); err != nil {
		return nil, err
	}
	plan := &importPlan{
		WorkflowPath: filepath.Join(configsDir, "workflows", skill.Workflow.ID+".md"),
	}

	if _, err := os.Stat(plan.WorkflowPath); err == nil {
		plan.ExistingWorkflowMsg = fmt.Sprintf("workflow file already exists: %s (use --rename-workflow)", plan.WorkflowPath)
	}

	workflowBytes, err := registry.MarshalWorkflowMarkdown(skill.Workflow)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow: %w", err)
	}
	plan.WorkflowBytes = workflowBytes

	swarmID := explicitSwarmID
	if swarmID == "" {
		resolved, err := resolveTargetSwarmID(skill, configsDir)
		if err != nil {
			return nil, err
		}
		swarmID = resolved
	}
	if err := validateSkillConfigID("swarmId", swarmID); err != nil {
		return nil, err
	}
	plan.SwarmPath = filepath.Join(configsDir, "swarms", swarmID+".md")

	allowCreate := skillImportAsSwarm != "" || explicitSwarmID != ""
	swarm, missingMsg, err := loadOrCreateTargetSwarm(swarmID, plan.SwarmPath, allowCreate)
	if err != nil {
		return nil, err
	}
	if missingMsg != "" {
		plan.MissingSwarmMsg = missingMsg
		// Continue building the plan so the operator sees every
		// problem in one shot; the conflicts list still blocks
		// the write.
	}

	if swarm != nil {
		existing := make(map[string]bool, len(swarm.Roles))
		for _, r := range swarm.Roles {
			existing[r.Name] = true
		}
		for _, r := range skill.Roles {
			if existing[r.Name] {
				plan.ConflictRoles = append(plan.ConflictRoles, r.Name)
			}
		}
	}

	// If we have no swarm to merge into (--as-swarm with no
	// existing file), build the new swarm from scratch using the
	// imported roles.
	if swarm == nil {
		swarm = &registry.Swarm{
			ID:          swarmID,
			DisplayName: swarmID,
			Roles:       append([]registry.SwarmRole{}, skill.Roles...),
		}
	} else if len(plan.ConflictRoles) == 0 {
		swarm.Roles = append(swarm.Roles, skill.Roles...)
	}

	swarmBytes, err := registry.MarshalSwarmMarkdown(swarm)
	if err != nil {
		return nil, fmt.Errorf("marshal swarm: %w", err)
	}
	plan.SwarmBytes = swarmBytes

	return plan, nil
}

func validateSkillConfigID(label, id string) error {
	id = strings.TrimSpace(id)
	if !skillConfigIDRe.MatchString(id) {
		return fmt.Errorf("%s %q is unsafe; use letters, digits, '_' or '-' and no path separators", label, id)
	}
	clean := filepath.Clean(id)
	if clean != id || filepath.IsAbs(id) || strings.Contains(id, string(filepath.Separator)) {
		return fmt.Errorf("%s %q is unsafe; path traversal is not allowed", label, id)
	}
	return nil
}

// resolveTargetSwarmID picks the swarm to merge into, in
// priority order: --as-swarm, --into-swarm, --project.
// Returns a precise error if none of the three is set so the
// operator knows which flag they're missing.
func resolveTargetSwarmID(skill *registry.SwarmSkill, configsDir string) (string, error) {
	if skillImportAsSwarm != "" {
		return skillImportAsSwarm, nil
	}
	if skillImportIntoSwarm != "" {
		return skillImportIntoSwarm, nil
	}
	if skillImportProject != "" {
		// Read the project YAML on disk to find its swarmId.
		// Avoids a daemon round-trip so import works offline.
		swarmID, err := readProjectSwarmID(configsDir, skillImportProject)
		if err != nil {
			return "", err
		}
		return swarmID, nil
	}
	return "", fmt.Errorf("specify a target swarm via --project, --into-swarm, or --as-swarm")
}

// readProjectSwarmID parses the project's PROJECT.md (or YAML
// fallback) just enough to extract its swarmId. Reuses the
// registry's project loader so any quirks of the on-disk shape
// are handled in one place.
func readProjectSwarmID(configsDir, projectID string) (string, error) {
	projects, err := registry.LoadProjects(filepath.Join(configsDir, "projects"))
	if err != nil {
		return "", fmt.Errorf("load projects: %w", err)
	}
	p, ok := projects[projectID]
	if !ok || p == nil {
		return "", fmt.Errorf("project %q not found under %s", projectID, configsDir)
	}
	if p.SwarmID == "" {
		return "", fmt.Errorf("project %q has no swarmId configured", projectID)
	}
	return p.SwarmID, nil
}

// loadOrCreateTargetSwarm reads the swarm file at path. When the
// file is missing and the caller signalled allowCreate (operator
// passed --as-swarm, or an install path namespaced the swarm),
// the missing state is normal — we'll create from scratch.
// Otherwise the missing file becomes a conflict the plan surfaces.
func loadOrCreateTargetSwarm(swarmID, path string, allowCreate bool) (*registry.Swarm, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if allowCreate {
				return nil, "", nil
			}
			return nil, fmt.Sprintf("target swarm %q has no file at %s (use --as-swarm to create one)", swarmID, path), nil
		}
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	sw, err := registry.ParseSwarmMarkdown(data, filepath.Base(path))
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return sw, "", nil
}

func roleNames(roles []registry.SwarmRole) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = r.Name
	}
	sort.Strings(out)
	return out
}
