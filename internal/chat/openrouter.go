package chat

import (
	"context"
	"fmt"
	"strings"
)

// FreeModelSuffix is the tag OpenRouter appends to its zero-cost model
// variants (e.g. "deepseek/deepseek-r1:free"). The router uses it to
// dispatch free requests to OpenRouter regardless of vendor prefix, and
// the free-only guard uses it to reject anything that would bill.
const FreeModelSuffix = ":free"

// IsFreeModel reports whether a model ID is an OpenRouter free-tier
// variant — i.e. ends in ":free".
func IsFreeModel(model string) bool {
	return strings.HasSuffix(model, FreeModelSuffix)
}

// ErrNonFreeModel is returned by a free-only provider when a request
// targets a model that isn't an OpenRouter `:free` variant. It carries
// the offending model so the caller's logs name the misconfiguration.
type ErrNonFreeModel struct {
	Model string
}

func (e *ErrNonFreeModel) Error() string {
	return fmt.Sprintf("openrouter free_only: refusing non-free model %q (free model IDs end in %q)", e.Model, FreeModelSuffix)
}

// freeOnlyProvider wraps a Provider and refuses any completion whose
// effective model is not an OpenRouter `:free` variant, without making a
// network call. It exists so an operator who wants OpenRouter strictly
// for the free tier can't accidentally start spending — a role pinned to
// a paid model fails loudly instead of billing silently.
//
// The guard checks at call time against the inner provider's Model(),
// which the router has already pinned via WithModel for per-request
// overrides. WithModel returns another freeOnlyProvider so the guard
// survives the dispatch path.
//
// The wrapper always satisfies ModelOverridable, Pinger, and ModelLister,
// delegating to the inner provider when it supports them and degrading
// gracefully otherwise (ready-by-construction ping, empty catalogue). In
// production the inner is always *Client, which implements all three;
// the always-satisfy contract just keeps the router's type assertions
// honest.
type freeOnlyProvider struct {
	inner Provider
}

// NewFreeOnlyProvider wraps inner in a free-tier guard. Returns a Provider
// that also implements ModelOverridable / Pinger / ModelLister.
func NewFreeOnlyProvider(inner Provider) Provider {
	return &freeOnlyProvider{inner: inner}
}

// guard returns ErrNonFreeModel when the effective model isn't a `:free`
// variant. The empty model is treated as non-free — a free-only provider
// with no model pinned is a misconfiguration worth surfacing rather than
// silently forwarding to whatever default the inner carries.
func (p *freeOnlyProvider) guard() error {
	m := p.inner.Model()
	if !IsFreeModel(m) {
		return &ErrNonFreeModel{Model: m}
	}
	return nil
}

func (p *freeOnlyProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	if err := p.guard(); err != nil {
		return nil, err
	}
	return p.inner.Complete(ctx, messages)
}

func (p *freeOnlyProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	if err := p.guard(); err != nil {
		return nil, err
	}
	return p.inner.CompleteWithTools(ctx, messages, tools)
}

func (p *freeOnlyProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	if err := p.guard(); err != nil {
		return nil, err
	}
	return p.inner.CompleteWithToolsStream(ctx, messages, tools, onText)
}

func (p *freeOnlyProvider) Model() string         { return p.inner.Model() }
func (p *freeOnlyProvider) SetMetrics(m *Metrics) { p.inner.SetMetrics(m) }

// WithModel pins the requested model and re-wraps so the guard propagates
// through the router's per-request dispatch. When the inner provider
// isn't ModelOverridable, the model can't change — wrap the inner as-is.
func (p *freeOnlyProvider) WithModel(model string) Provider {
	if o, ok := p.inner.(ModelOverridable); ok {
		return &freeOnlyProvider{inner: o.WithModel(model)}
	}
	return &freeOnlyProvider{inner: p.inner}
}

// Ping delegates to the inner provider's readiness probe when it has one;
// a non-pingable inner is ready by construction.
func (p *freeOnlyProvider) Ping(ctx context.Context) error {
	if pg, ok := p.inner.(Pinger); ok {
		return pg.Ping(ctx)
	}
	return nil
}

// ListModels delegates to the inner provider's catalogue when it has one;
// a non-listing inner contributes nothing to model discovery.
func (p *freeOnlyProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if ml, ok := p.inner.(ModelLister); ok {
		return ml.ListModels(ctx)
	}
	return nil, nil
}

// Compile-time conformance checks.
var (
	_ Provider         = (*freeOnlyProvider)(nil)
	_ ModelOverridable = (*freeOnlyProvider)(nil)
	_ Pinger           = (*freeOnlyProvider)(nil)
	_ ModelLister      = (*freeOnlyProvider)(nil)
)
