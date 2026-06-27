// Package pricing maps model IDs to per-1M-token input/output costs so
// the executor can emit dollar metrics alongside token metrics. The
// table is loaded from a standalone YAML file (usually configs/pricing.yaml)
// so it can be updated without touching the daemon binary or the main
// vornik config.
package pricing

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// Entry is the per-1M-token cost for one model.
type Entry struct {
	// InputUSDPerMillion is the cost of 1,000,000 input (prompt) tokens.
	InputUSDPerMillion float64 `yaml:"input"`
	// OutputUSDPerMillion is the cost of 1,000,000 output (completion) tokens.
	OutputUSDPerMillion float64 `yaml:"output"`
	// ReasoningMultiplier corrects the completion-token cost for thinking
	// models that bill hidden chain-of-thought tokens under the
	// `completion_tokens` field. Bedrock/upstream gateways return the
	// combined count — there's no separate `reasoning_tokens` field to
	// subtract. Operators set this to the vendor's published "thinking
	// token" markup (e.g. 2.0 for a model that bills hidden-reasoning
	// tokens at 2× the visible-output rate), or leave unset (1.0) for
	// non-thinking models. Default 1.0 means "output is billed at the
	// configured output rate, period."
	ReasoningMultiplier float64 `yaml:"reasoning_multiplier"`
	// CacheCreationPerMillion is the per-1M-token cost for tokens written
	// to the provider's prompt cache (Anthropic / Bedrock prompt caching,
	// also called "cache writes"). When zero (default), the calculator
	// falls back to InputUSDPerMillion × DefaultCacheCreationMultiplier
	// (1.25, the Anthropic / Bedrock 5-min cache standard). Set explicitly
	// when a provider deviates — e.g., Anthropic's 1-hour cache at 2× input.
	CacheCreationPerMillion float64 `yaml:"cache_creation"`
	// CacheReadPerMillion is the per-1M-token cost for tokens served from
	// the provider's prompt cache. When zero (default), falls back to
	// InputUSDPerMillion × DefaultCacheReadMultiplier (0.10, the Anthropic
	// / Bedrock standard). The spend dashboard surfaces the dollar value
	// of (InputRate − CacheReadRate) × cache_read_tokens as "$ saved".
	CacheReadPerMillion float64 `yaml:"cache_read"`
}

// DefaultCacheCreationMultiplier is the fallback ratio applied to
// InputUSDPerMillion when Entry.CacheCreationPerMillion is unset or
// non-positive. Matches Anthropic / Bedrock 5-min cache write pricing.
const DefaultCacheCreationMultiplier = 1.25

// DefaultCacheReadMultiplier is the fallback ratio applied to
// InputUSDPerMillion when Entry.CacheReadPerMillion is unset or
// non-positive. Matches Anthropic / Bedrock cache read pricing.
const DefaultCacheReadMultiplier = 0.10

// EffectiveCacheCreationPerMillion returns the per-1M-token cache write
// rate, falling back to InputUSDPerMillion × DefaultCacheCreationMultiplier
// when the explicit field is unset or non-positive.
func (e Entry) EffectiveCacheCreationPerMillion() float64 {
	if e.CacheCreationPerMillion > 0 {
		return e.CacheCreationPerMillion
	}
	return e.InputUSDPerMillion * DefaultCacheCreationMultiplier
}

// EffectiveCacheReadPerMillion returns the per-1M-token cache read rate,
// falling back to InputUSDPerMillion × DefaultCacheReadMultiplier when
// the explicit field is unset or non-positive.
func (e Entry) EffectiveCacheReadPerMillion() float64 {
	if e.CacheReadPerMillion > 0 {
		return e.CacheReadPerMillion
	}
	return e.InputUSDPerMillion * DefaultCacheReadMultiplier
}

// effectiveOutputUSDPerMillion returns the output rate adjusted by the
// reasoning multiplier. 0 or unset multiplier is treated as 1.0 so
// non-thinking models — the common case — behave as before.
func (e Entry) effectiveOutputUSDPerMillion() float64 {
	mult := e.ReasoningMultiplier
	if mult <= 0 {
		mult = 1.0
	}
	return e.OutputUSDPerMillion * mult
}

// File is the on-disk pricing config shape.
type File struct {
	Models  map[string]Entry `yaml:"models"`
	Default Entry            `yaml:"default"`
}

// Table holds the loaded pricing data plus an unknown-model warning cache
// so a missing entry only logs once per model.
type Table struct {
	models   map[string]Entry
	fallback Entry

	mu       sync.Mutex
	warned   map[string]struct{}
	warnHook func(model string)
}

// Load reads and parses a pricing YAML file. An absent file returns an
// empty Table (all lookups return the zero-cost fallback) — this is the
// explicit opt-in: no file, no cost metric.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return nil, fmt.Errorf("read pricing file: %w", err)
	}

	var parsed File
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse pricing file: %w", err)
	}
	if parsed.Models == nil {
		parsed.Models = map[string]Entry{}
	}
	return &Table{
		models:   parsed.Models,
		fallback: parsed.Default,
		warned:   map[string]struct{}{},
	}, nil
}

// Empty returns a pricing table with no entries. Useful for tests and for
// deployments that haven't configured pricing yet — Lookup always returns
// (Entry{}, false).
func Empty() *Table {
	return &Table{
		models: map[string]Entry{},
		warned: map[string]struct{}{},
	}
}

// SetWarnHook registers a callback invoked the first time an unknown model
// is looked up. The callback is not invoked for subsequent lookups of the
// same model (a one-time warning avoids log spam).
func (t *Table) SetWarnHook(h func(model string)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.warnHook = h
	t.mu.Unlock()
}

// Lookup returns the per-1M-token costs for a model. If the model isn't
// in the table, the configured `default` entry is returned and `known`
// is false — so the caller can decide whether to emit metrics with a
// fallback rate or skip them entirely. A nil receiver is safe and
// returns (Entry{}, false).
func (t *Table) Lookup(model string) (entry Entry, known bool) {
	if t == nil {
		return Entry{}, false
	}
	if e, ok := t.models[model]; ok {
		return e, true
	}
	t.mu.Lock()
	if _, already := t.warned[model]; !already {
		t.warned[model] = struct{}{}
		hook := t.warnHook
		t.mu.Unlock()
		if hook != nil && model != "" {
			hook(model)
		}
	} else {
		t.mu.Unlock()
	}
	return t.fallback, false
}

// IDs returns every model identifier the table was loaded with, sorted
// for stable output. nil receiver returns nil. The result drives the
// model-discovery surface for chat providers without their own /v1/models
// endpoint (Anthropic/OpenAI subscriptions, CLI wrappers, Bedrock as a
// fallback) — pricing.yaml doubles as the operator-curated catalog.
func (t *Table) IDs() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.models))
	for id := range t.models {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CostUSD computes the dollar cost of a single LLM call given prompt and
// completion token counts. Returns 0 for unknown models when no default is
// configured. Applies the model's `reasoning_multiplier` to completion
// tokens so thinking models (kimi-k2-thinking etc.) that bill hidden
// chain-of-thought under `completion_tokens` show their true cost.
//
// Backwards-compatible wrapper for CostUSDWithCache(promptTokens,
// completionTokens, 0, 0) — call sites that don't have cache token
// counts handy can keep using this signature.
func (t *Table) CostUSD(model string, promptTokens, completionTokens int) float64 {
	return t.CostUSDWithCache(model, promptTokens, completionTokens, 0, 0)
}

// CostUSDWithCache computes the dollar cost of a single LLM call factoring
// in prompt-cache tokens. The four token classes are disjoint as reported
// by Anthropic + Bedrock: PromptTokens are fresh (non-cached) input,
// CacheCreationTokens are written into the cache for future reuse,
// CacheReadTokens are served from cache (billed at the discounted read
// rate), and CompletionTokens are output (subject to the reasoning
// multiplier for thinking models).
//
// Returns 0 for unknown models without a default. When cache rate fields
// are unset in the YAML, defaults to input × 1.25 (write) and × 0.10
// (read) — the Anthropic / Bedrock 5-min standard.
func (t *Table) CostUSDWithCache(model string, promptTokens, completionTokens, cacheCreationTokens, cacheReadTokens int) float64 {
	if t == nil {
		return 0
	}
	entry, _ := t.Lookup(model)
	if entry.InputUSDPerMillion == 0 && entry.OutputUSDPerMillion == 0 {
		return 0
	}
	return (float64(promptTokens)*entry.InputUSDPerMillion +
		float64(completionTokens)*entry.effectiveOutputUSDPerMillion() +
		float64(cacheCreationTokens)*entry.EffectiveCacheCreationPerMillion() +
		float64(cacheReadTokens)*entry.EffectiveCacheReadPerMillion()) / 1_000_000.0
}

// CacheSavingsUSD returns the dollar value of using cached prompt tokens —
// the difference between what cacheReadTokens would have cost at the full
// input rate vs. the discounted cache read rate. Drives the "$ saved by
// cache" tile on /ui/spend. Returns 0 for unknown models, zero/negative
// cacheReadTokens, or pricing that misconfigures cache_read above input
// (clamped to non-negative — a negative "saving" would confuse the UI).
func (t *Table) CacheSavingsUSD(model string, cacheReadTokens int) float64 {
	if t == nil || cacheReadTokens <= 0 {
		return 0
	}
	entry, _ := t.Lookup(model)
	if entry.InputUSDPerMillion <= 0 {
		return 0
	}
	saved := entry.InputUSDPerMillion - entry.EffectiveCacheReadPerMillion()
	if saved <= 0 {
		return 0
	}
	return float64(cacheReadTokens) * saved / 1_000_000.0
}
