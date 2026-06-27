// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

func TestConfigHandlers_GetReloadStatus(t *testing.T) {
	t.Run("returns status when reloader is available", func(t *testing.T) {
		watcher := config.NewWatcher([]string{"/tmp/test"},
			config.WithWatchLogger(zerolog.Nop()),
		)
		reloader := config.NewConfigReloader(watcher, zerolog.Nop())
		handlers := NewConfigHandlers(reloader)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/config/reload-status", nil)
		rec := httptest.NewRecorder()

		handlers.GetReloadStatus(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var resp ReloadStatusResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.HasErrors {
			t.Error("expected no errors initially")
		}
		if resp.PendingActivation {
			t.Error("expected no pending activation initially")
		}
	})

	t.Run("returns error when reloader is nil", func(t *testing.T) {
		handlers := NewConfigHandlers(nil)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/config/reload-status", nil)
		rec := httptest.NewRecorder()

		handlers.GetReloadStatus(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", rec.Code)
		}
	})

	t.Run("rejects non-GET methods", func(t *testing.T) {
		watcher := config.NewWatcher([]string{"/tmp/test"},
			config.WithWatchLogger(zerolog.Nop()),
		)
		reloader := config.NewConfigReloader(watcher, zerolog.Nop())
		handlers := NewConfigHandlers(reloader)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload-status", nil)
		rec := httptest.NewRecorder()

		handlers.GetReloadStatus(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", rec.Code)
		}
	})
}

func TestConfigHandlers_Reload(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		watcher := config.NewWatcher([]string{"/tmp/test"},
			config.WithWatchLogger(zerolog.Nop()),
		)
		reloader := config.NewConfigReloader(watcher, zerolog.Nop())
		handlers := NewConfigHandlers(reloader)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/config/reload", nil)
		rec := httptest.NewRecorder()

		handlers.Reload(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", rec.Code)
		}
	})

	t.Run("returns error when reloader is nil", func(t *testing.T) {
		handlers := NewConfigHandlers(nil)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", nil)
		rec := httptest.NewRecorder()

		handlers.Reload(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("expected status 503, got %d", rec.Code)
		}
	})

	// 2026-05-27: companion-onboarding silent-strip bug regression.
	// A successful reload that carried strip warnings (project's
	// workflow ref didn't resolve, etc.) used to return success +
	// no signal that anything was missing. Now the response body
	// echoes the warnings so operators can react programmatically.
	t.Run("success response surfaces strip warnings", func(t *testing.T) {
		watcher := config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop()))
		reloader := config.NewConfigReloader(watcher, zerolog.Nop())
		reloader.SetLoader(func() error { return nil })
		reloader.SetValidator(func() error {
			reloader.RecordReloadWarning("project 'companion-example' references non-existent workflow 'companion-architectural-review'")
			return nil
		})
		reloader.SetActivator(func() error { return nil })
		handlers := NewConfigHandlers(reloader)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", nil)
		rec := httptest.NewRecorder()
		handlers.Reload(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d; body=%s", rec.Code, rec.Body.String())
		}
		var body ReloadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !body.Success {
			t.Error("reload with non-fatal warnings should still report success=true")
		}
		if len(body.Warnings) != 1 {
			t.Fatalf("expected 1 warning in response body, got %d: %v", len(body.Warnings), body.Warnings)
		}
		if body.Warnings[0] != "project 'companion-example' references non-existent workflow 'companion-architectural-review'" {
			t.Errorf("warning text mismatch: %q", body.Warnings[0])
		}
	})

	t.Run("reload-status surfaces warnings from last cycle", func(t *testing.T) {
		watcher := config.NewWatcher([]string{}, config.WithWatchLogger(zerolog.Nop()))
		reloader := config.NewConfigReloader(watcher, zerolog.Nop())
		reloader.SetLoader(func() error { return nil })
		reloader.SetValidator(func() error {
			reloader.RecordReloadWarning("strip warning A")
			reloader.RecordReloadWarning("strip warning B")
			return nil
		})
		reloader.SetActivator(func() error { return nil })
		handlers := NewConfigHandlers(reloader)

		// Trigger the reload so the status field populates.
		reloadReq := httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", nil)
		reloadRec := httptest.NewRecorder()
		handlers.Reload(reloadRec, reloadReq)
		if reloadRec.Code != http.StatusOK {
			t.Fatalf("reload setup: expected 200, got %d", reloadRec.Code)
		}

		// Now hit status — must echo the same warnings.
		statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/config/reload-status", nil)
		statusRec := httptest.NewRecorder()
		handlers.GetReloadStatus(statusRec, statusReq)

		if statusRec.Code != http.StatusOK {
			t.Fatalf("status: expected 200, got %d", statusRec.Code)
		}
		var status ReloadStatusResponse
		if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if !status.HasWarnings {
			t.Error("HasWarnings must be true when warnings present")
		}
		if len(status.Warnings) != 2 {
			t.Fatalf("expected 2 warnings, got %d: %v", len(status.Warnings), status.Warnings)
		}
	})
}

func TestConfigHandlers_ResponseFormats(t *testing.T) {
	t.Run("ReloadStatusResponse has correct JSON tags", func(t *testing.T) {
		resp := ReloadStatusResponse{
			LastReload:        time.Now().Format(time.RFC3339),
			LastAttempt:       time.Now().Format(time.RFC3339),
			Errors:            []string{"test error"},
			HasErrors:         true,
			PendingActivation: true,
			Blocked:           true,
			BlockedReason:     "test blocked",
		}

		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		// Verify JSON field names
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if _, ok := raw["last_reload"]; !ok {
			t.Error("missing last_reload field")
		}
		if _, ok := raw["has_errors"]; !ok {
			t.Error("missing has_errors field")
		}
		if _, ok := raw["pending_activation"]; !ok {
			t.Error("missing pending_activation field")
		}
		if _, ok := raw["blocked_reason"]; !ok {
			t.Error("missing blocked_reason field")
		}
	})

	t.Run("ReloadResponse has correct JSON tags", func(t *testing.T) {
		resp := ReloadResponse{
			Success:   true,
			Message:   "test message",
			Timestamp: time.Now().Format(time.RFC3339),
		}

		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		if _, ok := raw["success"]; !ok {
			t.Error("missing success field")
		}
		if _, ok := raw["message"]; !ok {
			t.Error("missing message field")
		}
		if _, ok := raw["timestamp"]; !ok {
			t.Error("missing timestamp field")
		}
	})
}
