// Package chat — Claude CLI provider.
//
// This file implements a Provider that shells out to the `claude` CLI
// (Claude Code) and reuses its authentication. It exists for operators
// who already run `claude` on the host and don't want to provision a
// separate Anthropic API key for vornik.
//
// Architecture:
//
//	vornik (CLIClient)  --exec-->  claude -p "..." --output-format stream-json
//	                    <--stdout--  { events, one JSON object per line }
//
// Limitations — read before enabling this provider:
//
//  1. Tool calling is implemented via a PROMPT-ENGINEERING SHIM, not
//     native tool_use blocks. Tool definitions are appended to the
//     system prompt with an instruction to reply in a specific JSON
//     shape when the model wants to call a tool. This works well with
//     Claude models but is less reliable than native OpenAI-style tool
//     calls. If a call is malformed, the dispatcher sees it as a plain
//     text response and proceeds accordingly.
//
//  2. Claude Code's own built-in tools (Read/Write/Edit/Bash/…) are
//     explicitly DISABLED via `--disallowedTools` so the model doesn't
//     silently short-circuit its reasoning with filesystem actions.
//     Users see only vornik's tool vocabulary.
//
//  3. Streaming is at the text-chunk granularity Claude Code emits in
//     its stream-json format; per-token streaming is not available.
//
//  4. Subprocess invocation has non-trivial startup cost (~200ms).
//     Fine for the dispatcher loop (a few calls per turn); unsuitable
//     for high-QPS embedding or classification workloads.
//
// For production multi-tenant or high-throughput workloads, prefer the
// HTTP client against a real Anthropic API key.
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
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// CLIClient is a Provider implementation that shells out to the
// `claude` CLI binary. Construction is cheap; each request spawns a
// fresh subprocess so there's no connection-pool state to manage.
type CLIClient struct {
	// binary is the path to the claude CLI. Defaults to "claude"
	// (resolved via PATH) when NewCLIClient is called without
	// WithCLIBinary; absolute paths work too.
	binary string

	// model is the model identifier passed through to `claude --model`.
	// Leave empty to use whatever Claude Code's current session
	// default is (usually the subscription's default model).
	model string

	// timeout caps each subprocess invocation. 0 means no deadline
	// beyond the caller's context.
	timeout time.Duration

	// maxTurns guards against runaway multi-turn continuations in the
	// rare case where Claude Code decides to keep iterating. 1 is the
	// right value for a single LLM call; the dispatcher runs its own
	// tool-calling loop on top.
	maxTurns int

	// extraArgs lets operators pass arbitrary flags to `claude`
	// (e.g. --debug, --dangerously-skip-permissions). Forwarded after
	// the canonical flags we set; order is preserved.
	extraArgs []string

	// effortLevel sets CLAUDE_CODE_EFFORT_LEVEL for the subprocess.
	// On Opus 4.6 / Sonnet 4.6 this controls adaptive-reasoning depth:
	// higher levels generate more thinking tokens and take longer.
	// Empty = don't set the env var (use Claude Code's default).
	// "low" is the right setting for the dispatcher-proxy use case —
	// the model decides tool calls, the actual work happens
	// elsewhere, so extra reasoning adds latency without adding value.
	// See https://code.claude.com/docs/en/model-config#adjust-effort-level
	effortLevel string

	logger  zerolog.Logger
	metrics *Metrics

	// counter is used to generate correlation IDs for log scrubbing.
	// Not sensitive — just makes stderr grep-able across concurrent
	// invocations.
	counter uint64

	// modelCatalog is the catalog ListModels returns. Wired by the
	// container (typically from pricing.yaml entries that match the
	// Anthropic naming convention). nil → ListModels returns an
	// empty list (Claude Code's auth surface has no /v1/models).
	modelCatalog []ModelInfo
}

// CLIOption configures a CLIClient.
type CLIOption func(*CLIClient)

// WithCLIBinary overrides the claude binary path.
func WithCLIBinary(path string) CLIOption {
	return func(c *CLIClient) { c.binary = path }
}

// WithCLITimeout sets a per-request timeout (applied alongside the
// caller's context deadline, whichever is tighter).
func WithCLITimeout(d time.Duration) CLIOption {
	return func(c *CLIClient) { c.timeout = d }
}

// WithCLILogger configures the logger used for subprocess telemetry.
func WithCLILogger(l zerolog.Logger) CLIOption {
	return func(c *CLIClient) { c.logger = l }
}

// WithCLIEffortLevel sets the CLAUDE_CODE_EFFORT_LEVEL env var passed
// to the subprocess. Valid values: "", "low", "medium", "high",
// "xhigh", "max". Empty string = don't override (use Claude Code's
// default, which is xhigh on Opus 4.7 and high/medium on Sonnet 4.6
// depending on plan tier). Defaults to "low" in NewCLIClient because
// the proxy's tool-calling use case doesn't benefit from deep
// reasoning and the latency savings are substantial (Sonnet 4.6 at
// adaptive-high was generating ~14K thinking tokens per turn, ~280s
// at ~50 tok/s, exceeding agent-side 300s curl timeouts).
func WithCLIEffortLevel(level string) CLIOption {
	return func(c *CLIClient) { c.effortLevel = level }
}

// WithCLIModelCatalog supplies the catalog ListModels will return.
// The claude CLI wraps Claude Code's subscription auth, which exposes
// no public model-list endpoint — the daemon provides the catalog
// out-of-band, typically by filtering pricing.yaml.
func WithCLIModelCatalog(models []ModelInfo) CLIOption {
	return func(c *CLIClient) { c.modelCatalog = models }
}

// NewCLIClient constructs a CLIClient. `model` may be empty; in that
// case Claude Code's session default is used and the Model() getter
// returns an empty string (which the metrics layer handles by skipping
// per-model labels).
func NewCLIClient(model string, opts ...CLIOption) *CLIClient {
	c := &CLIClient{
		binary:      "claude",
		model:       model,
		maxTurns:    1,
		timeout:     DefaultTimeout,
		effortLevel: "low", // see WithCLIEffortLevel for rationale

		logger: zerolog.Nop(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Model implements Provider.
func (c *CLIClient) Model() string { return c.model }

// SetMetrics implements Provider.
func (c *CLIClient) SetMetrics(m *Metrics) {
	c.metrics = m
	if m != nil && c.model != "" {
		m.RequestsTotal.WithLabelValues(c.model, "error")
		m.ErrorsTotal.WithLabelValues(c.model, "error")
	}
}

// ListModels implements ModelLister. The claude-cli subprocess wraps
// the Claude Code subscription auth, which exposes no public model-
// list endpoint — the caller wires a catalog at construction time
// via WithCLIModelCatalog (typically derived from pricing.yaml).
// Returns nil when no catalog is configured.
func (c *CLIClient) ListModels(_ context.Context) ([]ModelInfo, error) {
	if len(c.modelCatalog) == 0 {
		return nil, nil
	}
	out := make([]ModelInfo, len(c.modelCatalog))
	copy(out, c.modelCatalog)
	return out, nil
}

// Ping implements Pinger by executing `<binary> --version`. That's
// the cheapest call the claude/codex CLI offers — it doesn't load
// the subscription auth, doesn't make a network round-trip, and
// exits within ~50ms when the binary is on PATH. A non-zero exit
// (typically "command not found") becomes a readiness failure so
// the daemon's startup gate can wait until the binary is reachable.
func (c *CLIClient) Ping(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.binary, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cli binary %q --version failed: %w (output: %s)", c.binary, err, truncate(string(out), 200))
	}
	return nil
}

// Complete implements Provider.
func (c *CLIClient) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return c.invoke(ctx, messages, nil, nil)
}

// CompleteWithTools implements Provider. Tool definitions are encoded
// into the system prompt; the response's ToolCalls slice is populated
// when the model replies with a shim-formatted tool-call JSON.
func (c *CLIClient) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return c.invoke(ctx, messages, tools, nil)
}

// CompleteWithToolsStream implements Provider. The text callback
// receives incremental accumulated text; tool-call detection runs once
// the stream completes (the shim can't partially validate JSON mid-
// stream without false positives on prose that happens to mention "{").
func (c *CLIClient) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return c.invoke(ctx, messages, tools, onText)
}

// ---------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------

// toolInstruction is the protocol the shim instructs the model to
// follow when it wants to call a tool. Kept terse so it doesn't chew
// a lot of the system-prompt budget. The dispatcher (and tests)
// re-parse the shape; don't change without updating parseToolCall.
const toolInstruction = `
You have access to the following tools. When you want to call a tool,
respond with ONLY a JSON object in this exact shape (no markdown fence,
no prose before or after):

  {"tool_call": {"name": "<tool_name>", "arguments": {...}}}

Otherwise respond with normal text. Choose tool calls only when the
tools listed below are the right way to make progress; return prose
for anything else. One tool call per reply.

AVAILABLE TOOLS:
`

// cliRequest is the full set of inputs a single claude invocation takes.
type cliRequest struct {
	systemPrompt string
	userTurn     string
	tools        []Tool
	onText       StreamCallback
}

// invoke is the single path through the CLI: build args, spawn, parse.
// Called by all Provider methods with varying inputs.
func (c *CLIClient) invoke(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	id := atomic.AddUint64(&c.counter, 1)
	start := time.Now()

	req, err := c.packMessages(messages, tools)
	if err != nil {
		c.recordMetrics(start, "error")
		return nil, err
	}
	req.onText = onText

	resp, rerr := c.runClaude(ctx, id, req)
	if rerr != nil {
		c.recordMetrics(start, classifyCLIError(rerr))
		return nil, rerr
	}
	if tools != nil {
		// Extract shim-encoded tool calls from the assistant text.
		if err := applyToolCallShim(resp); err != nil {
			c.logger.Warn().Err(err).Uint64("call_id", id).
				Msg("cli: malformed tool call JSON — treating as plain text")
		}
	}
	c.recordMetrics(start, "success")
	return resp, nil
}

// packMessages collapses the OpenAI-style message list into the two
// inputs the claude CLI accepts: a system prompt and a single user
// turn. Prior assistant/tool messages are re-serialised as a short
// transcript prepended to the user turn — Claude Code has no direct
// "load prior conversation" flag, so we lean on in-prompt history.
func (c *CLIClient) packMessages(messages []Message, tools []Tool) (*cliRequest, error) {
	if len(messages) == 0 {
		return nil, ErrEmptyMessages
	}

	var systemParts []string
	var userTurn strings.Builder
	var lastUser string

	// Separate system messages and turn-style messages. Keep the
	// user's latest message for the --print argument; render any
	// prior history (assistant replies, tool results) into the
	// transcript that precedes it.
	transcript := &strings.Builder{}
	for _, m := range messages {
		switch strings.ToLower(m.Role) {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user":
			if transcript.Len() > 0 {
				// Previous user turn goes into transcript — we only
				// pass the LAST user message as the direct prompt.
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

	if transcript.Len() > 0 {
		userTurn.WriteString("Prior conversation:\n")
		userTurn.WriteString(transcript.String())
		userTurn.WriteString("\nCurrent user message:\n")
	}
	userTurn.WriteString(lastUser)

	sys := strings.Join(systemParts, "\n\n")
	if len(tools) > 0 {
		sys += "\n\n" + toolInstruction + renderToolsForPrompt(tools)
	}

	return &cliRequest{
		systemPrompt: strings.TrimSpace(sys),
		userTurn:     userTurn.String(),
		tools:        tools,
	}, nil
}

// runClaude spawns the subprocess, pipes the user turn in (via stdin
// to avoid argv-size limits on long prompts), streams stream-json
// events, and returns the assembled ChatResponse.
func (c *CLIClient) runClaude(ctx context.Context, id uint64, req *cliRequest) (*ChatResponse, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	args := []string{
		"--print",                        // non-interactive, one-shot
		"--output-format", "stream-json", // line-delimited JSON events
		"--input-format", "text", // stdin is the user turn
		"--verbose", // stream-json requires verbose mode
		"--max-turns", fmt.Sprintf("%d", c.maxTurns),
		// Disable Claude Code's entire tool ecosystem — the dispatcher
		// only wants the model's reasoning and shim-formatted tool
		// calls, not Claude Code's built-ins.
		//
		//   --tools ""             → strips every built-in (Bash, Read,
		//                             Task, TodoWrite, Skill, Monitor,
		//                             EnterPlanMode, etc.). --disallowedTools
		//                             with a hand-curated list missed
		//                             Task → the model launched the
		//                             Explore subagent, which tried
		//                             blocked Bash and hit max_turns.
		//   --strict-mcp-config    → ignore the user's ~/.claude MCP
		//                             servers so the model can't call
		//                             mcp__claude_ai_Mermaid_Chart etc.
		//   --disable-slash-commands → skip Skill resolution.
		"--tools", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if req.systemPrompt != "" {
		// Claude Code supports --append-system-prompt (layers on top
		// of its built-in prompt). We use that because Claude Code's
		// prompt encodes important safety and formatting norms; fully
		// replacing it with --system-prompt has produced worse results
		// in testing.
		args = append(args, "--append-system-prompt", req.systemPrompt)
	}
	args = append(args, c.extraArgs...)

	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Stdin = strings.NewReader(req.userTurn)

	// Env overlay: start with the parent env and layer on the thinking
	// controls. CLAUDE_CODE_EFFORT_LEVEL=low alone still lets the
	// adaptive reasoner decide to think on "complex enough" prompts
	// (20KB user turn + 4 tools = always deemed complex → 14K
	// thinking tokens → 300s+ turns → curl timeouts on the agent
	// side). The hard disable is the pair documented at
	// https://code.claude.com/docs/en/model-config#adaptive-reasoning-and-fixed-thinking-budgets:
	//   CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1  — use fixed budget
	//   MAX_THINKING_TOKENS=0                    — budget of 0
	//
	// Together they zero out thinking on Sonnet 4.6, Opus 4.6, and
	// Haiku. Opus 4.7 always runs adaptive reasoning and ignores
	// both — which is what we want: coder roles on Opus keep their
	// reasoning, Sonnet orchestration roles get 2-5s turns.
	env := os.Environ()
	env = append(env,
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1",
		"MAX_THINKING_TOKENS=0",
	)
	if c.effortLevel != "" {
		env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+c.effortLevel)
	}
	cmd.Env = env

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cli: stdout pipe: %w", err)
	}

	c.logger.Debug().
		Uint64("call_id", id).
		Str("binary", c.binary).
		Str("model", c.model).
		Str("effort", c.effortLevel).
		Int("tool_count", len(req.tools)).
		Int("user_turn_bytes", len(req.userTurn)).
		Int("system_prompt_bytes", len(req.systemPrompt)).
		Msg("cli: invoking claude")

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cli: start %s: %w", c.binary, err)
	}

	resp, parseErr := parseStreamJSON(stdout, req.onText)
	// Drain/close even on parse error so the subprocess can exit.
	_ = stdout.Close()
	waitErr := cmd.Wait()

	if parseErr != nil {
		return nil, fmt.Errorf("cli: parse stdout: %w (stderr: %s)", parseErr, truncate(stderr.String(), 500))
	}
	if waitErr != nil {
		return nil, fmt.Errorf("cli: %s exited: %w (stderr: %s)", c.binary, waitErr, truncate(stderr.String(), 500))
	}

	if resp.Model == "" {
		resp.Model = c.model
	}
	return resp, nil
}

// recordMetrics mirrors the HTTP client's Prometheus reporting so both
// providers show up on the same dashboards.
func (c *CLIClient) recordMetrics(start time.Time, status string) {
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
// stream-json parsing
// ---------------------------------------------------------------------

// cliEvent is the minimal shape we need from Claude Code's stream-json.
// The CLI emits richer fields (tool_use blocks, system init metadata,
// cost breakdowns) that we selectively extract. Unknown fields are
// ignored so upstream schema drift doesn't crash the parser.
type cliEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message *cliEventMsg    `json:"message,omitempty"`
	Usage   *cliEventUsage  `json:"usage,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type cliEventMsg struct {
	ID         string           `json:"id,omitempty"`
	Role       string           `json:"role,omitempty"`
	Model      string           `json:"model,omitempty"`
	Content    []cliContentPart `json:"content,omitempty"`
	StopReason string           `json:"stop_reason,omitempty"`
	Usage      *cliEventUsage   `json:"usage,omitempty"`
}

// cliContentPart covers the two block kinds we care about. Claude Code
// emits "text" blocks for model prose and "tool_use" blocks when it
// calls one of its own tools (which we've disabled, but we still
// handle the shape defensively).
type cliContentPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type cliEventUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// parseStreamJSON reads newline-delimited JSON events from r and
// assembles a ChatResponse. onText (when non-nil) is called on each
// new chunk of assistant text. The parser is tolerant of unknown
// event types and malformed lines — it accumulates whatever signal it
// can and returns a populated response; hard errors only fire when
// the stream produces no usable content.
func parseStreamJSON(r io.Reader, onText StreamCallback) (*ChatResponse, error) {
	var (
		accumulated strings.Builder
		finalResp   = &ChatResponse{}
		haveText    bool
	)

	scanner := bufio.NewScanner(r)
	// Default buffer is 64KB — bump to handle long assistant replies
	// that arrive in a single tool_result event.
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var evt cliEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			// Garbled line — skip. Real errors get logged upstream.
			continue
		}

		switch evt.Type {
		case "assistant":
			if evt.Message == nil {
				continue
			}
			if evt.Message.Model != "" && finalResp.Model == "" {
				finalResp.Model = evt.Message.Model
			}
			if evt.Message.ID != "" && finalResp.ID == "" {
				finalResp.ID = evt.Message.ID
			}
			for _, part := range evt.Message.Content {
				if part.Type == "text" && part.Text != "" {
					accumulated.WriteString(part.Text)
					haveText = true
					if onText != nil {
						onText(accumulated.String())
					}
				}
			}
			if u := evt.Message.Usage; u != nil {
				finalResp.Usage.PromptTokens += u.InputTokens
				finalResp.Usage.CompletionTokens += u.OutputTokens
			}
		case "result":
			if evt.Error != "" {
				return nil, fmt.Errorf("cli: claude returned error: %s", evt.Error)
			}
			if u := evt.Usage; u != nil {
				// Result event's totals win over per-message tallies
				// (the CLI's own rollup includes any retries).
				if u.InputTokens > 0 {
					finalResp.Usage.PromptTokens = u.InputTokens
				}
				if u.OutputTokens > 0 {
					finalResp.Usage.CompletionTokens = u.OutputTokens
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cli: scan stdout: %w", err)
	}

	finalResp.Usage.TotalTokens = finalResp.Usage.PromptTokens + finalResp.Usage.CompletionTokens

	if !haveText {
		return nil, fmt.Errorf("cli: no assistant text in stream-json output")
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

// ---------------------------------------------------------------------
// Tool-call shim
// ---------------------------------------------------------------------

// renderToolsForPrompt formats the tool list in a shape the model can
// easily follow. Uses JSON Schema parameters verbatim so Claude's
// tool-prompt understanding applies even though this isn't a native
// tool_use request.
func renderToolsForPrompt(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s\n", t.Function.Name, t.Function.Description)
		if len(t.Function.Parameters) > 0 {
			fmt.Fprintf(&b, "  parameters: %s\n", string(t.Function.Parameters))
		}
	}
	return b.String()
}

// renderToolCallsForHistory serialises a prior assistant message's
// tool calls back into the shim format so the model sees its own
// history in a consistent shape.
func renderToolCallsForHistory(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	if len(calls) == 1 {
		return fmt.Sprintf(`{"tool_call":{"name":%q,"arguments":%s}}`, calls[0].Function.Name, coalesceJSON(calls[0].Function.Arguments))
	}
	var parts []string
	for _, tc := range calls {
		parts = append(parts, fmt.Sprintf(`{"name":%q,"arguments":%s}`, tc.Function.Name, coalesceJSON(tc.Function.Arguments)))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// applyToolCallShim scans the assistant text for a shim-shaped tool
// call envelope and, if found, moves it into ChatResponse.Choices[0].
// Message.ToolCalls — leaving Content empty so the dispatcher treats
// this as a tool-call turn, not a text reply.
func applyToolCallShim(resp *ChatResponse) error {
	if resp == nil || len(resp.Choices) == 0 {
		return nil
	}
	msg := &resp.Choices[0].Message
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return nil
	}

	name, args, ok := parseToolCall(text)
	if !ok {
		return nil
	}
	msg.Content = ""
	msg.ToolCalls = []ToolCall{{
		ID:   fmt.Sprintf("call_cli_%d", time.Now().UnixNano()),
		Type: "function",
		Function: FunctionCall{
			Name:      name,
			Arguments: string(args),
		},
	}}
	resp.Choices[0].FinishReason = "tool_calls"
	return nil
}

// parseToolCall looks for a `{"tool_call": {...}}` envelope anywhere
// in the text. Matches the shim protocol defined in toolInstruction.
// Returns the extracted name and raw JSON arguments.
//
// The parser uses json.Decoder rather than a brute-force outer-brace
// slice so it survives two real-world model behaviors:
//
//   - Prose preface: "Sure, I'll look that up. {envelope}"
//   - Trailing content: "{envelope}\n\n{hallucinated tool_result}" —
//     when max-turns=1, the model sometimes emits the call AND a
//     made-up response to it, back-to-back. The decoder stops at the
//     first complete JSON value and ignores the rest, which is
//     exactly what we want.
func parseToolCall(text string) (name string, args json.RawMessage, ok bool) {
	// Fast rejection: if the literal key isn't present, skip.
	if !strings.Contains(text, `"tool_call"`) {
		return "", nil, false
	}

	// Slide forward until we hit a `{` that actually starts a JSON
	// object containing our key. A single-pass scan handles code
	// fences, prose preface, and indented JSON alike.
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		var envelope struct {
			ToolCall struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"tool_call"`
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		if err := dec.Decode(&envelope); err != nil {
			continue
		}
		if envelope.ToolCall.Name == "" {
			continue
		}
		argsJSON := envelope.ToolCall.Arguments
		if len(argsJSON) == 0 {
			argsJSON = json.RawMessage("{}")
		}
		return envelope.ToolCall.Name, argsJSON, true
	}
	return "", nil, false
}

// ---------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------

func fallback(primary, backup string) string {
	if primary != "" {
		return primary
	}
	return backup
}

func coalesceJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// classifyCLIError maps a subprocess error to a short metric label.
// Kept aligned with the HTTP client's status labels ("error",
// "timeout") so a single Grafana query works for both providers.
func classifyCLIError(err error) string {
	if err == nil {
		return "success"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "signal: killed"):
		return "timeout"
	case strings.Contains(msg, "exit status"):
		return "cli_nonzero_exit"
	case strings.Contains(msg, "no assistant text"):
		return "empty_response"
	default:
		return "error"
	}
}

// WithModel implements ModelOverridable. Returns a shallow-copy
// CLIClient pinned to `model`, so the chat-completions proxy can route
// per-request model selection through a fresh Provider without
// touching the one the dispatcher is using. Metrics and logger are
// shared by reference — cheap, and correct: two concurrent
// invocations should show up on the same counters / same log stream.
func (c *CLIClient) WithModel(model string) Provider {
	if c == nil {
		return c
	}
	clone := *c
	clone.model = model
	return &clone
}

// Compile-time conformance checks.
var _ Provider = (*CLIClient)(nil)
var _ ModelOverridable = (*CLIClient)(nil)
