package agentbench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Model context-window discovery.
//
// WHY THIS EXISTS. `context_size` shapes every agent's compaction behaviour:
// entrypoint.sh budgets the conversation at
// (context - max_tokens - 2048) * 3 bytes/token * 80%. A model with no
// `agent_llm.model_limits` entry silently inherits the daemon-wide global, and
// nothing anywhere reports the substitution.
//
// That inheritance caused the 2026-07-12 context-overflow incident and then
// caused it again in the 2026-08-16 long-horizon arm, where
// Qwen/Qwen3.8-27B-FP8 inherited 100000 against a real 32768 — a 3.05x
// over-estimate producing 14 of the arm's 73 failures.
//
// WHY IT IS DISCOVERY RATHER THAN A CONSTANT. The window is operator-
// configurable: this vLLM deployment can be raised to 500K or 1M, which
// Qwen3.8 supports. Pinning today's 32768 would be wrong the moment the server
// changes, and wrong in the more insidious direction — the agent would compact
// at 32K against a 1M window and discard context it was entitled to use.
// Under-estimating wastes capability as surely as over-estimating overflows.
//
// WHY THE RESULT IS PROVENANCE, NOT JUST A CHECK. Two arms served by a 32K
// server and a 1M server are not comparable even under byte-identical config.
// That is the three-artifact problem HarnessBuild/DaemonBuild were added to
// close: an axis that moves the numbers while nothing declares the arm changed.

// reMaxModelLen extracts the window from an OpenAI-compatible server's
// refusal. vLLM phrases it as
//
//	max_tokens=99000000 cannot be greater than max_model_len=max_total_tokens=32768
//
// and some builds emit only the max_model_len clause. Anchoring on
// `max_model_len=` and taking the LAST `=`-separated integer handles both
// without being tuned to one value or one order of magnitude.
var reMaxModelLen = regexp.MustCompile(`max_model_len=(?:max_total_tokens=)?(\d+)`)

// parseMaxModelLen pulls the server-reported window out of an error body.
// Returns ok=false when the body says nothing about the window, which is
// distinct from the server reporting a window of zero.
func parseMaxModelLen(body string) (int, bool) {
	m := reMaxModelLen.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// DiscoverModelWindow asks an OpenAI-compatible endpoint for its context
// window by requesting an impossible number of output tokens and reading the
// limit out of the refusal.
//
// This is deliberate rather than a fallback: /v1/models does not carry
// max_model_len on every build, and a probe that the server ANSWERS is worth
// more than a field that may be absent. It costs one 400 response and no
// tokens — the request is refused before any generation happens.
//
// Returns 0 with a nil error when the endpoint answers but says nothing about
// a window: that is "undiscovered", not "zero", and CheckConfiguredWindow
// treats it as inconclusive rather than as a mismatch.
func DiscoverModelWindow(ctx context.Context, endpoint, apiKey, model string) (int, error) {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":99000000,"messages":[{"role":"user","content":"x"}]}`, model)

	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		endpoint+"/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, fmt.Errorf("model-window probe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("model-window probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap the read: a misconfigured endpoint could stream something large,
	// and the limit clause is always near the front.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, fmt.Errorf("model-window probe: read body: %w", err)
	}

	n, ok := parseMaxModelLen(string(raw))
	if !ok {
		return 0, nil
	}
	return n, nil
}

// WindowVerdictKind labels the outcome of comparing configured against
// discovered.
type WindowVerdictKind string

const (
	// WindowOK — configured matches what the server reports.
	WindowOK WindowVerdictKind = "ok"
	// WindowOverEstimate — configured exceeds the server's window. The agent
	// budgets a conversation the model cannot hold and overflows mid-step.
	WindowOverEstimate WindowVerdictKind = "over_estimate"
	// WindowUnderEstimate — configured is well below the server's window. Safe,
	// but the agent compacts earlier than it needs to and discards usable
	// context.
	WindowUnderEstimate WindowVerdictKind = "under_estimate"
	// WindowUnconfigured — no model_limits entry, so the model silently
	// inherits the daemon-wide global. The 2026-07-12 trap.
	WindowUnconfigured WindowVerdictKind = "unconfigured"
	// WindowUndiscovered — the probe could not establish a window. Says
	// nothing about the configuration either way.
	WindowUndiscovered WindowVerdictKind = "undiscovered"
)

// underEstimateTolerance is how far below the server's window a configured
// value may sit before it is worth reporting. A little headroom is normal and
// often deliberate; a third of the window is not.
const underEstimateTolerance = 0.75

// WindowVerdict is one model's configured-vs-observed result.
type WindowVerdict struct {
	Model      string
	Configured int
	Discovered int
	Verdict    WindowVerdictKind
	// Fatal is true when the run must not proceed. Only the two directions
	// that silently corrupt a run set it: an over-estimate (overflow) and an
	// unconfigured model (inherits the global, invisibly).
	Fatal   bool
	Message string
}

// CheckConfiguredWindow compares a model's configured context size against the
// window its server actually reports.
//
// Asymmetric by design. An over-estimate overflows mid-step and is fatal. An
// under-estimate merely wastes capability and may be a deliberate constraint
// on the arm, so it warns. An unconfigured model is fatal despite looking
// harmless: inheriting the global is exactly the silent substitution that has
// now caused two incidents, and it cannot be noticed at config-read time.
func CheckConfiguredWindow(model string, configured, discovered int) WindowVerdict {
	v := WindowVerdict{Model: model, Configured: configured, Discovered: discovered}

	switch {
	case discovered <= 0:
		v.Verdict = WindowUndiscovered
		v.Message = fmt.Sprintf("model %q: context window could not be discovered from the endpoint; "+
			"configured value %d is unverified", model, configured)
	case configured <= 0:
		v.Verdict, v.Fatal = WindowUnconfigured, true
		v.Message = fmt.Sprintf("model %q has no agent_llm.model_limits entry and silently inherits the "+
			"daemon-wide global; the endpoint reports %d. An inherited window is invisible at config "+
			"read and caused the 2026-07-12 overflow incident — set it explicitly", model, discovered)
	case configured > discovered:
		v.Verdict, v.Fatal = WindowOverEstimate, true
		v.Message = fmt.Sprintf("model %q is configured for a %d-token window but the endpoint serves "+
			"%d. The agent budgets a conversation the model cannot hold and overflows mid-step",
			model, configured, discovered)
	case float64(configured) < float64(discovered)*underEstimateTolerance:
		v.Verdict = WindowUnderEstimate
		v.Message = fmt.Sprintf("model %q is configured for %d tokens but the endpoint serves %d; "+
			"the agent will compact earlier than necessary and discard usable context. Not fatal — "+
			"this may be a deliberate constraint", model, configured, discovered)
	default:
		v.Verdict = WindowOK
		v.Message = fmt.Sprintf("model %q: configured %d, endpoint serves %d", model, configured, discovered)
	}
	return v
}
