package dispatcher

import "context"

// originatingChannelCtxKey is the context-key type used to thread
// the Request.OriginatingChannel + OriginatingSessionID through
// the tool-execution call stack. The dispatcher's Process /
// ProcessStreaming entrypoints stash the values on the context
// before invoking the agent loop, and createTask reads them when
// deciding which per-channel ChannelFollowupRegistrar to call.
//
// Context-threaded rather than added to Execute's signature so
// adding more channel-aware tools later doesn't ripple a third,
// fourth, etc. argument through every Execute call site + test
// fixture.
type originatingChannelCtxKey struct{}

type originatingChannelCtxValue struct {
	channel   string
	sessionID string
}

// withOriginatingChannel returns a derived context that carries
// the originating channel + sessionID for downstream tool calls.
// Empty values fall through (no key set) so synthesised internal
// turns don't pollute the context.
func withOriginatingChannel(ctx context.Context, channel, sessionID string) context.Context {
	if channel == "" && sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, originatingChannelCtxKey{}, originatingChannelCtxValue{
		channel:   channel,
		sessionID: sessionID,
	})
}

// originatingChannelFromContext extracts the channel + sessionID
// stashed by withOriginatingChannel. Returns empty strings when
// the context wasn't set (synthesised turn or test fixture);
// callers treat that as "no per-channel follow-up wiring."
func originatingChannelFromContext(ctx context.Context) (channel, sessionID string) {
	if ctx == nil {
		return "", ""
	}
	v, ok := ctx.Value(originatingChannelCtxKey{}).(originatingChannelCtxValue)
	if !ok {
		return "", ""
	}
	return v.channel, v.sessionID
}
