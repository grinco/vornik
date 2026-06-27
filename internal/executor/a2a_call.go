package executor

// Outbound A2A client step. The `a2a_call` workflow step type
// lets a vornik workflow delegate to a third-party A2A-compliant
// agent (vendor scraper, partner specialist, another vornik
// daemon's published workflow) without a custom dispatcher
// shim.
//
// Wire:
//   1. POST <agent_url>/tasks with the prompt as the only text
//      part. Captures the task id + stream URL from the
//      response.
//   2. GET the stream URL as SSE. Accumulates `event: message`
//      text parts; tracks the latest `state` from `event:
//      status` frames.
//   3. Resolves the step on terminal state — completed → success,
//      failed/canceled → on_fail, input-required surfaces a
//      checkpoint (deferred; for v1 we treat input-required as
//      failure since the dispatcher's checkpoint plumbing is
//      task-anchored).
//
// What this DOES NOT do (deferred):
//   - Schema validation against step.Expect — partner agents
//     don't yet ship structured response schemas in the spec we
//     support.
//   - Live-pubsub bridge: streaming the partner's progress
//     events into the operator's live view. The step still
//     blocks on its single goroutine; the existing step-
//     started / step-completed events fire on entry + exit.
//   - input-required → checkpoint. Will surface in Phase C.
//   - Outbound auth via the secrets store; for v1 we read an
//     env var named by step.APIKeyEnv, which keeps secrets out
//     of the YAML.

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
	"os"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
)

// a2aCallStreamMaxIdle is the largest gap between SSE events
// we'll wait before declaring the partner hung. Cleanly bounded
// — without it a wedged partner pins the executor goroutine
// indefinitely.
const a2aCallStreamMaxIdle = 90 * time.Second

// a2aCallDefaultTimeout caps the total step duration when the
// workflow step's Timeout field is zero. 5 minutes balances
// "long enough for real partner work" against "doesn't pin the
// goroutine forever".
const a2aCallDefaultTimeout = 5 * time.Minute

// a2aSubmitResponse mirrors the inbound `taskSubmitResponse`
// shape on the server side. Defined locally so this package
// doesn't import internal/conversation/a2a (which would create
// an executor→conversation dependency that doesn't exist
// elsewhere).
type a2aSubmitResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	StreamURL string `json:"streamUrl"`
}

// a2aCallResult is the structured output the step writes to
// state.LastResult on success. JSON-encoded so downstream
// steps + gates can read fields directly.
type a2aCallResult struct {
	TaskID       string `json:"taskId"`
	State        string `json:"state"`
	Text         string `json:"text,omitempty"`
	PartnerAgent string `json:"partner_agent"`
}

// handleA2ACallStep runs one a2a_call step end-to-end. Returns
// the final result + nil on success, or an error to fire
// step.OnFail. The caller (workflow.go dispatch site) is
// responsible for transitioning state.LastResult + the next
// step pointer.
func (e *Executor) handleA2ACallStep(ctx context.Context, stepID string, step *registry.WorkflowStep) (*a2aCallResult, error) {
	if step == nil {
		return nil, fmt.Errorf("a2a_call %s: step is nil", stepID)
	}
	agentURL := strings.TrimRight(strings.TrimSpace(step.AgentURL), "/")
	if agentURL == "" {
		return nil, fmt.Errorf("a2a_call %s: agent_url is required", stepID)
	}
	prompt := strings.TrimSpace(step.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("a2a_call %s: prompt is required", stepID)
	}

	timeout := a2aCallDefaultTimeout
	if step.Timeout != "" {
		if d, err := time.ParseDuration(step.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	apiKey := ""
	if step.APIKeyEnv != "" {
		apiKey = strings.TrimSpace(os.Getenv(step.APIKeyEnv))
	}

	// 1. Submit the task.
	submitBody, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"parts": []map[string]any{{"type": "text", "text": prompt}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("a2a_call %s: marshal request: %w", stepID, err)
	}
	submitURL := agentURL + "/tasks"
	req, err := http.NewRequestWithContext(stepCtx, http.MethodPost, submitURL, bytes.NewReader(submitBody))
	if err != nil {
		return nil, fmt.Errorf("a2a_call %s: build submit request: %w", stepID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vornik-a2a-client/1")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := a2aHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a_call %s: submit: %w", stepID, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("a2a_call %s: submit HTTP %d: %s", stepID, resp.StatusCode, truncateForLog(string(body), 240))
	}
	var submitResp a2aSubmitResponse
	if err := json.Unmarshal(body, &submitResp); err != nil {
		return nil, fmt.Errorf("a2a_call %s: parse submit response: %w", stepID, err)
	}
	if submitResp.TaskID == "" || submitResp.StreamURL == "" {
		return nil, fmt.Errorf("a2a_call %s: submit response missing taskId or streamUrl", stepID)
	}

	// 2. Consume the SSE stream. resolveStreamURL handles the
	// path-only case (server emitted a path because publicBaseURL
	// wasn't set) by reattaching the agent's host.
	streamURL, err := resolveStreamURL(agentURL, submitResp.StreamURL)
	if err != nil {
		return nil, fmt.Errorf("a2a_call %s: resolve stream URL: %w", stepID, err)
	}
	final, text, err := consumeA2ASSEStream(stepCtx, streamURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("a2a_call %s: stream: %w", stepID, err)
	}

	result := &a2aCallResult{
		TaskID:       submitResp.TaskID,
		State:        final,
		Text:         text,
		PartnerAgent: agentURL,
	}
	switch final {
	case "completed":
		return result, nil
	case "failed", "canceled":
		return result, fmt.Errorf("a2a_call %s: partner ended in state %q", stepID, final)
	default:
		// E.g. "input-required" — for v1 we surface as
		// failure so on_fail fires. Phase C will route to a
		// checkpoint instead.
		return result, fmt.Errorf("a2a_call %s: partner ended in state %q (not handled in this slice)", stepID, final)
	}
}

// a2aHTTPClient is the package-shared HTTP client for outbound
// A2A calls. Bounded transport timeouts; the per-call deadline
// comes from the workflow step's Timeout via context.
var a2aHTTPClient = &http.Client{
	Timeout: 0, // controlled by per-call context
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
}

// resolveStreamURL handles the case where the server emitted a
// path-only stream URL (publicBaseURL not configured). In that
// case we re-attach the host from the original agent URL.
func resolveStreamURL(agentURL, streamURL string) (string, error) {
	agent, err := url.Parse(agentURL)
	if err != nil || agent.Scheme == "" || agent.Host == "" {
		return "", fmt.Errorf("invalid agent_url %q", agentURL)
	}
	if agent.Scheme != "http" && agent.Scheme != "https" {
		return "", fmt.Errorf("agent_url scheme must be http or https")
	}
	stream, err := url.Parse(streamURL)
	if err != nil {
		return "", fmt.Errorf("invalid stream URL %q: %w", streamURL, err)
	}
	if stream.IsAbs() {
		if stream.Scheme != "http" && stream.Scheme != "https" {
			return "", fmt.Errorf("stream URL scheme must be http or https")
		}
		if !sameOrigin(agent, stream) {
			return "", fmt.Errorf("stream URL must stay on the agent origin")
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

// consumeA2ASSEStream reads the SSE stream from the partner,
// accumulating text parts and tracking the last seen state.
// Returns the terminal state + accumulated text.
//
// Frame format (per a2a/sse.go on the server side):
//
//	event: status
//	data: {"taskId":"...","state":"working","final":false,"payload":...}
//
//	event: artifact
//	data: {"taskId":"...","payload":...}
//
//	event: message
//	data: {... text part ...}
//
// Frames are blank-line delimited. We don't try to parse
// payloads of artifact / message frames here — too partner-
// specific; just track terminal status and concatenate any
// text parts that arrive.
func consumeA2ASSEStream(ctx context.Context, streamURL, apiKey string) (state string, text string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "vornik-a2a-client/1")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := a2aHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("stream connect: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", "", fmt.Errorf("stream HTTP %d: %s", resp.StatusCode, truncateForLog(string(body), 240))
	}

	scanner := bufio.NewScanner(resp.Body)
	// 4 MiB max per SSE frame — generous; partner agents
	// sometimes inline big artifact payloads.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		currentEvent string
		dataBuf      strings.Builder
		textOut      strings.Builder
		lastState    string
		lastFinal    bool
		idleTimer    = time.NewTimer(a2aCallStreamMaxIdle)
	)
	defer idleTimer.Stop()

	scanCh := make(chan string, 1)
	scanErr := make(chan error, 1)
	// consumerGone unblocks the producer goroutine when this function
	// returns early (ctx cancelled, idle timeout). Closing resp.Body
	// only unblocks an in-progress scanner.Scan() READ — it does NOT
	// unblock a goroutine parked on the `scanCh <-` SEND, so pre-fix a
	// partner that streamed a burst then stalled stranded one
	// goroutine (plus its 4 MiB scanner buffer) per affected call,
	// forever (bug-sweep follow-up 2026-06-04). scanErr is buffered,
	// so that send never blocks.
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
		scanErr <- scanner.Err()
	}()

	flushFrame := func() {
		if currentEvent == "" && dataBuf.Len() == 0 {
			return
		}
		switch currentEvent {
		case "status":
			var payload struct {
				State string `json:"state"`
				Final bool   `json:"final"`
			}
			if err := json.Unmarshal([]byte(dataBuf.String()), &payload); err == nil {
				if payload.State != "" {
					lastState = payload.State
				}
				if payload.Final {
					lastFinal = true
				}
			}
		case "message":
			var payload struct {
				Text  string         `json:"text"`
				Parts []a2aTextPart  `json:"parts"`
				Reply string         `json:"reply"`
				Extra map[string]any `json:"-"`
			}
			if err := json.Unmarshal([]byte(dataBuf.String()), &payload); err == nil {
				if payload.Text != "" {
					if textOut.Len() > 0 {
						textOut.WriteString("\n")
					}
					textOut.WriteString(payload.Text)
				}
				for _, p := range payload.Parts {
					if p.Type == "text" && p.Text != "" {
						if textOut.Len() > 0 {
							textOut.WriteString("\n")
						}
						textOut.WriteString(p.Text)
					}
				}
			}
		}
		currentEvent = ""
		dataBuf.Reset()
	}

	for {
		select {
		case <-ctx.Done():
			return lastState, textOut.String(), ctx.Err()
		case <-idleTimer.C:
			return lastState, textOut.String(), errors.New("partner SSE stream idle")
		case line, ok := <-scanCh:
			if !ok {
				flushFrame()
				if lastState == "" {
					return "", textOut.String(), errors.New("stream closed before any status frame")
				}
				return lastState, textOut.String(), nil
			}
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(a2aCallStreamMaxIdle)
			switch {
			case line == "":
				flushFrame()
				if lastFinal {
					return lastState, textOut.String(), nil
				}
			case strings.HasPrefix(line, ":"):
				// Comment / keepalive — ignore.
			case strings.HasPrefix(line, "event:"):
				currentEvent = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(strings.TrimSpace(line[len("data:"):]))
			}
		case err := <-scanErr:
			// Drain any remaining scanCh lines so the partial
			// last frame still gets flushed before we decide.
			for line := range scanCh {
				if line == "" {
					flushFrame()
				} else if strings.HasPrefix(line, "event:") {
					currentEvent = strings.TrimSpace(line[len("event:"):])
				} else if strings.HasPrefix(line, "data:") {
					if dataBuf.Len() > 0 {
						dataBuf.WriteByte('\n')
					}
					dataBuf.WriteString(strings.TrimSpace(line[len("data:"):]))
				}
			}
			flushFrame()
			if err != nil {
				return lastState, textOut.String(), fmt.Errorf("stream scan: %w", err)
			}
			if lastState == "" {
				return "", textOut.String(), errors.New("stream closed before any status frame")
			}
			return lastState, textOut.String(), nil
		}
	}
}

// a2aTextPart mirrors the message-part shape on the server-side
// submit handler. Local definition; no cross-package import.
type a2aTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// truncateForLog clips arbitrary partner bytes for log + error
// messages so we don't leak megabyte payloads into journald.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
