package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"vornik.io/vornik/internal/persistence"
)

// liveClientHello is the first frame the client sends on a
// successful upgrade. Carries the seq cursor for replay. A
// last_seq of 0 means "send the full ring buffer".
type liveClientHello struct {
	LastSeq int64 `json:"last_seq"`
}

const (
	// liveWriteTimeout caps each write to the client so a wedged
	// reader can't backpressure the publisher's fan-out.
	liveWriteTimeout = 5 * time.Second
	// livePingInterval is how often the server pings the client
	// to detect a half-open connection. Three missed pongs at
	// this interval = 45s — well under any reasonable proxy
	// idle timeout.
	livePingInterval = 15 * time.Second
	// liveReadLimit caps the maximum WebSocket frame size we
	// read from the client. The hello frame carries a single
	// JSON int — 4 KiB is generous. Without a limit a hostile
	// client could send a multi-GB frame and OOM the daemon
	// before the library returns from Read.
	liveReadLimit = 4 * 1024
)

// ExecutionLive handles GET /api/v1/executions/{id}/live.
// Upgrades the connection to a WebSocket and streams LiveEvents
// for the execution until the client disconnects or the daemon
// shuts down.
//
// Auth: the existing API-key middleware has already validated
// the bearer key before the handler runs (or, when keys are
// disabled, anyone reaches this point). Project scope is
// checked against the execution's project_id before the
// upgrade.
//
// Connection lifecycle:
//  1. Validate execution + caller scope. 404 / 403 on failure;
//     socket is NOT opened so the operator gets a clean HTTP
//     response shape.
//  2. Upgrade. The handler accepts compressed frames + the
//     OriginPatterns wildcard since the daemon serves browsers
//     on the same host.
//  3. Read the client hello frame ({"last_seq": N}) with a
//     short deadline. Bad/absent hello falls back to
//     last_seq=0 (full replay).
//  4. Subscribe to the publisher; replay any buffered events
//     with seq >= last_seq+1; stream live thereafter.
//  5. Send a keepalive ping every livePingInterval. Drop the
//     connection when three pings go un-ponged or the writer
//     times out.
//  6. On any error or context cancel, close cleanly with a
//     normal-closure code.
func (s *Server) ExecutionLive(w http.ResponseWriter, r *http.Request, executionID string) {
	if s.liveSub == nil {
		respondError(w, http.StatusServiceUnavailable, "LIVE_DISABLED",
			"live observation not wired on this deployment")
		return
	}
	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	// Auth + scope happen BEFORE the upgrade so a 403 lands as a
	// plain HTTP response. Once the socket's open we can only
	// communicate via close frames + status codes.
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Str("executionId", executionID).
			Msg("live: failed to load execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return
	}
	if exec == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	// Some browsers can't set Authorization headers on the
	// upgrade request — allow `?key=` as a fallback the existing
	// AuthMiddleware honours.
	//
	// Origin enforcement: the library's default behaviour authorises
	// the request host (same-origin browsers always pass). Reverse-
	// proxy deployments add their public host through
	// WithLiveAllowedOrigins so the daemon's own UI keeps working
	// behind nginx/Caddy/Cloudflare. Wildcards permitted (path.Match
	// semantics) for *.example.com style configs. Empty list ==
	// same-origin only, which is the secure default.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.liveAllowedOrigins,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("executionId", executionID).
			Str("origin", r.Header.Get("Origin")).
			Msg("live: websocket upgrade failed")
		return
	}
	// Cap the read side so a hostile client can't OOM the daemon
	// with an outsized hello frame. Applies to every subsequent
	// read on this connection too.
	conn.SetReadLimit(liveReadLimit)
	// Sentinel close on any return path. NormalClosure (1000) =
	// "we're done"; the JS client handles it as a clean close.
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Detached context so close-on-cancel works even after the
	// handler returns. Bounded by max stream duration (1h) to
	// guarantee resource release on operator browsers that get
	// left open indefinitely.
	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Hour)
	defer cancel()

	// Pull the client hello with a brief read deadline. Absence
	// or malformed JSON falls back to the query cursor, then 0.
	hello := readLiveHello(ctx, conn, parseLiveLastSeqFromQuery(r))

	events, unsub, err := s.liveSub.Subscribe(executionID, liveSubscribeFromSeq(hello.LastSeq))
	if err != nil {
		s.logger.Warn().Err(err).Str("executionId", executionID).
			Msg("live: subscribe failed")
		_ = conn.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer unsub()

	// Heartbeat goroutine — server pings the client; the library
	// auto-replies to client pings. Three missed pings (45s)
	// equals dead.
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(livePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				close(pingDone)
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, livePingInterval)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					close(pingDone)
					return
				}
			}
		}
	}()

	// Writer loop. Each event becomes a single JSON text frame.
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingDone:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			body, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, liveWriteTimeout)
			err = conn.Write(writeCtx, websocket.MessageText, body)
			writeCancel()
			if err != nil {
				return
			}
		}
	}
}

func liveSubscribeFromSeq(lastSeq int64) int64 {
	if lastSeq <= 0 {
		return 0
	}
	return lastSeq + 1
}

// readLiveHello pulls the client's first frame (a JSON object
// with last_seq) with a 2-second read deadline. Returns a
// zero-value hello (full replay) on absence or parse error —
// older clients that don't know about the protocol still see
// the live stream from the beginning of the ring.
func readLiveHello(ctx context.Context, conn *websocket.Conn, fallbackLastSeq int64) liveClientHello {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, body, err := conn.Read(readCtx)
	if err != nil {
		return liveClientHello{LastSeq: fallbackLastSeq}
	}
	var hello liveClientHello
	if jerr := json.Unmarshal(body, &hello); jerr != nil {
		// Tolerant: also accept ?last_seq=N on the upgrade URL
		// when the client can't ship a body before the server
		// expects it.
		return liveClientHello{LastSeq: fallbackLastSeq}
	}
	if hello.LastSeq < 0 {
		hello.LastSeq = 0
	}
	return hello
}

// parseLiveLastSeqFromQuery is a small helper so the JS client
// can opt in to ?last_seq=N when the hello-frame round-trip is
// awkward (e.g. on reconnect, before the new socket has had a
// chance to write). Keeps the API permissive; the hello frame
// still wins when both are set.
func parseLiveLastSeqFromQuery(r *http.Request) int64 {
	raw := r.URL.Query().Get("last_seq")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
