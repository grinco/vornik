// Cross-replica live-events publisher (horizontal-scaling
// follow-on). Wraps the in-process publisher with:
//
//   - Persistence via persistence.ExecutionLiveEventRepository:
//     every Publish appends a row, so a non-emitting replica or a
//     late-joining subscriber can replay.
//   - Cross-replica fanout via Postgres NOTIFY: the emitting
//     replica fires `NOTIFY vornik_live, "<execID>|<seq>|<nodeID>"`,
//     and every other replica's LISTEN goroutine ingests the
//     matching row into its local in-process ring so its
//     subscribers see the same stream.
//
// Single-process / SQLite deployments don't need any of this —
// the bare inProcessPublisher already covers them. The container
// constructs a dbBackedPublisher only when the repo is wired (the
// Postgres branch) AND a notifier/listener pair is supplied.
//
// Failure mode: an Append or NOTIFY error doesn't break the local
// stream. The wrapper logs + falls back to in-process-only
// delivery. The user watching this replica's stream still sees
// every event their own replica produced.

package livepubsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Notifier sends a Postgres NOTIFY to the channel. Implementations:
// PostgresNotifier (production) wraps *sql.DB.ExecContext; tests
// use stub implementations that record calls.
type Notifier interface {
	Notify(ctx context.Context, channel, payload string) error
}

// Listener consumes Postgres LISTEN notifications and feeds them
// on the returned channel. Cancelling the supplied context is the
// caller's shutdown signal; the listener should drain + close.
type Listener interface {
	// Start subscribes to the channel and returns a stream of
	// notification payloads. Returns an error if the initial
	// LISTEN handshake failed.
	Start(ctx context.Context, channel string) (<-chan Notification, error)
}

// Notification mirrors lib/pq's notification struct without the
// pq dependency leaking out of the postgres-specific driver.
type Notification struct {
	Channel string
	Payload string
}

// NotifyChannel is the Postgres LISTEN/NOTIFY channel every
// replica subscribes to. One channel for all executions; the
// payload format below carries the execution_id so the listener
// can filter.
const NotifyChannel = "vornik_live"

// dbBackedPublisher implements Publisher by composing an
// in-process publisher (for local fanout) with a DB repository
// + Notifier (for cross-replica fanout).
type dbBackedPublisher struct {
	inner    *inProcessPublisher
	repo     persistence.ExecutionLiveEventRepository
	notifier Notifier
	logger   zerolog.Logger
	nodeID   string

	// listenerCtx + cancel control the LISTEN goroutine. nil when
	// the wrapper is constructed without a listener (NOTIFY-only
	// deployments — they still cross-replicate but only one-way).
	listenerCancel context.CancelFunc
	listenerDone   chan struct{}

	// stopMu serialises Close calls.
	stopMu sync.Mutex
	closed bool
}

// NewDBBackedConfig bundles the dependencies a cross-replica
// publisher needs. nodeID identifies this daemon so the
// LISTEN goroutine can drop self-emitted notifications.
type NewDBBackedConfig struct {
	Inner    *inProcessPublisher
	Repo     persistence.ExecutionLiveEventRepository
	Notifier Notifier
	Listener Listener
	NodeID   string
	Logger   zerolog.Logger
}

// NewDBBacked constructs the wrapper + starts the LISTEN
// goroutine (if a listener was supplied). The returned shutdown
// closure cancels the goroutine and waits for it to drain;
// callers defer it in the container's shutdown sequence.
//
// inner may be nil — the wrapper allocates a default in-process
// publisher in that case.
func NewDBBacked(ctx context.Context, cfg NewDBBackedConfig) (Publisher, func(), error) {
	if cfg.Repo == nil {
		return nil, nil, errors.New("livepubsub: NewDBBacked requires Repo")
	}
	inner := cfg.Inner
	if inner == nil {
		inner = &inProcessPublisher{streams: map[string]*stream{}, ringSize: envInt("VORNIK_LIVE_RING_SIZE", 200)}
	}
	p := &dbBackedPublisher{
		inner:    inner,
		repo:     cfg.Repo,
		notifier: cfg.Notifier,
		logger:   cfg.Logger,
		nodeID:   cfg.NodeID,
	}
	if cfg.Listener != nil {
		lctx, cancel := context.WithCancel(ctx)
		p.listenerCancel = cancel
		p.listenerDone = make(chan struct{})
		notifications, err := cfg.Listener.Start(lctx, NotifyChannel)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("livepubsub: start listener: %w", err)
		}
		go p.runListenLoop(lctx, notifications)
	}
	return p, p.close, nil
}

// Publish writes the event to the DB, NOTIFY-broadcasts the
// (execID, seq, nodeID) tuple to every other replica, and fans
// out locally. Returns the DB-allocated seq.
//
// On DB failure, falls back to local-only Publish via the inner
// publisher (so a transient blip doesn't blank the user's
// stream). Operators see a single warn log; metrics on the DB
// side surface the underlying error.
func (p *dbBackedPublisher) Publish(ctx context.Context, executionID, kind string, payload any) int64 {
	if executionID == "" || kind == "" {
		return 0
	}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		// Should never happen for the documented payload structs.
		// Log + fall back to a degraded local publish without DB.
		p.logger.Warn().Err(marshalErr).
			Str("execution_id", executionID).
			Str("kind", kind).
			Msg("livepubsub: marshal failed; falling back to in-process publish")
		return p.inner.Publish(ctx, executionID, kind, payload)
	}
	seq, err := p.repo.Append(ctx, executionID, kind, raw)
	if err != nil {
		// DB blip — keep the local stream alive so the user doesn't
		// see a gap. Cross-replica fanout is lost for this event;
		// the next successful Append re-syncs.
		p.logger.Warn().Err(err).
			Str("execution_id", executionID).
			Str("kind", kind).
			Msg("livepubsub: DB append failed; falling back to in-process publish")
		return p.inner.Publish(ctx, executionID, kind, payload)
	}

	// Local delivery uses the DB-authoritative seq + timestamp so
	// every replica's subscriber sees the same wire-format.
	evt := LiveEvent{
		ExecutionID: executionID,
		Seq:         seq,
		Timestamp:   time.Now().UTC(),
		Kind:        kind,
		Payload:     payload,
	}
	p.inner.IngestRemote(evt)

	// Cross-replica NOTIFY. Failures here are non-fatal — the
	// local stream is already served, and downstream replicas
	// only miss this one event (they'll catch up on the next
	// notification's ListSince fallback).
	if p.notifier != nil {
		notif := fmt.Sprintf("%s|%d|%s", executionID, seq, p.nodeID)
		if err := p.notifier.Notify(ctx, NotifyChannel, notif); err != nil {
			p.logger.Warn().Err(err).
				Str("execution_id", executionID).
				Int64("seq", seq).
				Msg("livepubsub: NOTIFY failed; cross-replica fanout will miss this event")
		}
	}
	return seq
}

// Subscribe delegates to the in-process publisher for the live
// stream + supplements the replay from the DB when the
// requested fromSeq is older than the in-memory ring.
//
// The DB-replay path fires only when fromSeq is older than the
// in-memory oldest. Most reconnects from the UI ask for
// "everything I've seen + this seq" — typically <200 events back,
// so the ring covers them and no DB query happens.
func (p *dbBackedPublisher) Subscribe(executionID string, fromSeq int64) (<-chan LiveEvent, func(), error) {
	// Probe the in-memory ring for its current oldest seq. If
	// fromSeq is older, fetch the missing prefix from the DB and
	// inject it before the live subscribe so the subscriber sees
	// a continuous stream.
	if fromSeq > 0 {
		p.inner.mu.Lock()
		s, exists := p.inner.streams[executionID]
		p.inner.mu.Unlock()
		var oldestInMem int64 = -1
		if exists {
			s.mu.Lock()
			if len(s.ring) > 0 {
				oldestInMem = s.ring[0].Seq
			}
			s.mu.Unlock()
		}
		// "DB might cover the gap" condition: the ring is empty
		// (fresh subscriber on this replica) OR the oldest ring
		// entry is newer than fromSeq.
		if oldestInMem < 0 || fromSeq < oldestInMem {
			p.replayFromDB(executionID, fromSeq, oldestInMem)
		}
	}
	return p.inner.Subscribe(executionID, fromSeq)
}

// SubscribeAll delegates to the inner in-process publisher's fleet tap.
// Cross-replica events arrive via the LISTEN loop → IngestRemote → inner
// deliver, which fans to the inner's fleet subscribers — so this single tap
// sees events from every replica, not just the local one.
func (p *dbBackedPublisher) SubscribeAll() (<-chan LiveEvent, func(), error) {
	return p.inner.SubscribeAll()
}

// replayFromDB pulls historical events from the persistence layer
// and injects them into the in-memory ring via IngestRemote so
// the upcoming Subscribe call serves them transparently.
//
// untilSeq bounds the fetch: when the ring already has [until..],
// we only need [fromSeq..until-1]. -1 means "no upper bound" (ring
// is empty — fetch everything from fromSeq onward, capped by a
// safety limit).
func (p *dbBackedPublisher) replayFromDB(executionID string, fromSeq, untilSeq int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Cap the replay at a healthy bound — a subscriber asking for
	// fromSeq=0 on an execution with 100k events would otherwise
	// blow up memory + the per-subscriber channel buffer. The
	// matching ReplayGapMarker from inner.Subscribe will fire if
	// we're missing the very oldest events.
	const replayLimit = 2000
	rows, err := p.repo.ListSince(ctx, executionID, fromSeq, replayLimit)
	if err != nil {
		p.logger.Warn().Err(err).
			Str("execution_id", executionID).
			Int64("from_seq", fromSeq).
			Msg("livepubsub: DB replay failed; subscriber will get ring-only replay")
		return
	}
	for _, row := range rows {
		if untilSeq >= 0 && row.Seq >= untilSeq {
			break
		}
		var payload any
		if len(row.Payload) > 0 {
			_ = json.Unmarshal(row.Payload, &payload)
		}
		p.inner.IngestRemote(LiveEvent{
			ExecutionID: row.ExecutionID,
			Seq:         row.Seq,
			Timestamp:   row.CreatedAt,
			Kind:        row.Kind,
			Payload:     payload,
		})
	}
}

// runListenLoop consumes Postgres notifications from the listener
// channel and ingests each remote event into the local in-process
// publisher. Self-emitted notifications are skipped (we already
// fanned out locally in Publish).
//
// On listener-channel close (driver-side reconnect failure or
// context cancellation), the loop returns and signals done.
// Operators see the underlying error via the listener's own
// logging path; production wires the postgres listener with
// auto-reconnect, so a transient blip recovers without daemon
// intervention.
func (p *dbBackedPublisher) runListenLoop(ctx context.Context, notifications <-chan Notification) {
	defer close(p.listenerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-notifications:
			if !ok {
				p.logger.Warn().Msg("livepubsub: listener channel closed; cross-replica fanout stopped")
				return
			}
			p.handleNotification(ctx, n.Payload)
		}
	}
}

// handleNotification parses one notification payload of the form
// "<execID>|<seq>|<nodeID>" and ingests the matching DB row into
// the local in-process publisher. Self-emitted notifications
// (matching nodeID) are skipped to avoid double-fanout.
func (p *dbBackedPublisher) handleNotification(ctx context.Context, payload string) {
	execID, seq, emitterNodeID, ok := parseNotifyPayload(payload)
	if !ok {
		p.logger.Warn().Str("payload", payload).Msg("livepubsub: malformed NOTIFY payload; skipping")
		return
	}
	if p.nodeID != "" && emitterNodeID == p.nodeID {
		// Self-emit — we already fanned out locally during Publish.
		return
	}

	// Pull the exact row from DB. Bounded latency: <5ms typical
	// under normal load. Cross-replica latency is dominated by
	// the LISTEN/NOTIFY hop + this single-row SELECT.
	rows, err := p.repo.ListSince(ctx, execID, seq, 1)
	if err != nil {
		p.logger.Warn().Err(err).
			Str("execution_id", execID).
			Int64("seq", seq).
			Msg("livepubsub: failed to fetch remote event; skipping")
		return
	}
	for _, row := range rows {
		if row.Seq != seq {
			continue
		}
		var pl any
		if len(row.Payload) > 0 {
			_ = json.Unmarshal(row.Payload, &pl)
		}
		p.inner.IngestRemote(LiveEvent{
			ExecutionID: row.ExecutionID,
			Seq:         row.Seq,
			Timestamp:   row.CreatedAt,
			Kind:        row.Kind,
			Payload:     pl,
		})
		return
	}
}

// parseNotifyPayload splits "<execID>|<seq>|<nodeID>" into its
// three components. seq must parse as an int64. Returns false on
// any parse failure — the caller logs + drops.
func parseNotifyPayload(s string) (execID string, seq int64, nodeID string, ok bool) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) != 3 {
		return "", 0, "", false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	return parts[0], n, parts[2], true
}

// close cancels the listener goroutine and waits for it to drain.
// Idempotent; subsequent calls return immediately.
func (p *dbBackedPublisher) close() {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.listenerCancel != nil {
		p.listenerCancel()
	}
	if p.listenerDone != nil {
		<-p.listenerDone
	}
}
