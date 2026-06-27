package cli

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

//go:embed presets/*.md
var swarmPresetsFS embed.FS

var (
	initSwarmConfigDir string
	initSwarmTemplate  string
	initSwarmDryRun    bool
	initSwarmForce     bool
	initSwarmList      bool
)

var initSwarmCmd = &cobra.Command{
	Use:   "swarm <name>",
	Short: "Create a SWARM.md config from a preset template",
	Long: `Generate a new swarm config by copying one of the built-in preset
templates and rewriting its swarmId to the name you pass. The generated
file is a WORKFLOW.md-style SWARM.md (YAML frontmatter + Markdown body)
and is validated against the full registry before it lands on disk.

Available presets (use --list to see the one-line description of each):
  basic            lead + coder + reviewer — minimal code swarm
  dev              lead + feasibility + analyst + coder + tester + reviewer + scout + architect — full code stack
  research         lead + researcher + writer — good for digest / scan / summary projects
  companion-ingest reviewer + analyst + summarizer + rag-ingester — companion swarm with async-ingest role

Examples:
  vornikctl init swarm my-swarm --template basic
  vornikctl init swarm itpe-triage --template research --dry-run
  vornikctl init swarm my-ingest-swarm --template companion-ingest --force
  vornikctl init swarm --list`,
	RunE: runInitSwarm,
}

func init() {
	initSwarmCmd.Flags().StringVar(&initSwarmConfigDir, "config-dir", "", "Registry config directory (default: VORNIK_CONFIGS_DIR or ./configs)")
	initSwarmCmd.Flags().StringVar(&initSwarmTemplate, "template", "basic", "Preset template: basic | dev | research | companion-ingest")
	initSwarmCmd.Flags().BoolVar(&initSwarmDryRun, "dry-run", false, "Print generated SWARM.md instead of writing it")
	initSwarmCmd.Flags().BoolVar(&initSwarmForce, "force", false, "Overwrite an existing swarm file")
	initSwarmCmd.Flags().BoolVar(&initSwarmList, "list", false, "List available preset templates and exit")
	initCmd.AddCommand(initSwarmCmd)
}

func runInitSwarm(cmd *cobra.Command, args []string) error {
	if initSwarmList {
		return listPresets(cmd)
	}
	if len(args) < 1 {
		return fmt.Errorf("swarm name is required (e.g. `vornikctl init swarm my-swarm --template basic`)")
	}
	swarmID := sanitizeProjectID(args[0])
	if swarmID == "" {
		return fmt.Errorf("swarm name must contain at least one letter or number")
	}

	presetName := strings.ToLower(strings.TrimSpace(initSwarmTemplate))
	preset, err := readPreset(presetName)
	if err != nil {
		return err
	}

	// Substitute the placeholder swarmId with the user's name. Use an
	// exact string match rather than a regex so we never corrupt any
	// YAML content that happens to match a similar pattern.
	const placeholder = `swarmId: "__TEMPLATE__"`
	replacement := fmt.Sprintf("swarmId: %q", swarmID)
	if !strings.Contains(preset, placeholder) {
		return fmt.Errorf("preset %q is missing the %q placeholder — please report", presetName, placeholder)
	}
	generated := strings.Replace(preset, placeholder, replacement, 1)

	if initSwarmDryRun {
		fmt.Print(generated)
		return nil
	}

	configDir := initSwarmConfigDir
	if configDir == "" {
		configDir = resolveConfigsDir("")
	}
	if configDir == "" {
		return fmt.Errorf("could not locate configs/ directory (set --config-dir or VORNIK_CONFIGS_DIR)")
	}

	path := filepath.Join(configDir, "swarms", swarmID+".md")
	if !initSwarmForce {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("swarm config already exists: %s (use --force to overwrite)", path)
		}
	}
	if err := validateGeneratedSwarm(configDir, path, []byte(generated)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 0o600 — SWARM.md frontmatter often references role-model
	// API gateway tokens.
	if err := os.WriteFile(path, []byte(generated), 0o600); err != nil {
		return err
	}
	fmt.Printf("Created %s\n", path)
	fmt.Printf("Next steps:\n")
	fmt.Printf("  1. Edit %s to tune role prompts and models.\n", path)
	fmt.Printf("  2. Reference the swarm from a project: swarmId: %q.\n", swarmID)
	fmt.Printf("  3. Reload: vornikctl config reload\n")
	return nil
}

func listPresets(cmd *cobra.Command) error {
	entries, err := swarmPresetsFS.ReadDir("presets")
	if err != nil {
		return fmt.Errorf("read embedded presets: %w", err)
	}
	type presetInfo struct {
		Name string
		Desc string
	}
	var out []presetInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		body, err := swarmPresetsFS.ReadFile(filepath.Join("presets", e.Name()))
		if err != nil {
			continue
		}
		// displayName line gives the one-line description.
		desc := presetDisplayName(string(body))
		out = append(out, presetInfo{Name: name, Desc: desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	for _, p := range out {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-10s %s\n", p.Name, p.Desc)
	}
	return nil
}

// presetDisplayName pulls the displayName: line out of the preset body.
// Linear scan rather than YAML decode so listPresets doesn't need the
// registry stack (and so a malformed preset still lists with its file
// name instead of failing the whole list).
func presetDisplayName(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "displayName:") {
			v := strings.TrimPrefix(trim, "displayName:")
			v = strings.TrimSpace(v)
			v = strings.Trim(v, `"`)
			return v
		}
	}
	return "(no description)"
}

// readPreset fetches the named preset's raw YAML body from the embedded
// filesystem. Returns a clear error if the caller passes a name that
// isn't shipped — the --list output enumerates valid choices.
func readPreset(name string) (string, error) {
	// Reject traversal / oddities up front — embed.FS doesn't follow
	// them, but a clean error is friendlier than "file not found".
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return "", fmt.Errorf("invalid template name %q — use --list to see available presets", name)
	}
	body, err := swarmPresetsFS.ReadFile("presets/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("unknown preset %q — run `vornikctl init swarm --list` for available templates", name)
	}
	return string(body), nil
}

// validateGeneratedSwarm mirrors validateGeneratedProject: copy the
// live registry into a scratch dir, overlay the new swarm file, and
// Load the whole thing to confirm the generated YAML is valid in
// context (no duplicate IDs, no schema violations).
func validateGeneratedSwarm(configDir, swarmPath string, data []byte) error {
	tmp, err := os.MkdirTemp("", "vornik-init-swarm-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := copyRegistryDir(filepath.Join(configDir, sub), filepath.Join(tmp, sub)); err != nil {
			// A missing subdir in the source is fine (empty deployment).
			if !os.IsNotExist(err) {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(tmp, "swarms"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "swarms", filepath.Base(swarmPath)), data, 0o600); err != nil {
		return err
	}
	reg := registry.New()
	if err := reg.Load(tmp); err != nil {
		return fmt.Errorf("generated swarm failed registry validation: %w", err)
	}
	return nil
}
