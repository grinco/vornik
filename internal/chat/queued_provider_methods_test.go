// Coverage for the post-construction surface of QueuedProvider —
// Model / SetMetrics / WithModel / ListModels / Ping /
// ListModelsAggregated. The bedrock of the queue (do/pop/worker)
// is exercised by priority_queue_test.go's TestQueuedProviderOrdersBacklogByPriority.

package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

// queueListingStub is a Provider that also implements ModelLister + Pinger
// + ModelOverridable so the queued-wrapper delegators have something
// to forward to.
type queueListingStub struct {
	namedStubProvider
	listed     []ModelInfo
	listErr    error
	pingErr    error
	withCalls  int
	setMetrics bool
}

func (s *queueListingStub) ListModels(_ context.Context) ([]ModelInfo, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]ModelInfo, len(s.listed))
	copy(out, s.listed)
	return out, nil
}

func (s *queueListingStub) Ping(_ context.Context) error { return s.pingErr }
func (s *queueListingStub) SetMetrics(_ *Metrics)        { s.setMetrics = true }
func (s *queueListingStub) WithModel(m string) Provider {
	s.withCalls++
	clone := *s
	clone.lastModel = m
	return &clone
}

// non-ModelLister / non-Pinger / non-ModelOverridable stub for the
// "underlying doesn't implement X" branches.
type plainProvider struct{ namedStubProvider }

func TestQueuedProvider_NilWhenMaxConcurrentZero(t *testing.T) {
	inner := &plainProvider{namedStubProvider{name: "inner"}}
	got := NewQueuedProvider(inner, 0)
	if got != inner {
		t.Errorf("maxConcurrent<=0 must return underlying unchanged; got %T", got)
	}
	if got := NewQueuedProvider(nil, 4); got != nil {
		t.Errorf("nil provider must return nil; got %T", got)
	}
}

func TestQueuedProvider_ModelDelegates(t *testing.T) {
	inner := &queueListingStub{namedStubProvider: namedStubProvider{name: "x", lastModel: "stub-model"}}
	q := NewQueuedProvider(inner, 1)
	if got := q.Model(); got != "stub-model" {
		t.Errorf("Model: got %q, want stub-model", got)
	}
}

func TestQueuedProvider_SetMetricsForwards(t *testing.T) {
	inner := &queueListingStub{namedStubProvider: namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1)
	q.SetMetrics(nil) // SetMetrics signature requires *Metrics; nil is the no-op fast path
	if !inner.setMetrics {
		t.Error("SetMetrics did not forward to underlying provider")
	}
}

func TestQueuedProvider_WithModelClones(t *testing.T) {
	inner := &queueListingStub{namedStubProvider: namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1).(*QueuedProvider)
	clone := q.WithModel("custom")
	if clone == nil {
		t.Fatal("WithModel returned nil")
	}
	if inner.withCalls != 1 {
		t.Errorf("WithModel passed through: got %d invocations, want 1", inner.withCalls)
	}
	if clone.Model() != "custom" {
		t.Errorf("clone Model: got %q, want custom", clone.Model())
	}
}

func TestQueuedProvider_WithModel_NonOverridableReturnsSelf(t *testing.T) {
	inner := &plainProvider{namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1).(*QueuedProvider)
	clone := q.WithModel("y")
	if clone != q {
		t.Errorf("non-overridable underlying: WithModel should return self; got %T", clone)
	}
}

func TestQueuedProvider_ListModelsForwards(t *testing.T) {
	inner := &queueListingStub{
		namedStubProvider: namedStubProvider{name: "x"},
		listed:            []ModelInfo{{ID: "a"}, {ID: "b"}},
	}
	q := NewQueuedProvider(inner, 1)
	got, err := q.(*QueuedProvider).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d models, want 2", len(got))
	}
}

func TestQueuedProvider_ListModels_NonListerReturnsNil(t *testing.T) {
	inner := &plainProvider{namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1)
	got, err := q.(*QueuedProvider).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != nil {
		t.Errorf("non-lister: got %v, want nil", got)
	}
}

func TestQueuedProvider_Ping_Forwards(t *testing.T) {
	inner := &queueListingStub{
		namedStubProvider: namedStubProvider{name: "x"},
		pingErr:           errors.New("backend down"),
	}
	q := NewQueuedProvider(inner, 1)
	err := q.(*QueuedProvider).Ping(context.Background())
	if err == nil {
		t.Fatal("expected error from underlying Ping")
	}
}

func TestQueuedProvider_Ping_NonPingerReturnsNil(t *testing.T) {
	inner := &plainProvider{namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1)
	err := q.(*QueuedProvider).Ping(context.Background())
	if err != nil {
		t.Errorf("non-pinger: got %v, want nil", err)
	}
}

func TestQueuedProvider_ListModelsAggregated_RouterPath(t *testing.T) {
	fallback := &queueListingStub{
		namedStubProvider: namedStubProvider{name: "fb"},
		listed:            []ModelInfo{{ID: "default"}},
	}
	router, err := NewRouter(fallback, nil, WithRouterLogger(zerolog.Nop()))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	q := NewQueuedProvider(router, 1).(*QueuedProvider)
	agg, ok := q.ListModelsAggregated(context.Background())
	if !ok {
		t.Fatal("Router-backed queue should expose aggregated listing")
	}
	if len(agg.Providers) == 0 {
		t.Error("ListModelsAggregated: empty Providers map for healthy router")
	}
}

func TestQueuedProvider_ListModelsAggregated_NonRouterReturnsFalse(t *testing.T) {
	inner := &queueListingStub{namedStubProvider: namedStubProvider{name: "x"}}
	q := NewQueuedProvider(inner, 1).(*QueuedProvider)
	_, ok := q.ListModelsAggregated(context.Background())
	if ok {
		t.Error("non-router backed queue: ListModelsAggregated must return ok=false")
	}
}
