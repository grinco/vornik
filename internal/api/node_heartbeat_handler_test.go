package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
)

type recordingClusterNodes struct{ upserts []*persistence.ClusterNode }

func (r *recordingClusterNodes) Upsert(_ context.Context, n *persistence.ClusterNode) error {
	r.upserts = append(r.upserts, n)
	return nil
}
func (r *recordingClusterNodes) List(context.Context) ([]*persistence.ClusterNode, error) {
	return r.upserts, nil
}
func (r *recordingClusterNodes) DeleteByInstanceID(context.Context, string) error { return nil }
func (r *recordingClusterNodes) DeleteStale(context.Context, time.Duration, []string) (int, error) {
	return 0, nil
}

// TestNodeHeartbeat_IncrementsReceivedCounter pins the always-on positive
// signal: a successful heartbeat bumps vornik_node_heartbeat_received_total
// labelled by the reporting node's profile.
func TestNodeHeartbeat_IncrementsReceivedCounter(t *testing.T) {
	repo := &recordingClusterNodes{}
	srv := NewServer(WithClusterNodeRepository(repo))
	srv.apiMetrics = NewAPIMetrics(prometheus.NewRegistry())
	b, _ := json.Marshal(nodeHeartbeatEnvelope{InstanceID: "dmz-1", Profile: "webhook", Version: "v1"})
	rw := httptest.NewRecorder()
	srv.NodeHeartbeat(rw, httptest.NewRequest(http.MethodPost, "/internal/v1/node-heartbeat", bytes.NewReader(b)))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rw.Code)
	}
	if got := testutil.ToFloat64(srv.apiMetrics.NodeHeartbeatReceivedTotal.WithLabelValues("webhook")); got != 1 {
		t.Fatalf("node_heartbeat_received_total{profile=webhook} = %v, want 1", got)
	}
}

func TestNodeHeartbeat_UpsertsRelayedNode(t *testing.T) {
	repo := &recordingClusterNodes{}
	srv := NewServer(WithClusterNodeRepository(repo))
	env := nodeHeartbeatEnvelope{
		InstanceID:   "dmz-webhook-1",
		Profile:      "webhook",
		Version:      "v-test",
		Address:      "10.0.9.4:443",
		Capabilities: map[string]bool{"ServeWebhooks": true, "RelayMode": true},
	}
	b, _ := json.Marshal(env)
	rw := httptest.NewRecorder()
	srv.NodeHeartbeat(rw, httptest.NewRequest(http.MethodPost, "/internal/v1/node-heartbeat", bytes.NewReader(b)))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rw.Code, rw.Body.String())
	}
	if len(repo.upserts) != 1 {
		t.Fatalf("expected exactly one upsert, got %d", len(repo.upserts))
	}
	got := repo.upserts[0]
	if got.InstanceID != "dmz-webhook-1" {
		t.Fatalf("upserted instance_id = %q, want %q", got.InstanceID, "dmz-webhook-1")
	}
	if got.Profile != "webhook" {
		t.Fatalf("upserted profile = %q, want %q", got.Profile, "webhook")
	}
	if got.Version != "v-test" {
		t.Fatalf("upserted version = %q, want %q", got.Version, "v-test")
	}
	// LastSeen is intentionally zero here: the handler no longer stamps it.
	// In production the DB's Upsert sets last_seen = now() atomically; the stub
	// repo used in this test does not simulate that (no DB clock). The assertion
	// above proves the relayed node was registered with the correct identity.
	if !got.Capabilities["ServeWebhooks"] || !got.Capabilities["RelayMode"] {
		t.Fatalf("capabilities not relayed correctly: %+v", got.Capabilities)
	}
}

func TestNodeHeartbeat_RejectsBadRequest(t *testing.T) {
	srv := NewServer(WithClusterNodeRepository(&recordingClusterNodes{}))
	// non-POST
	rw := httptest.NewRecorder()
	srv.NodeHeartbeat(rw, httptest.NewRequest(http.MethodGet, "/internal/v1/node-heartbeat", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d", rw.Code)
	}
	// missing instance_id
	rw2 := httptest.NewRecorder()
	srv.NodeHeartbeat(rw2, httptest.NewRequest(http.MethodPost, "/internal/v1/node-heartbeat", bytes.NewReader([]byte(`{"profile":"webhook"}`))))
	if rw2.Code != http.StatusBadRequest {
		t.Fatalf("missing instance_id want 400, got %d", rw2.Code)
	}
	// repo not configured
	srv3 := NewServer()
	rw3 := httptest.NewRecorder()
	srv3.NodeHeartbeat(rw3, httptest.NewRequest(http.MethodPost, "/internal/v1/node-heartbeat", bytes.NewReader([]byte(`{"instance_id":"x"}`))))
	if rw3.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil repo want 503, got %d", rw3.Code)
	}
}
