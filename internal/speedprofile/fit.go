// Package speedprofile separates how fast a MODEL generates from how long its
// TOOLS take, using measurements Vornik already records.
//
// Why this exists. Every step timeout in Vornik is absolute wall-clock,
// calibrated on whatever hardware the defaults were chosen on. Self-hosted
// deployments vary by three orders of magnitude in inference speed, so a budget
// that is generous on one host is impossible on another. Sizing a budget needs a
// number for "how fast is this model here", and the obvious one — completion
// tokens over step duration — is wrong: it folds container start and tool
// execution into the model's rate, so a deployment that adds one slow MCP tool
// would silently grant every step using that model more time.
//
// Regression separates them. Step duration is modelled as
//
//	duration_ms = fixed + perCompletionToken*completionTokens + perToolCall*toolCalls
//
// Fitted on real steps this gives the model's decode rate AND the per-tool-call
// cost as independent terms, from execution_step_outcomes.duration_ms /
// .tool_calls_used and task_llm_usage's token counts — all of which exist today.
//
// Prompt tokens are deliberately NOT a term. They fitted slightly negative on
// the first real dataset (plausibly prompt caching, plausibly collinearity with
// completion length), and a term that cannot be estimated is worse than one
// folded honestly into the fixed cost. Prefill therefore lives in `fixed` until
// a dataset with genuinely varied prompt lengths says otherwise.
package speedprofile

import (
	"fmt"
	"math"
)

// Sample is one step's measurements.
type Sample struct {
	CompletionTokens int
	ToolCalls        int
	DurationMS       int64
}

// Profile is a fitted model of one (provider, model) pair's timing.
type Profile struct {
	Model   string
	Samples int

	// FixedMS is per-step cost independent of work done: container start,
	// scheduling, and — until it can be estimated — prompt prefill.
	FixedMS float64
	// MSPerCompletionToken is the model's decode cost. This is the term a
	// timeout should scale on.
	MSPerCompletionToken float64
	// MSPerToolCall is tool cost, held separate SO THAT it cannot inflate the
	// model's rate. A rising value here is a tool problem and must never buy
	// the model more time.
	//
	// FLEET-LEVEL DIAGNOSTIC ONLY. It is averaged across every role using this
	// model, and roles differ enormously in tool intensity — a researcher may
	// average 15 calls a step where a coder averages 2. Using this value to
	// predict one role's step duration would be wrong in both directions at
	// once. Per-role prediction needs a per-role fit (review-20260815-c93b).
	MSPerToolCall float64

	// DecodeStdErrMS is the standard error of MSPerCompletionToken. Reported
	// because collinearity between token counts and tool calls inflates the
	// VARIANCE of this coefficient without biasing it: the point estimate stays
	// honest while the confidence interval widens, and a wide one must not
	// quietly size a timeout.
	DecodeStdErrMS float64
}

// DecodeUncertaintyRatio is the standard error as a fraction of the estimate.
// Above MaxDecodeUncertainty the rate is too loose to scale on.
func (p Profile) DecodeUncertaintyRatio() float64 {
	if p.MSPerCompletionToken <= 0 {
		return math.Inf(1)
	}
	return p.DecodeStdErrMS / p.MSPerCompletionToken
}

// DecodeTokensPerSec is the figure a timeout scales on.
func (p Profile) DecodeTokensPerSec() float64 {
	if p.MSPerCompletionToken <= 0 {
		return 0
	}
	return 1000 / p.MSPerCompletionToken
}

// PredictMS estimates a step's duration from the shape of its work. Sizing a
// budget this way beats scaling a single scalar: a step that writes 4,000 tokens
// and calls 30 tools is not the same job as one that writes 200 and calls 2.
func (p Profile) PredictMS(completionTokens, toolCalls int) float64 {
	return p.FixedMS +
		p.MSPerCompletionToken*float64(completionTokens) +
		p.MSPerToolCall*float64(toolCalls)
}

// MinSamples is the fewest steps a fit may be trusted from.
//
// Not a statistical threshold so much as a refusal to publish a number nobody
// should act on: three points fit a three-parameter model exactly and tell you
// nothing. The benchmark's own sigma work is the cautionary tale — n=4 produced
// a spread 4.3x too small, in the direction that manufactures confidence.
const MinSamples = 30

// Fit solves the least-squares system, or explains why it will not.
//
// Refuses rather than returning a shaky answer. A profile is used to size
// timeouts, so a wrong one is worse than none: an absent profile falls back to
// configured defaults, while a wrong one silently mis-sizes every step.
func Fit(model string, samples []Sample) (Profile, error) {
	if len(samples) < MinSamples {
		return Profile{}, fmt.Errorf("speedprofile %q: %d samples, need %d — an under-determined "+
			"fit sizes timeouts from noise", model, len(samples), MinSamples)
	}

	// Normal equations for [1, completion, toolCalls].
	var sxx [3][3]float64
	var sxy [3]float64
	for _, s := range samples {
		x := [3]float64{1, float64(s.CompletionTokens), float64(s.ToolCalls)}
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				sxx[i][j] += x[i] * x[j]
			}
			sxy[i] += x[i] * float64(s.DurationMS)
		}
	}

	// Collinearity is the failure mode that matters here. If tool calls track
	// completion tokens closely — plausible, since a chatty step both writes
	// more and calls more — the two coefficients are not separately
	// identifiable, and the fit would apportion cost between them arbitrarily.
	// Splitting them is the entire point, so refuse instead.
	if r := correlation(samples); math.Abs(r) > MaxCollinearity {
		return Profile{}, fmt.Errorf("speedprofile %q: completion tokens and tool calls "+
			"correlate at r=%.3f (limit %.2f) — their coefficients are not separately "+
			"identifiable, and separating them is the point of this fit",
			model, r, MaxCollinearity)
	}

	if cv := completionCV(samples); cv < MinCompletionCV {
		return Profile{}, fmt.Errorf("speedprofile %q: completion tokens vary too little "+
			"(CV %.2f, need %.2f) — the samples pin the fixed cost but barely determine the "+
			"slope, so the decode rate would be sized from noise", model, cv, MinCompletionCV)
	}

	coef, ok := solve3(sxx, sxy)
	if !ok {
		return Profile{}, fmt.Errorf("speedprofile %q: singular system — the samples do not "+
			"vary enough in shape to estimate three coefficients", model)
	}

	p := Profile{
		Model:                model,
		Samples:              len(samples),
		FixedMS:              coef[0],
		MSPerCompletionToken: coef[1],
		MSPerToolCall:        coef[2],
	}

	p.DecodeStdErrMS = decodeStdErr(samples, coef, sxx)
	if p.MSPerCompletionToken > 0 && p.DecodeUncertaintyRatio() > MaxDecodeUncertainty {
		return Profile{}, fmt.Errorf("speedprofile %q: decode rate is %.0f tok/s but its "+
			"standard error is %.0f%% of the estimate (limit %.0f%%) — that is a range, not a "+
			"number, and a timeout scaled on it would swing between runs",
			model, p.DecodeTokensPerSec(), p.DecodeUncertaintyRatio()*100, MaxDecodeUncertainty*100)
	}

	// A negative decode cost is not a slow model, it is a bad fit. Timeouts
	// must never be sized from it.
	if p.MSPerCompletionToken <= 0 {
		return Profile{}, fmt.Errorf("speedprofile %q: fitted decode cost %.3f ms/token is not "+
			"positive — the fit does not describe generation and must not size a timeout",
			model, p.MSPerCompletionToken)
	}
	return p, nil
}

// MaxCollinearity is how closely completion tokens and tool calls may track each
// other before their coefficients stop being separable.
const MaxCollinearity = 0.95

// MaxDecodeUncertainty is the widest relative standard error a decode rate may
// carry and still size a timeout. Beyond it the estimate is a range, not a
// number, and a scaling factor built on it would swing between runs.
const MaxDecodeUncertainty = 0.30

// MinCompletionCV is the smallest coefficient of variation the completion-token
// column may have. Sample COUNT is not enough: fifty steps that all generate
// about the same number of tokens pin the fixed cost and leave the slope barely
// determined, which is how a confident-looking fit ends up sized from noise.
const MinCompletionCV = 0.30

// correlation returns Pearson's r between completion tokens and tool calls.
func correlation(samples []Sample) float64 {
	n := float64(len(samples))
	var sx, sy float64
	for _, s := range samples {
		sx += float64(s.CompletionTokens)
		sy += float64(s.ToolCalls)
	}
	mx, my := sx/n, sy/n
	var num, dx, dy float64
	for _, s := range samples {
		a := float64(s.CompletionTokens) - mx
		b := float64(s.ToolCalls) - my
		num += a * b
		dx += a * a
		dy += b * b
	}
	if dx == 0 || dy == 0 {
		// One of them never varies: it cannot be estimated at all, which the
		// singular check below reports more precisely.
		return 0
	}
	return num / math.Sqrt(dx*dy)
}

// solve3 solves a 3x3 system by Gaussian elimination with partial pivoting.
func solve3(a [3][3]float64, b [3]float64) ([3]float64, bool) {
	for i := 0; i < 3; i++ {
		p := i
		for r := i + 1; r < 3; r++ {
			if math.Abs(a[r][i]) > math.Abs(a[p][i]) {
				p = r
			}
		}
		if math.Abs(a[p][i]) < 1e-9 {
			return [3]float64{}, false
		}
		a[i], a[p] = a[p], a[i]
		b[i], b[p] = b[p], b[i]
		for r := i + 1; r < 3; r++ {
			f := a[r][i] / a[i][i]
			for c := i; c < 3; c++ {
				a[r][c] -= f * a[i][c]
			}
			b[r] -= f * b[i]
		}
	}
	var out [3]float64
	for i := 2; i >= 0; i-- {
		s := b[i]
		for j := i + 1; j < 3; j++ {
			s -= a[i][j] * out[j]
		}
		out[i] = s / a[i][i]
	}
	return out, true
}

// completionCV is the coefficient of variation of the completion-token column.
func completionCV(samples []Sample) float64 {
	n := float64(len(samples))
	var sum float64
	for _, s := range samples {
		sum += float64(s.CompletionTokens)
	}
	mean := sum / n
	if mean == 0 {
		return 0
	}
	var sq float64
	for _, s := range samples {
		d := float64(s.CompletionTokens) - mean
		sq += d * d
	}
	return math.Sqrt(sq/n) / mean
}

// decodeStdErr estimates the standard error of the completion-token coefficient.
//
// Residual variance times the corresponding diagonal of (XᵀX)⁻¹, which is the
// textbook OLS result. Computed by solving XᵀX·e = e₂ rather than inverting the
// whole matrix — the same elimination the fit already uses.
func decodeStdErr(samples []Sample, coef [3]float64, sxx [3][3]float64) float64 {
	n := len(samples)
	if n <= 3 {
		return math.Inf(1)
	}
	var rss float64
	for _, s := range samples {
		pred := coef[0] + coef[1]*float64(s.CompletionTokens) + coef[2]*float64(s.ToolCalls)
		d := float64(s.DurationMS) - pred
		rss += d * d
	}
	sigma2 := rss / float64(n-3)
	unit, ok := solve3(sxx, [3]float64{0, 1, 0})
	if !ok || unit[1] < 0 {
		return math.Inf(1)
	}
	return math.Sqrt(sigma2 * unit[1])
}
