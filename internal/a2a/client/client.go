// Package client is the shared outbound A2A protocol client. It
// submits a task to a remote A2A agent, consumes the SSE stream to a
// terminal state, and returns the answer. Two callers share it:
//
//   - the `a2a_call` workflow step (internal/executor), and
//   - the agent-initiated `consult_*` tools (internal/dispatcher),
//
// so the protocol logic — submit, stream URL resolution, SSE framing,
// terminal resolution — lives here once (a2a-expert-federation-design
// §5). It has no dependency on the inbound server package.
//
// Answer delivery: a vornik inbound streams the task's ResultEnvelope
// as an `event: artifact` frame; third-party partners may instead use
// `event: message` text. The client surfaces whichever arrives as
// CallResult.Answer, preferring the structured artifact (§7).
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxSubmitResponseBytes = 1 << 20
	// streamMaxIdle bounds the gap between SSE events before we
	// declare the partner hung; without it a wedged partner pins the
	// caller goroutine indefinitely.
	streamMaxIdle = 90 * time.Second
	// DefaultTimeout caps total call duration when CallRequest.Timeout
	// is zero.
	DefaultTimeout = 5 * time.Minute
)

// CallRequest is one outbound A2A call.
type CallRequest struct {
	AgentURL string         // e.g. https://host/a2a/v1/agents/<project>/<workflow>
	APIKey   string         // X-API-Key value (empty = unauthenticated)
	Text     string         // the single text part submitted as the task message
	Metadata map[string]any // task metadata (carries the consult hop counter)
	Timeout  time.Duration  // total call deadline; DefaultTimeout when zero
}

// CallResult is the outcome of a completed A2A call.
type CallResult struct {
	TaskID       string          // remote task id
	State        string          // terminal A2A state (completed / failed / canceled / …)
	Answer       string          // best-effort answer text (artifact payload preferred, else message text)
	Artifact     json.RawMessage // raw artifact payload, when the partner sent one
	PartnerAgent string          // the agent URL called
}

// Client performs outbound A2A calls. Safe for concurrent use.
type Client struct {
	http *http.Client
}

// New returns a Client with bounded transport timeouts. The per-call
// deadline comes from CallRequest.Timeout via context.
func New() *Client {
	return &Client{http: &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxConnsPerHost:     8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			if !sameOrigin(via[len(via)-1].URL, req.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}}
}

type submitResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	StreamURL string `json:"streamUrl"`
}

// Call runs one A2A call end-to-end: submit → stream → resolve. On a
// terminal `completed` state it returns the result + nil; on
// failed/canceled/other it returns the result + a non-nil error so the
// caller can branch.
func (c *Client) Call(ctx context.Context, r CallRequest) (CallResult, error) {
	agentURL := strings.TrimRight(strings.TrimSpace(r.AgentURL), "/")
	if agentURL == "" {
		return CallResult{}, errors.New("a2a client: agent_url is required")
	}
	if strings.TrimSpace(r.Text) == "" {
		return CallResult{}, errors.New("a2a client: text is required")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sr, err := c.submit(callCtx, agentURL, r)
	if err != nil {
		return CallResult{}, err
	}
	streamURL, err := resolveStreamURL(agentURL, sr.StreamURL)
	if err != nil {
		return CallResult{}, fmt.Errorf("a2a client: resolve stream URL: %w", err)
	}
	state, answer, artifact, err := c.consumeSSE(callCtx, streamURL, r.APIKey)
	if err != nil {
		return CallResult{}, fmt.Errorf("a2a client: stream: %w", err)
	}

	res := CallResult{
		TaskID:       sr.TaskID,
		State:        state,
		Answer:       answer,
		Artifact:     artifact,
		PartnerAgent: agentURL,
	}
	switch state {
	case "completed":
		return res, nil
	case "failed", "canceled":
		return res, fmt.Errorf("a2a client: partner ended in state %q", state)
	default:
		return res, fmt.Errorf("a2a client: partner ended in state %q (not handled)", state)
	}
}

// submit POSTs the task to <agentURL>/tasks and parses the response.
func (c *Client) submit(ctx context.Context, agentURL string, r CallRequest) (submitResponse, error) {
	msg := map[string]any{"parts": []map[string]any{{"type": "text", "text": r.Text}}}
	reqBody := map[string]any{"message": msg}
	if len(r.Metadata) > 0 {
		reqBody["metadata"] = r.Metadata
	}
	submitBody, err := json.Marshal(reqBody)
	if err != nil {
		return submitResponse{}, fmt.Errorf("a2a client: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL+"/tasks", bytes.NewReader(submitBody))
	if err != nil {
		return submitResponse{}, fmt.Errorf("a2a client: build submit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vornik-a2a-client/1")
	if r.APIKey != "" {
		req.Header.Set("X-API-Key", r.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return submitResponse{}, fmt.Errorf("a2a client: submit: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSubmitResponseBytes+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return submitResponse{}, fmt.Errorf("a2a client: read submit response: %w", readErr)
	}
	if len(body) > maxSubmitResponseBytes {
		return submitResponse{}, fmt.Errorf("a2a client: submit response exceeds %d bytes", maxSubmitResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return submitResponse{}, fmt.Errorf("a2a client: submit HTTP %d: %s", resp.StatusCode, truncateForLog(string(body), 240))
	}
	var sr submitResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return submitResponse{}, fmt.Errorf("a2a client: parse submit response: %w", err)
	}
	if sr.TaskID == "" || sr.StreamURL == "" {
		return submitResponse{}, errors.New("a2a client: submit response missing taskId or streamUrl")
	}
	return sr, nil
}

// resolveStreamURL handles a path-only stream URL (server emitted a
// path because its publicBaseURL wasn't set) by reattaching the agent
// host, and enforces the stream stays on the agent origin.
func resolveStreamURL(agentURL, streamURL string) (string, error) {
	agent, err := url.Parse(agentURL)
	if err != nil || agent.Scheme == "" || agent.Host == "" {
		return "", fmt.Errorf("invalid agent_url %q", agentURL)
	}
	if agent.Scheme != "http" && agent.Scheme != "https" {
		return "", errors.New("agent_url scheme must be http or https")
	}
	stream, err := url.Parse(streamURL)
	if err != nil {
		return "", fmt.Errorf("invalid stream URL %q: %w", streamURL, err)
	}
	if stream.IsAbs() {
		if stream.Scheme != "http" && stream.Scheme != "https" {
			return "", errors.New("stream URL scheme must be http or https")
		}
		if !sameOrigin(agent, stream) {
			return "", errors.New("stream URL must stay on the agent origin")
		}
		return stream.String(), nil
	}
	if !strings.HasPrefix(streamURL, "/") {
		return "", fmt.Errorf("relative stream URL must start with '/' (got %q)", streamURL)
	}
	return agent.ResolveReference(stream).String(), nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// sseAccum accumulates the parsed SSE frames of one stream: the terminal
// state, the structured artifact payload, and any message text.
type sseAccum struct {
	text      strings.Builder
	artifact  json.RawMessage
	lastState string
	lastFinal bool
	event     string
	data      strings.Builder
}

// line feeds one raw SSE line. A blank line flushes the current frame
// and reports whether it was terminal (final=true).
func (a *sseAccum) line(l string) (final bool) {
	switch {
	case l == "":
		a.flush()
		return a.lastFinal
	case strings.HasPrefix(l, ":"):
		// comment / keepalive — ignore
	case strings.HasPrefix(l, "event:"):
		a.event = strings.TrimSpace(l[len("event:"):])
	case strings.HasPrefix(l, "data:"):
		if a.data.Len() > 0 {
			a.data.WriteByte('\n')
		}
		a.data.WriteString(strings.TrimSpace(l[len("data:"):]))
	}
	return false
}

// flush decodes the buffered frame into the accumulator.
func (a *sseAccum) flush() {
	if a.event == "" && a.data.Len() == 0 {
		return
	}
	raw := a.data.String()
	switch a.event {
	case "status":
		var p struct {
			State string `json:"state"`
			Final bool   `json:"final"`
		}
		if json.Unmarshal([]byte(raw), &p) == nil {
			if p.State != "" {
				a.lastState = p.State
			}
			if p.Final {
				a.lastFinal = true
			}
		}
	case "artifact":
		var p struct {
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(raw), &p) == nil && len(p.Payload) > 0 {
			a.artifact = p.Payload
		}
	case "message":
		var p struct {
			Text  string     `json:"text"`
			Parts []textPart `json:"parts"`
		}
		if json.Unmarshal([]byte(raw), &p) == nil {
			appendText(&a.text, p.Text)
			for _, part := range p.Parts {
				if part.Type == "text" {
					appendText(&a.text, part.Text)
				}
			}
		}
	}
	a.event = ""
	a.data.Reset()
}

// result derives the final (state, answer, artifact, err). A vornik
// artifact answer is preferred; message text is the fallback.
func (a *sseAccum) result() (string, string, json.RawMessage, error) {
	ans := answerFromArtifact(a.artifact)
	if ans == "" {
		ans = a.text.String()
	}
	if a.lastState == "" {
		return "", ans, a.artifact, errors.New("stream closed before any status frame")
	}
	return a.lastState, ans, a.artifact, nil
}

// consumeSSE reads the partner's SSE stream, tracking the terminal state
// and capturing the answer. A vornik inbound sends the answer as an
// `event: artifact` frame (its payload is the ResultEnvelope);
// third-party partners may send `event: message` text.
func (c *Client) consumeSSE(ctx context.Context, streamURL, apiKey string) (string, string, json.RawMessage, error) {
	resp, err := c.openStream(ctx, streamURL, apiKey)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	acc := &sseAccum{}
	idleTimer := time.NewTimer(streamMaxIdle)
	defer idleTimer.Stop()

	scanCh := make(chan string, 1)
	scanErrCh := make(chan error, 1)
	consumerGone := make(chan struct{})
	defer close(consumerGone)
	go func() {
		defer close(scanCh)
		for scanner.Scan() {
			select {
			case scanCh <- scanner.Text():
			case <-consumerGone:
				return
			}
		}
		scanErrCh <- scanner.Err()
	}()

	for {
		select {
		case <-ctx.Done():
			return acc.lastState, acc.text.String(), acc.artifact, ctx.Err()
		case <-idleTimer.C:
			return acc.lastState, acc.text.String(), acc.artifact, errors.New("partner SSE stream idle")
		case l, ok := <-scanCh:
			if !ok {
				acc.flush()
				return acc.result()
			}
			resetTimer(idleTimer, streamMaxIdle)
			if acc.line(l) {
				return acc.result()
			}
		case scanErr := <-scanErrCh:
			for l := range scanCh {
				acc.line(l)
			}
			acc.flush()
			if scanErr != nil {
				return acc.lastState, acc.text.String(), acc.artifact, fmt.Errorf("stream scan: %w", scanErr)
			}
			return acc.result()
		}
	}
}

// openStream issues the GET and validates the response is a live 2xx.
func (c *Client) openStream(ctx context.Context, streamURL, apiKey string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "vornik-a2a-client/1")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stream connect: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stream HTTP %d: %s", resp.StatusCode, truncateForLog(string(b), 240))
	}
	return resp, nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func appendText(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(s)
}

// answerFromArtifact best-effort extracts human-readable answer text
// from a structured artifact payload. A vornik product-qa ResultEnvelope
// is {"answer": "...", "citations": [...]}; other workflows may use a
// different field, so we probe the common ones before falling back to
// the compact JSON so the caller always gets *something*.
func answerFromArtifact(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	// Payload may itself be a JSON string.
	var asStr string
	if json.Unmarshal(payload, &asStr) == nil && asStr != "" {
		return asStr
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) == nil {
		for _, key := range []string{"answer", "text", "result", "content", "message"} {
			if raw, ok := obj[key]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil && s != "" {
					return s
				}
			}
		}
	}
	// Structured but no known text field — return the compact JSON so
	// the caller can parse it against the workflow's schema.
	return string(payload)
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
