package chat

import "context"

// Per-request options threaded through context.Context. The
// chat-proxy reads the corresponding fields off the incoming
// ChatRequest and stamps the context before calling the Provider;
// providers consult the helpers below in their complete-path so
// the values land on the wire to the upstream backend.
//
// Why context-values instead of new Provider methods: the Provider
// interface is consumed by autonomy / dispatcher / the chat proxy,
// each with different threading models. Adding a CompleteOptions
// struct would force every existing caller to update. Context-
// values are additive — old callers ignore them, new providers opt
// in by reading them.

type responseFormatContextKey struct{}
type maxTokensContextKey struct{}

// WithRequestResponseFormat annotates ctx with the type-only
// per-request OpenAI-style response_format directive (e.g.
// "json_object"). For the typed json_schema variant, use
// WithRequestResponseFormatStruct instead — that one carries the
// full struct including the schema body.
//
// The pair (string accessor + struct accessor) lets simple
// providers ignore the schema and just check the Type ("is
// it json_object?") while bedrock's strict-tool path reads the
// full struct via ResponseFormatStructFromContext.
func WithRequestResponseFormat(ctx context.Context, format string) context.Context {
	if format == "" {
		return ctx
	}
	return context.WithValue(ctx, responseFormatContextKey{}, format)
}

// WithRequestResponseFormatStruct stamps the full ResponseFormat
// (including any json_schema body) onto ctx. Stamps the Type
// shorthand too so callers using the simple accessor see the
// directive without having to switch on the struct.
func WithRequestResponseFormatStruct(ctx context.Context, rf *ResponseFormat) context.Context {
	if rf == nil || rf.Type == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, responseFormatContextKey{}, rf.Type)
	ctx = context.WithValue(ctx, responseFormatStructKey{}, rf)
	return ctx
}

type responseFormatStructKey struct{}

// ResponseFormatFromContext returns the per-request response_format
// type shorthand (e.g. "json_object" / "json_schema") set by
// WithRequestResponseFormat or WithRequestResponseFormatStruct, or
// "" when absent.
func ResponseFormatFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(responseFormatContextKey{}).(string); ok {
		return v
	}
	return ""
}

// ResponseFormatStructFromContext returns the full ResponseFormat
// struct when one was stamped via WithRequestResponseFormatStruct,
// or nil. Use this when you need the json_schema body — providers
// that only care about the type should keep using the string
// accessor.
func ResponseFormatStructFromContext(ctx context.Context) *ResponseFormat {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(responseFormatStructKey{}).(*ResponseFormat); ok {
		return v
	}
	return nil
}

// WithRequestMaxTokens annotates ctx with a per-request output-token cap.
// Wins over the Provider's construction-time WithRequestMaxTokens default
// — operators set chat.router.<sub>.max_tokens for the global
// ceiling and override per-request when a specific role needs more
// or less headroom. 0 leaves the field unset (meaning "use the
// Provider default"); negatives are coerced to 0 so a malformed
// payload doesn't accidentally raise the cap.
func WithRequestMaxTokens(ctx context.Context, maxTokens int) context.Context {
	if maxTokens <= 0 {
		return ctx
	}
	return context.WithValue(ctx, maxTokensContextKey{}, maxTokens)
}

// MaxTokensFromContext returns the per-request max_tokens cap set
// by WithRequestMaxTokens, or 0 when absent. Providers should treat 0 as
// "no per-request override; use the construction-time default".
func MaxTokensFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if v, ok := ctx.Value(maxTokensContextKey{}).(int); ok && v > 0 {
		return v
	}
	return 0
}

type callSiteContextKey struct{}

// WithCallSite annotates ctx with a short label identifying the
// subsystem issuing an LLM call (e.g. "memetic.architect",
// "instinct.distiller", "chat.proxy", "judge"). The LoggingProvider
// reads it so every completion line is attributable to the code path
// that made the call — without it, built-in callers (the architect,
// instinct distillation) are indistinguishable from chat traffic in
// the log. Empty label is a no-op; unset reads back as "" and the
// logger falls back to "unknown".
func WithCallSite(ctx context.Context, site string) context.Context {
	if site == "" {
		return ctx
	}
	return context.WithValue(ctx, callSiteContextKey{}, site)
}

// CallSiteFromContext returns the call-site label set by WithCallSite,
// or "" when absent.
func CallSiteFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(callSiteContextKey{}).(string); ok {
		return v
	}
	return ""
}

type cacheStrategyContextKey struct{}

// WithRequestCacheStrategy stamps the prompt-cache directive onto
// ctx. The chat-proxy reads CacheStrategy off the inbound
// ChatRequest and calls this before invoking the Provider;
// providers consult CacheStrategyFromContext in their converter
// path so the cache pragma lands on the wire to Bedrock / Anthropic.
//
// Nil / off strategy is a no-op (the converter falls through to
// the non-caching path).
func WithRequestCacheStrategy(ctx context.Context, s *CacheStrategy) context.Context {
	if s == nil || s.Mode == "" || s.Mode == CacheModeOff {
		return ctx
	}
	return context.WithValue(ctx, cacheStrategyContextKey{}, s)
}

// CacheStrategyFromContext returns the per-request prompt-cache
// directive set by WithRequestCacheStrategy, or nil when absent.
// Providers without native cache support ignore this; the
// Bedrock + Anthropic converters consume it to insert
// CachePointBlock / cache_control respectively.
func CacheStrategyFromContext(ctx context.Context) *CacheStrategy {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(cacheStrategyContextKey{}).(*CacheStrategy); ok {
		return v
	}
	return nil
}
