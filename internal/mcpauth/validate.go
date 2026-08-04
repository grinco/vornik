package mcpauth

import (
	"fmt"
	"strings"
)

// reservedHeaders are the headers the MCP protocol owns. internal/mcp drops a
// configured header colliding with one of these (applyConfigHeaders), so an
// auth block naming one would silently never be sent — the "works in config,
// fails at runtime with no explanation" shape this validation exists to
// prevent.
//
// Duplicated rather than imported because this package must not depend on
// internal/mcp (see the package doc). internal/mcp owns the authoritative set
// and pins the two together with a drift test.
var reservedHeaders = map[string]struct{}{
	"content-type":         {},
	"accept":               {},
	"mcp-protocol-version": {},
	"mcp-session-id":       {},
}

// ReservedHeaderNames returns the protocol-owned header names, lower case. Used
// by internal/mcp's drift test to prove the two lists agree.
func ReservedHeaderNames() []string {
	out := make([]string, 0, len(reservedHeaders))
	for h := range reservedHeaders {
		out = append(out, h)
	}
	return out
}

// Validate checks an auth block against the transport it is configured on.
//
// It implements the design's §4 rules — mode enum, transport/mode pairing,
// endpoint pairing, and references-not-literals — plus four fail-loud
// additions, each of which catches a mistake whose runtime symptom is silence
// rather than an error:
//
//   - a static header colliding with a protocol-owned header (dropped by the
//     client, so the request goes out unauthenticated);
//   - a header name, env var name, or value prefix that cannot legally be sent
//     (CR/LF would corrupt every request from that server);
//   - an env var in the daemon's own VORNIK_ namespace;
//   - a field belonging to a different mode, which is always a typo or a
//     leftover from an edit and does nothing at all today.
//
// `transport` may be empty for a name-only project entry that inherits its
// connection details from the daemon catalog. In that case the
// transport-dependent rules are skipped — the wiring layer resolves the
// inherited transport and validates again — but every transport-independent
// rule still applies, so a malformed block cannot hide behind inheritance.
//
// Error messages name the offending FIELD and never echo its VALUE: they reach
// boot logs and the control-plane proposal UI, where quoting a rejected
// literal would publish the very credential the rule exists to keep out.
func (a Auth) Validate(transport string) error {
	mode := a.EffectiveMode()
	switch mode {
	case ModeNone, ModeOAuth, ModeStatic, ModeEnv:
	default:
		return fmt.Errorf("unknown mode %q (want one of: %s, %s, %s, %s)",
			a.Mode, ModeNone, ModeOAuth, ModeStatic, ModeEnv)
	}

	if err := a.validateStrayFields(mode); err != nil {
		return err
	}
	// A transport-dependent rule can only fire when the transport is known.
	// An empty transport means a name-only project entry whose connection
	// details (including the transport) are inherited at wiring time, where
	// Resolve validates again with the resolved value.
	transportKnown := strings.TrimSpace(transport) != ""
	isStdio := transport == "stdio"

	switch mode {
	case ModeStatic:
		if transportKnown && isStdio {
			return fmt.Errorf("mode %q is not valid on the stdio transport: a subprocess has no HTTP request to carry a header (use mode %q)", ModeStatic, ModeEnv)
		}
		return a.validateStatic()
	case ModeEnv:
		if transportKnown && !isStdio {
			return fmt.Errorf("mode %q is only valid on the stdio transport: there is no subprocess to give an environment to (use mode %q or %q)", ModeEnv, ModeStatic, ModeOAuth)
		}
		return a.validateEnv()
	case ModeOAuth:
		if transportKnown && isStdio {
			return fmt.Errorf("mode %q is not valid on the stdio transport: the MCP spec says a stdio client should read credentials from its environment (use mode %q)", ModeOAuth, ModeEnv)
		}
		return a.validateOAuth()
	}
	return nil // ModeNone
}

func (a Auth) validateStatic() error {
	if strings.TrimSpace(a.ValueFrom) == "" {
		return fmt.Errorf("value_from is required for mode %q", ModeStatic)
	}
	if _, ok := ParseSecretRef(a.ValueFrom); !ok {
		return fmt.Errorf("value_from must be a secret:// reference (got a literal or ${ENV} placeholder; write value_from: secret://<name> and put the value in the secret store)")
	}
	if h := strings.TrimSpace(a.Header); h != "" {
		if !validHeaderName(h) {
			return fmt.Errorf("header %q is not a valid HTTP header name", h)
		}
		if _, reserved := reservedHeaders[strings.ToLower(h)]; reserved {
			return fmt.Errorf("header %q is protocol-owned and would be dropped before the request is sent; choose another header", h)
		}
	}
	if strings.ContainsAny(a.ValuePrefix, "\r\n") {
		return fmt.Errorf("value_prefix must not contain CR or LF")
	}
	// value_prefix is the one credential-ADJACENT field that holds a literal,
	// so it is the one place the "config holds references, never secrets"
	// invariant could be defeated by pasting a token into it
	// (review-20260804-350e finding 3). A real prefix is a scheme word:
	// "Bearer ", "Token ", "ApiKey ". A length cap is a deterministic rule
	// rather than a token-shaped heuristic — no false negatives to reason
	// about, and the error says where the value belongs.
	if len(a.ValuePrefix) > maxValuePrefixLen {
		return fmt.Errorf("value_prefix is longer than %d characters, which suggests the credential itself was pasted into it; it holds only a scheme word such as %q, and the credential belongs in the secret named by value_from", maxValuePrefixLen, "Bearer ")
	}
	return nil
}

// maxValuePrefixLen bounds the static-auth prefix. The longest real-world
// prefix is a handful of characters; anything longer is a mistake.
const maxValuePrefixLen = 32

func (a Auth) validateEnv() error {
	if len(a.EnvFrom) == 0 {
		return fmt.Errorf("env_from is required for mode %q", ModeEnv)
	}
	for name, ref := range a.EnvFrom {
		if !validEnvName(name) {
			return fmt.Errorf("env_from key %q is not a valid environment variable name", name)
		}
		if strings.HasPrefix(name, "VORNIK_") {
			return fmt.Errorf("env_from key %q must not use the VORNIK_ prefix, which is reserved for the daemon's own environment", name)
		}
		if _, ok := ParseSecretRef(ref); !ok {
			return fmt.Errorf("env_from[%q] must be a secret:// reference (got a literal or ${ENV} placeholder)", name)
		}
	}
	return nil
}

func (a Auth) validateOAuth() error {
	authSet := strings.TrimSpace(a.AuthorizationEndpoint) != ""
	tokenSet := strings.TrimSpace(a.TokenEndpoint) != ""
	if authSet != tokenSet {
		return fmt.Errorf("authorization_endpoint and token_endpoint must be set together (they replace discovery entirely, so half of the pair is unusable)")
	}
	for field, ep := range map[string]string{
		"authorization_endpoint": a.AuthorizationEndpoint,
		"token_endpoint":         a.TokenEndpoint,
	} {
		if ep != "" && !strings.HasPrefix(ep, "https://") {
			return fmt.Errorf("%s must be https", field)
		}
	}
	if strings.TrimSpace(a.ClientSecretFrom) == "" {
		return nil
	}
	if _, ok := ParseSecretRef(a.ClientSecretFrom); !ok {
		return fmt.Errorf("client_secret_from must be a secret:// reference (got a literal or ${ENV} placeholder)")
	}
	if strings.TrimSpace(a.ClientID) == "" {
		return fmt.Errorf("client_id is required alongside client_secret_from: a dynamically registered client receives its own secret, so a configured secret with no configured id cannot be used")
	}
	return nil
}

// validateStrayFields rejects a field that belongs to a different mode. Such a
// field is silently ignored at runtime, which makes a typo'd or half-edited
// block look like it works.
func (a Auth) validateStrayFields(mode string) error {
	type field struct {
		name string
		set  bool
		// modes that legitimately use this field
		modes []string
	}
	fields := []field{
		{"scopes", len(a.Scopes) > 0, []string{ModeOAuth}},
		{"client_id", strings.TrimSpace(a.ClientID) != "", []string{ModeOAuth}},
		{"client_secret_from", strings.TrimSpace(a.ClientSecretFrom) != "", []string{ModeOAuth}},
		{"authorization_endpoint", strings.TrimSpace(a.AuthorizationEndpoint) != "", []string{ModeOAuth}},
		{"token_endpoint", strings.TrimSpace(a.TokenEndpoint) != "", []string{ModeOAuth}},
		{"header", strings.TrimSpace(a.Header) != "", []string{ModeStatic}},
		{"value_from", strings.TrimSpace(a.ValueFrom) != "", []string{ModeStatic}},
		{"value_prefix", a.ValuePrefix != "", []string{ModeStatic}},
		{"env_from", len(a.EnvFrom) > 0, []string{ModeEnv}},
	}
	for _, f := range fields {
		if !f.set {
			continue
		}
		valid := false
		for _, m := range f.modes {
			if m == mode {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("%s is not valid for mode %q (it applies to mode %q)",
				f.name, mode, strings.Join(f.modes, "/"))
		}
	}
	return nil
}

// validHeaderName accepts RFC 7230 token characters, which is what
// http.Header.Set will transmit unmangled.
func validHeaderName(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return s != ""
}

// validEnvName accepts the POSIX-portable environment variable name shape.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
