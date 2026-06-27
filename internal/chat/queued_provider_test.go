package chat

import (
	"context"
	"errors"
	"testing"
)

// simpleStubProvider is a minimal Provider implementation that records
// each call so the test can assert delegation.
type simpleStubProvider struct {
	model         string
	completes     int
	tools         int
	streams       int
	modelSet      int
	withModelHits int
	listModelsHit int
	pingHit       int
	pingErr       error
	listModelsErr error
	models        []ModelInfo
}

func (s *simpleStubProvider) Complete(_ context.Context, _ []Message) (*ChatResponse, error) {
	s.completes++
	return &ChatResponse{Model: s.model}, nil
}

func (s *simpleStubProvider) CompleteWithTools(_ context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	s.tools++
	return &ChatResponse{Model: s.model}, nil
}

func (s *simpleStubProvider) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	s.streams++
	return &ChatResponse{Model: s.model}, nil
}

func (s *simpleStubProvider) Model() string         { return s.model }
func (s *simpleStubProvider) SetMetrics(_ *Metrics) { s.modelSet++ }

func (s *simpleStubProvider) WithModel(m string) Provider {
	s.withModelHits++
	clone := *s
	clone.model = m
	clone.withModelHits = 0 // counter is per-instance; clear on clone
	return &clone
}

func (s *simpleStubProvider) ListModels(_ context.Context) ([]ModelInfo, error) {
	s.listModelsHit++
	return s.models, s.listModelsErr
}

func (s *simpleStubProvider) Ping(_ context.Context) error {
	s.pingHit++
	return s.pingErr
}

// TestQueuedProvider_DelegationSurface covers every Provider method on
// QueuedProvider including WithModel + ListModels + Ping + the
// nil-maxConcurrent shortcircuit.
func TestQueuedProvider_DelegationSurface(t *testing.T) {
	// maxConcurrent ≤ 0 returns the original provider unchanged.
	original := &simpleStubProvider{model: "m"}
	if got := NewQueuedProvider(original, 0); got != original {
		t.Error("maxConcurrent=0 should return underlying provider")
	}
	if got := NewQueuedProvider(nil, 1); got != nil {
		t.Error("nil provider should return nil")
	}

	stub := &simpleStubProvider{model: "m", models: []ModelInfo{{ID: "m"}}}
	qp := NewQueuedProvider(stub, 1)

	if qp.Model() != "m" {
		t.Errorf("Model = %q", qp.Model())
	}
	qp.SetMetrics(nil)
	if stub.modelSet != 1 {
		t.Errorf("SetMetrics passes through; modelSet = %d", stub.modelSet)
	}

	// Complete / CompleteWithTools / CompleteWithToolsStream all delegate.
	if _, err := qp.Complete(context.Background(), nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := qp.CompleteWithTools(context.Background(), nil, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if _, err := qp.CompleteWithToolsStream(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("CompleteWithToolsStream: %v", err)
	}
	if stub.completes != 1 || stub.tools != 1 || stub.streams != 1 {
		t.Errorf("delegation counts wrong: %d/%d/%d", stub.completes, stub.tools, stub.streams)
	}

	// WithModel returns a clone wrapping the new sub.
	clone := qp.(ModelOverridable).WithModel("m2")
	if clone.Model() != "m2" {
		t.Errorf("WithModel.Model = %q", clone.Model())
	}

	// ListModels delegates.
	ml, err := qp.(ModelLister).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ml) != 1 {
		t.Errorf("ListModels = %d", len(ml))
	}
	if stub.listModelsHit != 1 {
		t.Errorf("ListModels passthrough = %d", stub.listModelsHit)
	}

	// Ping delegates.
	if err := qp.(Pinger).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if stub.pingHit != 1 {
		t.Errorf("Ping passthrough = %d", stub.pingHit)
	}

	// Ping error propagates.
	stub.pingErr = errors.New("boom")
	if err := qp.(Pinger).Ping(context.Background()); err == nil {
		t.Error("expected Ping error")
	}
}

// TestQueuedProvider_ListModelsAggregated covers the *Router-aware
// surface used by the API handler.
func TestQueuedProvider_ListModelsAggregated(t *testing.T) {
	stub := &simpleStubProvider{model: "m"}
	qp := NewQueuedProvider(stub, 1)
	if _, ok := qp.(*QueuedProvider).ListModelsAggregated(context.Background()); ok {
		t.Error("non-Router-wrapped queue should return ok=false")
	}

	// Wrap a Router.
	r, err := NewRouter(stub, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	wrapped := NewQueuedProvider(r, 1).(*QueuedProvider)
	if _, ok := wrapped.ListModelsAggregated(context.Background()); !ok {
		t.Error("Router-wrapped queue should return ok=true")
	}
}

// TestQueuedProvider_CompleteAndPriorityDefaults exercises a queued
// Complete and the requestPriority defaults.
func TestQueuedProvider_CompleteAndPriorityDefaults(t *testing.T) {
	stub := &simpleStubProvider{model: "m"}
	qp := NewQueuedProvider(stub, 1)

	ctx := WithRequestPriority(context.Background(), 5)
	if _, err := qp.Complete(ctx, []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Without a priority context, requestPriority defaults.
	if got := requestPriority(context.Background()); got != defaultRequestPriority {
		t.Errorf("default priority = %d", got)
	}
	if got := requestPriority(context.TODO()); got != defaultRequestPriority {
		t.Errorf("nil ctx priority = %d", got)
	}
}

// TestQueuedProvider_WithModelNonOverridable returns the same provider
// when the underlying doesn't implement ModelOverridable.
type bareProvider struct{}

func (b *bareProvider) Complete(_ context.Context, _ []Message) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (b *bareProvider) CompleteWithTools(_ context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (b *bareProvider) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (b *bareProvider) Model() string         { return "bare" }
func (b *bareProvider) SetMetrics(_ *Metrics) {}

func TestQueuedProvider_BareProvider(t *testing.T) {
	qp := NewQueuedProvider(&bareProvider{}, 1)
	// WithModel on a non-overridable underlying returns the queue itself.
	if got := qp.(ModelOverridable).WithModel("x"); got != qp {
		t.Error("non-overridable underlying should return the queue itself")
	}
	// ListModels on a non-lister returns (nil, nil).
	ml, err := qp.(ModelLister).ListModels(context.Background())
	if err != nil {
		t.Errorf("ListModels: %v", err)
	}
	if ml != nil {
		t.Errorf("ListModels should be nil, got %v", ml)
	}
	// Ping on a non-pinger returns nil.
	if err := qp.(Pinger).Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
