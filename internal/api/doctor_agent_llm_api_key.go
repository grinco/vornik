package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// placeholderTokens are whole-value (normalized) markers of an unfilled key.
// Curated from the repo's example configs (which ship EMPTY) plus common forms.
// Whole-value match only — a real key like "sk-prod-your-server-key" must pass.
var placeholderTokens = map[string]bool{
	"": true, "changeme": true, "change-me": true, "your-api-key": true,
	"your_api_key": true, "replace-with-your-key": true, "replace-with-key": true,
	"replace_me": true, "replaceme": true, "todo": true, "fixme": true,
	"example": true, "dummy": true,
}

func isPlaceholderKey(v string) bool {
	n := strings.ToLower(strings.TrimSpace(v))
	n = strings.Trim(n, "<>[]{}\"'")
	if placeholderTokens[n] {
		return true
	}
	// Bare angle/bracket placeholder like "<your key here>".
	if strings.HasPrefix(strings.TrimSpace(v), "<") && strings.HasSuffix(strings.TrimSpace(v), ">") {
		return true
	}
	return false
}

// endpointNeedsKey reports whether endpoint is a real (non-loopback) upstream
// that would be expected to require an API key. Loopback / localhost targets
// (a local model server) may not.
func endpointNeedsKey(endpoint string) bool {
	// No endpoint means there is no upstream to authenticate against, so an
	// empty key cannot be a fault. Reached by any deployment that routes via
	// sub-providers instead of a single chat.endpoint (chat.provider:
	// "router"), where url.Parse("") yields an empty host that would
	// otherwise fall through to the "needs a key" default and hard-ERROR a
	// perfectly healthy daemon.
	if strings.TrimSpace(endpoint) == "" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// providerDelegatesCredentials reports whether cfg.Chat.Provider hands the
// upstream call (and therefore the credential) to a sub-provider. For those,
// chat.api_key is not the key that authenticates the real upstream — the
// router's per-sub-provider blocks are — so its emptiness says nothing about
// whether the daemon can reach a model.
func providerDelegatesCredentials(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "router")
}

// nativeAnthropicHosts are upstream hosts that speak Anthropic's native API
// (x-api-key header, /v1/models) rather than the OpenAI-compatible surface
// (Authorization: Bearer, /models) liveProbe assumes. Probing these hosts
// the OpenAI-compatible way always 401s regardless of key validity, which
// would produce a false ERROR on a perfectly good shipped default.
var nativeAnthropicHosts = map[string]bool{
	"api.anthropic.com": true,
}

// isNativeAnthropicEndpoint reports whether endpoint targets a known
// native-Anthropic host.
func isNativeAnthropicEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return nativeAnthropicHosts[u.Hostname()]
}

// liveProbe performs the real network check: a 5s-timeout GET to
// <endpoint>/models with the given key as a bearer token. This is the
// default probeFunc (nil ⇒ this); unit tests inject their own probeFunc so
// they never hit the network.
func liveProbe(ctx context.Context, endpoint, key string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/models", nil)
	if err != nil {
		return 0, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// checkAgentLLMAPIKey is the doctor check guarding another fresh-install
// "Invalid API key" failure mode (F2c): the UPSTREAM chat.api_key itself is
// empty, a placeholder left over from an example config, or simply rejected
// by chat.endpoint. Unlike checkAgentLLMTopology (which guards the per-task
// key vs. daemon-proxy routing), this check validates the credential the
// daemon itself uses to talk to the real upstream — a present-but-rejected
// key (or an empty one on a real upstream) makes every job 401 just the
// same. The live probe is injected via probeFunc so unit tests never hit
// the network; nil (production) falls back to liveProbe's real HTTP call.
func (h *DoctorHandlers) checkAgentLLMAPIKey(ctx context.Context) DoctorCheck {
	name := "agent_llm_api_key"
	// A router deployment authenticates per sub-provider (bedrock/vertex/…),
	// several of which use no api_key at all (AWS SigV4, GCP ADC). Reading the
	// empty top-level chat.api_key as a fault would ERROR on every such
	// daemon; the per-model reachability those sub-providers actually depend on
	// is what model_health and model_route_coverage report.
	if providerDelegatesCredentials(h.chatProvider) {
		return DoctorCheck{Name: name, Status: "OK", Message: "chat.provider=router delegates credentials to sub-providers; see model_health for upstream reachability"}
	}
	if isPlaceholderKey(h.chatAPIKey) {
		if h.chatAPIKey == "" && !endpointNeedsKey(h.chatEndpoint) {
			return DoctorCheck{Name: name, Status: "OK", Message: "no upstream key needed for a local endpoint"}
		}
		return DoctorCheck{Name: name, Status: "ERROR", Message: "upstream LLM credential (chat.api_key) is empty or a placeholder"}
	}
	// Native Anthropic uses x-api-key + /v1/models, not the OpenAI-compatible
	// Authorization: Bearer + /models liveProbe sends. Probing it that way
	// always 401s regardless of key validity, so skip the live probe and
	// fall back to the presence+placeholder check already done above.
	if isNativeAnthropicEndpoint(h.chatEndpoint) {
		// doctor-vacuous: OK is correct here, not SKIPPED. The presence +
		// placeholder check above DID run and passed; only the live probe is
		// skipped, and deliberately — this endpoint always 401s the
		// OpenAI-shaped probe regardless of key validity, so running it would
		// produce a false ERROR rather than a real answer. The message states
		// the limitation ("presence check only") so the operator is not misled
		// about what was verified.
		return DoctorCheck{Name: name, Status: "OK", Message: "upstream key present; skipped live probe for native Anthropic endpoint (presence check only)"}
	}
	probe := h.probeFunc
	if probe == nil {
		probe = liveProbe
	}
	code, err := probe(ctx, h.chatEndpoint, h.chatAPIKey)
	switch {
	case err != nil:
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "upstream key present; live probe skipped (" + err.Error() + ")"}
	case code == 401 || code == 403:
		return DoctorCheck{Name: name, Status: "ERROR", Message: "upstream rejected chat.api_key (HTTP " + http.StatusText(code) + ") against chat.endpoint"}
	default:
		return DoctorCheck{Name: name, Status: "OK", Message: "upstream key present and accepted by chat.endpoint"}
	}
}
