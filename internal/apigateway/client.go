package apigateway

import (
	"context"
	"errors"
)

var (
	// ErrGatewayAuth — the gateway rejected our internal key-auth token (401):
	// misconfiguration, distinct from "not configured" (design §5.1).
	ErrGatewayAuth = errors.New("gateway authentication failed")
	// ErrUpstreamMethod — the gateway has no route/method for this call (404/405):
	// a policy boundary, not a transient failure (design §6.1).
	ErrUpstreamMethod = errors.New("gateway rejected method for route")
	// ErrInvalidPath — the requested path contains a ".." segment. The daemon
	// rejects it as a conservative pre-filter before any network call; the
	// gateway route allowlist remains authoritative (design §5, C2 / review F2).
	ErrInvalidPath = errors.New("query_api: path must not contain '..'")
	// ErrGatewayRequest is a transport/build failure whose underlying error may
	// contain the full request URL and query. Callers surface only this sentinel.
	ErrGatewayRequest = errors.New("gateway request failed")
)

// Request is a provider call the tool asks the gateway to make.
type Request struct {
	Provider string
	Method   string
	Path     string
	Query    map[string]any
	Body     map[string]any
}

// Response is the gateway's reply (already read).
type Response struct {
	Status int
	Body   string
}

// Client is the seam the query_api tool depends on. The concrete
// implementation (GatewayClient) lives in the internal/apigateway/gateway
// subpackage so it can pull in integrations.DialGuard without dragging that
// (transitively dispatcher-importing) dependency into every consumer of this
// interface — dispatcher imports only this integrations-free package.
type Client interface {
	Call(ctx context.Context, req Request) (Response, error)
}

// ProviderLister is an OPTIONAL capability: an enumerable provider catalog
// for the list_apis dispatcher tool (design §5.2, query_api provider-
// discovery). It is deliberately NOT part of Client — adding a method there
// would be a compile-breaking change for every implementer, including the
// test fakes that only ever needed Call. The concrete gateway client
// satisfies this via Registry.Describe(); a fake used only for query_api
// tests need not implement it.
type ProviderLister interface {
	ListProviders() []ProviderInfo
}
