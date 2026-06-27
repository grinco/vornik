package ui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
)

// TaskEventsStream serves the SSE stream for a single task.
// Path: /ui/tasks/{id}/events.
//
// Wire format (per SSE spec):
//
//	event: <kind>
//	data: <html fragment>
//
//	event: ping
//	data: <timestamp>
//
// htmx-ext-sse listens via:
//
//	<div hx-ext="sse" sse-connect="/ui/tasks/<id>/events"
//	     sse-swap="status"  hx-target="#task-status-pill-mobile" />
//
// We send a "ping" every 30s so middleware proxies / mobile
// browsers don't reap the idle connection.
func (s *Server) TaskEventsStream(w http.ResponseWriter, r *http.Request) {
	if s.sseBus == nil {
		http.Error(w, "SSE bus not configured", http.StatusServiceUnavailable)
		return
	}
	// Extract taskID from /tasks/<id>/events path. The /ui prefix
	// is already stripped by uiSubtreeHandler.
	path := strings.TrimPrefix(r.URL.Path, "/tasks/")
	path = strings.TrimSuffix(path, "/events")
	taskID := path
	if taskID == "" || strings.Contains(taskID, "/") {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	// Project-scope check before subscribing. A scoped key for
	// project A must not observe project B's live event stream.
	// 404 (not 403) so foreign task existence isn't leaked.
	if s.taskRepo != nil {
		lookupCtx, lookupCancel := context.WithTimeout(r.Context(), 3*time.Second)
		task, err := s.taskRepo.Get(lookupCtx, taskID)
		lookupCancel()
		if err == nil && task != nil && task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
			http.NotFound(w, r)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sub, cancel := s.sseBus.Subscribe(taskID)
	defer cancel()

	// Initial hello so htmx-ext-sse establishes the channel.
	_, _ = fmt.Fprintf(w, "event: hello\ndata: %s\n\n", taskID)
	flusher.Flush()

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			// SSE spec: each line of data prefixed with "data: ".
			// HTML fragments must not contain bare "\n\n" — they'd
			// split the event. Replace double-newlines defensively.
			data := strings.ReplaceAll(ev.Data, "\n\n", "\n")
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, data)
			flusher.Flush()
		case t := <-pingTicker.C:
			_, _ = fmt.Fprintf(w, "event: ping\ndata: %d\n\n", t.Unix())
			flusher.Flush()
		}
	}
}
