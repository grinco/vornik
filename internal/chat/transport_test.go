package chat

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSharedHTTPTransport_TunedSingleton pins the connection-pool tuning
// and the singleton contract.
func TestSharedHTTPTransport_TunedSingleton(t *testing.T) {
	a := sharedHTTPTransport()
	b := sharedHTTPTransport()
	if a != b {
		t.Fatal("sharedHTTPTransport must return the same singleton on every call")
	}
	if a.MaxIdleConnsPerHost != chatTransportMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d (Go's default of 2 forces TLS churn at queue=8)", a.MaxIdleConnsPerHost, chatTransportMaxIdleConnsPerHost)
	}
	if a.MaxConnsPerHost != 0 {
		t.Fatalf("MaxConnsPerHost = %d, want 0 (parallelism is gated at the chat queue, not the transport)", a.MaxConnsPerHost)
	}
	if a.MaxIdleConns != 100 {
		t.Fatalf("MaxIdleConns = %d, want 100", a.MaxIdleConns)
	}
}

// TestNewClient_WiresSharedTransport is the regression guard: a bare
// &http.Client{} silently falls back to the under-pooled
// http.DefaultTransport. NewClient must wire the tuned shared transport.
func TestNewClient_WiresSharedTransport(t *testing.T) {
	c := NewClient("https://example.invalid", "k", "m")
	if c.httpClient == nil || c.httpClient.Transport == nil {
		t.Fatal("NewClient must wire a Transport, not fall back to DefaultTransport")
	}
	if c.httpClient.Transport != sharedHTTPTransport() {
		t.Fatal("NewClient should use the shared chat transport")
	}
}

// TestSubscriptionClients_WireSharedTransport — the Claude/Codex
// subscription clients get the shared transport while preserving their
// ctx-governed (Timeout: 0) request semantics.
func TestSubscriptionClients_WireSharedTransport(t *testing.T) {
	cs := NewClaudeSubscriptionClient("m")
	if cs.http.Transport != sharedHTTPTransport() {
		t.Fatal("Claude subscription client should use the shared chat transport")
	}
	if cs.http.Timeout != 0 {
		t.Fatalf("Claude subscription client Timeout = %v, want 0 (ctx-governed)", cs.http.Timeout)
	}
	cx := NewCodexSubscriptionClient("m")
	if cx.http.Transport != sharedHTTPTransport() {
		t.Fatal("Codex subscription client should use the shared chat transport")
	}
	if cx.http.Timeout != 0 {
		t.Fatalf("Codex subscription client Timeout = %v, want 0 (ctx-governed)", cx.http.Timeout)
	}
}

// TestChatTransport_PoolsConnectionsAcrossWaves proves the tuning has
// teeth at runtime, not just in config fields. Wave 1 forces N truly
// simultaneous connections open (a server-side barrier holds every
// handler until all N have arrived), so N connections are dialed and
// returned to the idle pool. Wave 2 fires N concurrent requests again:
// with the tuned pool (idle/host = 32 ≥ N) every one reuses an idle
// connection, so ZERO new connections are dialed. Under Go's default
// (idle/host = 2) wave 2 would dial N-2 fresh connections — the churn
// this change removes.
func TestChatTransport_PoolsConnectionsAcrossWaves(t *testing.T) {
	const n = 8

	arrived := make(chan struct{}, n)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var dials int64
	tr := newChatTransport()
	baseDial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt64(&dials, 1)
		return baseDial(ctx, network, addr)
	}
	client := &http.Client{Transport: tr}

	do := func() {
		resp, err := client.Get(srv.URL)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Wave 1: barrier-synchronised so all N connections are open at once.
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); do() }()
	}
	for i := 0; i < n; i++ {
		<-arrived // every handler is now blocked → N live connections
	}
	close(release)
	wg.Wait()

	afterWave1 := atomic.LoadInt64(&dials)
	if afterWave1 != n {
		t.Fatalf("wave 1 dialed %d connections, want %d (HTTP/1.1, all held open by the barrier)", afterWave1, n)
	}

	// Wave 2: the idle pool (32 ≥ N) should absorb every request.
	close2 := make(chan struct{})
	var wg2 sync.WaitGroup
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			<-close2
			do()
		}()
	}
	close(close2)
	wg2.Wait()

	if newDials := atomic.LoadInt64(&dials) - afterWave1; newDials != 0 {
		t.Fatalf("wave 2 dialed %d new connections; the tuned pool (idle/host=%d) should reuse all %d idle connections", newDials, chatTransportMaxIdleConnsPerHost, afterWave1)
	}
}
