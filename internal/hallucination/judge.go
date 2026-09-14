package hallucination

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmjudge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
)

// Verdict is the rendered output of one judge evaluation.
// Distinct from Signal because the judge produces a holistic
// pass/fail/abstain decision plus a confidence score, not just
// a list of findings — the per-claim findings live inside
// Signals as a stylistic mirror of Phase 1's shape so the UI
// can reuse the same row renderer.
type Verdict struct {
	// Decision: pass / fail / abstain. Abstain means "I can't
	// tell" — the judge couldn't get enough evidence to make a
	// call (e.g. the task produced no artifacts and no audit).
	// Treated as a separate category so dashboards don't credit
	// abstentions toward a quality bar.
	Decision string `json:"decision"`
	// Confidence is the model's self-reported 0.0-1.0 score.
	// Worth surfacing but not load-bearing — small models
	// produce overconfident scores; the rollup tile shows
	// confidence-weighted means rather than raw averages.
	Confidence float64 `json:"confidence"`
	// Summary is a one-or-two-sentence explanation. Renders
	// directly in the UI; kept short so the row stays scannable.
	Summary string `json:"summary"`
	// Signals are per-claim findings, mirroring the Phase 1
	// detector output. Optional — many judges return only the
	// holistic decision without itemising.
	Signals []Signal `json:"signals,omitempty"`
}

// JudgeMetrics carries the infrastructure-side data the runner
// needs to record token usage and cost for a judge call.
// Returned alongside the Verdict so the runner can write a
// task_llm_usage row + populate the verdict's cost_usd field
// without coupling Judge implementations to persistence.
//
// Zero values are valid — they mean "judge didn't reach an LLM"
// (StubJudge, abstain-on-config-missing path), which is the
// case where there's no usage to record.
type JudgeMetrics struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	// CacheCreationTokens / CacheReadTokens are the LLM-caching
	// phase-A observability fields. Populated when the judge's
	// chat call rides over Bedrock or Anthropic; zero on
	// providers without prompt caching.
	CacheCreationTokens int
	CacheReadTokens     int
}

// Judge evaluates a completed task against its produced
// artifacts and audit trail. Implementations:
//
//   - LLMJudge: production path, calls a chat.Provider with a
//     framing prompt and parses the structured response.
//   - StubJudge: deterministic test path; returns a fixed verdict.
//
// The interface deliberately doesn't expose model selection — that
// belongs to the implementation. The runner wires whichever Judge
// the operator configured (per-project or daemon-wide) and calls
// Evaluate; Judge owns the model choice and prompt internally.
//
// Returns three values: the verdict itself, the metrics needed
// to record cost (zero values are valid), and any
// implementation-side error. A nil verdict is never paired with
// a nil error — implementations always return at least an
// abstain on internal failure.
type Judge interface {
	Evaluate(ctx context.Context, in JudgeInput) (*Verdict, *JudgeMetrics, error)
}

// JudgeInput is the world-state the judge inspects. Built once
// at task-completion time and handed to the judge intact —
// implementations are pure functions over this struct.
type JudgeInput struct {
	Task         *persistence.Task
	Execution    *persistence.Execution
	Artifacts    []*persistence.Artifact
	AuditEntries []*persistence.ToolAuditEntry
	// LastResultText is the agent's final assistant text from
	// the last step (typically the writer's summary). The judge
	// scans this for "I did X" claims and tries to ground them
	// against artifacts + audit.
	LastResultText string
}

// LLMJudge calls a chat.Provider with a structured prompt and
// parses the response into a Verdict. Stays narrow on purpose —
// no MCP, no tool use; the judge sees the whole evidence base
// in one shot and decides. That's faster, cheaper, and easier
// to audit than a tool-using judge would be.
type LLMJudge struct {
	// Client is the chat provider — production wires the same
	// Bedrock/Vertex/etc. provider used elsewhere with a model
	// override applied per call.
	Client chat.Provider
	// Model is the LLM model identifier. Nominally a smaller /
	// cheaper model than the worker roles use; the judge runs
	// per-task (rather than per-step), so cost adds up only
	// linearly in task count.
	Model string
	// Prompt overrides the default framing prompt. Empty falls
	// back to the package default (judgeDefaultPrompt). Letting
	// operators customise the framing matters for non-English
	// projects and domain-specific evaluation criteria.
	Prompt string
	// Pricing is the cost table; nil disables CostUSD calculation
	// (token counts still count via the provider response).
	Pricing *pricing.Table
	// MaxEvidenceBytes caps the total size of artifact/audit
	// excerpts that go into the prompt — beyond this, large
	// research-task outputs blow the context window of small
	// judges. Default 8 KiB, configurable via WithMaxEvidence.
	MaxEvidenceBytes int
}

const judgeDefaultPrompt = `You are a hallucination auditor for a multi-agent task system.

You will be given:
  - The task's input prompt.
  - The agent's final summary text.
  - A list of artifacts the agent produced (names + sizes).
  - A condensed tool-call audit (names + truncated inputs/outputs).

Your job: decide whether the agent's final summary is supported by
the artifacts and audit. Look for unsupported claims (URLs the
agent says it visited that don't appear in the audit; counts that
don't match the artifacts; assertions of success when the audit
shows failures or rate-limits; references to files that weren't
produced).

Reply with ONLY a single JSON object, no other text:
{
  "decision": "pass" | "fail" | "abstain",
  "confidence": 0.0..1.0,
  "summary": "one or two sentences explaining the decision",
  "signals": [
    {"detector":"judge","severity":"warn|high","claim_type":"url|path|task_id|project_id|artifact_name|numeric|other","claim_value":"<the literal claim>","detail":"<why it's unsupported>"}
  ]
}

Use "fail" only when you have concrete evidence of an unsupported claim.
Use "abstain" when the evidence is too thin to decide either way.
Use "pass" when claims are supported (or there are no assertive claims).
Confidence is your own self-rating; don't inflate it.

TRADING-SPECIFIC ARITHMETIC CHECK. When the agent's output is
a trading proposal envelope (contains "approved" or "proposals"
arrays with "stop_loss_price" / "limit_price" / "entry" fields),
verify the stop-loss arithmetic on each entry:
  pct_off = |entry - stop_loss_price| / entry × 100
A pct_off outside [3, 30] is suspicious — typical strategy
rules cluster between 5% and 15%. Flag any pct_off < 3 with a
signal:
  {"detector":"judge","severity":"warn","claim_type":"numeric","claim_value":"stop_too_tight","detail":"<symbol> stop $X is N% from entry $Y — likely arithmetic error or bug-class strategy bypass"}
A pct_off > 30 with similar wording but claim_value
"stop_too_wide". This rule fires post-facto; the broker's
MaxStopLossPct gate refuses these at place time, but the judge
verdict surfaces them in the per-task UI for review even when
the order never landed. The check applies whether the values
came from the strategist's proposals[] or the risk-officer's
approved[] — both shapes carry the same fields.`

// Evaluate is the production-path judge call. Failures (LLM
// errors, JSON parse errors, malformed payloads) return an
// abstain verdict rather than blocking — the runner surfaces
// the error in the log; pass/fail is a quality signal, not a
// gate, in Phase 3.
//
// The returned JudgeMetrics carries token counts even on
// abstain paths that DID reach the LLM, so the runner records
// the cost of a model-that-couldn't-decide alongside the cost
// of a clean pass/fail. Pre-LLM abstains (config missing,
// nil client) return zero metrics.
func (j *LLMJudge) Evaluate(ctx context.Context, in JudgeInput) (*Verdict, *JudgeMetrics, error) {
	if j == nil || j.Client == nil || j.Model == "" {
		return abstainVerdict("judge not configured"), &JudgeMetrics{}, nil
	}
	prompt := j.Prompt
	if prompt == "" {
		prompt = judgeDefaultPrompt
	}
	evidence := j.formatEvidence(in)
	msgs := []chat.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: evidence},
	}
	// Non-streaming, no tools — one round trip. Pass model
	// override via ModelOverridable when supported; fall back to
	// the bare client (the global model wins) for providers
	// that don't surface a per-call override.
	client := j.Client
	if mo, ok := client.(chat.ModelOverridable); ok && j.Model != "" {
		client = mo.WithModel(j.Model)
	}
	// Retry transient gateway failures (5xx + 429) with capped
	// exponential backoff. Live evidence: Vertex AI's openapi
	// endpoint surfaces "RESOURCE_EXHAUSTED: queue full" 429s
	// during traffic spikes and "unexpected EOF" connection
	// drops on long-lived idle pools. Pre-fix the judge abstained
	// on the first attempt — verdict tiles filled with
	// "abstained: LLM error" rows that hid real signal.
	resp, err := llmjudge.CompleteJudgeWithRetry(ctx, client, msgs, 3)
	if err != nil {
		return abstainVerdict("LLM error: " + err.Error()), &JudgeMetrics{Model: j.Model}, nil
	}
	metrics := &JudgeMetrics{
		Model:               j.Model,
		PromptTokens:        resp.Usage.PromptTokens,
		CompletionTokens:    resp.Usage.CompletionTokens,
		CacheCreationTokens: resp.Usage.CacheCreationTokens,
		CacheReadTokens:     resp.Usage.CacheReadTokens,
	}
	// Some providers report the served model under resp.Model
	// rather than honouring the client-side WithModel pin (esp.
	// when the call hits a fallback). Prefer the response's
	// reported model when present so usage rows attribute cost
	// to the model that actually billed.
	if resp.Model != "" {
		metrics.Model = resp.Model
	}
	if len(resp.Choices) == 0 {
		return abstainVerdict("empty LLM response"), metrics, nil
	}
	verdict := parseVerdict(resp.Choices[0].Message.Content)
	if verdict == nil {
		return abstainVerdict("could not parse judge JSON"), metrics, nil
	}
	return verdict, metrics, nil
}

// formatEvidence builds the user-message body that the judge
// reads. Artifacts and audit entries are truncated so a large
// research task doesn't blow the judge's context window.
func (j *LLMJudge) formatEvidence(in JudgeInput) string {
	cap := j.MaxEvidenceBytes
	if cap <= 0 {
		cap = 8 * 1024
	}
	var b strings.Builder
	if in.Task != nil {
		fmt.Fprintf(&b, "Task ID: %s\nProject: %s\n", in.Task.ID, in.Task.ProjectID)
		if len(in.Task.Payload) > 0 {
			var p map[string]any
			if json.Unmarshal(in.Task.Payload, &p) == nil {
				if t, ok := p["taskType"].(string); ok {
					fmt.Fprintf(&b, "Type: %s\n", t)
				}
				if c, ok := p["context"].(map[string]any); ok {
					if pr, ok := c["prompt"].(string); ok {
						fmt.Fprintf(&b, "Original prompt: %s\n", truncate(pr, 1000))
					}
				}
			}
		}
	}
	b.WriteString("\nFinal summary text:\n")
	b.WriteString(truncate(in.LastResultText, 2000))
	b.WriteString("\n\nArtifacts:\n")
	if len(in.Artifacts) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, a := range in.Artifacts {
			if a == nil {
				continue
			}
			size := int64(0)
			if a.SizeBytes != nil {
				size = *a.SizeBytes
			}
			fmt.Fprintf(&b, "  - %s (%s, %d bytes)\n", a.Name, a.ArtifactClass, size)
		}
	}
	b.WriteString("\nTool-call audit (most recent first, truncated):\n")
	count := 0
	for i := len(in.AuditEntries) - 1; i >= 0 && count < 30; i-- {
		e := in.AuditEntries[i]
		if e == nil {
			continue
		}
		count++
		fmt.Fprintf(&b, "  [%s] in=%s out=%s\n",
			e.ToolName,
			truncate(strings.TrimSpace(e.ToolInput), 200),
			truncate(strings.TrimSpace(e.ToolOutput), 200),
		)
	}
	out := b.String()
	if len(out) > cap {
		out = out[:cap] + "\n…(truncated)"
	}
	return out
}

// parseVerdict is the hallucination-shaped wrapper over
// llmjudge.ParseVerdict (the tolerant extraction — code fences,
// prefatory prose, <think> blocks — lives there since the 2026-09-13
// extraction so the configuration assistant's judge shares it). This
// wrapper re-reads the raw JSON into the package's own Verdict so the
// per-claim Signals survive, and stamps RecordedAt. Returns nil on
// unrecoverable parse failure — the caller falls back to abstain in
// that case.
func parseVerdict(text string) *Verdict {
	_, _, _, raw, ok := llmjudge.ParseVerdict(text)
	if !ok {
		return nil
	}
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	// Stamp RecordedAt on each signal so they persist with a
	// real timestamp (parser doesn't carry it from the LLM).
	now := time.Now().UTC()
	for i := range v.Signals {
		if v.Signals[i].RecordedAt.IsZero() {
			v.Signals[i].RecordedAt = now
		}
	}
	return &v
}

// abstainVerdict is the safe fallback for any judge failure —
// LLM error, parse error, missing config. Renders as "abstain"
// in the UI so the operator sees the judge ran but couldn't
// decide.
func abstainVerdict(reason string) *Verdict {
	return &Verdict{
		Decision:   persistence.JudgeVerdictAbstain,
		Confidence: 0.0,
		Summary:    llmjudge.AbstainSummary(reason),
	}
}

// StubJudge is a deterministic test path that returns a fixed
// Verdict regardless of input. Production wires LLMJudge.
type StubJudge struct {
	Out     *Verdict
	Metrics *JudgeMetrics
	Err     error
}

// Evaluate returns the configured fixed verdict + metrics.
// Nil metrics are normalised to a zero-valued struct so callers
// don't have to nil-check on the test path.
func (s *StubJudge) Evaluate(_ context.Context, _ JudgeInput) (*Verdict, *JudgeMetrics, error) {
	m := s.Metrics
	if m == nil {
		m = &JudgeMetrics{}
	}
	return s.Out, m, s.Err
}
