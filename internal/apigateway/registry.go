// Package apigateway holds the daemon-side provider registry, method policy,
// and gateway HTTP client for the query_api tool. Credentials live in the
// gateway (Kong), never here. See
// https://docs.vornik.io
package apigateway

import (
	"errors"
	"sort"
	"strings"
)

var (
	// ErrUnknownProvider — the requested provider is not registered.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrMethodNotAllowed — the daemon method policy refused this method
	// (read-only by default; writes require writes_enabled AND the method
	// listed). This is the conservative pre-filter; the gateway route set is
	// the authoritative allowlist (design §6.1).
	ErrMethodNotAllowed = errors.New("method not allowed by provider policy")
)

// Provider is one registered upstream. BasePath is the gateway path prefix.
type Provider struct {
	BasePath       string
	AllowedMethods []string
	WritesEnabled  bool
	Description    string
	// Examples are sample request lines (e.g. "GET /weather/current?city=Prague")
	// surfaced to the LLM via Registry.Describe(). Optional.
	Examples []string
}

// Registry maps provider name → Provider.
type Registry map[string]Provider

// Lookup returns the provider for name (case-sensitive) and whether it exists.
func (r Registry) Lookup(name string) (Provider, bool) {
	p, ok := r[name]
	return p, ok
}

// ProviderInfo is the non-secret, LLM-facing view of a registered provider.
type ProviderInfo struct {
	Name           string
	Description    string
	AllowedMethods []string
	WritesEnabled  bool
	Examples       []string
}

// Describe returns every registered provider as ProviderInfo, sorted by Name
// (deterministic output for tests + stable agent-facing ordering). Always
// returns a non-nil slice, even for an empty registry.
func (r Registry) Describe() []ProviderInfo {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ProviderInfo, 0, len(names))
	for _, name := range names {
		p := r[name]
		out = append(out, ProviderInfo{
			Name:           name,
			Description:    p.Description,
			AllowedMethods: p.AllowedMethods,
			WritesEnabled:  p.WritesEnabled,
			Examples:       p.Examples,
		})
	}
	return out
}

func isReadMethod(m string) bool {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case "GET", "HEAD":
		return true
	}
	return false
}

// MethodAllowed enforces read-only-by-default. GET/HEAD are always allowed.
// Any other method requires WritesEnabled AND the method present in
// AllowedMethods; otherwise ErrMethodNotAllowed.
func MethodAllowed(p Provider, method string) error {
	m := strings.ToUpper(strings.TrimSpace(method))
	if isReadMethod(m) {
		return nil
	}
	if !p.WritesEnabled {
		return ErrMethodNotAllowed
	}
	for _, a := range p.AllowedMethods {
		if strings.ToUpper(strings.TrimSpace(a)) == m {
			return nil
		}
	}
	return ErrMethodNotAllowed
}
