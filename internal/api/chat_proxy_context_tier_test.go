package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/chat"
)

// stubTierMetrics captures ObserveContextTier calls so the tests can
// assert the chat-proxy emits the right (project, tier, headroom)
// tuple. Implements ChatContextTierMetrics with no internal state
// beyond the captured slice; the test asserts directly on it.
type stubTierMetrics struct {
	observed []struct {
		project  string
		tier     chat.ContextTier
		headroom float64
	}
}

func (s *stubTierMetrics) ObserveContextTier(project string, tier chat.ContextTier, headroom float64) {
	s.observed = append(s.observed, struct {
		project  string
		tier     chat.ContextTier
		headroom float64
	}{project, tier, headroom})
}

// TestAttachContextTier_StampsHeaderAndObservesMetric — the happy
// path: budget is configured, response carries prompt tokens, header
// + metric both fire with the matching tier.
func TestAttachContextTier_StampsHeaderAndObservesMetric(t *testing.T) {
	metrics := &stubTierMetrics{}
	s := &Server{
		chatContextBudget:     200_000,
		chatDispatcherMetrics: metrics,
	}
	resp := &chat.ChatResponse{}
	resp.Usage.PromptTokens = 160_000 // 80% used → tier DEGRADING

	w := httptest.NewRecorder()
	s.attachContextTier(w, "alpha", resp)

	assert.Equal(t, "degrading", w.Header().Get(HeaderContextTier))
	assert.Equal(t, "20", w.Header().Get(HeaderContextHeadroomPct))
	if assert.Len(t, metrics.observed, 1) {
		assert.Equal(t, "alpha", metrics.observed[0].project)
		assert.Equal(t, chat.TierDegrading, metrics.observed[0].tier)
		assert.InDelta(t, 20.0, metrics.observed[0].headroom, 0.001)
	}
}

// TestAttachContextTier_DisabledWhenBudgetUnset — zero budget means
// the deployment opted out of the tier surface. Header omitted,
// metric not bumped. Pins the back-compat guarantee: clients that
// upgraded the daemon without setting chat_context_budget see exactly
// the legacy response shape.
func TestAttachContextTier_DisabledWhenBudgetUnset(t *testing.T) {
	metrics := &stubTierMetrics{}
	s := &Server{chatDispatcherMetrics: metrics} // chatContextBudget = 0
	resp := &chat.ChatResponse{}
	resp.Usage.PromptTokens = 100_000

	w := httptest.NewRecorder()
	s.attachContextTier(w, "alpha", resp)

	assert.Empty(t, w.Header().Get(HeaderContextTier))
	assert.Empty(t, w.Header().Get(HeaderContextHeadroomPct))
	assert.Empty(t, metrics.observed)
}

// TestAttachContextTier_SkipsWhenNoPromptTokens — some providers
// (notably claude-cli historically) don't surface prompt-token
// counts. The proxy must not stamp a bogus "peak" against zero used;
// instead, omit the header so the client knows the signal is
// unavailable.
func TestAttachContextTier_SkipsWhenNoPromptTokens(t *testing.T) {
	metrics := &stubTierMetrics{}
	s := &Server{chatContextBudget: 200_000, chatDispatcherMetrics: metrics}
	resp := &chat.ChatResponse{} // Usage zeroed

	w := httptest.NewRecorder()
	s.attachContextTier(w, "alpha", resp)

	assert.Empty(t, w.Header().Get(HeaderContextTier))
	assert.Empty(t, metrics.observed)
}

// TestAttachContextTier_NilResponseSafe — provider error path can
// leave us with a nil response; the helper must not panic.
func TestAttachContextTier_NilResponseSafe(t *testing.T) {
	s := &Server{chatContextBudget: 200_000}
	w := httptest.NewRecorder()
	assert.NotPanics(t, func() { s.attachContextTier(w, "alpha", nil) })
}

// TestAttachContextTier_NilMetricsStillStampsHeader — a deployment
// without the dispatcher metrics wired (test rigs, telemetry-disabled
// builds) should still surface the header so SDK clients see the
// tier. The metric is the only thing that gets skipped.
func TestAttachContextTier_NilMetricsStillStampsHeader(t *testing.T) {
	s := &Server{chatContextBudget: 200_000} // no metrics
	resp := &chat.ChatResponse{}
	resp.Usage.PromptTokens = 30_000 // 15% used → tier PEAK

	w := httptest.NewRecorder()
	s.attachContextTier(w, "alpha", resp)

	assert.Equal(t, "peak", w.Header().Get(HeaderContextTier))
	headroom, err := strconv.Atoi(w.Header().Get(HeaderContextHeadroomPct))
	assert.NoError(t, err)
	assert.Equal(t, 85, headroom)
}

// TestAttachContextTier_OverflowClampedToPoor — used > budget can
// happen on a misconfigured deployment (operator pinned a budget
// smaller than the upstream model's actual window). The response
// must still stamp; clamps to "poor" / 0% headroom.
func TestAttachContextTier_OverflowClampedToPoor(t *testing.T) {
	s := &Server{chatContextBudget: 100_000}
	resp := &chat.ChatResponse{}
	resp.Usage.PromptTokens = 150_000

	w := httptest.NewRecorder()
	s.attachContextTier(w, "alpha", resp)

	assert.Equal(t, "poor", w.Header().Get(HeaderContextTier))
	assert.Equal(t, "0", w.Header().Get(HeaderContextHeadroomPct))
}

// TestWithPromptCacheMode_StampsField — the ServerOption sets the
// daemon-wide prompt-cache default that the chat-completions proxy
// stamps onto inbound requests lacking an explicit CacheStrategy.
// Empty input is preserved (operators flip to "off" by leaving it
// blank); non-empty values land verbatim. The chat-proxy reads the
// field; this test only pins the option-to-field plumbing.
func TestWithPromptCacheMode_StampsField(t *testing.T) {
	s := &Server{}
	WithPromptCacheMode("aggressive")(s)
	assert.Equal(t, "aggressive", s.promptCacheMode)
	WithPromptCacheMode("")(s) // explicitly clear — opt-out
	assert.Equal(t, "", s.promptCacheMode)
	WithPromptCacheMode("conservative")(s)
	assert.Equal(t, "conservative", s.promptCacheMode)
}

// TestWithChatContextBudget_NegativeIgnored — defensive option
// hygiene. The operator passing 0 / negative should leave the budget
// at its current value rather than disabling the surface mid-config
// load.
func TestWithChatContextBudget_NegativeIgnored(t *testing.T) {
	s := &Server{chatContextBudget: 200_000}
	WithChatContextBudget(-1)(s)
	assert.Equal(t, 200_000, s.chatContextBudget,
		"negative budget should not clobber a previously-set value")
	WithChatContextBudget(0)(s)
	assert.Equal(t, 200_000, s.chatContextBudget,
		"zero budget should not clobber a previously-set value")
	WithChatContextBudget(100_000)(s)
	assert.Equal(t, 100_000, s.chatContextBudget)
}

// TestWithChatContextTierMetrics_WiresAndAllowsNil — the option
// stamps the metrics handle into the Server; passing nil is a valid
// "no telemetry" path that the chat-proxy honours by skipping the
// observation while still emitting the header.
func TestWithChatContextTierMetrics_WiresAndAllowsNil(t *testing.T) {
	s := &Server{}
	m := &stubTierMetrics{}
	WithChatContextTierMetrics(m)(s)
	assert.Same(t, m, s.chatDispatcherMetrics)
	WithChatContextTierMetrics(nil)(s)
	assert.Nil(t, s.chatDispatcherMetrics)
}

// TestChatCompletions_StampsContextTierHeader — full handler
// integration: a chat-completions call with a configured budget and
// prompt-token usage in the response surfaces the X-Vornik-Context-
// Tier header. Pins the wiring between attachContextTier and the
// public ChatCompletions surface.
func TestChatCompletions_StampsContextTierHeader(t *testing.T) {
	resp := &chat.ChatResponse{
		ID:    "resp-tier-1",
		Model: "claude-sonnet-4-6",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
		},
	}
	resp.Usage.PromptTokens = 175_000 // 87.5% used → DEGRADING band
	metrics := &stubTierMetrics{}
	s := &Server{
		logger:                zerolog.Nop(),
		chatProvider:          &stubProvider{resp: resp},
		chatContextBudget:     200_000,
		chatDispatcherMetrics: metrics,
	}

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "degrading", w.Header().Get(HeaderContextTier))
	assert.Equal(t, "12", w.Header().Get(HeaderContextHeadroomPct))
	assert.Len(t, metrics.observed, 1, "ChatCompletions must bump the metric exactly once per call")
}

// TestChatCompletions_NoTierHeaderWhenBudgetUnset — legacy back-compat
// path. A deployment that upgraded to this daemon without setting the
// budget gets exactly the legacy response shape.
func TestChatCompletions_NoTierHeaderWhenBudgetUnset(t *testing.T) {
	resp := &chat.ChatResponse{
		ID:    "resp-no-budget",
		Model: "m",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
		},
	}
	resp.Usage.PromptTokens = 100_000
	s := &Server{logger: zerolog.Nop(), chatProvider: &stubProvider{resp: resp}}

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(HeaderContextTier))
	assert.Empty(t, w.Header().Get(HeaderContextHeadroomPct))
}
