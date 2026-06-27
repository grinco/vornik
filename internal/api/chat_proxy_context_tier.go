package api

// Chat-proxy context-budget tier surface — Slice 3 of the
// context-tier track. Stateless OpenAI-compatible proxy doesn't carry
// per-session history, so the tier is computed off the actual
// prompt-token count the upstream provider reported (resp.Usage.
// PromptTokens) divided by the deployment-configured
// chatContextBudget.
//
// Surfaces:
//
//   - `X-Vornik-Context-Tier` response header — readable by an
//     OpenAI-SDK consumer that wants to display a "context filling
//     up" banner without parsing a proprietary body envelope.
//   - `X-Vornik-Context-Headroom-Pct` response header — exact %
//     remaining (integer 0-100), in case the consumer wants to
//     render a progress bar instead of a discrete tier.
//   - vornik_chat_context_tier_total{project,tier} Prometheus counter
//     and vornik_chat_context_headroom_pct{project} histogram — both
//     shared with the dispatcher path so per-project alerts cover
//     both Telegram/UI and external-API traffic.
//
// Disabled (header omitted, metric skipped) when:
//
//   - The deployment didn't configure a chatContextBudget (zero).
//   - The upstream provider didn't report prompt-token usage.
//   - The response was nil (provider error path — caller already
//     handled the error response shape).
//
// Header name + format are intentionally lowercase tier strings
// (matches chat.ContextTier.String()) so a client doing string
// comparison doesn't need to fold case.

import (
	"net/http"
	"strconv"

	"vornik.io/vornik/internal/chat"
)

// HeaderContextTier is the response header carrying the tier band.
// Lowercased value ("peak" / "good" / "degrading" / "poor").
const HeaderContextTier = "X-Vornik-Context-Tier"

// HeaderContextHeadroomPct is the response header carrying the exact
// remaining-budget % (integer 0-100). Paired with HeaderContextTier
// so consumers can pick the granularity that suits them.
const HeaderContextHeadroomPct = "X-Vornik-Context-Headroom-Pct"

// attachContextTier computes the tier from resp.Usage.PromptTokens
// against s.chatContextBudget, stamps the X-Vornik-Context-Tier and
// X-Vornik-Context-Headroom-Pct headers, and bumps the dispatcher
// metrics counter when one is wired. No-op when the budget is unset
// or the response carries no prompt-token data. metricsProject is
// the same projectID resolution the chat-proxy uses for cost
// attribution: caller passes the post-header value so it can supply
// the right label.
func (s *Server) attachContextTier(w http.ResponseWriter, metricsProject string, resp *chat.ChatResponse) {
	if s == nil || w == nil || resp == nil {
		return
	}
	if s.chatContextBudget <= 0 {
		return
	}
	used := resp.Usage.PromptTokens
	if used <= 0 {
		// Some providers (claude-cli historically) don't report usage.
		// Skip rather than stamp a bogus "peak" against zero used.
		return
	}
	tier := chat.TierFromUsage(used, s.chatContextBudget)
	headroom := chat.HeadroomPct(used, s.chatContextBudget)
	w.Header().Set(HeaderContextTier, tier.String())
	w.Header().Set(HeaderContextHeadroomPct, strconv.Itoa(int(headroom)))
	if s.chatDispatcherMetrics != nil {
		s.chatDispatcherMetrics.ObserveContextTier(metricsProject, tier, headroom)
	}
}
