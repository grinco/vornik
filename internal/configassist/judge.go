package configassist

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmjudge"
	"vornik.io/vornik/internal/secrethygiene"
)

// JudgeInput is what the judge sees (design §7): the intent, the diff, the
// class and the grounding's evidence — and NOT the author's reasoning
// (test 9): a judge that reads the author's justification is agreeing
// with it.
type JudgeInput struct {
	Intent   string
	Diff     string
	Class    string
	Evidence string
}

// JudgeVerdict is the judge's answer with the NOT JUDGED state made
// explicit: Judged=false is never a pass (design §7.1).
type JudgeVerdict struct {
	Judged      bool     `json:"judged"`
	Decision    string   `json:"decision"` // pass | fail | abstain
	Confidence  float64  `json:"confidence"`
	Summary     string   `json:"summary"`
	QuotedLines []string `json:"quoted_lines,omitempty"`
	Reason      string   `json:"reason,omitempty"` // why NOT JUDGED / abstain
	Model       string   `json:"model,omitempty"`
	PromptTok   int      `json:"prompt_tokens,omitempty"`
	CompleteTok int      `json:"completion_tokens,omitempty"`
}

// Pass reports a judged pass — the only state that may unlock auto-apply.
func (v JudgeVerdict) Pass() bool { return v.Judged && v.Decision == llmjudge.DecisionPass }

const judgePrompt = `You are the safety judge for a configuration change proposed by a separate assistant model. You are shown the operator's request, the blast-radius class the change was assigned, the evidence available, and the unified diff. You are NOT shown the assistant's reasoning, on purpose.

Answer two questions: (1) does the diff do what the operator asked? (2) does it do anything the operator did not ask for? Flag an ambiguous request you had to disambiguate.

Reply with ONLY a JSON object: {"decision":"pass"|"fail"|"abstain","confidence":0.0-1.0,"summary":"one or two sentences that QUOTE the changed lines verbatim","quoted_lines":["the exact + or - lines you relied on"]}. A summary that does not quote the changed lines is treated as abstain.`

// RunJudge evaluates the diff. Egress gate first (design §7.3, test 34): a
// diff that itself carries a raw secret is NOT sent — the proposal is filed
// NOT JUDGED. An unavailable or unparseable judge is NOT JUDGED (test 7);
// abstain stays abstain (test 6); a verdict that does not quote the changed
// lines is abstain (test 18).
func RunJudge(ctx context.Context, provider chat.Provider, in JudgeInput) JudgeVerdict {
	if provider == nil {
		return JudgeVerdict{Judged: false, Decision: llmjudge.DecisionAbstain, Reason: "judge not configured"}
	}
	if findings := secrethygiene.ScanText(in.Diff); len(findings) > 0 {
		keys := make([]string, 0, len(findings))
		for _, f := range findings {
			keys = append(keys, f.Key)
		}
		return JudgeVerdict{Judged: false, Decision: llmjudge.DecisionAbstain, Reason: "diff carries what looks like a raw secret (" + strings.Join(keys, ", ") + "); not sent to the judge"}
	}
	user := fmt.Sprintf("OPERATOR REQUEST:\n%s\n\nASSIGNED CLASS: %s\n\nEVIDENCE:\n%s\n\nUNIFIED DIFF:\n%s\n", in.Intent, in.Class, in.Evidence, in.Diff)
	msgs := []chat.Message{{Role: "system", Content: judgePrompt}, {Role: "user", Content: user}}
	resp, err := llmjudge.CompleteWithRetry(ctx, provider, msgs, 3, "config-assistant-judge")
	if err != nil {
		return JudgeVerdict{Judged: false, Decision: llmjudge.DecisionAbstain, Reason: "judge unavailable: " + err.Error()}
	}
	v := JudgeVerdict{Judged: true, Model: resp.Model, PromptTok: resp.Usage.PromptTokens, CompleteTok: resp.Usage.CompletionTokens}
	if len(resp.Choices) == 0 {
		v.Decision, v.Reason = llmjudge.DecisionAbstain, llmjudge.AbstainSummary("empty judge response")
		return v
	}
	decision, conf, summary, raw, ok := llmjudge.ParseVerdict(resp.Choices[0].Message.Content)
	if !ok {
		v.Decision, v.Reason = llmjudge.DecisionAbstain, llmjudge.AbstainSummary("could not parse judge JSON")
		return v
	}
	v.Decision, v.Confidence, v.Summary = decision, conf, summary
	var extra struct {
		Quoted []string `json:"quoted_lines"`
	}
	_ = json.Unmarshal(raw, &extra)
	v.QuotedLines = extra.Quoted
	if v.Decision != llmjudge.DecisionAbstain && !quotesChangedLines(v, in.Diff) {
		v.Decision = llmjudge.DecisionAbstain
		v.Reason = "verdict did not quote the changed lines; treated as abstain (design test 18)"
	}
	return v
}

// quotesChangedLines reports whether the verdict's summary or quoted lines
// contain at least one changed line of the diff.
func quotesChangedLines(v JudgeVerdict, diff string) bool {
	changed := ChangedLines(diff)
	if len(changed) == 0 {
		return true
	}
	hay := v.Summary + "\n" + strings.Join(v.QuotedLines, "\n")
	for _, l := range changed {
		if strings.Contains(hay, l) {
			return true
		}
	}
	return false
}
