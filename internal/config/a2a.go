package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// A2AConfig is the outbound A2A federation config: the domain-expert
// peers this instance can consult, and the consult guardrails. Each
// peer materialises one agent-callable tool named consult_<peer-key>,
// gated per-project by permissions.allowedTools. See
// https://docs.vornik.io §2.
type A2AConfig struct {
	// Peers maps a domain-neutral peer key (which becomes the local tool
	// name, e.g. "vornik_expert") to its connection detail.
	Peers map[string]A2APeer `yaml:"peers,omitempty" json:"peers,omitempty" doc:"Consultable A2A peers keyed by tool name (consult_<key>)."`

	// Consult holds the shared consult guardrails.
	Consult A2AConsultConfig `yaml:"consult,omitempty" json:"consult,omitempty" doc:"Consult guardrails: timeout, per-task cap, hop limit."`
}

// A2APeer is one consultable remote agent.
type A2APeer struct {
	// URL is the remote agent endpoint, e.g.
	// https://companion.lan/a2a/v1/agents/companion-example/product-qa
	URL string `yaml:"url" json:"url" doc:"Remote A2A agent URL (…/a2a/v1/agents/<project>/<workflow>)."`

	// APIKey is the X-API-Key sent to the peer. Use a ${VAR} placeholder
	// so the secret stays out of YAML; it is expanded from the daemon
	// environment at config load (the standard vornik secret convention).
	APIKey string `yaml:"api_key,omitempty" json:"api_key,omitempty" doc:"X-API-Key for the peer; use ${VAR} so the secret is env-sourced."`

	// InsecureHTTP allows a plain http:// URL (a LAN peer without TLS).
	// Default false: https is required. Remote peers must stay https.
	InsecureHTTP bool `yaml:"insecure_http,omitempty" json:"insecure_http,omitempty" doc:"Allow http:// for this peer (LAN, no TLS). Default false."`

	// Description is a fallback tool description used only when the peer's
	// agent card can't be fetched at boot; normally the card supplies it.
	Description string `yaml:"description,omitempty" json:"description,omitempty" doc:"Fallback tool description when the peer's agent card is unreachable."`
}

// A2AConsultConfig holds the consult guardrails.
type A2AConsultConfig struct {
	// Timeout caps one consult's wall-clock as a duration string (e.g. "3m").
	// Empty / unparseable → DefaultConsultTimeout. A string (not time.Duration)
	// because the YAML decoder can't parse "3m" into a time.Duration — it would
	// silently stay 0 — matching every other duration knob in this config.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" doc:"Per-consult wall-clock timeout as a duration string (e.g. 3m). Default 3m — a consult drives a full agentic expert run, not a quick lookup."`

	// MaxCallsPerTask caps consults per originating task (anti-spam).
	// Zero → DefaultConsultMaxCallsPerTask.
	MaxCallsPerTask int `yaml:"max_calls_per_task,omitempty" json:"max_calls_per_task,omitempty" doc:"Max consults one task may make. Default 8."`

	// MaxHops bounds the consult-chain depth so mutual peers can't cycle
	// A→B→A. Zero → DefaultConsultMaxHops.
	MaxHops int `yaml:"max_hops,omitempty" json:"max_hops,omitempty" doc:"Max consult-chain depth (loop guard). Default 2."`
}

// Consult guardrail defaults.
const (
	// 3m, not 60s: a consult runs a full container-agent expert (RAG retrieve +
	// synthesize, several LLM iterations) — observed ~90s. At 60s the consult
	// timed out and the caller fell back to (stale) local memory instead of the
	// fresh answer it was waiting for (task …470f, 2026-08-01).
	DefaultConsultTimeout         = 3 * time.Minute
	DefaultConsultMaxCallsPerTask = 8
	DefaultConsultMaxHops         = 2
)

// ConsultHopHeader is the A2A task-metadata key carrying the consult
// hop counter. Shared by the inbound loop-guard and the consult tool.
const ConsultHopHeader = "x-vornik-consult-hops"

// peerKeyRe constrains peer keys to safe tool-name characters (the key
// becomes consult_<key>, an LLM-visible tool identifier).
var peerKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,48}$`)

// EffectiveTimeout returns the per-consult timeout, applying the default
// for a zero value.
func (c A2AConsultConfig) EffectiveTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(c.Timeout)); err == nil && d > 0 {
		return d
	}
	return DefaultConsultTimeout
}

// EffectiveMaxCallsPerTask returns the per-task consult cap, applying the
// default for a zero value.
func (c A2AConsultConfig) EffectiveMaxCallsPerTask() int {
	if c.MaxCallsPerTask <= 0 {
		return DefaultConsultMaxCallsPerTask
	}
	return c.MaxCallsPerTask
}

// EffectiveMaxHops returns the consult-chain hop limit, applying the
// default for a zero value.
func (c A2AConsultConfig) EffectiveMaxHops() int {
	if c.MaxHops <= 0 {
		return DefaultConsultMaxHops
	}
	return c.MaxHops
}

// Validate checks the A2A config. Empty config is valid (feature off).
func (a A2AConfig) Validate() error {
	for key, peer := range a.Peers {
		if !peerKeyRe.MatchString(key) {
			return fmt.Errorf("a2a.peers: invalid peer key %q (allowed: lowercase letters, digits, underscore; must start alnum; ≤49 chars)", key)
		}
		if strings.TrimSpace(peer.URL) == "" {
			return fmt.Errorf("a2a.peers.%s: url is required", key)
		}
		u, err := url.Parse(peer.URL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("a2a.peers.%s: invalid url %q", key, peer.URL)
		}
		switch u.Scheme {
		case "https":
			// always allowed
		case "http":
			if !peer.InsecureHTTP {
				return fmt.Errorf("a2a.peers.%s: http:// url requires insecure_http: true (LAN only)", key)
			}
		default:
			return fmt.Errorf("a2a.peers.%s: url scheme must be http or https, got %q", key, u.Scheme)
		}
	}
	if a.Consult.MaxCallsPerTask < 0 || a.Consult.MaxHops < 0 {
		return fmt.Errorf("a2a.consult: max_calls_per_task/max_hops must be non-negative")
	}
	if t := strings.TrimSpace(a.Consult.Timeout); t != "" {
		if _, err := time.ParseDuration(t); err != nil {
			return fmt.Errorf("a2a.consult.timeout %q is not a valid duration (e.g. 3m): %w", a.Consult.Timeout, err)
		}
	}
	return nil
}
