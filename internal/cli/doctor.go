package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Items   []string `json:"items,omitempty"`
	Fixed   int      `json:"fixed,omitempty"`
}

type doctorReport struct {
	Timestamp string        `json:"timestamp"`
	Checks    []doctorCheck `json:"checks"`
	Summary   string        `json:"summary"`
}

var (
	doctorFix  bool
	doctorJSON bool
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose and repair common issues",
	Long: `Run diagnostic checks against the vornik daemon and optionally fix issues.

Operational state:
  stale_leases          Tasks stuck in LEASED/RUNNING with expired leases
  orphaned_watchers     Telegram watchers for tasks already completed/failed
  stuck_executions      Executions in RUNNING/PENDING for over 1 hour
  task_state_audit      Terminal tasks with leaked lease fields

Schema & storage:
  config_validation     Validate all project/swarm/workflow YAML configs
  workflow_swarm_compat Flag (project, workflow) pairs whose swarm can't satisfy the workflow's roles
  role_prompt_sanity    Lint swarm role prompts: tool refs vs allowedTools, output shape, untrusted_content awareness
  eval_suite_lint       Parse configs/evals/*.json; flag suites with missing/incompatible project/workflow/swarm
  database_schema       Verify expected tables and indexes exist (incl. 2026.4.11+ additions)
  orphan_fk_rows        Detect orphan rows in audit / llm_usage / watchers referencing missing tasks
  orphan_worktrees      .worktrees/ subdirs with no matching live task
  config_crlf           Config files with CRLF line endings (UI YAML-writer drift); --fix normalizes to LF

Runtime:
  podman_config         Check podman availability and rootless configuration
  agent_images          Verify agent images referenced in swarm configs are available
  env_file_freshness    Flag EnvironmentFile= entries modified after daemon
                        start (systemd reads them only at ExecStart, so
                        post-edit secrets are invisible until restart)

Security:
  api_security_posture  Flag API auth disabled with non-loopback listen address
  api_key_strength      Detect weak / placeholder API keys
  secrets_permissions   Secrets files/dirs with world-readable permissions
  config_secret_hygiene Config.yaml plaintext secrets or loose permissions (recommends ${ENV_VAR})

Models:
  model_health          Role-pinned models with high recent failure rate or
                        degenerate output; RECOMMENDS the role's modelFallback
                        (diagnostic only — never auto-switches a model)
  model_route_coverage  Role-pinned models that don't resolve to a chat
                        model_route prefix or are missing from pricing.yaml

Cost & budget:
  pricing_coverage      Models in swarm configs missing from pricing.yaml
  autonomy_budget_guard Autonomy-enabled projects with no hard $ cap
  budget_utilisation    Projects at ≥80%% of daily or monthly hard cap
  dispatcher_role       When telegram.dispatcher_project_id is set,
                        the chosen project's swarm should declare a
                        "dispatcher" role so dashboard role+model
                        aggregation rows align with the swarm catalogue.

Use --fix to automatically repair stale_leases, orphaned_watchers,
stuck_executions, task_state_audit, orphan_fk_rows, orphan_worktrees,
secrets_permissions, and dispatcher_role findings. Schema, runtime,
security posture, pricing, and budget checks are diagnostic only
because they require operator config changes or external runtime actions.`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorFix, "fix", false, "Automatically repair detected issues")
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "Output in JSON format")
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()

	path := "/api/v1/doctor"
	if doctorFix {
		path += "?fix=true"
	}

	resp, err := client.Post(path, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var report doctorReport
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if doctorJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	// Pretty-print
	for _, check := range report.Checks {
		icon := statusIcon(check.Status)
		line := fmt.Sprintf("%s  %-22s %s", icon, check.Name, check.Message)
		if check.Fixed > 0 {
			line += fmt.Sprintf("  [fixed %d]", check.Fixed)
		}
		fmt.Println(line)

		for _, item := range check.Items {
			fmt.Printf("     %s\n", item)
		}
	}

	fmt.Println()
	fmt.Println(report.Summary)

	// Exit code 1 if there are unfixed issues
	for _, check := range report.Checks {
		if check.Status != "OK" {
			os.Exit(1)
		}
	}

	return nil
}

func statusIcon(status string) string {
	switch strings.ToUpper(status) {
	case "OK":
		return "OK "
	case "WARNING":
		return "!! "
	case "ERROR":
		return "ERR"
	default:
		return "?  "
	}
}
