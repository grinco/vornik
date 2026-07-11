package service

// Task 3.4 — tests for the fixit_ui_bridge_adapter.go adapters that wire
// task 3.1's IntegrationProbeProvider/ReloadStatusProvider and task
// 3.3's IntegrationReprober against *ui.Server's real state. Covers (a)
// the fail-closed degrade when the Container or its uiServer is absent
// (the ordering constraint documented on Container.uiServer and this
// file's header comment), and (b) a genuine seeded-state round trip
// using ui.Server's REAL, unmodified integration catalog: the "telegram"
// kind short-circuits to OutcomeFail with no network dial when its
// candidate carries no bot_token (internal/integrations/prober_telegram.go)
// — exactly what ui.Server.currentIntegrationValues returns on a server
// with no config wired — so reprobing it deterministically exercises the
// real Prober.Probe -> cache-store path without touching the network.

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/ui"
)

// --- fail-closed on a genuinely-absent provider (nil Container / nil uiServer) ---

func TestFixitIntegrationProbeProvider_NilContainer_FailsClosed(t *testing.T) {
	p := fixitIntegrationProbeProvider{}
	result, docURL, ok, err := p.LatestProbe(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindRedIntegration, ID: "telegram"})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, docURL)
	assert.Equal(t, integrations.ProbeResult{}, result)
}

func TestFixitIntegrationProbeProvider_NoUIServer_FailsClosed(t *testing.T) {
	p := fixitIntegrationProbeProvider{c: &Container{}}
	_, _, ok, err := p.LatestProbe(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindRedIntegration, ID: "telegram"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestFixitReloadStatusProvider_NilContainer_FailsClosed(t *testing.T) {
	p := fixitReloadStatusProvider{}
	rv, ok, err := p.LatestReloadError(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindFailedReload})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, fixitdoctor.ReloadValidationError{}, rv)
}

func TestFixitReloadStatusProvider_NoUIServer_FailsClosed(t *testing.T) {
	p := fixitReloadStatusProvider{c: &Container{}}
	_, ok, err := p.LatestReloadError(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindFailedReload})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestFixitIntegrationReprober_NilContainer_FailsClosed(t *testing.T) {
	p := fixitIntegrationReprober{}
	_, healthy, err := p.Reprobe(context.Background(), "proj-1", "telegram")
	assert.Error(t, err, "must fail closed (an error), never silently claim healthy")
	assert.False(t, healthy)
}

// TestContainerUIServer_ZeroValueAtomicPointer_LoadIsNilSafe pins down the
// companion-review fix (2026-07-10, IMPORTANT #1): Container.uiServer is now
// atomic.Pointer[ui.Server], and a never-Store()'d Container must Load() a
// plain nil — no zero-value panic, no torn read — so every adapter's
// existing nil-degrade branch above keeps working unchanged.
func TestContainerUIServer_ZeroValueAtomicPointer_LoadIsNilSafe(t *testing.T) {
	var c Container
	assert.Nil(t, c.uiServer.Load())
}

// TestFixitUIBridgeAdapters_ConcurrentStoreAndRead_NoRace exercises the race
// IMPORTANT #1 called out: a request goroutine reading c.uiServer while
// initHTTPServer's Store races it. Run with -race — pre-fix (a plain
// *ui.Server field written and read without synchronization) this would
// trip the race detector; atomic.Pointer.Store/.Load makes it well-defined
// regardless of which side of the store the read lands on.
func TestFixitUIBridgeAdapters_ConcurrentStoreAndRead_NoRace(t *testing.T) {
	c := &Container{}
	srv := ui.NewServer()
	probeProvider := fixitIntegrationProbeProvider{c: c}
	reloadProvider := fixitReloadStatusProvider{c: c}
	reprober := fixitIntegrationReprober{c: c}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		c.uiServer.Store(srv)
	}()
	go func() {
		defer wg.Done()
		_, _, _, err := probeProvider.LatestProbe(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindRedIntegration, ID: "telegram"})
		assert.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		_, _, err := reloadProvider.LatestReloadError(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindFailedReload})
		assert.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		_, _, _ = reprober.Reprobe(context.Background(), "", "telegram")
	}()
	wg.Wait()
}

func TestFixitIntegrationReprober_NoUIServer_FailsClosed(t *testing.T) {
	p := fixitIntegrationReprober{c: &Container{}}
	_, healthy, err := p.Reprobe(context.Background(), "proj-1", "telegram")
	assert.Error(t, err)
	assert.False(t, healthy)
}

// --- real, seeded round trip (no network) ---------------------------------

func TestFixitIntegrationReprober_And_ProbeProvider_RoundTripViaRealUIServer(t *testing.T) {
	srv := ui.NewServer()
	c := &Container{}
	c.uiServer.Store(srv)

	// telegram's candidate is empty on a server with no config wired, so
	// its real Prober short-circuits deterministically to OutcomeFail —
	// no network dial.
	reprober := fixitIntegrationReprober{c: c}
	summary, healthy, err := reprober.Reprobe(context.Background(), "", "telegram")
	require.NoError(t, err)
	assert.False(t, healthy, "telegram with no bot_token candidate must report unhealthy")
	assert.NotEmpty(t, summary)

	// The reprobe must have updated ui.Server's cache — the SAME cache
	// red_integration grounding reads through LatestIntegrationProbe.
	probeProvider := fixitIntegrationProbeProvider{c: c}
	result, docURL, ok, err := probeProvider.LatestProbe(context.Background(), fixitdoctor.FailureRef{
		Kind: fixitdoctor.FailureKindRedIntegration, ID: "telegram",
	})
	require.NoError(t, err)
	require.True(t, ok, "expected the reprobe above to have seeded the cache")
	assert.Equal(t, integrations.OutcomeFail, result.Outcome)
	assert.NotEmpty(t, docURL)
}

func TestFixitIntegrationReprober_UnknownKind_PropagatesUIServerError(t *testing.T) {
	srv := ui.NewServer()
	c := &Container{}
	c.uiServer.Store(srv)
	p := fixitIntegrationReprober{c: c}
	_, healthy, err := p.Reprobe(context.Background(), "proj-1", "not-a-real-kind")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-real-kind", "the failing kind id should be identifiable from the error")
	assert.False(t, healthy)
}

func TestFixitIntegrationProbeProvider_UnknownKind_NotOK(t *testing.T) {
	srv := ui.NewServer()
	c := &Container{}
	c.uiServer.Store(srv)
	p := fixitIntegrationProbeProvider{c: c}
	_, _, ok, err := p.LatestProbe(context.Background(), fixitdoctor.FailureRef{
		Kind: fixitdoctor.FailureKindRedIntegration, ID: "not-a-real-kind",
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

// fakeUIConfigReloader is a ui.ConfigReloader (Reload() error) that also
// exposes Status() config.ReloadStatus — the duck-typed optional
// capability ui.Server's reload-error/status seams consult via type
// assertion (mirrors ui package's own statusReloader test fake; defined
// here too since that fake is unexported to the ui package's test
// files).
type fakeUIConfigReloader struct {
	status config.ReloadStatus
}

func (f fakeUIConfigReloader) Reload() error               { return nil }
func (f fakeUIConfigReloader) Status() config.ReloadStatus { return f.status }

func TestFixitReloadStatusProvider_ReturnsMessageFromUIServerState(t *testing.T) {
	srv := ui.NewServer(ui.WithConfigReloader(fakeUIConfigReloader{
		status: config.ReloadStatus{HasErrors: true, Errors: []string{"validate: bad llm.timeout"}},
	}))
	c := &Container{}
	c.uiServer.Store(srv)
	p := fixitReloadStatusProvider{c: c}

	rv, ok, err := p.LatestReloadError(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindFailedReload})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, rv.Message, "llm.timeout")
	// task 3.4's documented degrade: no structured key-path source exists
	// at this layer, so it stays unpopulated rather than fabricated.
	assert.Empty(t, rv.OffendingKeyPath)
	assert.Empty(t, rv.OffendingValue)
}

func TestFixitReloadStatusProvider_NoErrors_NotOK(t *testing.T) {
	srv := ui.NewServer(ui.WithConfigReloader(fakeUIConfigReloader{status: config.ReloadStatus{HasErrors: false}}))
	c := &Container{}
	c.uiServer.Store(srv)
	p := fixitReloadStatusProvider{c: c}

	_, ok, err := p.LatestReloadError(context.Background(), fixitdoctor.FailureRef{Kind: fixitdoctor.FailureKindFailedReload})
	require.NoError(t, err)
	assert.False(t, ok)
}
