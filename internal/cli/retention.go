package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/retention"
	"vornik.io/vornik/internal/storage"
)

// resolveConfigsDir picks the registry config dir using the same
// heuristics as the daemon's resolveRegistryConfigDir (and extends
// them for CLI paths that run without an explicit --config flag).
// Order:
//  1. $VORNIK_CONFIGS_DIR — explicit operator override.
//  2. configPath argument — {dir(configPath)}/configs, then dir(configPath).
//  3. $VORNIK_CONFIG env var — same derivation as (2).
//  4. Well-known operator paths: ~/.config/vornik/configs,
//     /etc/vornik/configs.
//  5. ./configs — last-resort "run from the repo root" form.
//
// Before this was extended, `vornikctl init swarm` + `vornikctl init
// project` would error out unless the operator happened to be in a
// directory with a configs/ subdir. Every other CLI command works
// from anywhere because it lets config.Load() do the discovery —
// init subcommands call this helper directly.
func resolveConfigsDir(configPath string) string {
	if env := os.Getenv("VORNIK_CONFIGS_DIR"); env != "" {
		if hasRegistryLayout(env) {
			return env
		}
	}
	candidates := []string{}
	addCandidatesFromConfigFile := func(path string) {
		if path == "" {
			return
		}
		base := filepath.Dir(path)
		candidates = append(candidates, filepath.Join(base, "configs"), base)
	}
	addCandidatesFromConfigFile(configPath)
	addCandidatesFromConfigFile(os.Getenv("VORNIK_CONFIG"))
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".config", "vornik", "configs"),
		)
	}
	candidates = append(candidates,
		"/etc/vornik/configs",
		"configs",
	)
	for _, c := range candidates {
		if hasRegistryLayout(c) {
			return c
		}
	}
	return ""
}

func hasRegistryLayout(dir string) bool {
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

var (
	retentionProject string
	retentionApply   bool
	retentionJSON    bool
)

var retentionCmd = &cobra.Command{
	Use:   "retention",
	Short: "Preview or apply retention pruning",
	Long: `Prune historical operational state older than the configured retention
windows. Defaults to preview mode — counts what would be pruned without
deleting anything. Add --apply to actually delete.

Windows default to:
  task_llm_usage  = 90 days
  tool_audit_log  = 30 days
  terminal tasks  = 60 days
  terminal execs  = 60 days
  artifacts       = 60 days  (DB + file on disk)

Minimum floor is 1 day regardless of config. project_memory_chunks is
NEVER pruned.

Examples:
  vornikctl retention                          # preview all projects
  vornikctl retention --project janka          # preview one project
  vornikctl retention --project janka --apply  # actually prune janka
`,
	RunE: runRetention,
}

func init() {
	retentionCmd.Flags().StringVar(&retentionProject, "project", "", "operate on a single project (default: all)")
	retentionCmd.Flags().BoolVar(&retentionApply, "apply", false, "actually delete rows (default: preview only)")
	retentionCmd.Flags().BoolVar(&retentionJSON, "json", false, "emit machine-readable JSON instead of a table")
	rootCmd.AddCommand(retentionCmd)
}

func runRetention(cmd *cobra.Command, args []string) error {
	cfg, configPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()

	// Load the registry so project-level overrides apply. Discover the
	// config dir the same way the daemon does: VORNIK_CONFIGS_DIR env,
	// then a "configs/" sibling of the main config file, then "configs"
	// relative to cwd.
	reg := registry.New()
	configsDir := resolveConfigsDir(configPath)
	if configsDir == "" {
		return fmt.Errorf("could not locate configs/ directory (set VORNIK_CONFIGS_DIR or run from repo root)")
	}
	if err := reg.Load(configsDir); err != nil {
		return fmt.Errorf("load registry from %s: %w", configsDir, err)
	}

	defaults := retention.Policy{
		TaskLLMUsageDays:          cfg.Retention.TaskLLMUsageDays,
		ToolAuditDays:             cfg.Retention.ToolAuditDays,
		ChatAuditDays:             cfg.Retention.ChatAuditDays,
		TasksDays:                 cfg.Retention.TasksDays,
		ExecutionsDays:            cfg.Retention.ExecutionsDays,
		ArtifactsDays:             cfg.Retention.ArtifactsDays,
		TaskMessagesDays:          cfg.Retention.TaskMessagesDays,
		MemoryChunksDays:          cfg.Retention.MemoryChunksDays,
		MemoryIngestAuditDays:     cfg.Retention.MemoryIngestAuditDays,
		MemoryPolicyEvalAllowDays: cfg.Retention.MemoryPolicyEvalAllowDays,
		MemoryPolicyEvalBlockDays: cfg.Retention.MemoryPolicyEvalBlockDays,
		ArtifactsRoot:             cfg.Storage.ArtifactsPath,
	}

	db, err := requirePostgresDB(backend, "retention")
	if err != nil {
		return err
	}
	sweeper := retention.New(db, zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger())

	projects := reg.ListProjects()
	if retentionProject != "" {
		filtered := projects[:0]
		for _, p := range projects {
			if p != nil && p.ID == retentionProject {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("project %q not found", retentionProject)
		}
		projects = filtered
	}
	if len(projects) == 0 {
		fmt.Println("no projects configured")
		return nil
	}

	type projectCounts struct {
		Project               string `json:"project"`
		LLMUsage              int    `json:"llm_usage"`
		ToolAudit             int    `json:"tool_audit"`
		Tasks                 int    `json:"tasks"`
		Executions            int    `json:"executions"`
		Artifacts             int    `json:"artifacts"`
		ArtifactFiles         int    `json:"artifact_files"`
		TaskMessages          int    `json:"task_messages"`
		MemoryChunks          int    `json:"memory_chunks"`
		MemoryIngestAudit     int    `json:"memory_ingest_audit"`
		MemoryPolicyEvalAllow int    `json:"memory_policy_eval_allow"`
		MemoryPolicyEvalBlock int    `json:"memory_policy_eval_block"`
		// ChatAuditLog is genuinely per project (the prune is project-scoped).
		// step_prompts and chat_system_prompts are NOT — see globalCounts.
		ChatAuditLog int    `json:"chat_audit_log"`
		Error        string `json:"error,omitempty"`
	}
	// The content-addressed prompt stores are pruned BY REFERENCE, not by
	// project: a body is shared across projects when its bytes are, so the
	// sweeper's count for them is a property of the whole database that each
	// per-project pass reports again. Rendering it in the per-project table
	// made a column an operator would sum — 6 unreferenced bodies read as 48
	// across eight projects. Reported once, below the table, instead.
	//
	// PREVIEW takes the max and APPLY the sum, and the difference is real: in
	// preview every pass measures the same untouched set, so they are all the
	// same number; under --apply the first pass deletes them and the rest
	// legitimately find nothing, so the sum is what was actually removed.
	var globalStepPrompts, globalChatPrompts, globalLLMExchanges int
	rows := make([]projectCounts, 0, len(projects))
	for _, p := range projects {
		policy := retention.Resolve(p.ID, retention.Policy{
			TaskLLMUsageDays:          p.Retention.TaskLLMUsageDays,
			ToolAuditDays:             p.Retention.ToolAuditDays,
			ChatAuditDays:             p.Retention.ChatAuditDays,
			TasksDays:                 p.Retention.TasksDays,
			ExecutionsDays:            p.Retention.ExecutionsDays,
			ArtifactsDays:             p.Retention.ArtifactsDays,
			TaskMessagesDays:          p.Retention.TaskMessagesDays,
			MemoryChunksDays:          p.Retention.MemoryChunksDays,
			MemoryIngestAuditDays:     p.Retention.MemoryIngestAuditDays,
			MemoryPolicyEvalAllowDays: p.Retention.MemoryPolicyEvalAllowDays,
			MemoryPolicyEvalBlockDays: p.Retention.MemoryPolicyEvalBlockDays,
		}, defaults)

		var (
			counts retention.Counts
			sErr   error
		)
		if retentionApply {
			counts, sErr = sweeper.Sweep(ctx, policy)
		} else {
			counts, sErr = sweeper.Preview(ctx, policy)
		}
		row := projectCounts{
			Project:               p.ID,
			LLMUsage:              counts.TaskLLMUsage,
			ToolAudit:             counts.ToolAudit,
			Tasks:                 counts.Tasks,
			Executions:            counts.Executions,
			Artifacts:             counts.Artifacts,
			ArtifactFiles:         counts.ArtifactFiles,
			TaskMessages:          counts.TaskMessages,
			MemoryChunks:          counts.MemoryChunks,
			MemoryIngestAudit:     counts.MemoryIngestAudit,
			MemoryPolicyEvalAllow: counts.MemoryPolicyEvalAllow,
			MemoryPolicyEvalBlock: counts.MemoryPolicyEvalBlock,
			ChatAuditLog:          counts.ChatAuditLog,
		}
		globalStepPrompts = accumulateGlobal(globalStepPrompts, counts.StepPrompts, retentionApply)
		globalChatPrompts = accumulateGlobal(globalChatPrompts, counts.ChatSystemPrompts, retentionApply)
		globalLLMExchanges = accumulateGlobal(globalLLMExchanges, counts.LLMExchanges, retentionApply)
		if sErr != nil {
			row.Error = sErr.Error()
			if !retentionJSON {
				fmt.Fprintf(os.Stderr, "warn: project %s: %v\n", p.ID, sErr)
			}
		}
		rows = append(rows, row)
	}

	if retentionJSON {
		// Structured output for pipelines — includes per-project errors
		// in-band so wrapper scripts don't have to multiplex stderr.
		out := struct {
			Applied  bool            `json:"applied"`
			Global   globalCounts    `json:"global"`
			Projects []projectCounts `json:"projects"`
		}{
			Applied: retentionApply,
			Global: globalCounts{
				StepPrompts:       globalStepPrompts,
				ChatSystemPrompts: globalChatPrompts,
				LLMExchanges:      globalLLMExchanges,
			},
			Projects: rows,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if retentionApply {
		fmt.Println("applying retention — rows will be deleted and artifact files unlinked.")
	} else {
		fmt.Println("preview only — no changes. use --apply to actually prune.")
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "project\tllm_usage\taudit\ttasks\texecutions\tartifacts\tfiles\ttask_msgs\tmem_chunks\tingest_audit\tfw_eval_allow\tfw_eval_block\tchat_audit"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			row.Project, row.LLMUsage, row.ToolAudit, row.Tasks,
			row.Executions, row.Artifacts, row.ArtifactFiles,
			row.TaskMessages, row.MemoryChunks, row.MemoryIngestAudit,
			row.MemoryPolicyEvalAllow, row.MemoryPolicyEvalBlock,
			row.ChatAuditLog); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// Printed once, and said to be global, because it is: these bodies are
	// shared across projects and pruned by reference.
	_, _ = fmt.Fprintf(os.Stdout,
		"\ncontent-addressed prompt bodies nothing references (GLOBAL, not per project):\n"+
			"  step_prompts %d  •  chat_system_prompts %d  •  llm_exchanges (execution gone) %d\n",
		globalStepPrompts, globalChatPrompts, globalLLMExchanges)
	return nil
}

// accumulateGlobal folds one project pass's count of a GLOBAL prune into the
// running total, and the two modes genuinely differ:
//
//   - preview: every pass measures the same untouched set, so they all report
//     the same number and the answer is that number, not its multiple;
//   - apply: the first pass deletes the set and later passes legitimately find
//     nothing, so the sum is what was actually removed.
//
// Summing in preview is the defect this replaces: six unreferenced prompt
// bodies rendered as forty-eight across eight projects.
func accumulateGlobal(running, pass int, applied bool) int {
	if applied {
		return running + pass
	}
	return max(running, pass)
}

// globalCounts are the sweeper results that belong to the database rather than
// to any one project.
type globalCounts struct {
	StepPrompts       int `json:"step_prompts"`
	ChatSystemPrompts int `json:"chat_system_prompts"`
	LLMExchanges      int `json:"llm_exchanges"`
}
