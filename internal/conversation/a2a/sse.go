package a2a

// SSE bridge: read livepubsub events for one execution, translate
// to A2A's SSE event shape, write to the client until the task
// reaches a terminal state or the client disconnects.
//
// The translation is deliberately small in this slice:
//
//   livepubsub.Kind              →  A2A SSE event
//   step_started, paused          →  event: status
//   step_completed                →  event: status + (artifact)
//   forked, project_spawned, …    →  event: status (informational)
//   closed (synthetic terminator) →  event: status + final=true
//
// LLM-token events are NOT proxied yet — A2A clients expect
// coarse-grained status, not per-token streams. Per-token
// streaming becomes meaningful when we expose a Chat-style
// agent later; deferred.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// executionLookup is the narrow read surface the SSE handler
// uses to find the latest execution for an A2A task. The wiring
// layer adapts persistence.ExecutionRepository.GetByTaskID into
// this shape; the indirection keeps the package free of a
// persistence-repo import.
type executionLookup interface {
	GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error)
}

// taskLookup verifies the task belongs to the agent's project
// scope before opening the stream. Without this an A2A caller
// could request a stream for a task from a different project
// just by guessing its ID.
type taskLookup interface {
	Get(ctx context.Context, taskID string) (*persistence.Task, error)
}

// SSEDeps bundles the dependencies the streaming endpoint needs
// on top of what Handler already carries. Kept as a separate
// type so the production wiring can pass them explicitly without
// growing Handler's surface for tests that only exercise the
// card / submit paths.
type SSEDeps struct {
	Executions executionLookup
	Tasks      taskLookup
}

// streamDeps is the singleton the handlers reach into. Wired
// once at boot via WireSSE. Nil until then; the SSE handler
// 503s out cleanly.
var streamDeps *SSEDeps

// WireSSE plumbs the execution + task lookups the SSE handler
// needs. Called once at daemon startup from internal/api wiring.
func WireSSE(d *SSEDeps) {
	streamDeps = d
}

// handleTaskStream serves the GET /a2a/v1/agents/<p>/<wf>/tasks/<id>
// endpoint as SSE. The handler:
//
//  1. Verifies the task belongs to the agent's project.
//  2. Looks up the latest execution; replays buffered events.
//  3. Subscribes to live events and translates each into an
//     A2A SSE frame.
//  4. Exits on terminal status, client disconnect, or deadline.
func (h *Handler) handleTaskStream(w http.ResponseWriter, r *http.Request, agent *PublishedAgent, taskID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if h.LiveSubscriber == nil || streamDeps == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "live streaming not configured on this daemon")
		return
	}
	task, err := streamDeps.Tasks.Get(r.Context(), taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "task "+taskID+" not found")
		return
	}
	if task.ProjectID != agent.ProjectID {
		// Don't leak existence of out-of-scope tasks — same code
		// path as "not found".
		writeError(w, http.StatusNotFound, "NOT_FOUND", "task "+taskID+" not found")
		return
	}
	exec, err := streamDeps.Executions.GetByTaskID(r.Context(), taskID)
	if err != nil || exec == nil {
		// Task created but no execution yet — return a single
		// status event + close. The client can re-open later.
		startSSE(w)
		writeSSEStatus(w, "submitted", taskID, false, nil)
		return
	}
	events, cancel, err := h.LiveSubscriber.Subscribe(exec.ID, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "subscribe failed: "+err.Error())
		return
	}
	defer cancel()

	startSSE(w)
	flusher, _ := w.(http.Flusher)

	// Ping every 15s so reverse proxies keep the connection
	// open. SSE comment lines (": …") are stripped by clients.
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	clientGone := r.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case <-pingTicker.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case ev, ok := <-events:
			if !ok {
				// Stream closed.
				return
			}
			final := isTerminalKind(ev.Kind)
			translateAndWrite(w, ev, taskID, final)
			if flusher != nil {
				flusher.Flush()
			}
			if final {
				return
			}
		}
	}
}

// startSSE writes the SSE response headers. Splitting it out so
// the early "no execution yet" branch and the streaming branch
// agree on the header shape.
func startSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: disable proxy buffering
	w.WriteHeader(http.StatusOK)
}

// translateAndWrite converts one livepubsub event into the A2A
// SSE event shape and writes it. The payload is JSON; the event
// name follows the spec's vocabulary.
func translateAndWrite(w http.ResponseWriter, ev livepubsub.LiveEvent, taskID string, final bool) {
	switch ev.Kind {
	case livepubsub.KindStepStarted,
		livepubsub.KindStepCompleted,
		livepubsub.KindPaused,
		livepubsub.KindResumed,
		livepubsub.KindForked,
		livepubsub.KindProjectSpawned,
		livepubsub.KindClosed:
		writeSSEStatus(w, statusFromKind(ev.Kind), taskID, final, ev.Payload)
	case livepubsub.KindOutcomeRecorded:
		// Outcome events carry structured agent output — promote
		// to an A2A artifact part so consumers see the result
		// envelope explicitly.
		writeSSEArtifact(w, taskID, ev.Payload)
	default:
		// Unknown kinds become informational status events. The
		// A2A client either understands the payload shape or
		// ignores it.
		writeSSEStatus(w, "running", taskID, final, ev.Payload)
	}
}

// writeSSEStatus emits one `event: status` frame.
func writeSSEStatus(w http.ResponseWriter, state, taskID string, final bool, payload any) {
	envelope := map[string]any{
		"taskId":  taskID,
		"state":   state,
		"final":   final,
		"payload": payload,
	}
	writeSSEFrame(w, "status", envelope)
}

// writeSSEArtifact emits one `event: artifact` frame.
func writeSSEArtifact(w http.ResponseWriter, taskID string, payload any) {
	envelope := map[string]any{
		"taskId":  taskID,
		"payload": payload,
	}
	writeSSEFrame(w, "artifact", envelope)
}

// writeSSEFrame writes the canonical SSE record:
//
//	event: <name>
//	data: <json>
//	<blank>
//
// Errors are swallowed — once the headers are sent we have no
// useful error channel back to the client.
func writeSSEFrame(w http.ResponseWriter, name string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteByte('\n')
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	_, _ = w.Write([]byte(b.String()))
}

// statusFromKind maps the livepubsub kind to an A2A state
// vocabulary. The A2A spec recognises submitted / working /
// input-required / completed / failed / canceled; we pick the
// closest match.
func statusFromKind(kind string) string {
	switch kind {
	case livepubsub.KindStepStarted, livepubsub.KindResumed:
		return "working"
	case livepubsub.KindStepCompleted, livepubsub.KindClosed:
		return "completed"
	case livepubsub.KindPaused:
		return "input-required"
	case livepubsub.KindForked:
		return "working"
	case livepubsub.KindProjectSpawned:
		return "working"
	}
	return "working"
}

// isTerminalKind decides when to close the stream. KindClosed is
// the synthetic frame the WS handler already uses for the same
// purpose; we honour it here so the SSE bridge stays in sync.
func isTerminalKind(kind string) bool {
	return kind == livepubsub.KindClosed
}

// Compile-time guard: the SSE helpers only ever return via the
// HTTP response, so the package has no orthogonal error to
// surface beyond the spec one.
var _ = errors.New
