// Package apiaccess is the shared post-auth capability gate for the
// third-party API gateway. It is invoked once a caller (chat human or task
// agent) is already authenticated for a project; it enforces the
// capability policy that is identical across both callers — provider
// required, per-project allowlist, and agent read-only-for-writes — and
// tags a successful response's provenance.
//
// It depends only on apigateway (the gateway seam + registry sentinels)
// and outputguard (provenance tags) — deliberately NOT the vornik
// registry: the per-project allowlist arrives via the injected Allowlist
// resolver so there is no fail-open caller parameter and both callers get
// identical enforcement (design
// https://docs.vornik.io §1).
//
// It TAGS only. It does NOT redact and does NOT byte-cap — those are
// caller-side (the chat dispatcher's downstream output_guard pass; the
// agent endpoint's redaction + cap). Doing them here would double-apply
// on the chat path (design §1/§5b).
package apiaccess

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
)

// maxListProviders caps ListProviders output (design §5.3 step 7): bounds
// the discovery response even when a large allowlist (or an unfiltered
// registry) would otherwise return everything.
const maxListProviders = 50

// Service is a capability gate over an already-authenticated project.
type Service struct {
	// Client is the gateway seam. Required.
	Client apigateway.Client
	// Allowlist loads permissions.api_providers for a project. An empty
	// (or nil) result ⇒ all providers allowed (operator convention,
	// mirrors AllowedTools/AllowedProjects). nil Allowlist ⇒ all
	// providers allowed. An error refuses the call (fail-closed on a
	// resolver failure).
	Allowlist func(projectID string) ([]string, error)
	// AgentWrites is the per-role write grant: it authorizes a write
	// method (non GET/HEAD) for the given project+role. nil ⇒ always
	// read-only (agent default). The chat adapter passes a role-blind
	// closure returning true (defers to the gateway's writes_enabled),
	// preserving chat write behavior (design §1, §5c).
	AgentWrites func(projectID, role string) bool
}

// Outcome is the result of Query. A non-empty Refusal means the call did
// NOT succeed and Refusal is a human-readable reason (NEVER a raw Go
// error). On success Refusal is empty, Body carries the gateway response
// body verbatim (not redacted, not capped), and Provenance is
// ProvenanceThirdParty.
type Outcome struct {
	Body       string
	Provenance outputguard.Provenance
	Refusal    string
}

// Query runs the shared capability gate then calls the gateway. Gate order
// (design §1) is exactly:
//  1. Validate+Resolve: empty provider ⇒ Refusal; default Method to "GET"
//     — BEFORE the allowlist gate, so "" provider can never reach the
//     gateway even under empty⇒all.
//  2. allowlist: provider ∉ allow (empty allow ⇒ all) ⇒ Refusal.
//  3. agent-write policy: a write method is refused unless
//     AgentWrites(project, role).
//  4. Client.Call; on error ⇒ mapped Refusal.
//  5. success ⇒ Outcome{Body, ProvenanceThirdParty}.
func (s *Service) Query(ctx context.Context, projectID, role string, req apigateway.Request) Outcome {
	// 1. Validate + resolve (before the allowlist gate).
	if strings.TrimSpace(req.Provider) == "" {
		return Outcome{Refusal: "`provider` is required."}
	}
	if strings.TrimSpace(req.Method) == "" {
		req.Method = "GET"
	}

	// 2. Per-project allowlist (loaded internally; empty ⇒ all).
	allow, err := s.loadAllowlist(projectID)
	if err != nil {
		return Outcome{Refusal: fmt.Sprintf("could not resolve the API allowlist for project %q: %v", projectID, err)}
	}
	if !providerAllowed(allow, req.Provider) {
		return Outcome{Refusal: fmt.Sprintf("provider %q is not enabled for project %q.", req.Provider, projectID)}
	}

	// 3. Agent read-only policy: a write method needs an explicit grant,
	// independent of the provider's gateway writes_enabled.
	if !isReadMethod(req.Method) {
		if s.AgentWrites == nil || !s.AgentWrites(projectID, role) {
			return Outcome{Refusal: fmt.Sprintf(
				"write method %s on provider %q is not permitted for this caller (read-only).",
				strings.ToUpper(strings.TrimSpace(req.Method)), req.Provider,
			)}
		}
	}

	// 4. Call the gateway (credential injected gateway-side).
	resp, err := s.Client.Call(ctx, req)
	if err != nil {
		return Outcome{Refusal: mapGatewayError(err, req)}
	}

	// 5. Success — tag only. No redaction, no cap (caller-side).
	return Outcome{Body: resp.Body, Provenance: outputguard.ProvenanceThirdParty}
}

// ListProviders returns the project's allowed providers (allowlist loaded
// internally), optionally filtered by a case-insensitive substring match
// over name/description, capped at maxListProviders. If the Client does
// not implement apigateway.ProviderLister it returns (nil, false, nil),
// mirroring the current list_apis behavior. An allowlist resolver error is
// returned.
//
// The second return value (truncated) reports whether the cap actually
// dropped entries — true ONLY when more than maxListProviders providers
// remained after filtering. A result of exactly maxListProviders is NOT
// truncated (nothing was dropped); callers must use this flag, not a
// len()==cap heuristic, to decide whether to surface a "results truncated"
// note (design §5.3 step 7).
func (s *Service) ListProviders(_ context.Context, projectID, query string) ([]apigateway.ProviderInfo, bool, error) {
	pl, ok := s.Client.(apigateway.ProviderLister)
	if s.Client == nil || !ok {
		return nil, false, nil
	}

	allow, err := s.loadAllowlist(projectID)
	if err != nil {
		return nil, false, err
	}

	providers := pl.ListProviders()
	kept := make([]apigateway.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		if providerAllowed(allow, p.Name) {
			kept = append(kept, p)
		}
	}

	if q := strings.ToLower(strings.TrimSpace(query)); q != "" {
		filtered := make([]apigateway.ProviderInfo, 0, len(kept))
		for _, p := range kept {
			if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.Description), q) {
				filtered = append(filtered, p)
			}
		}
		kept = filtered
	}

	truncated := false
	if len(kept) > maxListProviders {
		kept = kept[:maxListProviders]
		truncated = true
	}
	return kept, truncated, nil
}

// loadAllowlist resolves the per-project allowlist via the injected
// resolver. A nil resolver ⇒ nil allowlist (all providers allowed).
func (s *Service) loadAllowlist(projectID string) ([]string, error) {
	if s.Allowlist == nil {
		return nil, nil
	}
	return s.Allowlist(projectID)
}

// providerAllowed reports whether provider is in allow. An empty/nil allow
// means "all providers allowed" (operator convention). Membership is
// case-sensitive, matching apigateway.Registry.Lookup.
func providerAllowed(allow []string, provider string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if a == provider {
			return true
		}
	}
	return false
}

// isReadMethod reports whether method is a read (GET/HEAD). Anything else
// is a write for the agent read-only policy.
func isReadMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD":
		return true
	}
	return false
}

// IsReadMethod is the exported form of isReadMethod so the agent API handler
// can tell reads from writes with the SAME rule the gate uses — the agent-write
// policy (gateway.agent_writes) only resolves/audits/meters origin on writes,
// and reads must classify identically here and in the gate (single source of
// truth).
func IsReadMethod(method string) bool { return isReadMethod(method) }

// mapGatewayError translates the gateway sentinels into clear,
// policy-aware refusal text (design §6.1: a boundary, not a transient
// failure). Credentials are never surfaced — the client already scrubs the
// token from any raw error. The returned string carries NO tool-name
// prefix; each caller prefixes its own ("query_api: " for chat).
func mapGatewayError(err error, req apigateway.Request) string {
	switch {
	case errors.Is(err, apigateway.ErrUnknownProvider):
		return fmt.Sprintf("unknown provider %q — it is not registered.", req.Provider)
	case errors.Is(err, apigateway.ErrMethodNotAllowed), errors.Is(err, apigateway.ErrUpstreamMethod):
		return fmt.Sprintf("provider %q does not support %s on %q (read-only or route not configured).",
			req.Provider, strings.ToUpper(strings.TrimSpace(req.Method)), req.Path)
	case errors.Is(err, apigateway.ErrGatewayAuth):
		return "gateway authentication failed (daemon↔gateway token misconfigured)."
	default:
		return err.Error()
	}
}
