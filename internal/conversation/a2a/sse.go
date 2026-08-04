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
	"io"
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

// artifactLister lists a task's artifacts (metadata). The wiring adapts
// persistence.ArtifactRepository into this narrow shape.
type artifactLister interface {
	List(ctx context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error)
}

// artifactOpener reads an artifact's body by ID (backend-aware: local + S3).
// Satisfied by the daemon's artifact store (api.ArtifactOpener shape).
type artifactOpener interface {
	Open(ctx context.Context, artifactID string) (io.ReadCloser, error)
}

// SSEDeps bundles the dependencies the streaming endpoint needs
// on top of what Handler already carries. Kept as a separate
// type so the production wiring can pass them explicitly without
// growing Handler's surface for tests that only exercise the
// card / submit paths.
type SSEDeps struct {
	Executions executionLookup
	Tasks      taskLookup
	// Artifacts + ArtifactOpener source the answer on terminal: a completed
	// top-level task's deliverable is its non-transcript OUTPUT-class
	// artifact content (Task.ResultEnvelope is unwired for top-level tasks).
	// Both nil → fall back to ResultEnvelope (cross-project-style tasks).
	Artifacts      artifactLister
	ArtifactOpener artifactOpener
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
	startSSE(w)
	flusher, _ := w.(http.Flusher)

	// Wait for the execution to be created. An A2A caller opens the stream
	// immediately after POSTing the task, before the scheduler leases it, so
	// there is usually no execution row yet. Closing the stream now would make
	// the client see a lone "submitted" and give up — the 2026-08-01 loopback
	// e2e failure (`a2a client: partner ended in state "submitted"`), even
	// though the task then ran to completion. Instead keep the stream open
	// with keepalives and poll until the execution appears.
	exec := h.awaitExecution(r.Context(), taskID, w, flusher)
	if exec == nil {
		return // client disconnected or the task never started within the deadline
	}
	events, cancel, err := h.LiveSubscriber.Subscribe(exec.ID, 0)
	if err != nil {
		writeSSEStatus(w, "failed", taskID, true, map[string]any{"error": "subscribe failed"})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	defer cancel()

	// The task may already have finished during the queue/subscribe gap. A
	// subscription to an already-complete execution yields an OPEN channel with
	// no events (the terminal was published before we subscribed and isn't
	// replayed), so the loop would hang on keepalives forever — the 2026-08-01
	// loopback e2e "stuck consumer". Deliver the answer directly if the task is
	// already terminal.
	if finishIfTerminal(r.Context(), w, flusher, taskID) {
		return
	}

	h.streamEvents(r.Context(), w, flusher, taskID, events)
}

// streamEvents runs the SSE write loop: translate live events to A2A frames
// until a terminal one, emit periodic keepalives, and (as a safety net for a
// terminal live event that never arrives) re-check the task's terminal status
// on each tick. Returns when terminal, the client disconnects, or the event
// channel closes.
func (h *Handler) streamEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, taskID string, events <-chan livepubsub.LiveEvent) {
	pingTicker := time.NewTicker(terminalRecheckInterval)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			if finishIfTerminal(ctx, w, flusher, taskID) {
				return
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case ev, ok := <-events:
			if !ok {
				return // stream closed
			}
			final := isTerminalKind(ev.Kind)
			if final {
				// No live event carries the answer text — it lives in the task's
				// OUTPUT artifacts (or ResultEnvelope). Emit it BEFORE the
				// terminal status so the A2A caller receives it, then closes.
				emitAnswerArtifact(ctx, w, taskID)
			}
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

// answerInlineCapBytes bounds how much OUTPUT-artifact content is inlined into
// the answer frame. Matches the companion result() inline budget (64 KiB); the
// SSE frame rides well under any proxy limit either way.
const answerInlineCapBytes = 64 * 1024

// terminalRecheckInterval is how often the stream re-checks the task's terminal
// status (a fallback for a terminal live event that never arrives) while also
// emitting an SSE keepalive. Var so tests can shorten it.
var terminalRecheckInterval = 3 * time.Second

// finishIfTerminal emits the answer + a final status frame and returns true when
// the task has already reached a terminal status. This covers the race where the
// task finished before/around the live subscription — a subscription to an
// already-complete execution yields an open channel with no events, so the
// stream would otherwise hang on keepalives waiting for a terminal that already
// fired.
func finishIfTerminal(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, taskID string) bool {
	if streamDeps == nil || streamDeps.Tasks == nil {
		return false
	}
	task, err := streamDeps.Tasks.Get(ctx, taskID)
	if err != nil || task == nil {
		return false
	}
	state, ok := a2aStateForTerminalStatus(task.Status)
	if !ok {
		return false
	}
	emitAnswerArtifact(ctx, w, taskID)
	writeSSEStatus(w, state, taskID, true, nil)
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

// a2aStateForTerminalStatus maps a terminal task status to its A2A state,
// returning ok=false for any non-terminal status.
func a2aStateForTerminalStatus(s persistence.TaskStatus) (string, bool) {
	switch s {
	case persistence.TaskStatusCompleted:
		return "completed", true
	case persistence.TaskStatusFailed:
		return "failed", true
	case persistence.TaskStatusCancelled:
		return "canceled", true
	}
	return "", false
}

// emitAnswerArtifact writes the completed task's answer as an A2A
// `event: artifact` frame on terminal. No livepubsub event carries the answer
// text (OutcomeRecorded is class/notes; LLM tokens aren't proxied), so this is
// the ONLY channel by which an A2A caller receives the result. The answer of a
// top-level task is its non-transcript OUTPUT-class artifact content — the same
// deliverable the companion result() tool serves. ResultEnvelope is a fallback
// for cross-project-style tasks that populate it. The shared outbound client
// (internal/a2a/client) reads this frame's payload.
func emitAnswerArtifact(ctx context.Context, w http.ResponseWriter, taskID string) {
	if streamDeps == nil {
		return
	}
	if payload := buildAnswerFromArtifacts(ctx, taskID); len(payload) > 0 {
		writeSSEArtifact(w, taskID, json.RawMessage(payload))
		return
	}
	if streamDeps.Tasks != nil {
		if task, err := streamDeps.Tasks.Get(ctx, taskID); err == nil && task != nil && len(task.ResultEnvelope) > 0 {
			writeSSEArtifact(w, taskID, json.RawMessage(task.ResultEnvelope))
		}
	}
}

// buildAnswerFromArtifacts reads the task's non-transcript OUTPUT-class
// artifacts and returns a JSON answer payload, or nil when there is nothing to
// emit. A single JSON-object artifact (e.g. a workflow's result.json) is
// emitted verbatim so the client can extract its structured fields; otherwise
// the concatenated text content is wrapped as {"answer": …}.
func buildAnswerFromArtifacts(ctx context.Context, taskID string) []byte {
	if streamDeps.Artifacts == nil || streamDeps.ArtifactOpener == nil {
		return nil
	}
	arts, err := streamDeps.Artifacts.List(ctx, persistence.ArtifactFilter{TaskID: &taskID, PageSize: 200})
	if err != nil || len(arts) == 0 {
		return nil
	}
	budget := answerInlineCapBytes
	var b strings.Builder
	var single []byte
	count := 0
	for _, a := range arts {
		if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput || isTranscriptArtifactName(a.Name) {
			continue
		}
		if budget <= 0 {
			break
		}
		rc, oerr := streamDeps.ArtifactOpener.Open(ctx, a.ID)
		if oerr != nil {
			continue
		}
		content, _ := io.ReadAll(io.LimitReader(rc, int64(budget)))
		_ = rc.Close()
		if len(content) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.Write(content)
		budget -= len(content)
		single = content
		count++
	}
	if b.Len() == 0 {
		return nil
	}
	// A lone JSON-object artifact (result.json) is passed through so the client
	// sees the workflow's own {answer, citations, …} schema.
	if count == 1 {
		trimmed := strings.TrimSpace(string(single))
		if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
			return []byte(trimmed)
		}
	}
	payload, err := json.Marshal(map[string]string{"answer": b.String()})
	if err != nil {
		return nil
	}
	return payload
}

// isTranscriptArtifactName mirrors executor.IsTranscriptArtifact's rule
// (…-response.md are step transcripts, not the deliverable) without importing
// the executor package. Kept minimal; the canonical rule lives in
// internal/executor/artifacts.go.
func isTranscriptArtifactName(name string) bool {
	return strings.HasSuffix(name, "-response.md")
}

// executionWaitTimeout bounds how long the SSE stream waits for a just-
// submitted task's execution row to appear before giving up; executionPollInterval
// is the poll cadence. Vars (not consts) so tests can shorten them. The client
// keeps its own idle timer alive via the keepalive pings emitted while waiting.
var (
	executionWaitTimeout  = 120 * time.Second
	executionPollInterval = time.Second
)

// awaitExecution returns the task's execution, polling until it appears (the
// task starts QUEUED and is leased moments later), the deadline elapses, or the
// client disconnects. It emits an initial "submitted" status and periodic
// keepalives so a fast A2A client that opened the stream right after submit
// keeps waiting instead of seeing an empty close. startSSE must already have run.
func (h *Handler) awaitExecution(ctx context.Context, taskID string, w http.ResponseWriter, flusher http.Flusher) *persistence.Execution {
	if exec, err := streamDeps.Executions.GetByTaskID(ctx, taskID); err == nil && exec != nil {
		return exec
	}
	writeSSEStatus(w, "submitted", taskID, false, nil)
	if flusher != nil {
		flusher.Flush()
	}
	ticker := time.NewTicker(executionPollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(executionWaitTimeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-ticker.C:
			if exec, err := streamDeps.Executions.GetByTaskID(ctx, taskID); err == nil && exec != nil {
				return exec
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return nil // client gone
			}
			if flusher != nil {
				flusher.Flush()
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
