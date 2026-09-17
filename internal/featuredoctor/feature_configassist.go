package featuredoctor

import (
	"context"
	"fmt"

	"strings"

	"vornik.io/vornik/internal/modelfamily"
	"vornik.io/vornik/internal/version"
)

// configAssistantFeature declares the configuration and troubleshooting
// assistant (2026-09-13 design). Community edition; the model behind it is
// the operator's own cost.
//
// Two prerequisites are load-bearing and both are refusals, not warnings:
//
//   - the journal durability contract (review R6, plan §7g): SQLite
//     synchronous=FULL, Postgres synchronous_commit=on. A multi-file apply
//     commits its PREPARED journal row before the first write; without a
//     durable commit that row is not a recovery record. The feature refuses
//     to enable rather than silently downgrading to single-file mode.
//   - the assistant and the judge resolve to DIFFERENT provider families
//     (design §7.3, test 37). A same-family judge shares the author's blind
//     spots; the check is here and on the request path, never in a comment.
func configAssistantFeature() Feature {
	return Feature{
		ID:      "config-assistant",
		Title:   "Configuration assistant",
		Summary: "Natural-language configuration and troubleshooting: an in-process file-editing agent proposes reviewable, rollbackable changes to project autonomy settings, swarms and workflows, grounded in the deployment's own quality and autonomy-feed metrics and judged by a smaller model of a different family.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/config-assistant.md",
		Edition: version.EditionCommunity,
		Apply:   ReloadHot,
		Gates:   []Gate{{Key: "config_assistant.enabled", EnableTo: true}},
		Prereqs: []Prereq{
			{
				Name: "chat provider configured",
				Check: func(_ context.Context, d Deps) PrereqResult {
					if !chatProviderConfigured(d) {
						return PrereqResult{OK: false, Fixable: false,
							Detail:      "no chat provider configured",
							Remediation: "set chat.provider (+ chat.endpoint/chat.model or the router.* sub-providers) before enabling the assistant — every request is a model call"}
					}
					return PrereqResult{OK: true, Detail: "chat provider configured"}
				},
			},
			{
				Name:  "journal durability contract establishable",
				Check: checkJournalDurability,
			},
			{
				Name:  "assistant and judge models are different families",
				Check: checkJudgeFamilySeparation,
			},
		},
		Verify: func(ctx context.Context, d Deps) PrereqResult {
			if r := checkJournalDurability(ctx, d); !r.OK {
				return r
			}
			if r := checkJudgeFamilySeparation(ctx, d); !r.OK {
				return r
			}
			return PrereqResult{OK: true, Detail: "durable journal and different-family judge confirmed"}
		},
	}
}

// checkJournalDurability asks the store whether commits are durable enough
// for the apply journal. Nil prober = not wired = refuse.
func checkJournalDurability(ctx context.Context, d Deps) PrereqResult {
	if d.Durability == nil {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "durability prober not wired",
			Remediation: "this build cannot establish the journal durability contract; upgrade the daemon"}
	}
	rep := d.Durability.ProbeDurability(ctx)
	if !rep.OK {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "durable commit contract NOT established: " + rep.Detail,
			Remediation: durabilityRemediation(rep.Driver)}
	}
	return PrereqResult{OK: true, Detail: rep.Detail}
}

func durabilityRemediation(driver string) string {
	switch driver {
	case "sqlite":
		return "the daemon opens SQLite with synchronous=FULL from the 2026-09-13 build on; if the PRAGMA is being overridden, remove the override — the apply journal cannot be a recovery record on a non-durable commit"
	case "postgres":
		return "set synchronous_commit=on for the daemon's role/database (ALTER DATABASE … SET synchronous_commit = on) — the apply journal cannot be a recovery record on an asynchronous commit"
	default:
		return "establish a durable commit contract on the active store before enabling the assistant"
	}
}

// checkJudgeFamilySeparation reads config_assistant.model and
// config_assistant.judge_model (falling back to chat.model for an empty
// assistant id) and refuses a same-family or unclassifiable pair.
func checkJudgeFamilySeparation(_ context.Context, d Deps) PrereqResult {
	if d.Config == nil {
		return PrereqResult{OK: false, Fixable: false, Detail: "config unavailable"}
	}
	assistant := gateString(d, "config_assistant.model")
	if assistant == "" {
		assistant = gateString(d, "chat.model")
	}
	judge := gateString(d, "config_assistant.judge_model")
	if assistant == "" || judge == "" {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "config_assistant.model (or chat.model) and config_assistant.judge_model must both be set",
			Remediation: "set config_assistant.judge_model to a cheap hosted model of a different family from the assistant (design §7.3), e.g. nvidia.nemotron-nano-9b-v2 or openai.gpt-oss-20b against a GLM assistant"}
	}
	fa, fj := modelfamily.Family(assistant), modelfamily.Family(judge)
	if fa == modelfamily.Unknown || fj == modelfamily.Unknown {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      fmt.Sprintf("cannot classify the model family of %q (%s) / %q (%s); refusing rather than assuming they differ", assistant, fa, judge, fj),
			Remediation: "use model ids whose vendor prefix is recognisable (anthropic/claude, openai/gpt, google/gemini/gemma, nvidia/nemotron, glm, qwen, deepseek, mistral, llama, …) or extend internal/modelfamily"}
	}
	if fa == fj {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      fmt.Sprintf("assistant %q and judge %q are the SAME family (%s) — a same-family judge shares the author's blind spots", assistant, judge, fa),
			Remediation: "set config_assistant.judge_model to a model from a different provider family than config_assistant.model"}
	}
	return PrereqResult{OK: true, Detail: fmt.Sprintf("assistant %s (%s) judged by %s (%s)", assistant, fa, judge, fj)}
}

func gateString(d Deps, key string) string {
	if d.Config == nil {
		return ""
	}
	v, _ := d.Config.GateValue(key)
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func gateBool(d Deps, key string) bool {
	if d.Config == nil {
		return false
	}
	v, _ := d.Config.GateValue(key)
	b, _ := v.(bool)
	return b
}
