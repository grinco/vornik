package ui

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
)

// TaskLogsStream serves an SSE stream for the task detail log panel.
func (s *Server) TaskLogsStream(w http.ResponseWriter, r *http.Request, taskID string) {
	if taskID == "" {
		http.NotFound(w, r)
		return
	}
	// Project-scope check before opening the stream. A scoped key
	// for project A must not subscribe to project B's logs. Load
	// the task once up front to get project_id; if it can't be
	// resolved (no taskRepo wired) we leave the stream open — the
	// log source layer is the next defensive layer, and a misconfig
	// shouldn't black-hole the UX on auth-disabled deployments.
	if s.taskRepo != nil {
		lookupCtx, lookupCancel := context.WithTimeout(r.Context(), 3*time.Second)
		task, err := s.taskRepo.Get(lookupCtx, taskID)
		lookupCancel()
		if err == nil && task != nil && task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
			http.NotFound(w, r)
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(text string) error {
		if _, err := fmt.Fprint(w, sseData(logHTML(text))); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	last := ""
	for {
		text := s.fetchTaskLogs(r.Context(), taskID, 200)
		if text != last {
			// A write error almost always means the client has gone away.
			// Without this check the loop would keep polling until the next
			// ctx-tick, leaving a goroutine behind for every closed tab.
			if err := send(text); err != nil {
				return
			}
			last = text
		}

		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Server) fetchTaskLogs(ctx context.Context, taskID string, tail int) string {
	if s.taskLogSource != nil {
		logCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if logs, err := s.taskLogSource.TaskLogs(logCtx, taskID, tail); err == nil && strings.TrimSpace(logs) != "" {
			return trimLogLines(logs, tail)
		}
	}
	if s.execRepo != nil {
		logCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if exec, err := s.execRepo.GetByTaskID(logCtx, taskID); err == nil && exec != nil {
			if exec.ErrorMessage != nil && strings.TrimSpace(*exec.ErrorMessage) != "" {
				return trimLogLines(*exec.ErrorMessage, tail)
			}
		}
	}
	return "No logs available yet."
}

func logHTML(text string) string {
	escaped := html.EscapeString(strings.TrimRight(text, "\n"))
	if escaped == "" {
		escaped = "No logs available yet."
	}

	// Add color highlighting for log levels
	escaped = strings.ReplaceAll(escaped, "[INFO]", `<span class="text-blue-400 font-bold">[INFO]</span>`)
	escaped = strings.ReplaceAll(escaped, "[WARN]", `<span class="text-yellow-400 font-bold">[WARN]</span>`)
	escaped = strings.ReplaceAll(escaped, "[ERROR]", `<span class="text-red-500 font-bold">[ERROR]</span>`)
	escaped = strings.ReplaceAll(escaped, "[DEBUG]", `<span class="text-gray-500 font-bold">[DEBUG]</span>`)
	// Colorize dates (YYYY-MM-DD) if present at start of line
	// We'll keep it simple for now with just the levels to avoid expensive regex.

	return `<pre class="text-xs leading-5 text-gray-300 whitespace-pre-wrap font-mono">` + escaped + `</pre>`
}

func sseData(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

func trimLogLines(s string, max int) string {
	if max <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= max {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-max:], "\n")
}
