package mcpauth

import (
	"errors"
	"fmt"
	"os"
)

// ErrSecretUnresolved reports that a secret:// reference names a secret the
// store does not hold (or holds empty). Distinguishable so the wiring layer can
// tell a missing credential apart from a malformed config.
var ErrSecretUnresolved = errors.New("mcp auth secret unresolved")

// ErrOAuthNotWired reports that mode: oauth is configured but the OAuth flow
// has not shipped yet (design §11 steps 3-5). The config surface lands first
// deliberately, so an operator can write the block and validate it before the
// machinery exists. Sentinel rather than a plain error so the wiring layer logs
// once and leaves the server connectable (unauthenticated, which is honest)
// instead of dropping it.
var ErrOAuthNotWired = errors.New("mcp auth mode oauth is configured but OAuth support is not wired yet")

// SecretSource resolves a secret name to its value. ok is false when the name
// is unknown OR holds an empty value — an empty credential would be sent as
// "Bearer " and read as a server-side auth bug, so it is treated as absent.
type SecretSource interface {
	Get(name string) (string, bool)
}

// EnvSecretSource reads secrets from the process environment, which is where
// the daemon's secret store lands them: the daemon loads *.env from its secrets
// dir at boot, and projectdoctor.EnvSecrets.Set os.Setenv's a freshly-supplied
// value so it is live without a restart.
type EnvSecretSource struct{}

// Get implements SecretSource.
func (EnvSecretSource) Get(name string) (string, bool) {
	v := os.Getenv(name)
	return v, v != ""
}

// Grants says which secrets a caller may resolve.
type Grants struct {
	// Allowed is the project's permissions.secrets list. Empty grants
	// nothing (deny by default).
	Allowed []string
	// Unrestricted skips the allowlist. Set ONLY for daemon-scope servers
	// (config.yaml's mcp.servers), which are admin-configured and have no
	// project allowlist to check against.
	Unrestricted bool
}

// Injection is the resolved credential material, ready for the MCP client.
// Headers are attached to every HTTP request for that server; Env is merged
// into a stdio subprocess's environment. Exactly one of them is ever populated
// — a credential for a subprocess has no request to ride on, and vice versa.
type Injection struct {
	Headers map[string]string
	Env     map[string]string
}

// IsEmpty reports whether there is nothing to inject.
func (i Injection) IsEmpty() bool { return len(i.Headers) == 0 && len(i.Env) == 0 }

// Resolve turns an auth block into credential material.
//
// It re-validates first: config load is the primary gate, but a server can
// reach the wiring layer through a path that never validated (a name-only entry
// inheriting its transport, a test harness, a future caller), and injecting
// half of a malformed block would be worse than refusing it.
//
// Returns ErrOAuthNotWired for mode: oauth, ErrSecretUnresolved for a missing
// secret, and a plain error for a malformed block or an ungranted reference.
// No returned error ever contains a resolved secret value.
func Resolve(a Auth, transport string, src SecretSource, g Grants) (Injection, error) {
	if err := a.Validate(transport); err != nil {
		return Injection{}, err
	}
	if !g.Unrestricted {
		if err := a.ValidateSecretGrants(g.Allowed); err != nil {
			return Injection{}, err
		}
	}

	switch a.EffectiveMode() {
	case ModeNone:
		return Injection{}, nil

	case ModeOAuth:
		return Injection{}, ErrOAuthNotWired

	case ModeStatic:
		name, _ := ParseSecretRef(a.ValueFrom) // validated above
		value, ok := src.Get(name)
		if !ok {
			return Injection{}, fmt.Errorf("%w: value_from names secret %q, which the secret store does not hold", ErrSecretUnresolved, name)
		}
		header := a.Header
		if header == "" {
			header = "Authorization"
		}
		return Injection{Headers: map[string]string{header: a.ValuePrefix + value}}, nil

	case ModeEnv:
		env := make(map[string]string, len(a.EnvFrom))
		for varName, ref := range a.EnvFrom {
			name, _ := ParseSecretRef(ref) // validated above
			value, ok := src.Get(name)
			if !ok {
				return Injection{}, fmt.Errorf("%w: env_from[%q] names secret %q, which the secret store does not hold", ErrSecretUnresolved, varName, name)
			}
			env[varName] = value
		}
		return Injection{Env: env}, nil
	}
	return Injection{}, nil
}
