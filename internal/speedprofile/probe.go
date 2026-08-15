package speedprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// ProbeResult is one model's speed measured under controlled conditions.
//
// A DIFFERENT quantity from a fitted Profile, and the two must never be
// averaged. The fit measures decode under real load, with whatever queueing and
// contention the deployment actually has; the probe measures it with none of
// that. The GAP between them is the interesting number — it is the deployment's
// contention, and averaging would destroy exactly the signal worth having.
type ProbeResult struct {
	Model   string
	Samples int
	// MedianTokensPerSec is MARGINAL decode: the slope between a short and a
	// long generation, so per-request fixed cost (prefill, network round trip,
	// scheduling) cancels out.
	//
	// Measuring total elapsed over tokens instead folds that fixed cost into
	// the rate and UNDERSTATES the model. On the first real run it reported
	// 174 tok/s against a fitted 210 — "idle slower than loaded", which is
	// impossible, and was purely fixed cost being divided into the rate. The
	// fitted coefficient is a slope, so the probe must be one too or the two
	// are not comparable at all.
	MedianTokensPerSec float64
	MinTokensPerSec    float64
	MaxTokensPerSec    float64
	// FixedMS is the per-request overhead the slope removes, reported because
	// it is worth seeing rather than merely cancelled.
	FixedMS float64
}

// ProbeOptions configures a probe run.
type ProbeOptions struct {
	Endpoint string // OpenAI-compatible base, e.g. http://host:8000/v1
	APIKey   string
	Model    string
	// Samples is how many measured calls to make. The first call is ALWAYS
	// discarded on top of these: a cold model pays load and cache costs that no
	// steady-state step will ever pay again, and including it would understate
	// the host by a wide margin.
	Samples int
	// ShortTokens and LongTokens are the two output lengths whose difference
	// gives the slope. They must differ enough that the gap dominates timing
	// jitter.
	ShortTokens int
	LongTokens  int
	Timeout     time.Duration
}

// Probe measures raw decode speed by asking for a known amount of output.
//
// It answers what a fit cannot: a cold deployment with no history, whether a
// given box is fast enough before committing work to it, and whether a drop in
// the fitted rate is the model or the environment.
func Probe(ctx context.Context, hc *http.Client, opts ProbeOptions) (ProbeResult, error) {
	if opts.Samples < 1 {
		opts.Samples = 3
	}
	if opts.ShortTokens < 1 {
		opts.ShortTokens = 32
	}
	if opts.LongTokens < 1 {
		opts.LongTokens = 512
	}
	if opts.LongTokens <= opts.ShortTokens {
		return ProbeResult{}, fmt.Errorf("probe %q: long output (%d) must exceed short (%d) "+
			"or the slope is undefined", opts.Model, opts.LongTokens, opts.ShortTokens)
	}
	if opts.Timeout == 0 {
		opts.Timeout = 120 * time.Second
	}

	var rates, fixed []float64
	// +1: the discarded warm-up.
	for i := 0; i < opts.Samples+1; i++ {
		shortTok, shortSec, err := probeOnce(ctx, hc, opts, opts.ShortTokens)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("probe %q sample %d (short): %w", opts.Model, i, err)
		}
		longTok, longSec, err := probeOnce(ctx, hc, opts, opts.LongTokens)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("probe %q sample %d (long): %w", opts.Model, i, err)
		}
		if i == 0 {
			continue
		}
		dTok := float64(longTok - shortTok)
		dSec := longSec - shortSec
		if dTok <= 0 || dSec <= 0 {
			// The long call did not actually generate more, or did it no slower:
			// the slope is meaningless rather than merely noisy.
			continue
		}
		rates = append(rates, dTok/dSec)
		fixed = append(fixed, (shortSec-float64(shortTok)*dSec/dTok)*1000)
	}
	if len(rates) == 0 {
		return ProbeResult{}, fmt.Errorf("probe %q produced no usable slope: the long "+
			"generation never exceeded the short one in both tokens and time", opts.Model)
	}

	sort.Float64s(rates)
	sort.Float64s(fixed)
	return ProbeResult{
		FixedMS: fixed[len(fixed)/2],
		Model:   opts.Model,
		Samples: len(rates),
		// Median, not mean: one scheduling hiccup on a shared host would drag a
		// mean and quietly understate the hardware.
		MedianTokensPerSec: rates[len(rates)/2],
		MinTokensPerSec:    rates[0],
		MaxTokensPerSec:    rates[len(rates)-1],
	}, nil
}

// probeOnce returns the tokens generated and the seconds it took.
func probeOnce(ctx context.Context, hc *http.Client, opts ProbeOptions, maxTokens int) (int, float64, error) {
	body, err := json.Marshal(map[string]any{
		"model":      opts.Model,
		"max_tokens": maxTokens,
		// Deterministic and long: the prompt asks for continuous output so the
		// model does not stop early and make the sample shorter than requested.
		"temperature": 0,
		"messages": []map[string]string{{
			"role": "user",
			"content": "Count upward from one, writing each number as a word on its own line, " +
				"and keep going without stopping or commenting.",
		}},
	})
	if err != nil {
		return 0, 0, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		opts.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	}

	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}

	var out struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, fmt.Errorf("decode response: %w", err)
	}
	elapsed := time.Since(start).Seconds()
	if out.Usage.CompletionTokens == 0 {
		return 0, 0, fmt.Errorf("endpoint reported zero completion tokens; cannot measure a rate")
	}
	if elapsed <= 0 {
		return 0, 0, fmt.Errorf("non-positive elapsed time")
	}
	return out.Usage.CompletionTokens, elapsed, nil
}

// ContentionRatio compares a probe against a fit. Above ~1 the deployment is
// slower under load than the hardware itself is — queueing, concurrency
// pressure, or a busy host — and that is a scheduling story, not a model one.
func ContentionRatio(probe ProbeResult, fitted Profile) float64 {
	if fitted.DecodeTokensPerSec() <= 0 || probe.MedianTokensPerSec <= 0 {
		return 0
	}
	return probe.MedianTokensPerSec / fitted.DecodeTokensPerSec()
}
