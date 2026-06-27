// Package ui: tests for the /ui/admin/health/runtime page —
// voice + storage runtime readiness. Verifies the handler renders
// the available + unavailable shapes, and that the data threads
// through to the template.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubRuntimeReadiness lets the handler tests inject canned voice +
// storage statuses without spinning up a real probe.
type stubRuntimeReadiness struct {
	voice   VoiceRuntimeStatus
	storage StorageRuntimeStatus
}

func (s *stubRuntimeReadiness) VoiceStatus(_ context.Context) VoiceRuntimeStatus { return s.voice }
func (s *stubRuntimeReadiness) StorageStatus(_ context.Context) StorageRuntimeStatus {
	return s.storage
}

// TestAdminHealthRuntime_NotWired — without WithRuntimeReadinessSource
// the page renders an "Available: false" placeholder so operators
// see the route is alive even before the wiring lands.
func TestAdminHealthRuntime_NotWired(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/health/runtime", nil)
	rec := httptest.NewRecorder()
	srv.AdminHealthRuntime(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("unwired status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Runtime readiness source not wired") {
		t.Errorf("expected unwired-placeholder text; got body fragment %q", firstN(body, 200))
	}
}

// TestAdminHealthRuntime_WiredVoiceOnly — STT + TTS probes render
// in the voice table; storage section renders even with empty data.
func TestAdminHealthRuntime_WiredVoiceOnly(t *testing.T) {
	srv := NewServer(WithRuntimeReadinessSource(&stubRuntimeReadiness{
		voice: VoiceRuntimeStatus{
			STTProvider: "whisper-local",
			TTSProvider: "piper",
			Probes: []VoiceProbeStatus{
				{Label: "Whisper binary", Configured: true, Path: "/usr/bin/whisper-cpp", OK: true},
				{Label: "Piper binary", Configured: true, Path: "/missing", OK: false, Error: "file not found at configured path"},
			},
		},
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/health/runtime", nil)
	rec := httptest.NewRecorder()
	srv.AdminHealthRuntime(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"whisper-local", "piper", "Whisper binary", "/usr/bin/whisper-cpp",
		"Piper binary", "file not found",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected fragment %q in body; not present", want)
		}
	}
}

// TestAdminHealthRuntime_WiredFilesystemStorage — backend=filesystem
// renders the Writable badge and the path.
func TestAdminHealthRuntime_WiredFilesystemStorage(t *testing.T) {
	srv := NewServer(WithRuntimeReadinessSource(&stubRuntimeReadiness{
		storage: StorageRuntimeStatus{
			Backend:            "filesystem",
			FilesystemPath:     "/var/lib/vornik/artifacts",
			FilesystemWritable: true,
		},
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/health/runtime", nil)
	rec := httptest.NewRecorder()
	srv.AdminHealthRuntime(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"filesystem",
		"/var/lib/vornik/artifacts",
		"Writable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

// TestAdminHealthRuntime_WiredS3Storage — backend=s3 renders the
// six S3-specific fields (region, bucket, prefix, endpoint, path-
// style, reachable).
func TestAdminHealthRuntime_WiredS3Storage(t *testing.T) {
	srv := NewServer(WithRuntimeReadinessSource(&stubRuntimeReadiness{
		storage: StorageRuntimeStatus{
			Backend:        "s3",
			S3Region:       "us-east-1",
			S3Bucket:       "vornik-prod",
			S3Prefix:       "prefix-x",
			S3Endpoint:     "http://localhost:9000",
			S3UsePathStyle: true,
			S3Reachable:    false,
			S3Error:        "deferred reachability probe",
		},
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/health/runtime", nil)
	rec := httptest.NewRecorder()
	srv.AdminHealthRuntime(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"us-east-1", "vornik-prod", "prefix-x", "http://localhost:9000",
		"Reachable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body", want)
		}
	}
}

// TestAdminHealthIndex_LinksToRuntime — the landing page must list
// /ui/admin/health/runtime so the route is discoverable. Catches the
// regression of forgetting to add the tile when the page lands.
func TestAdminHealthIndex_LinksToRuntime(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/health", nil)
	rec := httptest.NewRecorder()
	srv.AdminHealthIndex(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "/ui/admin/health/runtime") {
		t.Errorf("admin health index missing runtime link; body fragment %q", firstN(body, 400))
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
