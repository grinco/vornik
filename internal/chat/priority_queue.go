package chat

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type priorityContextKey struct{}

const defaultRequestPriority = 1000

// WithRequestPriority annotates ctx with a scheduling priority for queued chat
// calls. Lower values run first; equal priorities keep FIFO order.
func WithRequestPriority(ctx context.Context, priority int) context.Context {
	return context.WithValue(ctx, priorityContextKey{}, priority)
}

func requestPriority(ctx context.Context) int {
	if ctx == nil {
		return defaultRequestPriority
	}
	if v, ok := ctx.Value(priorityContextKey{}).(int); ok {
		return v
	}
	return defaultRequestPriority
}

// QueuedProvider serializes or bounds access to an underlying Provider and
// orders queued calls by request priority. It is intended for shared local LLM
// backends where unbounded parallel calls make every project slower.
type QueuedProvider struct {
	provider Provider
	queue    *providerQueue
}

// NewQueuedProvider wraps provider with a priority queue. maxConcurrent <= 0
// returns provider unchanged.
func NewQueuedProvider(provider Provider, maxConcurrent int) Provider {
	if provider == nil || maxConcurrent <= 0 {
		return provider
	}
	return &QueuedProvider{
		provider: provider,
		queue:    newProviderQueue(maxConcurrent),
	}
}

func (p *QueuedProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return p.queue.do(ctx, func(runCtx context.Context) (*ChatResponse, error) {
		return p.provider.Complete(runCtx, messages)
	})
}

func (p *QueuedProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return p.queue.do(ctx, func(runCtx context.Context) (*ChatResponse, error) {
		return p.provider.CompleteWithTools(runCtx, messages, tools)
	})
}

func (p *QueuedProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return p.queue.do(ctx, func(runCtx context.Context) (*ChatResponse, error) {
		return p.provider.CompleteWithToolsStream(runCtx, messages, tools, onText)
	})
}

func (p *QueuedProvider) Model() string { return p.provider.Model() }

// SetMetrics wires Prometheus metrics into both the underlying provider
// (per-model request counters, durations, tokens, errors) and the queue
// itself (depth, in-flight, wait time, started/canceled counts). The
// queue stores the pointer atomically because wireComponentMetrics
// runs after the queue's workers are already spinning.
func (p *QueuedProvider) SetMetrics(m *Metrics) {
	p.queue.metrics.Store(m)
	p.provider.SetMetrics(m)
}

func (p *QueuedProvider) WithModel(model string) Provider {
	if o, ok := p.provider.(ModelOverridable); ok {
		clone := *p
		clone.provider = o.WithModel(model)
		return &clone
	}
	return p
}

// ListModels delegates to the underlying provider when it supports
// discovery. The queue is intentionally bypassed — metadata calls
// don't make LLM requests and shouldn't compete with completions for
// the bounded worker pool. This also keeps the API endpoint
// (GET /api/v1/models) responsive even when the queue is full.
func (p *QueuedProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if l, ok := p.provider.(ModelLister); ok {
		return l.ListModels(ctx)
	}
	return nil, nil
}

// Ping delegates to the wrapped provider. Like ListModels, it bypasses
// the queue — the readiness probe shouldn't compete with completions
// for the bounded worker pool, and a queue-saturated provider is still
// "ready" in the sense the startup gate cares about.
func (p *QueuedProvider) Ping(ctx context.Context) error {
	if pg, ok := p.provider.(Pinger); ok {
		return pg.Ping(ctx)
	}
	return nil
}

// ListModelsAggregated lets the API handler pull the full per-sub
// breakdown when the queued provider wraps a *Router. Without this,
// the type-assertion in the handler sees QueuedProvider (which
// implements ModelLister via the flat method above) and returns a
// flat list under the generic "chat" name — losing per-sub-provider
// attribution. We expose this only as a method so callers that
// already have a *Router skip it; the handler reaches for the
// aggregated shape first and falls back to the flat one.
func (p *QueuedProvider) ListModelsAggregated(ctx context.Context) (ListModelsResult, bool) {
	if r, ok := p.provider.(*Router); ok {
		return r.ListModels(ctx), true
	}
	return ListModelsResult{}, false
}

type queuedCall struct {
	ctx        context.Context
	priority   int
	seq        uint64
	run        func(context.Context) (*ChatResponse, error)
	done       chan queuedResult
	canceled   bool
	index      int
	enqueuedAt time.Time
}

type queuedResult struct {
	resp *ChatResponse
	err  error
}

type providerQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	calls   callHeap
	nextSeq uint64
	metrics atomic.Pointer[Metrics]
}

func newProviderQueue(maxConcurrent int) *providerQueue {
	q := &providerQueue{}
	q.cond = sync.NewCond(&q.mu)
	for i := 0; i < maxConcurrent; i++ {
		go q.worker()
	}
	return q
}

func (q *providerQueue) do(ctx context.Context, run func(context.Context) (*ChatResponse, error)) (*ChatResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	call := &queuedCall{
		ctx:        ctx,
		priority:   requestPriority(ctx),
		run:        run,
		done:       make(chan queuedResult, 1),
		enqueuedAt: time.Now(),
	}

	q.mu.Lock()
	call.seq = q.nextSeq
	q.nextSeq++
	heap.Push(&q.calls, call)
	q.cond.Signal()
	q.mu.Unlock()
	if m := q.metrics.Load(); m != nil {
		m.QueueDepth.Inc()
	}

	select {
	case result := <-call.done:
		return result.resp, result.err
	case <-ctx.Done():
		q.mu.Lock()
		call.canceled = true
		removed := false
		if call.index >= 0 && call.index < q.calls.Len() && q.calls[call.index] == call {
			heap.Remove(&q.calls, call.index)
			removed = true
		}
		q.mu.Unlock()
		q.cond.Signal()
		if removed {
			if m := q.metrics.Load(); m != nil {
				m.QueueDepth.Dec()
				m.QueueCallsTotal.WithLabelValues("canceled").Inc()
			}
		}
		return nil, ctx.Err()
	}
}

func (q *providerQueue) worker() {
	for {
		call := q.pop()
		if call == nil {
			continue
		}
		if m := q.metrics.Load(); m != nil {
			m.QueueDepth.Dec()
			m.QueueInFlight.Inc()
			m.QueueWaitSeconds.Observe(time.Since(call.enqueuedAt).Seconds())
			m.QueueCallsTotal.WithLabelValues("started").Inc()
		}
		resp, err := call.run(call.ctx)
		if m := q.metrics.Load(); m != nil {
			m.QueueInFlight.Dec()
		}
		call.done <- queuedResult{resp: resp, err: err}
	}
}

func (q *providerQueue) pop() *queuedCall {
	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		for q.calls.Len() == 0 {
			q.cond.Wait()
		}
		call := heap.Pop(&q.calls).(*queuedCall)
		if call.canceled || call.ctx.Err() != nil {
			if m := q.metrics.Load(); m != nil {
				m.QueueDepth.Dec()
				m.QueueCallsTotal.WithLabelValues("canceled").Inc()
			}
			continue
		}
		return call
	}
}

type callHeap []*queuedCall

func (h callHeap) Len() int { return len(h) }

func (h callHeap) Less(i, j int) bool {
	if h[i].priority == h[j].priority {
		return h[i].seq < h[j].seq
	}
	return h[i].priority < h[j].priority
}

func (h callHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *callHeap) Push(x any) {
	call := x.(*queuedCall)
	call.index = len(*h)
	*h = append(*h, call)
}

func (h *callHeap) Pop() any {
	old := *h
	n := len(old)
	call := old[n-1]
	old[n-1] = nil
	call.index = -1
	*h = old[:n-1]
	return call
}

var _ Provider = (*QueuedProvider)(nil)
var _ ModelOverridable = (*QueuedProvider)(nil)
