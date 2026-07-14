package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// modelRewritingStubProvider simulates a sub-provider whose WithModel
// clone reports a DIFFERENT model than what was actually requested —
// exactly what the rejected "ollama/"-prefix-stripping design
// (https://docs.vornik.io §2.1) would have
// done: WithModel("ollama/gpt-oss:120b") would strip and send
// "gpt-oss:120b" on the wire, so Model() reports "gpt-oss:120b" while
// the client's original request said "ollama/gpt-oss:120b". Neither the
// stub's response body nor Model() ever echoes the ORIGINAL requested
// string, matching a real HTTP round-trip where the upstream only ever
// sees (and echoes back) the post-strip model.
type modelRewritingStubProvider struct {
	stubProvider
}

func (o *modelRewritingStubProvider) Model() string { return "gpt-oss:120b" }
func (o *modelRewritingStubProvider) WithModel(_ string) chat.Provider {
	clone := *o
	return &clone
}

// TestChatCompletions_AuditCostUsesRawRequestModel pins the specific
// invariant https://docs.vornik.io §2.1
// verified and relies on: chat_proxy.go's
// `cost := s.computeChatCallCost(req.Model, resp)` — the value threaded
// into the chat_audit_log row — passes the raw, client-supplied
// `req.Model`. This call site does NOT self-correct via the response's
// own model field the way recordChatAPIUsage's internal cost
// recomputation does (that one prefers resp.Model, which real HTTP
// providers populate from the wire response and which a
// non-rewriting provider always keeps in sync with what was asked for
// — see the design doc for why that self-correction doesn't rescue the
// rejected prefix-stripping design here).
//
// Test setup: a real pricing.Table prices "ollama/gpt-oss:120b" (what a
// stripping wrapper would have left in req.Model) at a distinctive
// nonzero rate, while the provider's actually-reported served model
// ("gpt-oss:120b", what modelRewritingStubProvider.Model() returns) is
// absent from the table and therefore priced at $0. If cost computation
// ever started using resp.Model / provider.Model() instead of
// req.Model, this test would assert the wrong price and fail — it is
// deliberately not a token-count sanity check, it's a pinned regression
// against the exact code path §2.1 analyzed.
func TestChatCompletions_AuditCostUsesRawRequestModel(t *testing.T) {
	pricingPath := filepath.Join(t.TempDir(), "pricing.yaml")
	// Only "ollama/gpt-oss:120b" (the raw, unstripped request string) is
	// priced. "gpt-oss:120b" (the provider's reported served model) is
	// absent, so a lookup against it falls through to the zero-value
	// default and costs $0 — the two prices are deliberately
	// distinguishable.
	const pricingYAML = `
models:
  "ollama/gpt-oss:120b":
    input: 10.0
    output: 20.0
`
	require.NoError(t, os.WriteFile(pricingPath, []byte(pricingYAML), 0o600))

	stub := &modelRewritingStubProvider{
		stubProvider: stubProvider{
			resp: &chat.ChatResponse{
				Choices: []struct {
					Index        int          `json:"index"`
					Message      chat.Message `json:"message"`
					FinishReason string       `json:"finish_reason"`
				}{
					{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
				},
			},
		},
	}
	stub.resp.Usage.PromptTokens = 1_000_000
	stub.resp.Usage.CompletionTokens = 1_000_000

	auditRepo := newStubChatAuditRepoAPI()
	s := NewServer(WithLogger(zerolog.Nop()), WithChatAuditRepository(auditRepo), WithPricingPath(pricingPath))
	s.chatProvider = stub

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"ollama/gpt-oss:120b","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("X-Vornik-Project-ID", "proj-ollama-cloud")
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// recordChatAPIAudit runs in a fire-and-forget goroutine after the
	// response is flushed (see chat_proxy.go's telemetry comment), so
	// poll briefly instead of reading the snapshot synchronously.
	var entries []*persistence.ChatAuditEntry
	require.Eventually(t, func() bool {
		entries = auditRepo.snapshot()
		return len(entries) > 0
	}, time.Second, 5*time.Millisecond, "audit row must be recorded")

	// 1M prompt tokens @ $10/M + 1M completion tokens @ $20/M = $30.
	// Priced under the RAW req.Model key ("ollama/gpt-oss:120b"), not
	// the provider's reported "gpt-oss:120b" (which is unpriced → $0).
	assert.Equal(t, 30.0, entries[0].CostUSD,
		"audit cost must be computed against the raw request model — a provider reporting a "+
			"different served model (as the rejected prefix-stripping design would) must not "+
			"change which pricing.yaml row gets charged")
}
