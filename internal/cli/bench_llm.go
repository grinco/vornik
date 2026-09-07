package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// preflightBenchLLM proves the answer and judge models are actually servable by
// the resolved endpoint, before the run spends anything.
//
// WHY THIS IS A REFUSAL AND NOT A NOTE. --answer-model / --judge-model name a
// MODEL; the endpoint comes from VORNIK_BENCH_LLM_URL, falling back to
// OLLAMA_HOST and then to a local default. Nothing correlates the two, so naming
// a model the default endpoint cannot serve is a configuration error that
// produces a FULL-LENGTH run in which every item errors:
//
//	answer: answer generation: chat completion: http 404 from http://127.0.0.1:11434/v1
//
// Measured 2026-08-21: 120/120 items errored that way, ~20 minutes of ingest and
// recall wasted, and the failure surfaced only in the final scoreboard —
// retrieval had scored perfectly, so the run read as half-successful rather than
// misconfigured. Same class as the agent bench's --smoke and its model-window
// probe: prove the pipeline before spending on the pass.
//
// The refusal names BOTH values. Either alone leaves the operator guessing which
// half is wrong, and the usual cause is a model named without its endpoint.
//
// BOTH models are probed. A judged run uses the judge exactly as it uses the
// answerer, and a wrong judge model wastes the same pass. When they are the same
// model it is one call — the second would prove nothing.
//
// Nothing at all runs under --tier2-only, which constructs no client by design:
// a gate that required a judge could not run on a fork PR, so the flag must
// remove the dependency, not merely the traffic.
func preflightBenchLLM(ctx context.Context, progress io.Writer) error {
	if benchTier2Only {
		return nil
	}
	llm, err := newBenchLLM()
	if err != nil {
		return err
	}

	models := []string{llm.model}
	if benchJudgeModel != "" && benchJudgeModel != llm.model {
		models = append(models, benchJudgeModel)
	}
	for _, model := range models {
		// Say what is being waited on. This is the first call of the run, so it
		// pays any cold model load — up to the client's 10-minute ceiling — and
		// silence for that long reads as a hang. The ceiling is deliberately NOT
		// shortened here: turning a slow-but-working endpoint into a refusal
		// would block legitimate runs to catch a misconfiguration that the
		// error below already names.
		_, _ = fmt.Fprintf(progress, "  ..  preflight: asking %s for %s\n", llm.baseURL, model)

		// Short and answerable by anything: this proves the endpoint serves the
		// model, not that the model is any good.
		if _, err := llm.withModel(model).Complete(ctx, "Reply with the word: ok"); err != nil {
			// The remedy goes to the progress stream, not into the error string:
			// it is several lines of guidance, and an error value is one line.
			_, _ = fmt.Fprint(progress,
				"  !!  A model is named by --answer-model / --judge-model; the ENDPOINT comes\n"+
					"      from VORNIK_BENCH_LLM_URL (then OLLAMA_HOST, then\n"+
					"      http://127.0.0.1:11434/v1). Nothing correlates the two, so a model\n"+
					"      named without its endpoint runs the whole pass and errors on every\n"+
					"      item. Set VORNIK_BENCH_LLM_URL, or name a model this endpoint serves.\n")
			return fmt.Errorf("preflight: the endpoint %s cannot serve model %q: %w",
				llm.baseURL, model, err)
		}
	}
	_, _ = fmt.Fprintf(progress, "  ok  preflight: %s serves %s\n", llm.baseURL, strings.Join(models, ", "))
	return nil
}
