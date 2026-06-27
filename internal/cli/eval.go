package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type evalCase struct {
	Name       string `json:"name"`
	Prompt     string `json:"prompt"`
	WorkflowID string `json:"workflow_id,omitempty"`
	// Expect declares the predicate the case is judged against.
	// Empty Expect degrades to "task reached COMPLETED" — useful
	// for smoke tests where any successful run is a pass.
	Expect EvalExpectation `json:"expect,omitempty"`
}

type evalSuite struct {
	ProjectID  string     `json:"project_id"`
	WorkflowID string     `json:"workflow_id,omitempty"`
	Cases      []evalCase `json:"cases"`
}

// evalRunSummary is what gets persisted between runs so the next
// invocation can flag regressions. Stored as JSON in the per-user
// state dir; a fresh deployment with no prior history simply has
// no compare to do.
type evalRunSummary struct {
	Swarm     string                  `json:"swarm"`
	Project   string                  `json:"project"`
	Timestamp time.Time               `json:"timestamp"`
	Cases     map[string]evalCaseLast `json:"cases"`
}

type evalCaseLast struct {
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

type evalCreateResponse struct {
	TaskID string `json:"taskId"`
	Status string `json:"status"`
}

type evalTaskResponse struct {
	TaskID            string `json:"taskId"`
	Status            string `json:"status"`
	LastError         string `json:"lastError,omitempty"`
	ActiveExecutionID string `json:"activeExecutionId,omitempty"`
}

type evalExecutionResponse struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
}

var (
	evalProject string
	evalFile    string
	evalWait    bool
	evalTimeout time.Duration
	evalJSON    bool
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Run repeatable swarm evaluation tasks",
}

var evalRunCmd = &cobra.Command{
	Use:   "run <swarm>",
	Short: "Submit a repeatable eval suite and print a scoreboard",
	Args:  cobra.ExactArgs(1),
	RunE:  runEval,
}

func init() {
	evalRunCmd.Flags().StringVarP(&evalProject, "project", "p", "", "Project ID to submit eval tasks to")
	evalRunCmd.Flags().StringVar(&evalFile, "file", "", "Eval suite JSON file (default: configs/evals/<swarm>.json)")
	evalRunCmd.Flags().BoolVar(&evalWait, "wait", true, "Poll tasks until terminal status")
	evalRunCmd.Flags().DurationVar(&evalTimeout, "timeout", 30*time.Minute, "Maximum time to wait for all eval tasks")
	evalRunCmd.Flags().BoolVar(&evalJSON, "json", false, "Print scoreboard as JSON")
	evalCmd.AddCommand(evalRunCmd)
	rootCmd.AddCommand(evalCmd)
}

func runEval(cmd *cobra.Command, args []string) error {
	swarmID := args[0]
	suite, err := loadEvalSuite(swarmID, evalFile)
	if err != nil {
		return err
	}
	if evalProject != "" {
		suite.ProjectID = evalProject
	}
	if suite.ProjectID == "" {
		return fmt.Errorf("project_id is required (set --project or suite.project_id)")
	}
	if len(suite.Cases) == 0 {
		return fmt.Errorf("eval suite has no cases")
	}

	client := ClientFromEnv()
	type row struct {
		Name        string `json:"name"`
		TaskID      string `json:"task_id"`
		ExecutionID string `json:"execution_id,omitempty"`
		Status      string `json:"status"`
		Error       string `json:"error,omitempty"`
		Elapsed     string `json:"elapsed,omitempty"`
		// Passed is the predicate verdict — distinct from
		// Status (which is the task's terminal state). A task
		// can land in COMPLETED but still fail the predicate
		// (e.g. expected outcome=FAILED for a negative test).
		Passed bool   `json:"passed"`
		Reason string `json:"reason,omitempty"`
		// expect carries the case's predicate through to the
		// verdict step so the polling loop doesn't need a
		// separate map. Not serialised — the corpus is the
		// source of truth.
		expect EvalExpectation
	}
	scoreboard := make([]row, 0, len(suite.Cases))
	started := time.Now()

	for _, tc := range suite.Cases {
		workflowID := tc.WorkflowID
		if workflowID == "" {
			workflowID = suite.WorkflowID
		}
		body := map[string]any{
			"taskType":       tc.Prompt,
			"idempotencyKey": "eval:" + swarmID + ":" + sanitizeEvalName(tc.Name),
			"inputArtifacts": []map[string]string{
				{"name": "eval_swarm", "content": swarmID},
				{"name": "eval_case", "content": tc.Name},
			},
		}
		if workflowID != "" {
			body["workflowId"] = workflowID
		}
		resp, err := client.Post(fmt.Sprintf("/api/v1/projects/%s/tasks", suite.ProjectID), body)
		if err != nil {
			return fmt.Errorf("submit %q: %w", tc.Name, err)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("submit %q: %w", tc.Name, ParseAPIError(resp))
		}
		var created evalCreateResponse
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("submit %q: decode response: %w", tc.Name, err)
		}
		_ = resp.Body.Close()
		scoreboard = append(scoreboard, row{
			Name:   tc.Name,
			TaskID: created.TaskID,
			Status: created.Status,
			expect: tc.Expect,
		})
	}

	if evalWait {
		deadline := time.Now().Add(evalTimeout)
		for {
			allTerminal := true
			for i := range scoreboard {
				if isTerminalTaskStatus(scoreboard[i].Status) {
					continue
				}
				status, err := fetchEvalTask(client, suite.ProjectID, scoreboard[i].TaskID)
				if err != nil {
					return err
				}
				scoreboard[i].Status = status.Status
				scoreboard[i].Error = status.LastError
				if status.ActiveExecutionID != "" {
					scoreboard[i].ExecutionID = status.ActiveExecutionID
				}
				allTerminal = allTerminal && isTerminalTaskStatus(status.Status)
			}
			if allTerminal {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("eval timed out after %s", evalTimeout)
			}
			time.Sleep(2 * time.Second)
		}
	}
	// Apply each case's predicate now that every task has reached
	// terminal status (or the wait window expired). Cases that
	// timed out get Passed=false with a clear reason. Predicates
	// that need result.json fetch the execution row; predicates
	// that only check Outcome skip the fetch (saves an HTTP round
	// trip per case).
	for i := range scoreboard {
		row := &scoreboard[i]
		evidence := EvalEvidence{TerminalStatus: row.Status}
		if expectationNeedsResult(row.expect) && row.ExecutionID != "" {
			result, err := fetchEvalExecutionResult(client, row.ExecutionID)
			if err != nil {
				row.Passed = false
				row.Reason = "fetch execution result: " + err.Error()
				row.Elapsed = time.Since(started).Truncate(time.Second).String()
				continue
			}
			evidence.Result = result
		}
		verdict := EvaluateExpectation(row.expect, evidence)
		row.Passed = verdict.Passed
		row.Reason = verdict.Reason
		row.Elapsed = time.Since(started).Truncate(time.Second).String()
	}

	// Build a per-case verdict map for persistence + regression
	// compare. Decoupled from the scoreboard's local row type so
	// the helpers stay top-level testable.
	currentSummary := evalRunSummary{
		Swarm:     swarmID,
		Project:   suite.ProjectID,
		Timestamp: time.Now(),
		Cases:     make(map[string]evalCaseLast, len(scoreboard)),
	}
	for _, r := range scoreboard {
		currentSummary.Cases[r.Name] = evalCaseLast{Passed: r.Passed, Reason: r.Reason}
	}
	prev := loadEvalLastRun(swarmID)
	regressed, recovered := diffEvalRuns(prev, currentSummary)
	_ = saveEvalLastRun(swarmID, currentSummary)

	if evalJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"swarm":     swarmID,
			"project":   suite.ProjectID,
			"cases":     scoreboard,
			"regressed": regressed,
			"recovered": recovered,
		})
	}

	passed := 0
	for _, r := range scoreboard {
		if r.Passed {
			passed++
		}
	}
	fmt.Printf("Eval %s on project %s: %d/%d passed\n", swarmID, suite.ProjectID, passed, len(scoreboard))
	for _, r := range scoreboard {
		marker := "✓"
		if !r.Passed {
			marker = "✗"
		}
		line := fmt.Sprintf("  %s %s [%s] (%s)", marker, r.Name, r.Status, r.TaskID)
		if r.Reason != "" {
			line += " — " + r.Reason
		} else if r.Error != "" {
			line += " — " + r.Error
		}
		fmt.Println(line)
	}
	if len(regressed) > 0 {
		fmt.Printf("\nRegressions vs. last run (%d):\n", len(regressed))
		for _, name := range regressed {
			fmt.Println("  - " + name)
		}
	}
	if len(recovered) > 0 {
		fmt.Printf("\nRecovered since last run (%d):\n", len(recovered))
		for _, name := range recovered {
			fmt.Println("  + " + name)
		}
	}
	if passed < len(scoreboard) {
		// Non-zero exit so CI / scripts can branch on eval
		// failure without parsing the output.
		os.Exit(1)
	}
	return nil
}

func loadEvalSuite(swarmID, path string) (*evalSuite, error) {
	if path == "" {
		path = "configs/evals/" + swarmID + ".json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read eval suite %s: %w", path, err)
	}
	var suite evalSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		return nil, fmt.Errorf("parse eval suite %s: %w", path, err)
	}
	return &suite, nil
}

func fetchEvalTask(client *Client, projectID, taskID string) (*evalTaskResponse, error) {
	resp, err := client.Get(fmt.Sprintf("/api/v1/projects/%s/tasks/%s", projectID, taskID))
	if err != nil {
		return nil, fmt.Errorf("fetch task %s: %w", taskID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out evalTaskResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func isTerminalTaskStatus(status string) bool {
	switch status {
	case "COMPLETED", "FAILED", "CANCELLED":
		return true
	default:
		return false
	}
}

// expectationNeedsResult reports whether evaluating the predicate
// requires fetching the execution's result.json. Pure-Outcome
// predicates can short-circuit the fetch — saves one HTTP call per
// case across a 50-case corpus.
func expectationNeedsResult(e EvalExpectation) bool {
	return len(e.Equals) > 0 || len(e.Contains) > 0 || e.Regex != ""
}

// fetchEvalExecutionResult pulls the result.json blob for a
// terminated execution. Returns empty (not an error) when the
// execution is found but has no result yet — the predicate then
// runs against an empty body and decides for itself whether that
// counts as a fail.
func fetchEvalExecutionResult(client *Client, executionID string) (json.RawMessage, error) {
	resp, err := client.Get("/api/v1/executions/" + executionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var exec evalExecutionResponse
	if err := json.Unmarshal(body, &exec); err != nil {
		return nil, fmt.Errorf("decode execution: %w", err)
	}
	return exec.Result, nil
}

// evalLastRunPath returns the file path where the per-swarm last-run
// summary is stored. $XDG_STATE_HOME/vornik/evals/<swarm>.json with
// $HOME/.local/state as the fallback when XDG isn't set. Failure
// to determine a path is not fatal — saveEvalLastRun degrades to
// "no history" silently, same as a fresh deployment.
func evalLastRunPath(swarmID string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = home + "/.local/state"
	}
	return base + "/vornik/evals/" + sanitizeEvalName(swarmID) + ".json"
}

func loadEvalLastRun(swarmID string) *evalRunSummary {
	path := evalLastRunPath(swarmID)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s evalRunSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	return &s
}

func saveEvalLastRun(swarmID string, current evalRunSummary) error {
	path := evalLastRunPath(swarmID)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepathDir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// filepathDir is filepath.Dir without the filepath import; eval.go
// avoids a new import for one call.
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// diffEvalRuns compares the current run's verdicts against a prior
// summary and returns the case names that regressed (passed →
// failed) and recovered (failed → passed). Cases unique to either
// side are NOT counted as regressions/recoveries — adding or
// removing a case is a corpus change, not a quality signal.
func diffEvalRuns(prev *evalRunSummary, current evalRunSummary) (regressed, recovered []string) {
	if prev == nil {
		return nil, nil
	}
	for name, now := range current.Cases {
		before, ok := prev.Cases[name]
		if !ok {
			continue
		}
		switch {
		case before.Passed && !now.Passed:
			regressed = append(regressed, name)
		case !before.Passed && now.Passed:
			recovered = append(recovered, name)
		}
	}
	// Stable order in the output regardless of map iteration.
	sortStrings(regressed)
	sortStrings(recovered)
	return regressed, recovered
}

// sortStrings is sort.Strings without the sort import; one-line
// shell sort keeps the file's import set minimal.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func sanitizeEvalName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "case"
	}
	return b.String()
}
