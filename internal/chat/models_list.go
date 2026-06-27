package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ListModels implements ModelLister for the OpenAI-compatible HTTP
// client. It hits GET <endpoint>/models — the standard discovery
// endpoint every OpenAI-compat gateway speaks, including the Bedrock
// gateway at our HTTP sub-provider and Vertex's OpenAI-compat surface.
//
// The endpoint construction reuses c.endpoint, which normalizeEndpoint
// already trimmed of any trailing /chat/completions during NewClient,
// so the resulting URL is e.g. "<base>/v1/models" or
// "<base>/openapi/models" depending on how the operator configured it.
//
// On a non-2xx response the body is returned as part of the error so
// operators can see the gateway's complaint without having to enable
// debug logs. Body size is bounded by maxChatResponseBytes for the
// same hostile-gateway reason completion responses are.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	// Static override path: providers without a usable /v1/models
	// endpoint (Vertex's openapi surface, custom gateways) get a
	// curated list at construction time. The presence of the option
	// — not the list's length — gates the short-circuit, so an
	// explicitly empty catalog still skips the live fetch (Vertex
	// 404s on /v1/models regardless of whether we have models to
	// show). Returning a copy so the caller can't mutate the
	// canonical slice.
	if c.staticModelsSet {
		out := make([]ModelInfo, len(c.staticModels))
		copy(out, c.staticModels)
		return c.applyModelListFilter(out), nil
	}
	if c.endpoint == "" {
		return nil, ErrEmptyEndpoint
	}

	url := c.endpoint + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	c.setExtraHeaders(req)
	c.setAuthHeader(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Compact the upstream body so a verbose HTML error page
		// doesn't blow up the operator's terminal. Whitespace is
		// folded to single spaces and the result truncated to 256
		// chars — enough to identify the failure mode (auth, 404,
		// gateway-specific error JSON) without dumping a styled
		// Google error page into the CLI table.
		//
		// Note for Vertex specifically: Google's OpenAI-compat
		// surface only implements /chat/completions — `/models`
		// 404s. There's no clean discovery for Vertex MaaS via this
		// path; the native Publishers API is a separate project.
		preview := compactErrorPreview(string(body), 256)
		return nil, fmt.Errorf("models endpoint returned HTTP %d: %s", resp.StatusCode, preview)
	}

	// Parse the OpenAI-compat shape: {"object": "list", "data":
	// [{"id": "...", "object": "model", "created": <unix>,
	// "owned_by": "..."}]}. Some gateways return a bare array — handle
	// that too.
	var typed struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &typed); err == nil && typed.Data != nil {
		out := make([]ModelInfo, 0, len(typed.Data))
		for _, m := range typed.Data {
			if m.ID == "" {
				continue
			}
			out = append(out, ModelInfo{
				ID:      m.ID,
				Source:  "live",
				OwnedBy: m.OwnedBy,
				Created: m.Created,
			})
		}
		return c.applyModelListFilter(out), nil
	}

	// Fallback: bare array shape.
	var bare []struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	out := make([]ModelInfo, 0, len(bare))
	for _, m := range bare {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:      m.ID,
			Source:  "live",
			OwnedBy: m.OwnedBy,
			Created: m.Created,
		})
	}
	return c.applyModelListFilter(out), nil
}

// applyModelListFilter drops models for which the configured predicate
// returns false. nil filter is a pass-through. Filters in place and
// returns the (possibly shorter) slice.
func (c *Client) applyModelListFilter(models []ModelInfo) []ModelInfo {
	if c.modelListFilter == nil {
		return models
	}
	kept := models[:0]
	for _, m := range models {
		if c.modelListFilter(m) {
			kept = append(kept, m)
		}
	}
	return kept
}

// Compile-time conformance check.
var _ ModelLister = (*Client)(nil)

// Ping implements Pinger. We piggyback on the existing /v1/models
// path — it's the standard OpenAI-compat discovery endpoint, costs
// no tokens, and exercises the same network/auth path a real
// completion uses. A static-models override short-circuits to nil,
// since there's nothing live to probe in that configuration.
func (c *Client) Ping(ctx context.Context) error {
	if c.staticModelsSet {
		return nil
	}
	_, err := c.ListModels(ctx)
	return err
}

var _ Pinger = (*Client)(nil)

// compactErrorPreview folds whitespace runs (including newlines and
// tabs) into single spaces and truncates the result to max characters.
// Used when surfacing an upstream HTTP error body to the operator —
// keeps the CLI table tidy without losing the diagnostic text.
func compactErrorPreview(s string, max int) string {
	var b []byte
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if prevSpace {
				continue
			}
			b = append(b, ' ')
			prevSpace = true
			continue
		}
		b = append(b, string(r)...)
		prevSpace = false
	}
	out := string(b)
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}
