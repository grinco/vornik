package ui

// Task 3.4 — tests for the Fix-It Doctor read/reprobe seams
// (fixit_bridge.go): LatestIntegrationProbe / ReprobeIntegrationLive /
// LatestReloadError. These back the internal/service adapters that wire
// task 3.1's IntegrationProbeProvider/ReloadStatusProvider and task
// 3.3's IntegrationReprober against this package's real state; the
// adapters themselves (and the fixitdoctor-shaped translation) are
// tested in internal/service, since this package must not import
// internal/fixitdoctor (see fixit_bridge.go's file-level doc comment).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/onboarding"
)

// --- LatestIntegrationProbe -------------------------------------------

func TestLatestIntegrationProbe_ReturnsCachedResultAndDocURL(t *testing.T) {
	s := NewServer()
	s.storeIntegrationProbe("telegram", "", integrations.ProbeResult{
		Outcome: integrations.OutcomeFail, Summary: "invalid bot token",
	})

	result, docURL, ok := s.LatestIntegrationProbe("telegram", "")
	require.True(t, ok)
	assert.Equal(t, integrations.OutcomeFail, result.Outcome)
	assert.Equal(t, "invalid bot token", result.Summary)
	assert.NotEmpty(t, docURL, "telegram's registered DocURL should come through")
}

func TestLatestIntegrationProbe_NoCachedProbe_NotOK(t *testing.T) {
	s := NewServer()
	_, docURL, ok := s.LatestIntegrationProbe("telegram", "")
	assert.False(t, ok, "no probe has ever been cached — nothing to ground on yet")
	assert.NotEmpty(t, docURL, "the doc URL is still known even with no cached probe")
}

func TestLatestIntegrationProbe_UnknownKind_NotOK(t *testing.T) {
	s := NewServer()
	result, docURL, ok := s.LatestIntegrationProbe("not-a-real-kind", "")
	assert.False(t, ok)
	assert.Empty(t, docURL)
	assert.Equal(t, integrations.ProbeResult{}, result)
}

func TestLatestIntegrationProbe_ProjectScoped_KeyedSeparatelyFromDaemonScope(t *testing.T) {
	s := NewServer()
	s.storeIntegrationProbe("slack", "proj-a", integrations.ProbeResult{Outcome: integrations.OutcomeOK})

	_, _, okOther := s.LatestIntegrationProbe("slack", "proj-b")
	assert.False(t, okOther, "a different project's cache entry must not leak across projects")

	result, _, ok := s.LatestIntegrationProbe("slack", "proj-a")
	require.True(t, ok)
	assert.Equal(t, integrations.OutcomeOK, result.Outcome)
}

// --- ReprobeIntegrationLive ---------------------------------------------

func TestReprobeIntegrationLive_RunsLiveProbeAndUpdatesCache(t *testing.T) {
	prober := &fakeIntegrationProber{result: integrations.ProbeResult{Outcome: integrations.OutcomeOK, Summary: "bot: @vornik_bot"}}
	withFakeIntegrationsRegistry(t, fakeRegistryWithProber("telegram", prober))
	cfg := &config.Config{}
	cfg.Telegram.BotToken = "live-token"
	s := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))

	result, err := s.ReprobeIntegrationLive(context.Background(), "telegram", "")
	require.NoError(t, err)
	assert.Equal(t, integrations.OutcomeOK, result.Outcome)
	assert.Equal(t, 1, prober.calls, "must actually call the live Prober, not just read the cache")

	// The cache is updated exactly like a manual re-check — the catalog
	// tile must reflect the fresh result too.
	entry, ok := s.cachedIntegrationProbe("telegram", "")
	require.True(t, ok)
	assert.Equal(t, integrations.OutcomeOK, entry.Result.Outcome)
}

func TestReprobeIntegrationLive_UnknownKind_Errors(t *testing.T) {
	s := NewServer()
	_, err := s.ReprobeIntegrationLive(context.Background(), "not-a-real-kind", "")
	assert.Error(t, err)
}

func TestReprobeIntegrationLive_NotProbeableKind_Errors(t *testing.T) {
	// A kind with no Prober wired at all (every real catalog entry has
	// one today, but the seam must still fail closed rather than nil-
	// pointer-dereference if that ever changes).
	withFakeIntegrationsRegistry(t, func(integrations.DialGuard) []integrations.IntegrationKind {
		return []integrations.IntegrationKind{{ID: "no-prober", DisplayName: "No Prober", Scope: integrations.ScopeDaemon}}
	})
	s := NewServer()
	_, err := s.ReprobeIntegrationLive(context.Background(), "no-prober", "")
	assert.Error(t, err)
}

// --- LatestReloadError ---------------------------------------------------

func TestLatestReloadError_ReportsJoinedErrorsWhenPresent(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{
		status: config.ReloadStatus{HasErrors: true, Errors: []string{"validate: bad llm.timeout"}},
	}))

	message, ok := s.LatestReloadError()
	require.True(t, ok)
	assert.Contains(t, message, "llm.timeout")
}

func TestLatestReloadError_NoErrors_NotOK(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{status: config.ReloadStatus{HasErrors: false}}))
	_, ok := s.LatestReloadError()
	assert.False(t, ok)
}

func TestLatestReloadError_ReloaderWithoutStatus_NotOK(t *testing.T) {
	s := NewServer(WithConfigReloader(fakeBoundedReloader{outcome: config.ReloadApplied}))
	_, ok := s.LatestReloadError()
	assert.False(t, ok, "a reloader without Status() must degrade to not-ok, never panic")
}

func TestLatestReloadError_NoReloaderWired_NotOK(t *testing.T) {
	s := NewServer()
	_, ok := s.LatestReloadError()
	assert.False(t, ok)
}
