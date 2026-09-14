package featuredoctor

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/version"
)

// architectConsultFeature declares the configuration assistant's opt-in
// consultation of the vornik architect over A2A (2026-09-13 plan §8,
// review R8). Community edition, OFF by default, and gated by the SERVICE
// CONTRACT rather than by a licence check in code — the operator's
// decision. What makes a contractual gate supportable is that use is
// AUDITABLE: enabling writes an admin_audit event, every consultation is
// recorded BEFORE dispatch, and a nil audit sink means zero outbound calls.
//
// Prereqs: an a2a.peers entry for the configured peer, and an admin audit
// repository wired. The config-assistant feature itself must be enabled for
// a consultation to ever be reachable; that dependency is expressed here as
// a prereq so `doctor feature enable architect-consult` on a dark assistant
// says so.
func architectConsultFeature() Feature {
	return Feature{
		ID:      "architect-consult",
		Title:   "Architect consultation over A2A (contract-gated)",
		Summary: "Lets the configuration assistant ask the vornik architect one minimized, sanitized question per request over A2A. Use requires a subscription; enabling it creates an auditable record, and every consultation is recorded before it is sent.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/architect-consult.md",
		Edition: version.EditionCommunity,
		Apply:   ReloadHot,
		Gates:   []Gate{{Key: "config_assistant.consult.enabled", EnableTo: true}},
		Prereqs: []Prereq{
			{
				Name: "configuration assistant enabled",
				Check: func(_ context.Context, d Deps) PrereqResult {
					if !gateBool(d, "config_assistant.enabled") {
						return PrereqResult{OK: false, Fixable: false,
							Detail:      "config_assistant.enabled is false",
							Remediation: "enable the config-assistant feature first (vornikctl doctor feature enable config-assistant)"}
					}
					return PrereqResult{OK: true, Detail: "config assistant enabled"}
				},
			},
			{
				Name:  "a2a.peers entry for the architect",
				Check: checkConsultPeerConfigured,
			},
			{
				Name: "admin audit repository wired",
				Check: func(_ context.Context, d Deps) PrereqResult {
					if d.AdminAudit == nil {
						return PrereqResult{OK: false, Fixable: false,
							Detail:      "admin audit repository not wired — consultation would be unauditable",
							Remediation: "a consult is refused when its attempt cannot be recorded (review R8); this build has no audit sink on the active store"}
					}
					return PrereqResult{OK: true, Detail: "admin audit repository wired"}
				},
			},
		},
		Verify: func(ctx context.Context, d Deps) PrereqResult {
			if r := checkConsultPeerConfigured(ctx, d); !r.OK {
				return r
			}
			if d.AdminAudit == nil {
				return PrereqResult{OK: false, Detail: "consult on but admin audit repository not wired — every consultation will be refused"}
			}
			return PrereqResult{OK: true, Detail: "peer configured and audit sink wired"}
		},
	}
}

func checkConsultPeerConfigured(_ context.Context, d Deps) PrereqResult {
	peer := gateString(d, "config_assistant.consult.peer")
	if peer == "" {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "config_assistant.consult.peer is empty",
			Remediation: "set config_assistant.consult.peer to the a2a.peers key of the architect"}
	}
	if d.A2APeers == nil {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "a2a peer lister not wired",
			Remediation: "this build cannot confirm a2a.peers; upgrade the daemon"}
	}
	for _, name := range d.A2APeers.A2APeerNames() {
		if name == peer {
			return PrereqResult{OK: true, Detail: fmt.Sprintf("a2a.peers.%s configured", peer)}
		}
	}
	return PrereqResult{OK: false, Fixable: false,
		Detail:      fmt.Sprintf("a2a.peers has no entry %q (have: %s)", peer, strings.Join(d.A2APeers.A2APeerNames(), ", ")),
		Remediation: "add a2a.peers." + peer + " with the architect's URL and API key, or point config_assistant.consult.peer at an existing entry"}
}
