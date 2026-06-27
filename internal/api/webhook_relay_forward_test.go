package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/registry"
)

type fakeForwarder struct {
	called     bool
	statusCode int
}

func (f *fakeForwarder) Forward(ctx context.Context, projectID, source, deliveryID string, body []byte) (int, error) {
	f.called = true
	return f.statusCode, nil
}

// signWebhookRequest sets the X-Vornik-Signature header on req using the
// source's secret. Mirrors signWebhook from handlers_test.go but operates on
// *http.Request directly for convenience in relay-mode tests.
func signWebhookRequest(t *testing.T, req *http.Request, body []byte, source registry.ProjectWebhookSource) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(source.Secret))
	_, _ = mac.Write(body)
	req.Header.Set("X-Vornik-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
}

func TestIngestWebhook_RelayModeForwards(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	fwd := &fakeForwarder{statusCode: http.StatusAccepted}
	srv.SetWebhookRelay(fwd) // option-set in test; production uses WithWebhookRelay

	body := []byte(`{"action":"opened"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+project.ID+"/"+source.Name, bytes.NewReader(body))
	signWebhookRequest(t, req, body, source)
	rec := httptest.NewRecorder()
	srv.IngestWebhook(rec, req)

	if !fwd.called {
		t.Fatal("relay-mode IngestWebhook must forward via the relay client")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 mirrored from forwarder, got %d", rec.Code)
	}
}

// TestIngestWebhook_RelayMode_BadSignatureNotForwarded verifies that HMAC
// verification still runs on the DMZ node BEFORE the relay branch: a bad
// signature must 401 and must NOT call the forwarder.
func TestIngestWebhook_RelayMode_BadSignatureNotForwarded(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	fwd := &fakeForwarder{statusCode: http.StatusAccepted}
	srv.SetWebhookRelay(fwd)

	body := []byte(`{"action":"opened"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+project.ID+"/"+source.Name, bytes.NewReader(body))
	// Deliberately wrong secret — must be rejected before forwarding.
	mac := hmac.New(sha256.New, []byte("WRONG-secret"))
	_, _ = mac.Write(body)
	req.Header.Set("X-Vornik-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	srv.IngestWebhook(rec, req)

	if fwd.called {
		t.Fatal("bad signature must NOT reach the relay forwarder")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for bad signature, got %d", rec.Code)
	}
}

// TestIngestWebhook_RelayModeNoTaskRepo is the regression test for the
// 2026-06-12 incident: a DMZ RelayMode node has NO taskRepo (no DB) — only a
// relay forwarder. The old `taskRepo == nil` guard 503'd every GitHub delivery
// with WEBHOOK_NOT_CONFIGURED before the relay branch could run. Ingestion must
// work with taskRepo nil as long as a relay forwarder is wired.
func TestIngestWebhook_RelayModeNoTaskRepo(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	srv.taskRepo = nil // real DMZ shape — relay node has no database
	fwd := &fakeForwarder{statusCode: http.StatusAccepted}
	srv.SetWebhookRelay(fwd)

	body := []byte(`{"action":"opened"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+project.ID+"/"+source.Name, bytes.NewReader(body))
	signWebhookRequest(t, req, body, source)
	rec := httptest.NewRecorder()
	srv.IngestWebhook(rec, req)

	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("DMZ relay node (taskRepo nil + relay set) must NOT 503 WEBHOOK_NOT_CONFIGURED; got %d: %s", rec.Code, rec.Body.String())
	}
	if !fwd.called {
		t.Fatal("relay-mode IngestWebhook must forward via the relay client when taskRepo is nil")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 mirrored from forwarder, got %d", rec.Code)
	}
}
