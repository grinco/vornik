package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRelayWebhook_IncrementsReceivedCounter pins the always-on positive
// signal: a relayed delivery reaching this worker bumps
// vornik_webhook_relay_received_total.
func TestRelayWebhook_IncrementsReceivedCounter(t *testing.T) {
	srv, project, source := newWebhookTestServer(t)
	srv.apiMetrics = NewAPIMetrics(prometheus.NewRegistry())
	b, _ := json.Marshal(relayEnvelope{ProjectID: project.ID, Source: source.Name, DeliveryID: "d-metric", Body: []byte(`{"action":"opened"}`)})
	rec := httptest.NewRecorder()
	srv.RelayWebhook(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/webhook-relay", bytes.NewReader(b)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(srv.apiMetrics.WebhookRelayReceivedTotal); got != 1 {
		t.Fatalf("webhook_relay_received_total = %v, want 1", got)
	}
}

func TestRelayWebhook_EnqueuesVerifiedEvent(t *testing.T) {
	srv, project, source := newWebhookTestServer(t) // same helper as Task 2
	payload := relayEnvelope{
		ProjectID:  project.ID,
		Source:     source.Name,
		DeliveryID: "delivery-xyz",
		Body:       []byte(`{"action":"opened"}`),
	}
	b, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	srv.RelayWebhook(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/webhook-relay", bytes.NewReader(b)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRelayWebhook_RejectsUnknownProject(t *testing.T) {
	srv, _, source := newWebhookTestServer(t)
	b, _ := json.Marshal(relayEnvelope{ProjectID: "nope", Source: source.Name, Body: []byte(`{}`)})
	rec := httptest.NewRecorder()
	srv.RelayWebhook(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/webhook-relay", bytes.NewReader(b)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown project, got %d", rec.Code)
	}
}

func TestRelayWebhook_NonPost_Returns405(t *testing.T) {
	srv, _, _ := newWebhookTestServer(t)
	rec := httptest.NewRecorder()
	srv.RelayWebhook(rec, httptest.NewRequest(http.MethodGet, "/internal/v1/webhook-relay", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 for GET, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRelayWebhook_BadEnvelope_Returns400(t *testing.T) {
	srv, _, _ := newWebhookTestServer(t)
	rec := httptest.NewRecorder()
	srv.RelayWebhook(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/webhook-relay", bytes.NewReader([]byte("not json"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-JSON body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRelayWebhook_MissingFields_Returns400(t *testing.T) {
	srv, _, _ := newWebhookTestServer(t)
	cases := []struct {
		name string
		env  relayEnvelope
	}{
		{"empty ProjectID", relayEnvelope{ProjectID: "", Source: "github", Body: []byte(`{}`)}},
		{"empty Source", relayEnvelope{ProjectID: "proj-1", Source: "", Body: []byte(`{}`)}},
		{"both empty", relayEnvelope{Body: []byte(`{}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.env)
			rec := httptest.NewRecorder()
			srv.RelayWebhook(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/webhook-relay", bytes.NewReader(b)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400 for %s, got %d: %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}
