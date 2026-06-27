package featuredoctor

import (
	"context"

	"vornik.io/vornik/internal/version"
)

func instinctFeature() Feature {
	return Feature{
		ID:      "instinct",
		Title:   "Continuous-Learning Instinct Layer",
		Summary: "Learns confidence-scored remediation/quality patterns from telemetry.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/instinct.md",
		Edition: version.EditionEnterprise,
		Apply:   RestartRequired,
		Gates: []Gate{
			{Key: "instinct.enabled", EnableTo: true},
			{Key: "instinct.consumers.failure_playbooks", EnableTo: true},
			{Key: "instinct.consumers.architect_priors", EnableTo: true},
			{Key: "instinct.consumers.memory_hygiene", EnableTo: true},
			{Key: "instinct.consumers.application_feedback", EnableTo: true},
		},
		Prereqs: []Prereq{
			{
				Name: "distill model reachable",
				Check: func(ctx context.Context, d Deps) PrereqResult {
					model, _ := d.Config.GateValue("instinct.model")
					id, _ := model.(string)
					if id == "" {
						return PrereqResult{OK: true, Detail: "no distill model set; falls back to chat.model"}
					}
					if d.Models == nil {
						return PrereqResult{OK: false, Fixable: false,
							Detail: id + " not wired (model pinger unavailable)"}
					}
					if d.Models.Reachable(ctx, id) {
						return PrereqResult{OK: true, Detail: id + " reachable"}
					}
					return PrereqResult{OK: false, Fixable: false,
						Detail:      id + " not reachable",
						Remediation: "ensure the distill model (" + id + ") is pulled/served (e.g. `ollama pull`) or unset instinct.model"}
				},
			},
			{
				Name: "instinct.enabled set",
				Check: func(ctx context.Context, d Deps) PrereqResult {
					on, _ := d.Config.GateValue("instinct.enabled")
					if b, _ := on.(bool); b {
						return PrereqResult{OK: true}
					}
					return PrereqResult{OK: false, Fixable: true,
						Detail:      "consumers require the master switch",
						Remediation: "the doctor will set instinct.enabled=true as part of enable"}
				},
			},
		},
		Verify: func(ctx context.Context, d Deps) PrereqResult {
			// Worker is recording: at least one instinct exists OR an
			// application row was written recently. Read-only, fail-soft.
			if d.Instincts == nil {
				return PrereqResult{OK: false, Detail: "instinct repo not wired"}
			}
			counts, err := d.Instincts.CountByDomainStatus(ctx)
			if err != nil {
				return PrereqResult{OK: false, Detail: "count query failed: " + err.Error()}
			}
			if len(counts) > 0 {
				return PrereqResult{OK: true, Detail: "instincts present"}
			}
			return PrereqResult{OK: false,
				Detail: "no instincts yet (worker may not have run a tick)"}
		},
	}
}
