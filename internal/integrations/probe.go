// Package integrations implements the Guided Integrations Hub's probe
// layer (design doc §5.1–§5.3, §6, §8): a typed, secret-free credential
// probe abstraction shared across the four channel kinds (Telegram,
// email, GitHub App, Slack), plus the SSRF-guarded dialer every prober
// must dial through. This package deliberately has no UI, config-write, or
// metrics wiring — those are later Integrations Hub tasks; see
// https://docs.vornik.io
package integrations

import (
	"context"
	"strings"
	"time"
)

// integrationProbeTimeout is the default bound on a single Probe call when
// IntegrationKind.ProbeTimeout is unset (0). Matches the control-plane MCP
// probe's mcpProbeTimeout (internal/ui/admin_control_plane_mcp_probe.go) —
// generous enough for a slow provider round-trip, tight enough not to hang
// the caller.
const integrationProbeTimeout = 15 * time.Second

// Outcome is the explicit, machine-checkable classification of a probe
// result (design §5.2, review round 1 finding 4). It's a distinct field —
// never inferred from Failures or Summary text — so metrics and rendering
// never have to sniff strings.
type Outcome string

const (
	// OutcomeOK means the provider accepted the credential.
	OutcomeOK Outcome = "ok"
	// OutcomeFail means the provider REJECTED the credential (bad token,
	// invalid auth) — this is the user's problem to fix.
	OutcomeFail Outcome = "fail"
	// OutcomeError means the probe couldn't establish validity either way:
	// network failure, timeout, HTTP 429 (rate limited), 5xx, or a
	// malformed provider response. This is explicitly NOT the same as
	// OutcomeFail — a 429/5xx credential may be perfectly valid, so the
	// tile must say "couldn't reach — try again", not "invalid".
	OutcomeError Outcome = "error"
)

// CheckFailure is one structured, human-readable probe finding. Mirrors the
// shape onboarding.CheckFailure established for chat validation
// (internal/onboarding/chatvalidator.go), scoped down to what a credential
// probe needs: which field the finding is about, and why. (onboarding's
// richer Name/Severity/Message/Remediation shape doesn't fit a per-field
// credential check as directly — see the task report for why this package
// defines its own rather than importing onboarding's verbatim.)
type CheckFailure struct {
	// Field is the CredentialField.Key the finding is about (e.g.
	// "bot_token"). Empty when the finding isn't attributable to one field.
	Field string
	// Reason is the plain-language, secret-free explanation.
	Reason string
}

// ProbeResult is the typed result every Prober returns — the shape the
// health tile and the "Test connection" fragment both render from (design
// §5.2). Summary and Detail are guaranteed secret-free: probers build them
// from provider identity fields (bot username, workspace name, installation
// login) or from redactSecrets-cleaned error text, never from the raw
// credential.
type ProbeResult struct {
	OK       bool
	Outcome  Outcome
	Kind     string
	Summary  string
	Detail   string
	Failures []CheckFailure
	Latency  time.Duration
}

// CandidateConfig is the UNSAVED credential set a Prober validates against
// the real provider before anything is written to config or a secrets file.
// Values are literal, resolved values (including secrets) held in memory
// only for the duration of the probe call.
type CandidateConfig struct {
	Kind      string
	ProjectID string // "" for daemon scope
	Values    map[string]string
}

// Prober is the per-kind credential test. Implementations MUST NOT persist,
// log, or echo the credential — Probe runs against the candidate, not the
// saved config, and its only job is answering "would this work?".
type Prober interface {
	Kind() string
	Probe(ctx context.Context, cand CandidateConfig) ProbeResult
}

// redactSecrets scans msg for any of cand.Values and blanks them out. It is
// the shared defense-in-depth guard against a credential leaking into a
// ProbeResult.Detail or a log line via an underlying library's error
// formatting (notably: Go's net/http wraps the full request URL into
// transport errors, and a Telegram getMe URL embeds the bot token in its
// path). Values shorter than minRedactLen are left alone — port numbers and
// other short, non-secret candidate fields would otherwise make ordinary
// error text unreadable, and they carry negligible identifying risk. This is
// a known, accepted gap: a secret shorter than minRedactLen (rare in
// practice — real tokens/passwords far exceed it) would not be masked here.
// Redaction is defense-in-depth only; the primary secret-safety guarantee is
// that probers build Summary/Detail from provider identity, never the
// credential (see ProbeResult), and the primary SSRF defense is DialGuard.
func redactSecrets(msg string, cand CandidateConfig) string {
	const minRedactLen = 4
	for _, v := range cand.Values {
		if len(v) < minRedactLen {
			continue
		}
		msg = strings.ReplaceAll(msg, v, "[redacted]")
	}
	return msg
}

// probeTimeout resolves the effective timeout for a probe call: the
// per-kind override if set, else the package default.
func probeTimeout(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return integrationProbeTimeout
}
