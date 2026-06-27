package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// stubCacheMetrics captures ObserveCacheUsage calls so the tests can
// assert observeChatCacheUsage forwards the row's cache fields.
type stubCacheMetrics struct {
	calls []cacheCall
}

type cacheCall struct {
	model        string
	role         string
	source       string
	creation     int64
	read         int64
	dollarsSaved float64
}

func (s *stubCacheMetrics) ObserveCacheUsage(model, role, source string, creation, read int64, dollars float64) {
	s.calls = append(s.calls, cacheCall{model, role, source, creation, read, dollars})
}

// TestWithChatCacheMetrics_WiresAndAllowsNil — the option sets and
// clears the sink.
func TestWithChatCacheMetrics_WiresAndAllowsNil(t *testing.T) {
	m := &stubCacheMetrics{}
	s := &Server{}
	WithChatCacheMetrics(m)(s)
	assert.Same(t, m, s.chatCacheMetrics)
	WithChatCacheMetrics(nil)(s)
	assert.Nil(t, s.chatCacheMetrics)
}

// TestSetChatCacheMetrics — the post-construction setter the container
// uses (chat metrics built after the server).
func TestSetChatCacheMetrics(t *testing.T) {
	m := &stubCacheMetrics{}
	s := &Server{}
	s.SetChatCacheMetrics(m)
	assert.Same(t, m, s.chatCacheMetrics)
	var nilServer *Server
	assert.NotPanics(t, func() { nilServer.SetChatCacheMetrics(m) })
}

// TestObserveChatCacheUsage_ForwardsRowFields — a row with cache tokens
// forwards model/role/source + token counts. Pricing is unset so
// dollarsSaved is 0 (the happy path covering the metrics forward; the
// pricing math is unit-tested in internal/pricing).
func TestObserveChatCacheUsage_ForwardsRowFields(t *testing.T) {
	m := &stubCacheMetrics{}
	s := &Server{chatCacheMetrics: m}
	row := &persistence.TaskLLMUsage{
		Model:               "claude",
		Role:                "external_api",
		Source:              persistence.TaskLLMUsageSourceExternalAPI,
		CacheCreationTokens: 100,
		CacheReadTokens:     300,
	}
	s.observeChatCacheUsage(row)
	if assert.Len(t, m.calls, 1) {
		c := m.calls[0]
		assert.Equal(t, "claude", c.model)
		assert.Equal(t, "external_api", c.role)
		assert.Equal(t, string(persistence.TaskLLMUsageSourceExternalAPI), c.source)
		assert.Equal(t, int64(100), c.creation)
		assert.Equal(t, int64(300), c.read)
	}
}

// TestObserveChatCacheUsage_ComputesDollarsSavedFromPricing — with a
// pricing table configured, the read tokens served from cache produce a
// positive dollars-saved value forwarded to the sink.
func TestObserveChatCacheUsage_ComputesDollarsSavedFromPricing(t *testing.T) {
	dir := t.TempDir()
	pricingPath := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(pricingPath, []byte(
		"models:\n  claude-test: { input: 3.0, output: 15.0 }\n"), 0o600))

	m := &stubCacheMetrics{}
	s := &Server{chatCacheMetrics: m, pricingPath: pricingPath}
	row := &persistence.TaskLLMUsage{
		Model:           "claude-test",
		Role:            "external_api",
		Source:          persistence.TaskLLMUsageSourceExternalAPI,
		CacheReadTokens: 1_000_000,
	}
	s.observeChatCacheUsage(row)
	require.Len(t, m.calls, 1)
	// 1M read tokens at input $3 vs cache-read $0.30 (default 0.10×) → $2.70 saved.
	assert.InDelta(t, 2.70, m.calls[0].dollarsSaved, 1e-6)
}

// TestObserveChatCacheUsage_SkipsWhenNoCacheTokensOrNoSink — a row with
// no cache tokens, a nil row, or no wired sink records nothing.
func TestObserveChatCacheUsage_SkipsWhenNoCacheTokensOrNoSink(t *testing.T) {
	m := &stubCacheMetrics{}
	s := &Server{chatCacheMetrics: m}

	s.observeChatCacheUsage(&persistence.TaskLLMUsage{Model: "m"}) // no cache tokens
	s.observeChatCacheUsage(nil)
	assert.Empty(t, m.calls)

	// No sink wired: must not panic.
	noSink := &Server{}
	assert.NotPanics(t, func() {
		noSink.observeChatCacheUsage(&persistence.TaskLLMUsage{CacheReadTokens: 5})
	})
}
