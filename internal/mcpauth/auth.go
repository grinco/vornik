// Package mcpauth holds the configuration surface Vornik uses to authenticate
// to an MCP server, and to hand a stdio MCP subprocess its own upstream
// credentials.
//
// Design: https://docs.vornik.io
//
// # Dependency discipline
//
// This package is a LEAF and must stay one. `internal/registry` and
// `internal/config` both embed Auth, and both are deliberately lean — pulling
// persistence, HTTP or the MCP client in here would drag those dependencies
// into every consumer of a project or daemon config. When the OAuth token
// store lands (design §6), its interface may live here but its Postgres /
// SQLite implementation belongs in internal/persistence, following the
// repository pattern the rest of the tree uses.
//
// It also has no dependency on internal/mcp (the reverse is fine): the client
// is driven by the wiring layer, not the other way round.
package mcpauth

import (
	"fmt"
	"sort"
	"strings"
)

// Auth modes. A closed enum, validated at config load so a typo fails at boot
// rather than at the first tool call.
const (
	// ModeNone is the zero value's meaning: no authentication, which is
	// byte-for-byte the behaviour of every mcp.servers entry written before
	// this feature existed.
	ModeNone = "none"
	// ModeOAuth is spec OAuth 2.1 against a remote MCP server acting as a
	// resource server (design §2.1).
	ModeOAuth = "oauth"
	// ModeStatic is a fixed header carrying a bearer token or API key —
	// n8n's MCP Server Trigger, and any server that predates OAuth support.
	ModeStatic = "static"
	// ModeEnv injects credentials into a stdio subprocess's environment.
	// The MCP server holds its OWN upstream app credentials this way
	// (YouTube, Reddit, Instagram wrappers); Vornik runs no handshake.
	ModeEnv = "env"
)

// SecretRefPrefix marks a credential field as a REFERENCE to the secret store
// rather than a literal. It is the only accepted syntax in an auth block
// (design §4): `${ENV}` is rejected because it resolves from the daemon
// process environment — the wrong scope for a per-project credential, and a
// bypass of the permissions.secrets allowlist.
const SecretRefPrefix = "secret://"

// Auth configures how Vornik authenticates to an MCP server, or how it
// supplies upstream credentials to a stdio MCP subprocess.
//
// The zero value is mode "none". Every credential field holds a `secret://`
// reference, never a literal — that invariant is what lets an auth block
// travel through the control-plane proposal ledger as a reviewable diff
// without leaking anything.
type Auth struct {
	// Mode is none | oauth | static | env.
	Mode string `yaml:"mode" json:"mode,omitempty"`

	// --- mode: oauth ---

	// Scopes requested at authorization. Empty means the protected-resource
	// metadata's advertised scopes_supported.
	Scopes []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	// ClientID is set when the authorization server offers no dynamic client
	// registration (Slack, GitHub, Box) or when an operator prefers a
	// pre-registered client. Empty means attempt DCR.
	ClientID string `yaml:"client_id,omitempty" json:"client_id,omitempty"`
	// ClientSecretFrom is required for a confidential client (Slack's AS
	// accepts only client_secret_post).
	ClientSecretFrom string `yaml:"client_secret_from,omitempty" json:"client_secret_from,omitempty"`
	// AuthorizationEndpoint and TokenEndpoint override discovery entirely,
	// for a server that publishes no protected-resource metadata at any
	// well-known path (Intercom). Both must be set together.
	AuthorizationEndpoint string `yaml:"authorization_endpoint,omitempty" json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `yaml:"token_endpoint,omitempty" json:"token_endpoint,omitempty"`

	// --- mode: static ---

	// Header is the header to carry the credential. Empty means
	// "Authorization".
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	// ValueFrom is the secret:// reference holding the credential.
	ValueFrom string `yaml:"value_from,omitempty" json:"value_from,omitempty"`
	// ValuePrefix is prepended to the resolved value, e.g. "Bearer ".
	ValuePrefix string `yaml:"value_prefix,omitempty" json:"value_prefix,omitempty"`

	// --- mode: env (stdio only) ---

	// EnvFrom maps subprocess environment variable name -> secret://
	// reference. Values are resolved immediately before exec and never
	// written to config.
	EnvFrom map[string]string `yaml:"env_from,omitempty" json:"env_from,omitempty"`
}

// EffectiveMode returns the configured mode, resolving the empty string to
// ModeNone so callers never have to special-case the zero value.
func (a Auth) EffectiveMode() string {
	if strings.TrimSpace(a.Mode) == "" {
		return ModeNone
	}
	return a.Mode
}

// IsZero reports whether this block asks for nothing at all.
func (a Auth) IsZero() bool {
	return a.EffectiveMode() == ModeNone &&
		len(a.Scopes) == 0 && a.ClientID == "" && a.ClientSecretFrom == "" &&
		a.AuthorizationEndpoint == "" && a.TokenEndpoint == "" &&
		a.Header == "" && a.ValueFrom == "" && a.ValuePrefix == "" &&
		len(a.EnvFrom) == 0
}

// SecretRefs returns the secret-store names this block references, sorted and
// deduplicated. It is what the permissions.secrets allowlist is checked
// against, so it must enumerate EVERY credential field — a field it forgets is
// a credential that escapes the grant. A value that is not a well-formed
// reference is omitted deliberately: it is a validation error elsewhere, and
// reporting it here would let an invalid config satisfy an allowlist check for
// a secret it can never resolve.
func (a Auth) SecretRefs() []string {
	seen := make(map[string]struct{})
	add := func(v string) {
		if name, ok := ParseSecretRef(v); ok {
			seen[name] = struct{}{}
		}
	}
	add(a.ClientSecretFrom)
	add(a.ValueFrom)
	for _, v := range a.EnvFrom {
		add(v)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ValidateSecretGrants reports an error when this block references a secret
// the caller was not granted. `allowed` is the project's permissions.secrets
// list.
//
// Deny by default: an empty list grants nothing. That deliberately breaks with
// the "empty means all" convention used by allowedTools and api_providers,
// because this is a credential boundary rather than a catalog filter — and
// because auth blocks are net-new, so no existing deployment can be broken by
// the stricter reading.
func (a Auth) ValidateSecretGrants(allowed []string) error {
	refs := a.SecretRefs()
	if len(refs) == 0 {
		return nil
	}
	granted := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		granted[strings.TrimSpace(name)] = struct{}{}
	}
	for _, ref := range refs {
		if _, ok := granted[ref]; !ok {
			return fmt.Errorf("references secret %q, which is not in the project's permissions.secrets allowlist", ref)
		}
	}
	return nil
}

// ParseSecretRef extracts the secret name from a `secret://<name>` reference.
// It returns ok=false for anything else — a bare literal, a `${ENV}`
// placeholder, or a name containing characters a secret name cannot hold.
// Making this total is the point: there is no "is this a placeholder or a
// literal?" heuristic in the middle.
func ParseSecretRef(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, SecretRefPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(v, SecretRefPrefix)
	if name == "" || !validSecretName(name) {
		return "", false
	}
	return name, true
}

// validSecretName accepts the character set an env-backed secret name can
// actually have. Rejecting the rest here is what keeps a reference from
// carrying a path traversal, whitespace, or a smuggled header line.
func validSecretName(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}
