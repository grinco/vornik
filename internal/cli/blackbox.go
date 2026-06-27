package cli

// Black Box CLI — operator surface for the Autonomy Black Box
// arc (https://docs.vornik.io).
//
//   vornikctl blackbox trace <task_id>                    [--json]   — Phase A
//   vornikctl blackbox replay <task_id> --variable V --value X       — Phase C
//                                       --label L  [--role ROLE]    [--json]
//   vornikctl blackbox scorecard <a> <b>                  [--json]   — Phase C
//   vornikctl blackbox sideeffects                        [--json]   — Phase C
//
// All four require an admin-scoped API key (gated server-side by
// the same admin middleware as /admin/audit). Without admin
// scope the daemon returns 403 — translated here to a clear
// operator error.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	blackboxCmd = &cobra.Command{
		Use:   "blackbox",
		Short: "Autonomy Black Box — unified task traces, counterfactual replay, scorecard",
		Long: `Operator surface for the Autonomy Black Box arc.

Loads liability-grade unified traces (Phase A) and runs counterfactual
replays of any task with a single variable changed (Phase C: model
or prompt; budget / policy / tool_result / memory_chunk_excluded
are deferred to v2).

Requires an admin-scoped API key. The same gate matrix as
/admin/audit applies.`,
	}

	blackboxTraceCmd = &cobra.Command{
		Use:   "trace <task_id>",
		Short: "Fetch the unified trace for one task",
		Args:  cobra.ExactArgs(1),
		RunE:  runBlackBoxTrace,
	}

	blackboxReplayCmd = &cobra.Command{
		Use:   "replay <task_id>",
		Short: "Run a counterfactual replay with one variable changed",
		Long: `Submit a counterfactual replay of the named task. Exactly ONE variable
must be specified — the engine refuses multi-variate replays because the
output is too hard to reason about.

Supported variables:
  model              — swap the chat-router target. Optional --role
                       narrows the swap to one role; omitted = router-level.
  prompt             — override the system/user prompt for a role. --role REQUIRED.
  tool_result        — inject a synthetic response for a tool (JSON {tool: stub}).
  budget             — lower-only caps on max-iters / step-timeout / max-tokens.
  memory_chunk_excluded — drop specific memory chunks from recall.
  workflow           — route the replay at an alternate workflow genome
                       (used by the self-healing trial runner).

Deferred:
  policy (needs a policy primitive — returns ErrVariableNotYetImplemented).

The replay creates a NEW task that runs to completion under normal
scheduler dispatch. Tag the new run with --label for the compare view.
DENY-BY-DEFAULT: only replay-safe (allow-listed) tools the original
trace called run live during replay; every other tool is STUBBED (a
counterfactual cannot fire broker orders / messages / file writes). See
'vornikctl blackbox sideeffects' for the replay-safe allow-list.`,
		Args: cobra.ExactArgs(1),
		RunE: runBlackBoxReplay,
	}

	blackboxScorecardCmd = &cobra.Command{
		Use:   "scorecard <task_a> <task_b>",
		Short: "Compare two traces, print the diff",
		Long: `Assemble the two named traces and print a human-readable diff
covering status change, cost delta, latency delta, count deltas,
and per-step tool-call divergence. Symmetric: order of a and b
does not affect findings, only the sign of the deltas.`,
		Args: cobra.ExactArgs(2),
		RunE: runBlackBoxScorecard,
	}

	blackboxSideEffectsCmd = &cobra.Command{
		Use:   "sideeffects",
		Short: "Show the active replay-safe allow-list (deny-by-default)",
		Long: `Print the replay-safe allow-list the counterfactual gate is using.
DENY-BY-DEFAULT: only tools on this list run live during a
counterfactual replay; every other tool is short-circuited with a
synthesized 'skipped' (not_replay_safe) response. Tune the list via
the blackbox.replay_safe_tools config key + 'vornikctl daemon reload'.`,
		RunE: runBlackBoxSideEffects,
	}

	blackboxTraceJSON      bool
	blackboxReplayVariable string
	blackboxReplayValue    string
	blackboxReplayRole     string
	blackboxReplayLabel    string
	blackboxReplayJSON     bool
	blackboxScorecardJSON  bool
	blackboxSideJSON       bool
)

func init() {
	blackboxTraceCmd.Flags().BoolVar(&blackboxTraceJSON, "json", false, "Output raw JSON instead of human-readable summary")

	blackboxReplayCmd.Flags().StringVar(&blackboxReplayVariable, "variable", "", "Variable to mutate (model|prompt); v2 adds budget|policy|tool_result|memory_chunk_excluded")
	blackboxReplayCmd.Flags().StringVar(&blackboxReplayValue, "value", "", "New value for the variable (e.g. model ID for --variable model)")
	blackboxReplayCmd.Flags().StringVar(&blackboxReplayRole, "role", "", "Workflow role the variable targets; required for --variable prompt, optional for --variable model")
	blackboxReplayCmd.Flags().StringVar(&blackboxReplayLabel, "label", "", "Operator-readable label recorded on the new task (required)")
	blackboxReplayCmd.Flags().BoolVar(&blackboxReplayJSON, "json", false, "Output JSON instead of human-readable summary")

	blackboxScorecardCmd.Flags().BoolVar(&blackboxScorecardJSON, "json", false, "Output JSON instead of human-readable summary")
	blackboxSideEffectsCmd.Flags().BoolVar(&blackboxSideJSON, "json", false, "Output JSON instead of human-readable summary")

	blackboxCmd.AddCommand(blackboxTraceCmd)
	blackboxCmd.AddCommand(blackboxReplayCmd)
	blackboxCmd.AddCommand(blackboxScorecardCmd)
	blackboxCmd.AddCommand(blackboxSideEffectsCmd)
	rootCmd.AddCommand(blackboxCmd)
}

// blackboxReplayResp mirrors api.replayResponse. Kept separate so
// a wire-shape change is a deliberate code edit, not a silent
// drift.
type blackboxReplayResp struct {
	TaskID                 string `json:"task_id"`
	OriginalTaskID         string `json:"original_task_id"`
	Variable               string `json:"variable"`
	Label                  string `json:"label"`
	StampWarning           string `json:"stamp_warning,omitempty"`
	SideEffectingToolsHint string `json:"side_effecting_tools_hint,omitempty"`
}

// blackboxScorecardResp mirrors blackbox.Scorecard for CLI decode.
// Only the human-readable fields are scanned here; if the wire
// adds more, json.Decoder ignores them.
type blackboxScorecardResp struct {
	Trace1Header struct {
		TaskID       string  `json:"task_id"`
		Status       string  `json:"status"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"trace1"`
	Trace2Header struct {
		TaskID       string  `json:"task_id"`
		Status       string  `json:"status"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"trace2"`
	StatusChanged  bool     `json:"status_changed"`
	CostDeltaUSD   float64  `json:"cost_delta_usd"`
	CostDeltaPct   float64  `json:"cost_delta_pct"`
	StepCountDelta int      `json:"step_count_delta"`
	LLMCallDelta   int      `json:"llm_call_delta"`
	ToolCallDelta  int      `json:"tool_call_delta"`
	Findings       []string `json:"findings"`
}

func runBlackBoxTrace(_ *cobra.Command, args []string) error {
	taskID := args[0]
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/admin/blackbox/traces/" + url.PathEscape(taskID))
	if err != nil {
		return fmt.Errorf("blackbox trace: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if blackboxTraceJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	// Decode into a flexible map — the trace JSON has many
	// fields the CLI doesn't render in human mode (Events list
	// is the assembler's output; UI is the canonical reader).
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return fmt.Errorf("blackbox trace: decode: %w", err)
	}
	header, _ := m["header"].(map[string]any)
	counts, _ := m["counts"].(map[string]any)
	fmt.Printf("Task:        %v\n", header["task_id"])
	fmt.Printf("Project:     %v\n", header["project_id"])
	fmt.Printf("Workflow:    %v\n", header["workflow_id"])
	fmt.Printf("Status:      %v\n", header["status"])
	fmt.Printf("Cost (USD):  $%.4f\n", floatFromMap(header, "total_cost_usd"))
	fmt.Printf("Digest:      %v\n", header["trace_digest"])
	fmt.Println()
	fmt.Printf("Events: %d (LLM=%d Tool=%d Memory=%d Steps=%d Judge=%d)\n",
		intFromMap(counts, "events"),
		intFromMap(counts, "llm_calls"),
		intFromMap(counts, "tool_calls"),
		intFromMap(counts, "memory_reads"),
		intFromMap(counts, "steps"),
		intFromMap(counts, "judge_verdicts"),
	)
	fmt.Println()
	fmt.Println("Use --json for the full event stream.")
	return nil
}

func runBlackBoxReplay(_ *cobra.Command, args []string) error {
	taskID := args[0]
	if strings.TrimSpace(blackboxReplayVariable) == "" {
		return fmt.Errorf("blackbox replay: --variable required")
	}
	if strings.TrimSpace(blackboxReplayValue) == "" {
		return fmt.Errorf("blackbox replay: --value required")
	}
	if strings.TrimSpace(blackboxReplayLabel) == "" {
		return fmt.Errorf("blackbox replay: --label required (operator-readable description)")
	}

	body := map[string]string{
		"original_task_id": taskID,
		"variable":         blackboxReplayVariable,
		"value":            blackboxReplayValue,
		"label":            blackboxReplayLabel,
	}
	if blackboxReplayRole != "" {
		body["role"] = blackboxReplayRole
	}

	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/admin/blackbox/replay", body)
	if err != nil {
		return fmt.Errorf("blackbox replay: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 201 {
		return ParseAPIError(resp)
	}
	var out blackboxReplayResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("blackbox replay: decode: %w", err)
	}
	if blackboxReplayJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("Counterfactual created.\n\n")
	fmt.Printf("  New task ID:      %s\n", out.TaskID)
	fmt.Printf("  Original task:    %s\n", out.OriginalTaskID)
	fmt.Printf("  Variable changed: %s\n", out.Variable)
	fmt.Printf("  Label:            %s\n", out.Label)
	if out.StampWarning != "" {
		fmt.Printf("\n  Warning: %s\n", out.StampWarning)
	}
	if out.SideEffectingToolsHint != "" {
		fmt.Printf("\n  Note: %s\n", out.SideEffectingToolsHint)
	}
	fmt.Println()
	fmt.Printf("Poll status: vornikctl task status %s\n", out.TaskID)
	fmt.Printf("Compare:     vornikctl blackbox scorecard %s %s\n",
		out.OriginalTaskID, out.TaskID)
	return nil
}

func runBlackBoxScorecard(_ *cobra.Command, args []string) error {
	a, b := args[0], args[1]
	client := ClientFromEnv()
	path := "/api/v1/admin/blackbox/scorecard/" + url.PathEscape(a) + "/" + url.PathEscape(b)
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("blackbox scorecard: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if blackboxScorecardJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var sc blackboxScorecardResp
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return fmt.Errorf("blackbox scorecard: decode: %w", err)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "Trace 1\tTrace 2\tDelta\n")
	_, _ = fmt.Fprintf(tw, "-------\t-------\t-----\n")
	_, _ = fmt.Fprintf(tw, "%s\t%s\t\n", sc.Trace1Header.TaskID, sc.Trace2Header.TaskID)
	_, _ = fmt.Fprintf(tw, "status=%s\tstatus=%s\t%s\n", sc.Trace1Header.Status, sc.Trace2Header.Status, statusFlag(sc.StatusChanged))
	_, _ = fmt.Fprintf(tw, "$%.4f\t$%.4f\t%+.4f (%.1f%%)\n",
		sc.Trace1Header.TotalCostUSD, sc.Trace2Header.TotalCostUSD,
		sc.CostDeltaUSD, sc.CostDeltaPct)
	_ = tw.Flush()

	fmt.Println()
	fmt.Println("Findings:")
	if len(sc.Findings) == 0 {
		fmt.Println("  (none)")
	}
	for _, f := range sc.Findings {
		fmt.Printf("  - %s\n", f)
	}
	return nil
}

func runBlackBoxSideEffects(_ *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/admin/blackbox/sideeffects")
	if err != nil {
		return fmt.Errorf("blackbox sideeffects: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if blackboxSideJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return fmt.Errorf("blackbox sideeffects: decode: %w", err)
	}
	tools, _ := m["replay_safe_tools"].([]any)
	fmt.Printf("Replay-safe allow-list (%d tools, policy=%v, enforcement=%s):\n",
		len(tools), m["policy"], m["enforcement"])
	for _, t := range tools {
		fmt.Printf("  - %v\n", t)
	}
	if note, ok := m["note"].(string); ok {
		fmt.Printf("\nNote: %s\n", note)
	}
	return nil
}

func statusFlag(changed bool) string {
	if changed {
		return "CHANGED"
	}
	return "same"
}

func intFromMap(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func floatFromMap(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}
