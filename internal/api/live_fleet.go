package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// fleetSummaryKinds are the event kinds the fleet feed forwards — meaningful
// execution transitions, NOT the per-step token/tool firehose (that stays on
// the per-execution socket). A frame on any of these tells the "Now Running"
// grid "something moved; re-fetch".
var fleetSummaryKinds = map[string]struct{}{
	"step_started":   {},
	"step_completed": {},
	"paused":         {},
	"resumed":        {},
	"forked":         {},
	"closed":         {},
}

const (
	fleetStreamMaxDuration = time.Hour        // bound a left-open browser tab
	fleetHeartbeat         = 25 * time.Second // SSE comment keepalive
)

// FleetLive is the fleet "Now Running" SSE feed (live-operations F2). It taps
// every execution's summary events (livepubsub SubscribeAll) and emits a
// lightweight `fleet-changed` SSE event — project-scoped to what the caller's
// key may see — which the grid uses as an event-driven refresh trigger
// (replacing fixed polling; a slow poll remains as a backstop). One-way
// server→client, so SSE rather than the per-execution WebSocket.
//
// GET /api/v1/live/fleet
func (s *Server) FleetLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.liveSub == nil {
		respondError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "live publisher not configured")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "streaming unsupported")
		return
	}

	events, cancel, err := s.liveSub.SubscribeAll()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "subscribe failed")
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ctx, stop := context.WithTimeout(r.Context(), fleetStreamMaxDuration)
	defer stop()

	// Per-connection execution→project scope cache. allowed[execID] tells
	// whether the caller may see this execution; resolved lazily from the
	// execution repo on first sight (bounded by distinct execs seen).
	allowed := map[string]bool{}
	resolve := func(execID string) bool {
		if v, ok := allowed[execID]; ok {
			return v
		}
		ok := false
		if s.executionRepo != nil && execID != "" {
			rc, c := context.WithTimeout(ctx, 2*time.Second)
			if exec, err := s.executionRepo.Get(rc, execID); err == nil && exec != nil {
				ok = RequestAllowsProject(r, exec.ProjectID)
			}
			c()
		}
		allowed[execID] = ok
		return ok
	}

	hb := time.NewTicker(fleetHeartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt, open := <-events:
			if !open {
				return
			}
			if _, ok := fleetSummaryKinds[evt.Kind]; !ok {
				continue
			}
			if !resolve(evt.ExecutionID) {
				continue
			}
			// Minimal payload — the grid re-fetches its own state; this is
			// just the "something changed" nudge (+ which exec/kind, useful
			// for future targeted patching).
			if _, err := fmt.Fprintf(w, "event: fleet-changed\ndata: {\"execution_id\":%q,\"kind\":%q}\n\n", evt.ExecutionID, evt.Kind); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
