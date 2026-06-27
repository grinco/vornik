package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

var (
	initConfigDir   string
	initDisplayName string
	initSwarmID     string
	initWorkflowID  string
	initGoal        string
	initAutonomy    bool
	initDryRun      bool
	initForce       bool

	// 2026.6.0 — template gallery integration. When --template is
	// set, the wizard skips its built-in YAML synthesis and
	// materialises the named template instead. --param sets a
	// template parameter (repeatable; format: name=value).
	initTemplate string
	initParams   []string
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize vornik resources",
}

var initProjectCmd = &cobra.Command{
	Use:   "project <name>",
	Short: "Create a project YAML config",
	Long:  "Create a project config under configs/projects, validate it against the registry, and optionally print it with --dry-run.",
	Args:  cobra.ExactArgs(1),
	RunE:  runInitProject,
}

func init() {
	initProjectCmd.Flags().StringVar(&initConfigDir, "config-dir", "", "Registry config directory (default: VORNIK_CONFIGS_DIR or ./configs)")
	initProjectCmd.Flags().StringVar(&initDisplayName, "display-name", "", "Human-readable project name")
	initProjectCmd.Flags().StringVar(&initSwarmID, "swarm", "basic-swarm", "Swarm ID")
	initProjectCmd.Flags().StringVar(&initWorkflowID, "workflow", "adaptive", "Default workflow ID")
	initProjectCmd.Flags().StringVar(&initGoal, "goal", "", "Autonomy goal")
	initProjectCmd.Flags().BoolVar(&initAutonomy, "autonomy", false, "Enable autonomy in the generated project")
	initProjectCmd.Flags().BoolVar(&initDryRun, "dry-run", false, "Print generated YAML instead of writing it")
	initProjectCmd.Flags().BoolVar(&initForce, "force", false, "Overwrite an existing project file")
	initProjectCmd.Flags().StringVar(&initTemplate, "template", "", "Materialise from a template slug (e.g. personal-assistant, news-feed). Bypasses the built-in YAML generator; use --param name=value to set template parameters.")
	initProjectCmd.Flags().StringArrayVar(&initParams, "param", nil, "Template parameter (repeatable). Format: name=value")
	initCmd.AddCommand(initProjectCmd)
	rootCmd.AddCommand(initCmd)
}

// parseParamFlags splits "k=v" pairs from --param into a map.
// Duplicate keys take the last value (matches operator intuition
// when overriding). Empty or malformed entries return a structured
// error so the operator sees a clear message instead of a silent
// drop.
func parseParamFlags(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, item := range raw {
		eq := strings.Index(item, "=")
		if eq <= 0 {
			return nil, fmt.Errorf("--param %q: expected name=value", item)
		}
		name := strings.TrimSpace(item[:eq])
		val := item[eq+1:]
		if name == "" {
			return nil, fmt.Errorf("--param %q: name must be non-empty", item)
		}
		out[name] = val
	}
	return out, nil
}

// runInitProjectFromTemplate handles the --template branch of
// `vornikctl init project`. Loads the catalog, validates the
// parameters, materialises files, and writes them to disk.
// Mirrors the API endpoint's contract so the CLI and the web UI
// gallery stay behaviour-equivalent.
func runInitProjectFromTemplate(cmd *cobra.Command, args []string, configDir string) error {
	templatesDir := filepath.Join(configDir, "project-templates")
	if env := os.Getenv("VORNIK_TEMPLATES_DIR"); env != "" {
		templatesDir = env
	}
	cat, err := templates.Load(templatesDir)
	if err != nil {
		return fmt.Errorf("load template catalog %s: %w", templatesDir, err)
	}
	manifest, ok := cat.Get(initTemplate)
	if !ok {
		available := []string{}
		for _, m := range cat.List() {
			available = append(available, m.Slug)
		}
		if len(available) == 0 {
			return fmt.Errorf("template %q not found and no templates installed in %s", initTemplate, templatesDir)
		}
		return fmt.Errorf("template %q not found — available: %s", initTemplate, strings.Join(available, ", "))
	}

	params, perr := parseParamFlags(initParams)
	if perr != nil {
		return perr
	}
	// Fall back to the positional <name> arg for projectId when
	// it's not explicitly passed via --param projectId=... .
	if _, ok := params["projectId"]; !ok && len(args) > 0 {
		params["projectId"] = sanitizeProjectID(args[0])
	}

	rendered, rerr := cat.MaterialiseFiles(manifest, params)
	if rerr != nil {
		return rerr
	}
	if initDryRun {
		// Print each rendered file with a header so the operator
		// can review before writing.
		for _, target := range templates.SortedTargets(rendered) {
			body := rendered[target]
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "# === %s ===\n%s\n", target, body)
		}
		return nil
	}
	// Hydrate-and-validate the rendered set against the existing
	// registry before touching disk (companion-onboarding bug,
	// 2026-05-27): the template path used to write files straight
	// out, so a project referencing a workflow not yet present in
	// configs/workflows/ landed cleanly here only to be silently
	// stripped on the next config reload. Failing here surfaces a
	// clear error before any state changes.
	if err := validateRenderedTemplate(configDir, rendered); err != nil {
		return fmt.Errorf("generated template failed registry validation (no files written): %w", err)
	}
	// Refuse-on-conflict (unless --force) — matches the API
	// handler's CONFLICT semantics so the operator's data is
	// safe.
	targets := templates.SortedTargets(rendered)
	for _, target := range targets {
		fullPath := filepath.Join(configDir, target)
		if _, err := os.Stat(fullPath); err == nil && !initForce {
			return fmt.Errorf("file already exists: %s (use --force to overwrite)", fullPath)
		}
	}
	for _, target := range targets {
		body := rendered[target]
		fullPath := filepath.Join(configDir, target)
		if mkErr := os.MkdirAll(filepath.Dir(fullPath), 0o755); mkErr != nil {
			return mkErr
		}
		// 0o600 — project + swarm + workflow configs can carry
		// credentials interpolated from template params.
		if wErr := os.WriteFile(fullPath, []byte(body), 0o600); wErr != nil {
			return wErr
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created %s\n", fullPath)
	}
	return nil
}

func runInitProject(cmd *cobra.Command, args []string) error {
	projectID := sanitizeProjectID(args[0])
	if projectID == "" {
		return fmt.Errorf("project name must contain at least one letter or number")
	}
	configDir := initConfigDir
	if configDir == "" {
		configDir = resolveConfigsDir("")
	}
	if configDir == "" {
		return fmt.Errorf("could not locate configs/ directory (set --config-dir or VORNIK_CONFIGS_DIR)")
	}

	// Template branch (2026.6.0): when --template is set, bypass
	// the built-in YAML generator below and materialise the named
	// template instead. Mirrors the API surface so CLI and web UI
	// behaviour stays in lockstep.
	if initTemplate != "" {
		return runInitProjectFromTemplate(cmd, args, configDir)
	}

	display := initDisplayName
	if display == "" {
		display = promptDefault(cmd, "Display name", strings.ReplaceAll(projectID, "-", " "))
	}
	goal := initGoal
	if initAutonomy && goal == "" {
		goal = promptDefault(cmd, "Autonomy goal", "Keep the project healthy and make steady progress on its backlog.")
	}

	project := registry.Project{
		ID:                 projectID,
		DisplayName:        display,
		SwarmID:            initSwarmID,
		DefaultWorkflowID:  initWorkflowID,
		DefaultPriority:    50,
		MaxConcurrentTasks: 1,
		Autonomy: registry.ProjectAutonomy{
			Enabled:         initAutonomy,
			Goal:            goal,
			MaxTasksPerHour: 2,
			PollInterval:    "5m",
			RequireApproval: false,
			AllowedTaskTypes: []string{
				"feature",
				"bug-fix",
				"refactor",
				"test-coverage",
				"roadmap-revision",
			},
		},
		Permissions: registry.ProjectPermissions{
			Secrets: []string{},
			AllowedTools: []string{
				"file_read", "file_write", "run_shell", "file_edit",
				"read_many_files", "grep", "glob", "git_status", "git_diff", "git_log", "git_show",
			},
		},
	}

	data, err := yaml.Marshal(project)
	if err != nil {
		return err
	}
	if initDryRun {
		fmt.Print(string(data))
		return nil
	}

	path := filepath.Join(configDir, "projects", projectID+".yaml")
	if !initForce {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("project config already exists: %s (use --force to overwrite)", path)
		}
	}
	if err := validateGeneratedProject(configDir, path, data); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Created %s\n", path)
	return nil
}

// validateRenderedTemplate hydrates the rendered template files into
// a temp registry alongside the existing configs/ tree, then calls
// registry.Load to surface any cross-reference errors (project
// references a missing workflow, workflow step references a role not
// in the swarm, etc.). Returns nil on a clean load.
//
// Used by runInitProjectFromTemplate before writing files. Without
// this gate, a malformed template materialisation would slip past the
// CLI and get silently stripped at config-reload time (the
// 2026-05-27 companion-onboarding bug). The check is intentionally
// the same shape as validateGeneratedProject (the legacy YAML-form
// path) so behaviour is consistent across both init flows.
func validateRenderedTemplate(configDir string, rendered map[string]string) error {
	tmp, err := os.MkdirTemp("", "vornik-init-template-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		// Tolerate missing subdirs in configDir — a fresh install
		// may not have all three yet. copyRegistryDir errors when
		// the source dir doesn't exist, so guard first.
		src := filepath.Join(configDir, sub)
		if info, statErr := os.Stat(src); statErr != nil || !info.IsDir() {
			if mkErr := os.MkdirAll(filepath.Join(tmp, sub), 0o755); mkErr != nil {
				return mkErr
			}
			continue
		}
		if err := copyRegistryDir(src, filepath.Join(tmp, sub)); err != nil {
			return err
		}
	}
	// Drop every rendered file at its target path inside the temp
	// tree. Target paths come from the template manifest's files[]
	// list and are already relative to configDir.
	for target, body := range rendered {
		fullPath := filepath.Join(tmp, target)
		if mkErr := os.MkdirAll(filepath.Dir(fullPath), 0o755); mkErr != nil {
			return mkErr
		}
		if wErr := os.WriteFile(fullPath, []byte(body), 0o600); wErr != nil {
			return wErr
		}
	}
	reg := registry.New()
	if err := reg.Load(tmp); err != nil {
		return err
	}
	return nil
}

func validateGeneratedProject(configDir, projectPath string, data []byte) error {
	tmp, err := os.MkdirTemp("", "vornik-init-project-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := copyRegistryDir(filepath.Join(configDir, sub), filepath.Join(tmp, sub)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(tmp, "projects"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "projects", filepath.Base(projectPath)), data, 0o600); err != nil {
		return err
	}
	reg := registry.New()
	if err := reg.Load(tmp); err != nil {
		return fmt.Errorf("generated project failed registry validation: %w", err)
	}
	return nil
}

func copyRegistryDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyRegistryDir(from, to); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func promptDefault(cmd *cobra.Command, label, fallback string) string {
	if cmd.InOrStdin() != os.Stdin {
		return fallback
	}
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return fallback
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [%s]: ", label, fallback)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return fallback
	}
	return line
}

func sanitizeProjectID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b bytes.Buffer
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
