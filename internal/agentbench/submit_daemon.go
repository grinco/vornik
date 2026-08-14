package agentbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Daemon task submission (§10 step 7).
//
// Submits over the same companion-MCP surface membench's adapter uses, because
// that is the interface a benchmark is allowed to reach the daemon through: it
// carries a scoped token rather than an admin key, so a harness bug cannot
// become an administrative action.
//
// POLLS RATHER THAN STREAMS, deliberately. A benchmark run is long and mostly
// idle; a dropped stream mid-run would cost the whole arm, while a dropped poll
// costs one interval.

// DaemonConfig configures the submitter.
type DaemonConfig struct {
	BaseURL string
	Token   string
	Project string
	// PollInterval is how often a submitted task is checked. Defaults to 15s:
	// benchmark tasks run for minutes, and polling faster spends request budget
	// to learn nothing.
	PollInterval time.Duration
	// Timeout bounds a single task. A task that never terminates would
	// otherwise hold the whole arm open.
	Timeout time.Duration
	// HTTPClient is injected so tests can drive an httptest server.
	HTTPClient *http.Client
}

// DaemonTaskRunner submits benchmark tasks to a running daemon.
type DaemonTaskRunner struct {
	cfg DaemonConfig
}

// NewDaemonTaskRunner constructs the runner, applying defaults.
func NewDaemonTaskRunner(cfg DaemonConfig) *DaemonTaskRunner {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &DaemonTaskRunner{cfg: cfg}
}

// WriteTargetDatabase reports the database this daemon actually writes,
// satisfying membench.WriteTargetReporter.
//
// WHY THIS EXISTS AND WHY IT IS NOT OPTIONAL. CheckRunScope validates the
// database NAME an operator typed twice. It cannot prove the run will write that
// database, because this harness reaches a running daemon and the daemon writes
// to whatever it was configured with. On 2026-08-12 that gap put twelve fixture
// documents into a production corpus while the named throwaway was left with
// zero tables. An agent-benchmark run submits real tasks, so the same gap here
// would write benchmark work into whatever the daemon is pointed at.
func (d *DaemonTaskRunner) WriteTargetDatabase(ctx context.Context) (string, error) {
	var reply struct {
		Database string `json:"database"`
	}
	if err := d.call(ctx, "whoami", map[string]any{}, &reply); err != nil {
		return "", fmt.Errorf("whoami: %w", err)
	}
	return reply.Database, nil
}

// Run submits one task and waits for it to terminate.
func (d *DaemonTaskRunner) Run(ctx context.Context, spec TaskSpec) (TaskOutcome, error) {
	if d.cfg.BaseURL == "" || d.cfg.Token == "" {
		return TaskOutcome{}, fmt.Errorf("daemon runner needs a base URL and a token")
	}

	var submitted struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := d.call(ctx, "delegate", map[string]any{
		"workflow": spec.Workflow,
		"prompt":   spec.Prompt,
		"project":  d.cfg.Project,
	}, &submitted); err != nil {
		return TaskOutcome{}, fmt.Errorf("submit %q: %w", spec.ID, err)
	}
	if submitted.TaskID == "" {
		return TaskOutcome{}, fmt.Errorf("submit %q: daemon returned no task id", spec.ID)
	}

	deadline := time.Now().Add(d.cfg.Timeout)
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	for {
		outcome, done, err := d.poll(ctx, submitted.TaskID)
		if err != nil {
			return TaskOutcome{}, err
		}
		if done {
			return outcome, nil
		}
		if time.Now().After(deadline) {
			// Reported as an outcome rather than an error: the task DID run and
			// its trace is worth assembling, so the arm continues.
			return TaskOutcome{
				TaskID:    submitted.TaskID,
				Succeeded: false,
				ErrorText: fmt.Sprintf("timeout: %q did not terminate within %s",
					spec.ID, d.cfg.Timeout),
			}, nil
		}
		select {
		case <-ctx.Done():
			return TaskOutcome{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll reads a task's status once.
func (d *DaemonTaskRunner) poll(ctx context.Context, taskID string) (TaskOutcome, bool, error) {
	// NOTE: the companion status payload carries task_id / status / attempt /
	// workflow / project and NOTHING about executions. An earlier version read a
	// nonexistent execution_ids field, so every run assembled nothing and
	// reported zeroes. Executions are resolved from the ledger instead.
	var st struct {
		TaskID    string `json:"task_id"`
		Status    string `json:"status"`
		LastError string `json:"last_error"`
	}
	if err := d.call(ctx, "status", map[string]any{"task_id": taskID}, &st); err != nil {
		return TaskOutcome{}, false, fmt.Errorf("poll %s: %w", taskID, err)
	}

	switch strings.ToUpper(st.Status) {
	case "COMPLETED":
		return TaskOutcome{TaskID: taskID, Succeeded: true}, true, nil
	case "FAILED", "CANCELLED":
		errText := st.LastError
		if errText == "" {
			// A terminal failure with no recorded reason must not read as an
			// agent failure — ClassifyFailure files an empty reason as harness.
			errText = ""
		}
		return TaskOutcome{TaskID: taskID, Succeeded: false, ErrorText: errText}, true, nil
	default:
		return TaskOutcome{}, false, nil
	}
}

// call performs one companion-MCP tool call.
func (d *DaemonTaskRunner) call(ctx context.Context, tool string, args map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(d.cfg.BaseURL, "/") + "/api/v1/mcp/companion"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", tool, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("call %s: daemon returned HTTP %d", tool, resp.StatusCode)
	}

	var envelope struct {
		Error  *json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode %s response: %w", tool, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("call %s: daemon reported %s", tool, string(*envelope.Error))
	}
	if len(envelope.Result.Content) == 0 {
		return fmt.Errorf("call %s: daemon returned no content", tool)
	}
	if envelope.Result.IsError {
		return fmt.Errorf("call %s: %s", tool, envelope.Result.Content[0].Text)
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), out); err != nil {
		return fmt.Errorf("decode %s payload: %w", tool, err)
	}
	return nil
}
