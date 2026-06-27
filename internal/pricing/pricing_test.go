package pricing

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  minimax.minimax-m2:
    input: 0.30
    output: 1.20
  zai.glm-4.7-flash:
    input: 0.07
    output: 0.40
default:
  input: 1.00
  output: 3.00
`), 0o644))

	table, err := Load(path)
	require.NoError(t, err)

	got, known := table.Lookup("minimax.minimax-m2")
	assert.True(t, known)
	assert.Equal(t, 0.30, got.InputUSDPerMillion)
	assert.Equal(t, 1.20, got.OutputUSDPerMillion)

	flash, _ := table.Lookup("zai.glm-4.7-flash")
	assert.Equal(t, 0.07, flash.InputUSDPerMillion)
}

func TestLoad_MissingFileReturnsEmptyTable(t *testing.T) {
	table, err := Load("/nonexistent/pricing.yaml")
	require.NoError(t, err)
	require.NotNil(t, table)

	_, known := table.Lookup("any-model")
	assert.False(t, known)
	assert.Equal(t, 0.0, table.CostUSD("any-model", 1000, 500))
}

func TestLoad_MalformedFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`models: [[[`), 0o644))

	_, err := Load(path)
	require.Error(t, err)
}

func TestLookup_UnknownModelFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  known-model:
    input: 0.10
    output: 0.40
default:
  input: 0.50
  output: 2.00
`), 0o644))

	table, err := Load(path)
	require.NoError(t, err)

	entry, known := table.Lookup("mystery-model")
	assert.False(t, known)
	assert.Equal(t, 0.50, entry.InputUSDPerMillion)
	assert.Equal(t, 2.00, entry.OutputUSDPerMillion)
}

func TestLookup_WarnHookFiresOncePerModel(t *testing.T) {
	table := Empty()

	var calls int32
	var lastModel string
	table.SetWarnHook(func(m string) {
		atomic.AddInt32(&calls, 1)
		lastModel = m
	})

	_, _ = table.Lookup("unknown-1")
	_, _ = table.Lookup("unknown-1") // repeat
	_, _ = table.Lookup("unknown-2")
	_, _ = table.Lookup("unknown-1") // repeat again

	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
	assert.Equal(t, "unknown-2", lastModel)
}

func TestCostUSD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  flash:
    input: 0.07
    output: 0.40
  minimax:
    input: 0.30
    output: 1.20
`), 0o644))

	table, err := Load(path)
	require.NoError(t, err)

	// 1200 prompt × $0.07/M + 450 completion × $0.40/M = 0.0000840 + 0.0001800 = $0.000264
	cost := table.CostUSD("flash", 1200, 450)
	assert.InDelta(t, 0.000264, cost, 1e-9)

	// Unknown with no default → 0
	assert.Equal(t, 0.0, table.CostUSD("nope", 1000, 1000))
}

func TestReasoningMultiplierAffectsCompletionOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  plain:
    input: 0.10
    output: 1.00
  thinker:
    input: 0.10
    output: 1.00
    reasoning_multiplier: 2.0
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	// Same tokens on both; thinker costs 2× more on completion only.
	plainCost := table.CostUSD("plain", 1_000_000, 500_000)
	thinkerCost := table.CostUSD("thinker", 1_000_000, 500_000)

	// plain: 1M × 0.10 + 0.5M × 1.00 = 0.10 + 0.50 = 0.60
	// thinker: 1M × 0.10 + 0.5M × 2.00 = 0.10 + 1.00 = 1.10
	require.InDelta(t, 0.60, plainCost, 1e-9)
	require.InDelta(t, 1.10, thinkerCost, 1e-9)
}

func TestReasoningMultiplierZeroTreatedAsOne(t *testing.T) {
	// Unset/zero multiplier must not zero-out cost (that would be a
	// silent regression for every existing non-thinking entry).
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  m:
    input: 0.10
    output: 1.00
    reasoning_multiplier: 0
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	cost := table.CostUSD("m", 0, 1_000_000)
	require.InDelta(t, 1.0, cost, 1e-9)
}

func TestNilReceiverSafe(t *testing.T) {
	var t2 *Table
	_, known := t2.Lookup("x")
	assert.False(t, known)
	assert.Equal(t, 0.0, t2.CostUSD("x", 100, 100))
	assert.Equal(t, 0.0, t2.CostUSDWithCache("x", 100, 100, 100, 100))
	assert.Equal(t, 0.0, t2.CacheSavingsUSD("x", 100))
}

func TestEntry_EffectiveCacheRates_FallBackToInputMultiplier(t *testing.T) {
	// When the YAML doesn't specify cache rates, the calculator must
	// fall back to the Anthropic/Bedrock 5-min cache standard
	// (write = 1.25× input, read = 0.10× input). Operators with
	// pricing entries that predate the LLM caching work get
	// reasonable cost figures without an immediate YAML edit.
	e := Entry{InputUSDPerMillion: 10.0, OutputUSDPerMillion: 50.0}

	assert.InDelta(t, 12.5, e.EffectiveCacheCreationPerMillion(), 1e-9)
	assert.InDelta(t, 1.0, e.EffectiveCacheReadPerMillion(), 1e-9)
}

func TestEntry_EffectiveCacheRates_ExplicitOverridesDefault(t *testing.T) {
	// 1-hour caching on Anthropic is billed at 2× input, not 1.25×.
	// Operators set the explicit field; the calculator must honor it.
	e := Entry{
		InputUSDPerMillion:      10.0,
		OutputUSDPerMillion:     50.0,
		CacheCreationPerMillion: 20.0, // 2× input for 1hr cache
		CacheReadPerMillion:     2.5,  // higher than default 0.10×
	}

	assert.InDelta(t, 20.0, e.EffectiveCacheCreationPerMillion(), 1e-9)
	assert.InDelta(t, 2.5, e.EffectiveCacheReadPerMillion(), 1e-9)
}

func TestEntry_EffectiveCacheRates_NegativeTreatedAsUnset(t *testing.T) {
	// Defensive: a negative value in YAML must not produce a negative
	// effective rate (would zero or invert cost downstream).
	e := Entry{
		InputUSDPerMillion:      8.0,
		CacheCreationPerMillion: -1.0,
		CacheReadPerMillion:     -2.0,
	}

	assert.InDelta(t, 10.0, e.EffectiveCacheCreationPerMillion(), 1e-9)
	assert.InDelta(t, 0.8, e.EffectiveCacheReadPerMillion(), 1e-9)
}

func TestCostUSDWithCache_AnthropicDefaults(t *testing.T) {
	// Anthropic-shaped pricing entry: input $10/M, output $50/M; no
	// explicit cache rates, so defaults apply (write 1.25×, read 0.10×).
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  claude-opus-4-7:
    input: 10.00
    output: 50.00
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	// 1M fresh prompt + 1M completion + 1M cache-creation + 1M cache-read.
	// 10 + 50 + 12.5 + 1.0 = 73.50
	cost := table.CostUSDWithCache("claude-opus-4-7", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	assert.InDelta(t, 73.50, cost, 1e-9)

	// CostUSD path stays backward-compatible: same input/output without cache.
	plain := table.CostUSD("claude-opus-4-7", 1_000_000, 1_000_000)
	assert.InDelta(t, 60.0, plain, 1e-9)
}

func TestCostUSDWithCache_ZeroCacheTokensMatchesCostUSD(t *testing.T) {
	// Backward compat: when cache_creation + cache_read are zero, the
	// new method must return exactly what CostUSD returns. No silent
	// regression for existing entries.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  m:
    input: 0.30
    output: 1.20
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	plain := table.CostUSD("m", 1200, 450)
	withCache := table.CostUSDWithCache("m", 1200, 450, 0, 0)
	assert.Equal(t, plain, withCache)
}

func TestCostUSDWithCache_ExplicitCacheRatesUsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  claude-1hr-cache:
    input: 3.00
    output: 15.00
    cache_creation: 6.00
    cache_read: 0.30
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	// 100k cache_creation × $6/M = 0.6
	// 200k cache_read × $0.30/M = 0.06
	// 0 fresh input + 0 completion
	cost := table.CostUSDWithCache("claude-1hr-cache", 0, 0, 100_000, 200_000)
	assert.InDelta(t, 0.66, cost, 1e-9)
}

func TestCostUSDWithCache_ReasoningMultiplierAppliesToCompletionOnly(t *testing.T) {
	// Reasoning multiplier must NOT apply to cache rates — only to
	// completion tokens. Cache write/read are input-side; doubling
	// them would mis-price thinking models with prompt caching on.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  thinker:
    input: 10.00
    output: 50.00
    reasoning_multiplier: 2.0
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	// 1M cache_creation (defaults to input × 1.25 = 12.5) →
	// total = 12.5, NOT 25 (which would happen if reasoning_mult
	// inadvertently multiplied cache rates).
	cost := table.CostUSDWithCache("thinker", 0, 0, 1_000_000, 0)
	assert.InDelta(t, 12.5, cost, 1e-9)
}

func TestCacheSavingsUSD_HappyPath(t *testing.T) {
	// 1M tokens served from cache @ $10/M input rate but $1/M cache_read
	// rate → saved $9. Surfaces the "$ saved by cache today" tile.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  m:
    input: 10.00
    output: 50.00
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	saved := table.CacheSavingsUSD("m", 1_000_000)
	assert.InDelta(t, 9.0, saved, 1e-9)
}

func TestCacheSavingsUSD_ZeroWhenNoCacheReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  m:
    input: 10.00
    output: 50.00
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 0.0, table.CacheSavingsUSD("m", 0))
	assert.Equal(t, 0.0, table.CacheSavingsUSD("m", -100))
}

func TestCacheSavingsUSD_ZeroForUnknownModelWithNoDefault(t *testing.T) {
	table := Empty()
	assert.Equal(t, 0.0, table.CacheSavingsUSD("nope", 1_000_000))
}

func TestCacheSavingsUSD_ZeroWhenCacheReadRateExceedsInputRate(t *testing.T) {
	// Defensive: a misconfigured YAML where cache_read > input would
	// compute a negative "savings". Clamp to zero.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  weird:
    input: 1.00
    output: 5.00
    cache_read: 2.00
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 0.0, table.CacheSavingsUSD("weird", 1_000_000))
}

func TestLoad_ParsesCacheRateFields(t *testing.T) {
	// The YAML schema gains `cache_creation` + `cache_read` per entry.
	// Ensures we don't silently drop them on parse.
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  m:
    input: 1.00
    output: 5.00
    cache_creation: 1.25
    cache_read: 0.10
`), 0o644))
	table, err := Load(path)
	require.NoError(t, err)

	entry, known := table.Lookup("m")
	require.True(t, known)
	assert.InDelta(t, 1.25, entry.CacheCreationPerMillion, 1e-9)
	assert.InDelta(t, 0.10, entry.CacheReadPerMillion, 1e-9)
}
