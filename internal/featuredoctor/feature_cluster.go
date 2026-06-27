package featuredoctor

import (
	"context"

	"vornik.io/vornik/internal/version"
)

func clusterFeature() Feature {
	// profileValid checks that node.profile is one of the recognised presets
	// (empty string is treated as "all" — always valid). An unrecognised value
	// is an operator error that cannot be auto-fixed.
	profileValid := Prereq{
		Name: "node profile valid",
		Check: func(ctx context.Context, d Deps) PrereqResult {
			v, _ := d.Config.GateValue("node.profile")
			profile, _ := v.(string)
			switch profile {
			case "", "all", "ui", "worker", "webhook":
				return PrereqResult{OK: true, Detail: "node.profile " + nodeProfileLabel(profile) + " is valid"}
			default:
				return PrereqResult{
					OK:          false,
					Fixable:     false,
					Detail:      "node.profile " + profile + " is not a recognised preset",
					Remediation: `set node.profile to one of: all, ui, worker, webhook (or leave unset for "all")`,
				}
			}
		},
	}

	// relayCoherent checks that relay-related config is consistent with the
	// resolved node role. Coherence is determined from config values alone;
	// no live connectivity probes are performed (those need additional Deps
	// plumbing deferred to a later slice).
	//
	// Rules (mirror node_profile.go Validate logic):
	//  - node.relay.upstream set (used as the presence sentinel for relay config,
	//    C1 scope) → must be a relay node:
	//    serve_webhooks=true AND run_workers=false.
	//  - node.relay_ingress.addr set → must run workers (run_workers=true).
	relayCoherent := Prereq{
		Name: "relay config coherent",
		Check: func(ctx context.Context, d Deps) PrereqResult {
			profile := nodeProfileStr(d.Config)
			serveWebhooks := nodeCapBool(d.Config, "node.serve_webhooks", profileDefaultServeWebhooks(profile))
			runWorkers := nodeCapBool(d.Config, "node.run_workers", profileDefaultRunWorkers(profile))

			relayUpstream, _ := d.Config.GateValue("node.relay.upstream")
			relayUpstreamStr, _ := relayUpstream.(string)
			relaySet := relayUpstreamStr != ""

			ingressAddr, _ := d.Config.GateValue("node.relay_ingress.addr")
			ingressAddrStr, _ := ingressAddr.(string)
			ingressSet := ingressAddrStr != ""

			// relay.* keys present → must be a DMZ relay node (webhook-only).
			if relaySet && (!serveWebhooks || runWorkers) {
				return PrereqResult{
					OK:          false,
					Fixable:     false,
					Detail:      "node.relay.upstream is set but node is not a relay (serve_webhooks=true, run_workers=false) — relay config is only valid for a dedicated webhook node",
					Remediation: "set node.profile=webhook (or set serve_webhooks=true and run_workers=false) when configuring relay keys",
				}
			}

			// relay_ingress.* keys present → must run workers.
			if ingressSet && !runWorkers {
				return PrereqResult{
					OK:          false,
					Fixable:     false,
					Detail:      "node.relay_ingress.addr is set but node does not run workers — relay_ingress is only valid on worker/all nodes",
					Remediation: "set node.profile=worker (or set run_workers=true) when configuring relay_ingress",
				}
			}

			return PrereqResult{OK: true, Detail: "relay config coherent with node profile"}
		},
	}

	return Feature{
		ID:      "cluster",
		Title:   "Cluster topology and node roles",
		Summary: "Multi-node deployment with specialised profiles (ui/worker/webhook) and optional DMZ relay.",
		DocRef:  "docs/public/features/cluster.md",
		LLDRef:  "https://docs.vornik.io",
		Edition: version.EditionEnterprise,
		Apply:   RestartRequired,
		Prereqs: []Prereq{profileValid, relayCoherent},
	}
}

// nodeProfileStr reads node.profile from config, returning "" if absent/not a string.
func nodeProfileStr(cfg ConfigReader) string {
	v, _ := cfg.GateValue("node.profile")
	s, _ := v.(string)
	return s
}

// nodeProfileLabel returns a human-readable label for the profile value (empty → "all").
func nodeProfileLabel(profile string) string {
	if profile == "" {
		return `"" (all)`
	}
	return `"` + profile + `"`
}

// profileDefaultServeWebhooks returns the preset capability for serve_webhooks
// based on the profile name. Mirrors config.presetCaps.
func profileDefaultServeWebhooks(profile string) bool {
	switch profile {
	case "webhook":
		return true
	case "ui", "worker":
		return false
	default: // "", "all"
		return true
	}
}

// profileDefaultRunWorkers returns the preset capability for run_workers.
// Mirrors config.presetCaps.
func profileDefaultRunWorkers(profile string) bool {
	switch profile {
	case "worker":
		return true
	case "ui", "webhook":
		return false
	default: // "", "all"
		return true
	}
}

// nodeCapBool resolves an optional bool gate (a *bool pointer field in NodeConfig).
// When not set in config the profile default is returned.
func nodeCapBool(cfg ConfigReader, key string, defaultVal bool) bool {
	v, ok := cfg.GateValue(key)
	if !ok {
		return defaultVal
	}
	b, isBool := v.(bool)
	if !isBool {
		return defaultVal
	}
	return b
}
