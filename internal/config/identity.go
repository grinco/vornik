package config

import (
	"fmt"
	"strings"
	"time"
)

// IdentityConfig is the CE identity core — user accounts, groups, channel
// bindings (Telegram, Slack, …) and key→account ownership. It is the
// Community half of the split the operator fixed on 2026-09-13: accounts and
// channel/key mapping ship in CE; federated login (OIDC/SSO providers under
// auth.providers) stays Enterprise. Design of record:
// https://docs.vornik.io §2,
// §4, §5 and the normative review amendments R1–R5.
//
// The block is deliberately small. Identity STORAGE is always present (the
// tables ship in both stores); what this gates is whether the chat channels
// AUTHORISE through the CE resolver and whether key ownership is honoured.
type IdentityConfig struct {
	// Enabled turns the CE identity core on: chat channels resolve every
	// inbound sender through the account resolver, `/account link` is
	// served, and an API key's owner participates in authorisation. Off is
	// byte-identical to the pre-2026-09-13 daemon (hand-maintained
	// allowlists, unowned keys).
	Enabled bool `yaml:"enabled" doc:"Turn the CE identity core on: channel senders resolve to accounts, /account link is served, key ownership is honoured."`

	// ChannelCompat selects how a chat sender that is NOT linked to an
	// account is treated once Enabled is true (review R4):
	//   strict  — refused; only the bounded /account and help handling is
	//             reachable (the default for new installs).
	//   legacy  — a sender explicitly listed in the channel's legacy
	//             allowlist (telegram.allowed_users, slack.*) keeps its old
	//             ORDINARY-CHAT permissions only. Never opens the assistant
	//             door; an empty allowlist never grants. Counted by
	//             vornik_auth_legacy_allowlist_grants_total so the cutover
	//             can be measured.
	ChannelCompat string `yaml:"channel_compat" doc:"Unlinked chat senders: strict (refuse; default) or legacy (explicit allowlist entries keep ordinary-chat permissions during migration)."`

	// LinkCodeTTL is how long a /account link code stays redeemable. The
	// table's schema (migration 90) was written for a 10-minute code; this
	// keeps that default and lets an operator tighten it.
	LinkCodeTTL string `yaml:"link_code_ttl" doc:"Lifetime of an account-link code (duration string; default 10m)."`

	// LinkCodeRatePerHour caps link-code ISSUANCE per account and REDEMPTION
	// attempts per channel sender, per hour (review R4 rate limits).
	LinkCodeRatePerHour int `yaml:"link_code_rate_per_hour" doc:"Per-account issuance and per-sender redemption cap for link codes, per hour (default 10)."`
}

// Identity channel-compat modes.
const (
	IdentityCompatStrict = "strict"
	IdentityCompatLegacy = "legacy"

	IdentityDefaultLinkCodeTTL         = "10m"
	IdentityDefaultLinkCodeRatePerHour = 10
)

// EffectiveLinkCodeTTL returns the link-code lifetime with the default
// applied to an empty or unparseable value.
func (c IdentityConfig) EffectiveLinkCodeTTL() time.Duration {
	return parseDurationOr(c.LinkCodeTTL, 10*time.Minute)
}

// EffectiveChannelCompat returns the compat mode with the strict default
// applied to an empty value.
func (c IdentityConfig) EffectiveChannelCompat() string {
	if strings.TrimSpace(c.ChannelCompat) == "" {
		return IdentityCompatStrict
	}
	return strings.ToLower(strings.TrimSpace(c.ChannelCompat))
}

// LegacyCompat reports whether the legacy allowlist compat mode is active.
func (c IdentityConfig) LegacyCompat() bool {
	return c.EffectiveChannelCompat() == IdentityCompatLegacy
}

// applyDefaults fills zero fields with the shipped defaults.
func (c *IdentityConfig) applyDefaults() {
	if strings.TrimSpace(c.LinkCodeTTL) == "" {
		c.LinkCodeTTL = IdentityDefaultLinkCodeTTL
	}
	if c.LinkCodeRatePerHour <= 0 {
		c.LinkCodeRatePerHour = IdentityDefaultLinkCodeRatePerHour
	}
}

// Validate checks the identity block. An empty block is valid (feature off).
func (c IdentityConfig) Validate() error {
	switch c.EffectiveChannelCompat() {
	case IdentityCompatStrict, IdentityCompatLegacy:
	default:
		return fmt.Errorf("identity.channel_compat: %q is not one of strict|legacy", c.ChannelCompat)
	}
	if v := strings.TrimSpace(c.LinkCodeTTL); v != "" {
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("identity.link_code_ttl: %w", err)
		}
	}
	if c.LinkCodeRatePerHour < 0 {
		return fmt.Errorf("identity.link_code_rate_per_hour: must be >= 0")
	}
	return nil
}
