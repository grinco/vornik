// Package chat — Codex CLI provider.
//
// Analogous to cli_client.go (Claude Code) but shells out to the
// OpenAI `codex` CLI. Separate parser because the event stream shape
// differs meaningfully:
//
//	claude stream-json → {"type":"assistant","message":{content:[…]}}
//	                     {"type":"result","usage":{…}}
//
//	codex exec --json  → {"type":"thread.started",…}
//	                     {"type":"turn.started"}
//	                     {"type":"item.completed","item":{"type":"agent_message","text":…}}
//	                     {"type":"turn.completed","usage":{…}}
//	                     {"type":"error","message":…} + {"type":"turn.failed",…}
//
// The shim tool-calling infrastructure (toolInstruction,
// renderToolsForPrompt, applyToolCallShim, parseToolCall, packMessages)
// is shared with the Claude CLI client — we encode tools the same way
// and parse tool-call JSON out of the agent_message text.
//
// Notes for operators:
//
//  1. ChatGPT-account auth ignores the --model flag entirely — the
//     server picks the model based on the subscription tier. Pass
//     model=="" in that case to avoid the CLI's 400 "model not
//     supported" error. API-key auth honors --model normally.
//
//  2. Codex emits exit 0 even when the underlying API call errors;
//     detection happens in the parser via the "turn.failed" event.
//
//  3. --skip-git-repo-check keeps the CLI from refusing to start
//     when vornik's cwd isn't a git repo (it usually is, but the
//     flag is free and defensive).
package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// CodexCLIClient is a Provider that shells out to the `codex` CLI.
// Construction is cheap; each request spawns a fresh subprocess.
type CodexCLIClient struct {
	binary    string
	model     string
	timeout   time.Duration
	extraArgs []string
	logger    zerolog.Logger
	metrics   *Metrics
	counter   uint64
	// modelCatalog is the catalog ListModels returns. Wired by the
	// container from pricing.yaml entries that match the OpenAI
	// naming convention (gpt-*, o<digit>-*). nil → ListModels
	// returns an empty list (Codex's auth surface has no /v1/models
	// endpoint to call instead).
	modelCatalog []ModelInfo
}

// CodexOption configures a CodexCLIClient.
type CodexOption func(*CodexCLIClient)

// WithCodexBinary overrides the codex binary path.
func WithCodexBinary(path string) CodexOption {
	return func(c *CodexCLIClient) { c.binary = path }
}

// WithCodexTimeout caps each subprocess invocation.
func WithCodexTimeout(d time.Duration) CodexOption {
	return func(c *CodexCLIClient) { c.timeout = d }
}

// WithCodexLogger sets the logger used for subprocess telemetry.
func WithCodexLogger(l zerolog.Logger) CodexOption {
	return func(c *CodexCLIClient) { c.logger = l }
}

// WithCodexModelCatalog sets the static catalog ListModels will
// return. The Codex CLI/ChatGPT-account surface has no public model-
// list endpoint, so the daemon supplies the catalog out-of-band —
// typically by filtering pricing.yaml entries.
func WithCodexModelCatalog(models []ModelInfo) CodexOption {
	return func(c *CodexCLIClient) { c.modelCatalog = models }
}

// NewCodexCLIClient constructs a CodexCLIClient. `model` may be empty;
// the codex CLI then uses whatever its config (~/.codex/config.toml)
// sets — important for ChatGPT-account auth where the CLI ignores
// per-request model selection.
func NewCodexCLIClient(model string, opts ...CodexOption) *CodexCLIClient {
	c := &CodexCLIClient{
		binary:  "codex",
		model:   model,
		timeout: DefaultTimeout,
		logger:  zerolog.Nop(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Model implements Provider.
func (c *CodexCLIClient) Model() string { return c.model }

// SetMetrics implements Provider.
func (c *CodexCLIClient) SetMetrics(m *Metrics) {
	c.metrics = m
	if m != nil && c.model != "" {
		m.RequestsTotal.WithLabelValues(c.model, "error")
		m.ErrorsTotal.WithLabelValues(c.model, "error")
	}
}

// ListModels implements ModelLister. The codex-cli subprocess wraps
// the ChatGPT-account auth surface, which exposes no public model-
// list endpoint, so the daemon supplies the catalog at construction
// time via WithCodexModelCatalog (typically derived from pricing.yaml).
func (c *CodexCLIClient) ListModels(_ context.Context) ([]ModelInfo, error) {
	if len(c.modelCatalog) == 0 {
		return nil, nil
	}
	out := make([]ModelInfo, len(c.modelCatalog))
	copy(out, c.modelCatalog)
	return out, nil
}

// WithModel implements ModelOverridable.
func (c *CodexCLIClient) WithModel(model string) Provider {
	if c == nil {
		return c
	}
	clone := *c
	clone.model = model
	return &clone
}

// Complete implements Provider.
func (c *CodexCLIClient) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return c.invoke(ctx, messages, nil, nil)
}

// CompleteWithTools implements Provider. Uses the same prompt shim as
// the Claude CLI client — tool definitions are injected into the
// system prompt and the response text is scanned for a tool_call
// envelope.
func (c *CodexCLIClient) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return c.invoke(ctx, messages, tools, nil)
}

// CompleteWithToolsStream implements Provider. Text deltas are emitted
// as codex streams its agent_message; tool-call detection runs once
// the stream completes.
func (c *CodexCLIClient) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return c.invoke(ctx, messages, tools, onText)
}

// ---------------------------------------------------------------------

func (c *CodexCLIClient) invoke(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	id := atomic.AddUint64(&c.counter, 1)
	start := time.Now()

	req, err := c.packMessages(messages, tools)
	if err != nil {
		c.recordMetrics(start, "error")
		return nil, err
	}
	req.onText = onText

	resp, rerr := c.runCodex(ctx, id, req)
	if rerr != nil {
		c.recordMetrics(start, classifyCLIError(rerr))
		return nil, rerr
	}
	if tools != nil {
		if err := applyToolCallShim(resp); err != nil {
			c.logger.Warn().Err(err).Uint64("call_id", id).
				Msg("codex: malformed tool call JSON — treating as plain text")
		}
	}
	c.recordMetrics(start, "success")
	return resp, nil
}

// packMessages collapses the OpenAI-style message list into a single
// prompt string for codex. Codex takes the user turn via stdin OR the
// PROMPT positional arg; we use stdin to dodge argv size limits.
//
// System prompt goes through `-c instructions="…"` which codex treats
// as a prepended instruction block. Unlike Claude's
// --append-system-prompt, codex doesn't have a dedicated flag for a
// per-invocation system prompt, so we inline it into the user turn
// too — labeled so the model treats it as meta-instructions.
func (c *CodexCLIClient) packMessages(messages []Message, tools []Tool) (*codexRequest, error) {
	if len(messages) == 0 {
		return nil, ErrEmptyMessages
	}

	var systemParts []string
	var lastUser string
	transcript := &strings.Builder{}

	for _, m := range messages {
		switch strings.ToLower(m.Role) {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user":
			if lastUser != "" {
				fmt.Fprintf(transcript, "USER: %s\n\n", lastUser)
			}
			lastUser = m.Content
		case "assistant":
			content := m.Content
			if len(m.ToolCalls) > 0 {
				content += "\n" + renderToolCallsForHistory(m.ToolCalls)
			}
			if content != "" {
				fmt.Fprintf(transcript, "ASSISTANT: %s\n\n", content)
			}
		case "tool":
			fmt.Fprintf(transcript, "TOOL[%s]: %s\n\n", fallback(m.Name, m.ToolCallID), m.Content)
		}
	}

	sys := strings.Join(systemParts, "\n\n")
	if len(tools) > 0 {
		sys += "\n\n" + toolInstruction + renderToolsForPrompt(tools)
	}
	sys = strings.TrimSpace(sys)

	// Build the stdin payload: system prompt (labeled), prior
	// transcript, then the most recent user turn. Codex doesn't
	// separate roles on its input so we rely on labels + the model's
	// comprehension.
	var buf strings.Builder
	if sys != "" {
		buf.WriteString("SYSTEM INSTRUCTIONS:\n")
		buf.WriteString(sys)
		buf.WriteString("\n\n")
	}
	if transcript.Len() > 0 {
		buf.WriteString("PRIOR CONVERSATION:\n")
		buf.WriteString(transcript.String())
		buf.WriteString("CURRENT USER MESSAGE:\n")
	}
	buf.WriteString(lastUser)

	return &codexRequest{
		prompt: buf.String(),
		tools:  tools,
	}, nil
}

type codexRequest struct {
	prompt string
	tools  []Tool
	onText StreamCallback
}

func (c *CodexCLIClient) runCodex(ctx context.Context, id uint64, req *codexRequest) (*ChatResponse, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	args := []string{
		"exec",
		"--json",                 // line-delimited JSONL events
		"--skip-git-repo-check",  // don't refuse when cwd isn't a repo
		"--sandbox", "read-only", // minimise side effects; codex shouldn't run
		// shell commands for our use case anyway, but
		// belt-and-braces against a runaway agent
	}
	if c.model != "" {
		// `-m` fails under ChatGPT-account auth with any explicit
		// value. Pass only when explicitly configured — operators on
		// ChatGPT leave it empty and configure model via
		// ~/.codex/config.toml.
		args = append(args, "-m", c.model)
	}
	args = append(args, c.extraArgs...)
	// Prompt is passed via stdin (not argv) to dodge size limits and
	// shell-quoting issues on long multi-turn conversations.

	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Stdin = strings.NewReader(req.prompt)
	// Prepend the codex binary's directory to PATH so its node
	// dependency is discoverable. systemd user services inherit a
	// minimal PATH that doesn't include /home/linuxbrew/.linuxbrew/bin
	// or wherever the operator's node + codex live, so `codex` itself
	// would fail with `env: 'node': No such file or directory` before
	// it ever reached the API. Only rewrite when we were given an
	// absolute binary path — if it's a bare name, PATH is already
	// the resolver and we leave things alone.
	if filepath.IsAbs(c.binary) {
		binDir := filepath.Dir(c.binary)
		env := os.Environ()
		var rewrote bool
		for i, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				existing := strings.TrimPrefix(kv, "PATH=")
				if !strings.Contains(existing, binDir) {
					env[i] = "PATH=" + binDir + string(os.PathListSeparator) + existing
				}
				rewrote = true
				break
			}
		}
		if !rewrote {
			env = append(env, "PATH="+binDir)
		}
		cmd.Env = env
	}

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}

	c.logger.Debug().
		Uint64("call_id", id).
		Str("binary", c.binary).
		Str("model", c.model).
		Int("tool_count", len(req.tools)).
		Int("prompt_bytes", len(req.prompt)).
		Msg("codex: invoking")

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: start %s: %w", c.binary, err)
	}

	resp, parseErr := parseCodexStream(stdout, req.onText)
	_ = stdout.Close()
	waitErr := cmd.Wait()

	if parseErr != nil {
		return nil, fmt.Errorf("codex: parse stdout: %w (stderr: %s)", parseErr, truncate(stderr.String(), 500))
	}
	if waitErr != nil {
		return nil, fmt.Errorf("codex: %s exited: %w (stderr: %s)", c.binary, waitErr, truncate(stderr.String(), 500))
	}

	if resp.Model == "" {
		resp.Model = c.model
	}
	return resp, nil
}

func (c *CodexCLIClient) recordMetrics(start time.Time, status string) {
	if c.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	c.metrics.RequestsTotal.WithLabelValues(c.model, status).Inc()
	c.metrics.RequestDuration.WithLabelValues(c.model).Observe(duration)
	if status != "success" {
		c.metrics.ErrorsTotal.WithLabelValues(c.model, status).Inc()
	}
}

// ---------------------------------------------------------------------
// codex event stream parsing
// ---------------------------------------------------------------------

type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id,omitempty"`
	Item     *codexEventItem `json:"item,omitempty"`
	Usage    *codexUsage     `json:"usage,omitempty"`
	Message  string          `json:"message,omitempty"` // carries stringified error JSON for type=error
	Error    json.RawMessage `json:"error,omitempty"`   // object for turn.failed
}

type codexEventItem struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens,omitempty"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
}

// parseCodexStream reads JSONL events from codex and assembles a
// ChatResponse. Returns a non-nil error when the stream reports a
// turn.failed/error event; the caller surfaces that to its own callers.
func parseCodexStream(r io.Reader, onText StreamCallback) (*ChatResponse, error) {
	var (
		accumulated strings.Builder
		finalResp   = &ChatResponse{}
		haveText    bool
		turnError   string
	)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var evt codexEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "thread.started":
			if evt.ThreadID != "" && finalResp.ID == "" {
				finalResp.ID = evt.ThreadID
			}
		case "item.completed":
			if evt.Item == nil || evt.Item.Type != "agent_message" {
				// Non-agent items (tool_call metadata, etc.) — ignore.
				continue
			}
			if evt.Item.Text != "" {
				accumulated.WriteString(evt.Item.Text)
				haveText = true
				if onText != nil {
					onText(accumulated.String())
				}
			}
		case "turn.completed":
			if u := evt.Usage; u != nil {
				finalResp.Usage.PromptTokens = u.InputTokens
				finalResp.Usage.CompletionTokens = u.OutputTokens
			}
		case "error":
			if evt.Message != "" {
				turnError = evt.Message
			}
		case "turn.failed":
			if len(evt.Error) > 0 {
				turnError = string(evt.Error)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("codex: scan stdout: %w", err)
	}

	if turnError != "" {
		return nil, fmt.Errorf("codex: %s", truncate(turnError, 400))
	}

	finalResp.Usage.TotalTokens = finalResp.Usage.PromptTokens + finalResp.Usage.CompletionTokens

	if !haveText {
		return nil, fmt.Errorf("codex: no agent_message in stream output")
	}
	finalResp.Choices = []struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{
		{
			Index:        0,
			Message:      Message{Role: "assistant", Content: accumulated.String()},
			FinishReason: "stop",
		},
	}
	return finalResp, nil
}

// Compile-time conformance checks.
var _ Provider = (*CodexCLIClient)(nil)
var _ ModelOverridable = (*CodexCLIClient)(nil)
