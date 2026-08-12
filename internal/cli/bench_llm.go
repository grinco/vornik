package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// benchLLM is an OpenAI-compatible chat client for the benchmark harness.
//
// Deliberately a small self-contained client rather than a reuse of the daemon's
// chat stack: the harness must be able to point the answer model and the judge
// model at DIFFERENT endpoints (a local Ollama for answering, a cloud model for
// judging is the whole "judged" profile), and it must run without the daemon's
// provider registry being configured for either.
type benchLLM struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// newBenchLLM builds the client from the environment.
//
// Endpoint resolution, in order: VORNIK_BENCH_LLM_URL, then OLLAMA_HOST (with
// /v1 appended, which is Ollama's OpenAI-compatible surface), then a local
// default. Explicit beats inferred at every step so an operator can always
// override.
func newBenchLLM() (*benchLLM, error) {
	base := strings.TrimSpace(os.Getenv("VORNIK_BENCH_LLM_URL"))
	if base == "" {
		if host := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); host != "" {
			base = strings.TrimRight(host, "/") + "/v1"
		}
	}
	if base == "" {
		base = "http://127.0.0.1:11434/v1"
	}
	if benchAnswerModel == "" {
		return nil, fmt.Errorf("no answer model resolved; pass --profile or --answer-model")
	}
	return &benchLLM{
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  os.Getenv("VORNIK_BENCH_LLM_KEY"),
		model:   benchAnswerModel,
		// Generous: a local model on a cold load can take a while, and a timeout
		// here would be recorded as an error outcome and pollute the degraded rate.
		client: &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

// withModel returns a copy pinned to a different model, so the judge can run on
// its own model without a second client construction.
func (l *benchLLM) withModel(model string) *benchLLM {
	cp := *l
	cp.model = model
	return &cp
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Temperature 0 for reproducibility. It does not make an LLM deterministic —
	// which is exactly why the design refuses to pick a Tier-1 alert threshold
	// before measuring real run-to-run variance — but it removes the avoidable
	// share of the noise.
	Temperature float64 `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete satisfies membench.LLM.
func (l *benchLLM) Complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       l.model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("encode chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if l.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiKey)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat completion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat completion: http %d from %s", resp.StatusCode, l.baseURL)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("chat completion: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		// Distinguished from an empty string: no choices at all is a protocol
		// problem, and returning "" would be scored as a wrong answer.
		return "", fmt.Errorf("chat completion returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}
