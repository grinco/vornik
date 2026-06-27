package config

import (
	"fmt"
	"time"
)

// presetCaps maps a named profile to its base capability set (before
// per-flag overrides). An unrecognized / empty profile falls back to
// "all" — Validate() rejects an explicitly-unknown profile separately so
// resolution never panics on bad input.
func presetCaps(profile string) NodeCapabilities {
	switch profile {
	case "ui":
		return NodeCapabilities{ServeUI: true}
	case "worker":
		return NodeCapabilities{ServeAPI: true, RunWorkers: true}
	case "webhook":
		return NodeCapabilities{ServeWebhooks: true}
	default: // "all", "", or unknown
		return NodeCapabilities{ServeUI: true, ServeAPI: true, ServeWebhooks: true, RunWorkers: true}
	}
}

// ResolveNodeProfile expands the named preset, applies any explicitly-set
// flag overrides, and derives RelayMode (ServeWebhooks && !RunWorkers).
// Pure — no I/O.
func ResolveNodeProfile(n NodeConfig) NodeCapabilities {
	caps := presetCaps(n.Profile)
	if n.ServeUI != nil {
		caps.ServeUI = *n.ServeUI
	}
	if n.ServeAPI != nil {
		caps.ServeAPI = *n.ServeAPI
	}
	if n.ServeWebhooks != nil {
		caps.ServeWebhooks = *n.ServeWebhooks
	}
	if n.RunWorkers != nil {
		caps.RunWorkers = *n.RunWorkers
	}
	caps.RelayMode = caps.ServeWebhooks && !caps.RunWorkers
	return caps
}

// Validate enforces profile coherence. Called from Config.Validate.
func (n NodeConfig) Validate() error {
	switch n.Profile {
	case "", "all", "ui", "worker", "webhook":
		// ok
	default:
		return fmt.Errorf("node.profile %q invalid: want all|ui|worker|webhook", n.Profile)
	}

	caps := ResolveNodeProfile(n)
	if !caps.ServeUI && !caps.ServeAPI && !caps.ServeWebhooks && !caps.RunWorkers {
		return fmt.Errorf("node: at least one capability must be enabled")
	}

	// Relay config is required iff this is a DMZ relay node, forbidden otherwise.
	relaySet := n.Relay.Upstream != "" || n.Relay.ClientCert != "" ||
		n.Relay.ClientKey != "" || n.Relay.CA != ""
	if caps.RelayMode {
		if n.Relay.Upstream == "" || n.Relay.ClientCert == "" ||
			n.Relay.ClientKey == "" || n.Relay.CA == "" {
			return fmt.Errorf("node.relay.{upstream,client_cert,client_key,ca} are all required in webhook relay mode")
		}
		if n.Relay.Timeout != "" {
			d, err := time.ParseDuration(n.Relay.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("node.relay.timeout must be a positive duration, got %q", n.Relay.Timeout)
			}
		}
		if n.Relay.MaxRetries < 0 {
			return fmt.Errorf("node.relay.max_retries must be >= 0, got %d", n.Relay.MaxRetries)
		}
	} else if relaySet {
		return fmt.Errorf("node.relay is only valid for a webhook node (serve_webhooks without run_workers)")
	}

	// relay_ingress: server side of the seam — only on RunWorkers nodes,
	// all-or-nothing.
	ingressSet := n.RelayIngress.Addr != "" || n.RelayIngress.ServerCert != "" ||
		n.RelayIngress.ServerKey != "" || n.RelayIngress.ClientCA != ""
	if ingressSet {
		if !caps.RunWorkers {
			return fmt.Errorf("node.relay_ingress is only valid on a node that runs workers (the job tier)")
		}
		if n.RelayIngress.Addr == "" || n.RelayIngress.ServerCert == "" ||
			n.RelayIngress.ServerKey == "" || n.RelayIngress.ClientCA == "" {
			return fmt.Errorf("node.relay_ingress.{addr,server_cert,server_key,client_ca} are all required when relay_ingress is configured")
		}
	}
	return nil
}
