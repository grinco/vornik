package chat

import (
	"context"
	"testing"
)

func TestCacheStrategyActive(t *testing.T) {
	cases := []struct {
		name string
		s    *CacheStrategy
		want bool
	}{
		{"nil", nil, false},
		{"empty mode", &CacheStrategy{Mode: ""}, false},
		{"off", &CacheStrategy{Mode: CacheModeOff}, false},
		{"OFF uppercase", &CacheStrategy{Mode: "OFF"}, false},
		{"auto", &CacheStrategy{Mode: CacheModeAuto}, true},
		{"prefix", &CacheStrategy{Mode: CacheModePrefix}, true},
		{"PREFIX uppercase", &CacheStrategy{Mode: "PREFIX"}, true},
	}
	for _, c := range cases {
		if got := cacheStrategyActive(c.s); got != c.want {
			t.Errorf("%s: cacheStrategyActive = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShouldCacheSystemFromInput_OffStrategy(t *testing.T) {
	msgs := []Message{{Role: "system", Content: "x"}}
	if shouldCacheSystemFromInput(msgs, nil) {
		t.Error("nil strategy must not cache")
	}
	if shouldCacheSystemFromInput(msgs, &CacheStrategy{Mode: CacheModeOff}) {
		t.Error("off strategy must not cache")
	}
}

func TestShouldCacheSystemFromInput_AutoMarksSystem(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "you are an assistant"},
		{Role: "user", Content: "hi"},
	}
	if !shouldCacheSystemFromInput(msgs, &CacheStrategy{Mode: CacheModeAuto}) {
		t.Error("auto mode with a system message must cache")
	}
}

func TestShouldCacheSystemFromInput_AutoNoSystem(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}}
	if shouldCacheSystemFromInput(msgs, &CacheStrategy{Mode: CacheModeAuto}) {
		t.Error("auto mode with no system message must NOT cache")
	}
}

func TestShouldCacheSystemFromInput_PrefixRespectsFlag(t *testing.T) {
	// Prefix mode without any CachePrefix flag → no cache.
	msgs := []Message{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "hi"},
	}
	if shouldCacheSystemFromInput(msgs, &CacheStrategy{Mode: CacheModePrefix}) {
		t.Error("prefix mode without CachePrefix flag must NOT cache")
	}
	// Same input with CachePrefix=true on the system message → cache.
	msgs[0].CachePrefix = true
	if !shouldCacheSystemFromInput(msgs, &CacheStrategy{Mode: CacheModePrefix}) {
		t.Error("prefix mode with CachePrefix on system message must cache")
	}
}

func TestShouldCacheSystemFromInput_PrefixIgnoresNonSystemFlag(t *testing.T) {
	// Prefix=true on a user message must NOT trigger system-block
	// caching (the user-message cache marker is out of scope for
	// this slice).
	msgs := []Message{
		{Role: "system", Content: "system text"},
		{Role: "user", Content: "x", CachePrefix: true},
	}
	if shouldCacheSystemFromInput(msgs, &CacheStrategy{Mode: CacheModePrefix}) {
		t.Error("CachePrefix on user message must NOT cache system block in prefix mode")
	}
}

func TestCacheStrategyContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if CacheStrategyFromContext(ctx) != nil {
		t.Error("empty context should return nil strategy")
	}
	s := &CacheStrategy{Mode: CacheModeAuto}
	ctx = WithRequestCacheStrategy(ctx, s)
	got := CacheStrategyFromContext(ctx)
	if got == nil || got.Mode != CacheModeAuto {
		t.Errorf("round-trip lost strategy: %+v", got)
	}
}

func TestWithRequestCacheStrategy_DropsOffMode(t *testing.T) {
	ctx := context.Background()
	ctx = WithRequestCacheStrategy(ctx, &CacheStrategy{Mode: CacheModeOff})
	if CacheStrategyFromContext(ctx) != nil {
		t.Error("off mode strategy must not stamp ctx")
	}
}

func TestWithRequestCacheStrategy_DropsNil(t *testing.T) {
	ctx := WithRequestCacheStrategy(context.Background(), nil)
	if CacheStrategyFromContext(ctx) != nil {
		t.Error("nil strategy must not stamp ctx")
	}
}
