package livepubsub

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Publisher is the surface every event producer (executor step
// hooks, chat-stream tap, API steering handlers) and every
// consumer (WebSocket server in Phase B) hits. One instance per
// daemon; concurrency-safe.
type Publisher interface {
	// Publish records one event for the given execution. Returns
	// the assigned seq so callers that need correlation (e.g. the
	// audit row pointing at a specific publish) can stamp it. A
	// failed publish must NEVER block the caller — implementations
	// log + drop rather than backpressure.
	Publish(ctx context.Context, executionID, kind string, payload any) int64

	// Subscribe returns a channel of events for the execution,
	// starting at fromSeq (inclusive). The returned cancel
	// function removes the subscription + drains the channel; the
	// caller MUST call it (idempotent).
	//
	// fromSeq=0 means "start at the oldest event still in the
	// ring". A fromSeq newer than what's buffered returns no
	// replay; the next live event lands as normal.
	//
	// When the ring has dropped events the caller missed (i.e.
	// fromSeq < oldest_seq), the first frame on the channel is
	// a ReplayGapMarker with the actual oldest_seq value. UI
	// renders a "refresh for full state" hint.
	Subscribe(executionID string, fromSeq int64) (<-chan LiveEvent, func(), error)

	// SubscribeAll returns a channel of EVERY event across all executions,
	// live-only (no replay — per-execution seqs make a global cursor
	// meaningless). Backs the fleet "Now Running" monitoring view, which
	// seeds from the execution repo then stays live off this tap. The
	// returned cancel MUST be called (idempotent).
	SubscribeAll() (<-chan LiveEvent, func(), error)
}

// ReplayGapMarker is the synthetic event sent when a subscriber's
// fromSeq is older than what the ring buffer retains. Kind ==
// "replay_gap"; payload carries the actual oldest seq still
// available. Clients use this to decide whether to do a full
// reload.
const KindReplayGap = "replay_gap"

type ReplayGapPayload struct {
	OldestSeq int64 `json:"oldest_seq"`
}

// inProcessPublisher is the default implementation. Per-execution
// ring buffer (size VORNIK_LIVE_RING_SIZE, default 200) + per-
// execution subscriber list. Stale execution streams (no
// subscribers for an hour) are evicted by a background sweeper.
type inProcessPublisher struct {
	mu       sync.Mutex
	streams  map[string]*stream
	ringSize int

	// allSubs are FLEET subscribers (SubscribeAll): they receive every
	// event across all executions, live-only (no per-execution replay —
	// seqs are per-execution so a global replay cursor is meaningless).
	// Backs the fleet "Now Running" monitoring view. Guarded by p.mu.
	allSubs []*subscription

	// sweepInterval is exposed for tests. Zero disables the
	// sweeper goroutine (most tests).
	sweepInterval time.Duration
	sweepStop     chan struct{}

	// metrics is nil-safe; nil disables emission (observability off).
	metrics *Metrics
}

// Option configures an in-process publisher at construction.
type Option func(*inProcessPublisher)

// WithMetrics attaches the live-event metrics. A nil *Metrics is a no-op.
func WithMetrics(m *Metrics) Option {
	return func(p *inProcessPublisher) { p.metrics = m }
}

// stream is the per-execution state.
type stream struct {
	mu          sync.Mutex
	ring        []LiveEvent // bounded; oldest at ring[0]
	nextSeq     int64
	subscribers []*subscription
	lastActive  time.Time
}

type subscription struct {
	ch        chan LiveEvent
	cancelled atomic.Bool
	cancelFn  func()
}

// New constructs an in-process Publisher. Pass the ring size
// explicitly (0 → env / default 200). Production callers wire
// once via the service container; the WS server holds the
// returned Publisher.
func New(ringSize int, opts ...Option) Publisher {
	p := &inProcessPublisher{
		streams:  map[string]*stream{},
		ringSize: ringSize,
	}
	if p.ringSize <= 0 {
		p.ringSize = envInt("VORNIK_LIVE_RING_SIZE", 200)
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// NewWithSweeper wires a background sweeper that evicts streams
// with no subscribers + no recent publishes. interval==0 disables.
// Returned shutdown function stops the sweeper; safe to defer in
// production close paths.
func NewWithSweeper(ringSize int, interval time.Duration, opts ...Option) (Publisher, func()) {
	p := &inProcessPublisher{
		streams:       map[string]*stream{},
		ringSize:      ringSize,
		sweepInterval: interval,
		sweepStop:     make(chan struct{}),
	}
	if p.ringSize <= 0 {
		p.ringSize = envInt("VORNIK_LIVE_RING_SIZE", 200)
	}
	for _, opt := range opts {
		opt(p)
	}
	if interval > 0 {
		go p.runSweeper()
	}
	return p, func() {
		if p.sweepStop != nil {
			close(p.sweepStop)
		}
	}
}

func (p *inProcessPublisher) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	if executionID == "" || kind == "" {
		return 0
	}
	s := p.streamFor(executionID)
	s.mu.Lock()
	seq := s.nextSeq
	s.nextSeq++
	s.mu.Unlock()
	if p.metrics != nil {
		p.metrics.PublishedTotal.WithLabelValues(kind).Inc()
	}
	p.deliver(s, LiveEvent{
		ExecutionID: executionID,
		Seq:         seq,
		Timestamp:   time.Now().UTC(),
		Kind:        kind,
		Payload:     payload,
	})
	return seq
}

// IngestRemote pushes a pre-built LiveEvent into the ring + fans
// out to local subscribers without allocating a new seq. The
// cross-replica wrapper uses this to feed events received via
// Postgres NOTIFY into the in-memory stream — those events
// already carry the DB-allocated authoritative seq.
//
// nextSeq is bumped to max(nextSeq, evt.Seq+1) so a subsequent
// local Publish() doesn't reuse a seq that arrived from another
// replica.
func (p *inProcessPublisher) IngestRemote(evt LiveEvent) {
	if evt.ExecutionID == "" || evt.Kind == "" {
		return
	}
	s := p.streamFor(evt.ExecutionID)
	s.mu.Lock()
	if evt.Seq+1 > s.nextSeq {
		s.nextSeq = evt.Seq + 1
	}
	s.mu.Unlock()
	p.deliver(s, evt)
}

// deliver is the shared ring-push + subscriber-fanout path.
// Lifted out of Publish so IngestRemote can reuse it.
func (p *inProcessPublisher) deliver(s *stream, evt LiveEvent) {
	s.mu.Lock()
	// Push into ring buffer; drop oldest on overflow.
	if len(s.ring) < p.ringSize {
		s.ring = append(s.ring, evt)
	} else {
		// Shift in place. The ring is bounded; cost is O(ringSize)
		// per publish. Acceptable: ringSize defaults to 200 and a
		// busy stream emits maybe ~50 events/sec, so the shift is
		// well under a millisecond.
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = evt
	}
	s.lastActive = evt.Timestamp
	subs := append([]*subscription(nil), s.subscribers...)
	s.mu.Unlock()

	// Snapshot fleet subscribers (SubscribeAll) — they see every event.
	p.mu.Lock()
	allSubs := append([]*subscription(nil), p.allSubs...)
	p.mu.Unlock()

	// Fan out non-blocking. Subscribers with slow consumers drop
	// the event; their channel state stays consistent so the
	// next publish can still land. Dropping is operator-
	// observable via the Phase D metrics (events_dropped_total).
	for _, sub := range subs {
		if sub.cancelled.Load() {
			continue
		}
		select {
		case sub.ch <- evt:
		default:
			// Drop; subscriber is too slow.
			if p.metrics != nil {
				p.metrics.DroppedTotal.WithLabelValues("subscriber_slow").Inc()
			}
		}
	}
	for _, sub := range allSubs {
		if sub.cancelled.Load() {
			continue
		}
		select {
		case sub.ch <- evt:
		default:
			// Drop; fleet subscriber is too slow (it's a summary view,
			// a missed event self-heals on the next one / re-seed).
			if p.metrics != nil {
				p.metrics.DroppedTotal.WithLabelValues("fleet_slow").Inc()
			}
		}
	}
}

// SubscribeAll registers a FLEET subscriber that receives every event across
// all executions (live-only — no replay). Used by the "Now Running"
// monitoring view, which seeds its state from the execution repo and then
// keeps it live off this tap. Because the cross-replica wrapper feeds remote
// events through IngestRemote → deliver, a single SubscribeAll also sees
// events originating on other replicas. The caller MUST invoke the returned
// cancel to avoid a leak.
func (p *inProcessPublisher) SubscribeAll() (<-chan LiveEvent, func(), error) {
	sub := &subscription{ch: make(chan LiveEvent, 256)}
	cancel := func() {
		if sub.cancelled.Swap(true) {
			return
		}
		p.mu.Lock()
		out := p.allSubs[:0]
		for _, existing := range p.allSubs {
			if existing != sub {
				out = append(out, existing)
			}
		}
		p.allSubs = out
		p.mu.Unlock()
	}
	sub.cancelFn = cancel
	p.mu.Lock()
	p.allSubs = append(p.allSubs, sub)
	p.mu.Unlock()
	return sub.ch, cancel, nil
}

func (p *inProcessPublisher) Subscribe(executionID string, fromSeq int64) (<-chan LiveEvent, func(), error) {
	if executionID == "" {
		return nil, nil, errors.New("livepubsub: execution_id required")
	}
	s := p.streamFor(executionID)
	s.mu.Lock()
	defer s.mu.Unlock()

	sub := &subscription{ch: make(chan LiveEvent, 64)}
	cancel := func() {
		if sub.cancelled.Swap(true) {
			return
		}
		s.mu.Lock()
		out := s.subscribers[:0]
		for _, existing := range s.subscribers {
			if existing != sub {
				out = append(out, existing)
			}
		}
		s.subscribers = out
		s.mu.Unlock()
		// Drain to unblock any in-flight publishes. The channel
		// has bounded capacity; we don't close it (in-flight
		// publishers might be writing concurrently) — instead
		// rely on cancelled.Load() in Publish's fan-out loop.
	}
	sub.cancelFn = cancel

	// Replay ring entries with seq >= fromSeq.
	oldestSeq := int64(-1)
	if len(s.ring) > 0 {
		oldestSeq = s.ring[0].Seq
	}
	if oldestSeq >= 0 && fromSeq < oldestSeq && fromSeq > 0 {
		// Gap marker — subscriber asked for events we've already
		// dropped. Send one synthetic event then fall through to
		// stream live.
		sub.ch <- LiveEvent{
			ExecutionID: executionID,
			Seq:         -1,
			Timestamp:   time.Now().UTC(),
			Kind:        KindReplayGap,
			Payload:     ReplayGapPayload{OldestSeq: oldestSeq},
		}
	}
	for _, evt := range s.ring {
		if evt.Seq < fromSeq {
			continue
		}
		// Best-effort enqueue; if the buffer is already full we
		// drop the historical event (caller's channel is too
		// slow for replay either way).
		select {
		case sub.ch <- evt:
		default:
		}
	}
	s.subscribers = append(s.subscribers, sub)
	return sub.ch, cancel, nil
}

func (p *inProcessPublisher) streamFor(executionID string) *stream {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.streams[executionID]
	if !ok {
		s = &stream{lastActive: time.Now().UTC()}
		p.streams[executionID] = s
	}
	return s
}

// runSweeper is the background eviction goroutine. Cleans up
// streams with no subscribers that haven't seen activity in 2×
// the sweep interval — bounded memory growth even with a daemon
// that's seen millions of distinct executions.
func (p *inProcessPublisher) runSweeper() {
	tick := time.NewTicker(p.sweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-p.sweepStop:
			return
		case now := <-tick.C:
			p.evictIdle(now)
		}
	}
}

func (p *inProcessPublisher) evictIdle(now time.Time) {
	threshold := now.Add(-2 * p.sweepInterval)
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range p.streams {
		s.mu.Lock()
		idle := len(s.subscribers) == 0 && s.lastActive.Before(threshold)
		s.mu.Unlock()
		if idle {
			delete(p.streams, id)
		}
	}
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
