package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIBaseURL mirrors the base-URL idiom NewBot uses to build
// b.baseURL (bot.go: fmt.Sprintf("https://api.telegram.org/bot%s",
// config.Token)). Package var, like mcpProbeConnect
// (internal/ui/admin_control_plane_mcp_probe.go), so tests can point it at
// an httptest server without a live network call.
var APIBaseURL = "https://api.telegram.org"

// maxProbeResponseBytes caps how much of a getMe response is read — the
// payload is a few hundred bytes in practice; the cap protects against a
// misbehaving/hostile endpoint returning a giant body.
const maxProbeResponseBytes = 64 * 1024

// ProbeError is returned by ProbeToken when Telegram responds with a
// non-2xx status. StatusCode lets the caller (internal/integrations)
// classify 401/403 (invalid token) as OutcomeFail versus 429/5xx as
// OutcomeError, without either side depending on string-matching
// Description.
type ProbeError struct {
	StatusCode  int
	Description string
}

func (e *ProbeError) Error() string {
	return fmt.Sprintf("telegram: getMe returned HTTP %d: %s", e.StatusCode, e.Description)
}

// getMeResponse models the subset of Telegram's getMe response ProbeToken
// needs. Unknown fields are dropped by json.Unmarshal.
type getMeResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		Username string `json:"username"`
	} `json:"result"`
	Description string `json:"description"`
}

// ProbeToken validates a candidate bot token against Telegram's getMe
// endpoint, WITHOUT starting a bot or persisting anything. Returns the
// bot's username on success (the caller builds a secret-free
// "Connected as @<username>" Summary from it) or an error otherwise:
//   - *ProbeError for any non-2xx HTTP response (StatusCode carries the
//     classification signal)
//   - a plain error for a transport failure (dial/timeout) or an
//     unparseable body
//
// httpClient is caller-supplied so the integrations probe layer can pass a
// DialGuard-wrapped client (design §6) — ProbeToken never constructs its
// own unguarded client.
func ProbeToken(ctx context.Context, httpClient *http.Client, token string) (username string, err error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(token) == "" {
		return "", errors.New("telegram: bot token is empty")
	}

	url := APIBaseURL + "/bot" + token + "/getMe"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("telegram: build getMe request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		// http.Client wraps the full request URL (which embeds the token)
		// into *url.Error on transport failure. Re-wrap with a token-free
		// message rather than propagate err verbatim — the request path
		// alone is enough context, the token never should be.
		return "", fmt.Errorf("telegram: getMe request failed: %s", classifyTransportErr(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes))

	var parsed getMeResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return "", fmt.Errorf("telegram: getMe response parse (status %d): %w", resp.StatusCode, jsonErr)
	}

	if resp.StatusCode != http.StatusOK || !parsed.OK {
		return "", &ProbeError{StatusCode: resp.StatusCode, Description: parsed.Description}
	}
	if parsed.Result.Username == "" {
		return "", errors.New("telegram: getMe succeeded but the response has no username")
	}
	return parsed.Result.Username, nil
}

// classifyTransportErr strips anything that looks like a URL (and
// therefore might carry the token) out of a transport error, keeping only
// the underlying cause (dial refused, timeout, etc.).
func classifyTransportErr(err error) string {
	msg := err.Error()
	// net/url wraps as `Get "https://...": <cause>` — keep only <cause>.
	if idx := strings.LastIndex(msg, `": `); idx != -1 {
		return msg[idx+3:]
	}
	return msg
}
