package chat

import (
	"os"
	"time"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/rs/zerolog"
)

// bedrockTimingEnabled gates the VORNIK_BEDROCK_TIMING diagnostic: START/END
// timing logs around each Bedrock SDK call. Off unless the env var is set, so
// there is zero overhead in normal operation.
//
// Added 2026-06-24 after the zai.glm-5 latency investigation. The SDK-call
// duration is what distinguishes an upstream Bedrock latency tail (the
// Converse/ConverseStream call itself blocks for tens of seconds, even though
// most calls return in a few) from a vornik-side stall (the call returns fast
// and the time is spent elsewhere). It is TIMING-ONLY by design — it never
// writes request or response bodies to disk, so it carries no prompt/secret
// exposure.
func bedrockTimingEnabled() bool { return os.Getenv("VORNIK_BEDROCK_TIMING") != "" }

// bedrockToolCount is a nil-safe tool counter for either Converse input type.
func bedrockToolCount(tc *bedrocktypes.ToolConfiguration) int {
	if tc == nil {
		return 0
	}
	return len(tc.Tools)
}

// logBedrockSDKStart logs immediately before the SDK call so a call that never
// returns still leaves a record (the smoking gun for an upstream hang).
func logBedrockSDKStart(logger zerolog.Logger, op, model string, messages, tools int) {
	logger.Warn().
		Str("component", "bedrock-timing").Str("phase", "start").
		Str("op", op).Str("model", model).
		Int("messages", messages).Int("tools", tools).
		Msg("bedrock SDK call START")
}

// logBedrockSDKEnd logs the SDK-call duration + outcome.
func logBedrockSDKEnd(logger zerolog.Logger, op, model string, messages, tools int, start time.Time, err error) {
	ev := logger.Warn().
		Str("component", "bedrock-timing").Str("phase", "end").
		Str("op", op).Str("model", model).
		Int("messages", messages).Int("tools", tools).
		Dur("sdk_duration", time.Since(start))
	if err != nil {
		ev = ev.Str("err", err.Error())
	}
	ev.Msg("bedrock SDK call END")
}
