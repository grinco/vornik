package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GetTaskLogs handles GET /api/v1/projects/{projectId}/tasks/{taskId}/logs.
func (s *Server) GetTaskLogs(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	taskID := extractTaskID(r)
	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and taskId are required")
		return
	}

	tail := parseTailParam(r, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	logs, err := s.taskLogs(ctx, projectID, taskID, tail)
	if err != nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

func parseTailParam(r *http.Request, fallback int) int {
	tail := fallback
	if raw := r.URL.Query().Get("tail"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			tail = n
		}
	}
	if tail > 5000 {
		tail = 5000
	}
	return tail
}

func (s *Server) taskLogs(ctx context.Context, projectID, taskID string, tail int) (string, error) {
	if s.taskRepo != nil {
		task, err := s.taskRepo.Get(ctx, taskID)
		if err != nil || task == nil {
			return "", fmt.Errorf("task not found")
		}
		if task.ProjectID != projectID {
			return "", fmt.Errorf("task does not belong to project")
		}
	}

	if s.taskLogSource != nil {
		if logs, err := s.taskLogSource.TaskLogs(ctx, taskID, tail); err == nil && strings.TrimSpace(logs) != "" {
			return trimLines(logs, tail), nil
		}
	}

	logs, err := s.persistedTaskLogExcerpt(ctx, projectID, taskID)
	if err != nil {
		return "", err
	}
	return trimLines(logs, tail), nil
}

func (s *Server) persistedTaskLogExcerpt(ctx context.Context, projectID, taskID string) (string, error) {
	if s.executionRepo != nil {
		exec, err := s.executionRepo.GetByTaskID(ctx, taskID)
		if err == nil && exec != nil && exec.ProjectID == projectID {
			if exec.ErrorMessage != nil && strings.TrimSpace(*exec.ErrorMessage) != "" {
				return *exec.ErrorMessage, nil
			}
			if msg := resultMessage(exec.Result); msg != "" {
				return msg, nil
			}
		}
	}
	if s.taskRepo != nil {
		task, err := s.taskRepo.Get(ctx, taskID)
		if err == nil && task != nil && task.ProjectID == projectID && task.LastError != nil && strings.TrimSpace(*task.LastError) != "" {
			return *task.LastError, nil
		}
	}
	return "", fmt.Errorf("no logs available for task")
}

func resultMessage(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var result struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ""
	}
	if strings.TrimSpace(result.Message) != "" {
		return result.Message
	}
	if result.Status != "" {
		return "result status: " + result.Status
	}
	return ""
}

func trimLines(s string, max int) string {
	if max <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= max {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-max:], "\n")
}
