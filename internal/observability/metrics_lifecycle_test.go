package observability

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func TestStartServer_ServesMetricsEndpoint(t *testing.T) {
	addr := freePort(t)
	m := NewMetrics(Config{MetricsAddr: addr}, zerolog.Nop())

	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { errCh <- m.StartServer(ctx) }()

	// Wait for the listener to bind.
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /metrics never succeeded: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "vornik_observability_up") {
		t.Errorf("metrics body missing vornik_observability_up marker")
	}

	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer sCancel()
	if err := m.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Errorf("StartServer returned %v after Shutdown (want nil — ErrServerClosed should map to nil)", err)
	}
}

func TestShutdown_NilServerIsNoop(t *testing.T) {
	m := NewMetrics(Config{MetricsAddr: ":0"}, zerolog.Nop())
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before StartServer returned %v, want nil", err)
	}
}

func TestRegistry_ReturnsLiveRegistry(t *testing.T) {
	m := NewMetrics(Config{MetricsAddr: ":0"}, zerolog.Nop())
	if r := m.Registry(); r == nil {
		t.Fatal("Registry() returned nil")
	}
}

func TestMustRegister_RegistersCustomCollector(t *testing.T) {
	m := NewMetrics(Config{MetricsAddr: ":0"}, zerolog.Nop())
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "test",
		Name:      "lifecycle_marker",
		Help:      "marker used by metrics-lifecycle test",
	})
	m.MustRegister(c)
	c.Inc()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "vornik_test_lifecycle_marker" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("custom collector not visible on registry")
	}
}

func TestStartServer_PortAlreadyBoundReturnsError(t *testing.T) {
	// Bind a port and hold it; StartServer must fail with the
	// fmt.Errorf("metrics server error: %w", err) wrap.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	defer func() { _ = l.Close() }()

	m := NewMetrics(Config{MetricsAddr: addr}, zerolog.Nop())
	err = m.StartServer(context.Background())
	if err == nil {
		t.Fatal("StartServer should fail when port is busy")
	}
	if !strings.Contains(err.Error(), "metrics server error") {
		t.Errorf("error %q missing wrap prefix", err.Error())
	}
}
