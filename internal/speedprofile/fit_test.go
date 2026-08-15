package speedprofile

import (
	"math"
	"strings"
	"testing"
)

// synth builds samples from known coefficients so a fit can be checked against
// the truth it should recover.
func synth(fixed, perTok, perTool float64, shapes [][2]int) []Sample {
	out := make([]Sample, 0, len(shapes))
	for _, s := range shapes {
		d := fixed + perTok*float64(s[0]) + perTool*float64(s[1])
		out = append(out, Sample{CompletionTokens: s[0], ToolCalls: s[1], DurationMS: int64(d)})
	}
	return out
}

// Varied shapes: token counts and tool counts move independently, which is what
// makes the two costs separable.
func variedShapes(n int) [][2]int {
	out := make([][2]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, [2]int{100 + (i*137)%3000, 1 + (i*7)%40})
	}
	return out
}

// The whole point: a fit must recover the model's decode cost WITHOUT absorbing
// tool time into it. This is the failure the naive tokens/duration metric had.
func TestFit_SeparatesDecodeCostFromToolCost(t *testing.T) {
	const fixed, perTok, perTool = 2400.0, 6.75, 800.0

	p, err := Fit("m", synth(fixed, perTok, perTool, variedShapes(200)))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if math.Abs(p.MSPerCompletionToken-perTok) > 0.01 {
		t.Errorf("decode cost = %.3f ms/tok, want %.3f", p.MSPerCompletionToken, perTok)
	}
	if math.Abs(p.MSPerToolCall-perTool) > 1 {
		t.Errorf("tool cost = %.1f ms/call, want %.1f", p.MSPerToolCall, perTool)
	}
	if math.Abs(p.FixedMS-fixed) > 1 {
		t.Errorf("fixed = %.1f ms, want %.1f", p.FixedMS, fixed)
	}
	if got, want := p.DecodeTokensPerSec(), 1000/perTok; math.Abs(got-want) > 1 {
		t.Errorf("decode = %.1f tok/s, want %.1f", got, want)
	}
}

// A slow tool must move the TOOL coefficient and leave decode alone — the
// central objection to the metric this replaces. A deployment that adds a slow
// MCP tool must not thereby grant every step using that model more time.
func TestFit_ASlowToolDoesNotInflateTheModelsRate(t *testing.T) {
	shapes := variedShapes(200)
	fast, err := Fit("m", synth(2400, 6.75, 800, shapes))
	if err != nil {
		t.Fatalf("fit fast: %v", err)
	}
	// Same model, same generation cost; tools got 10x slower.
	slow, err := Fit("m", synth(2400, 6.75, 8000, shapes))
	if err != nil {
		t.Fatalf("fit slow: %v", err)
	}

	if math.Abs(slow.MSPerCompletionToken-fast.MSPerCompletionToken) > 0.01 {
		t.Errorf("decode moved from %.3f to %.3f because TOOLS got slower — that is the "+
			"conflation this fit exists to remove",
			fast.MSPerCompletionToken, slow.MSPerCompletionToken)
	}
	if slow.MSPerToolCall < 7000 {
		t.Errorf("tool cost = %.0f, want it to absorb the slowdown", slow.MSPerToolCall)
	}
}

// If tool calls track token counts, the two costs cannot be told apart and the
// fit would apportion them arbitrarily. Splitting them is the point, so refuse.
func TestFit_RefusesWhenToolCallsTrackTokens(t *testing.T) {
	shapes := make([][2]int, 0, 200)
	for i := 0; i < 200; i++ {
		tok := 100 + i*10
		shapes = append(shapes, [2]int{tok, tok / 100}) // perfectly collinear
	}

	_, err := Fit("m", synth(2400, 6.75, 800, shapes))
	if err == nil {
		t.Fatal("collinear samples produced a confident fit")
	}
	if !strings.Contains(err.Error(), "identifiable") {
		t.Errorf("refusal does not explain the problem: %v", err)
	}
}

// A profile sizes timeouts, so a wrong one is worse than none: absent falls back
// to configured defaults, wrong silently mis-sizes every step.
func TestFit_RefusesTooFewSamples(t *testing.T) {
	_, err := Fit("m", synth(2400, 6.75, 800, variedShapes(MinSamples-1)))
	if err == nil {
		t.Fatal("fitted three coefficients from fewer samples than the minimum")
	}
	if !strings.Contains(err.Error(), "noise") {
		t.Errorf("refusal does not say why it matters: %v", err)
	}
}

// A negative decode cost is not a slow model, it is a bad fit.
func TestFit_RefusesANonPositiveDecodeCost(t *testing.T) {
	// Duration falls as tokens rise — physically meaningless, and exactly what
	// a confounded dataset can produce.
	s := synth(50000, -3, 800, variedShapes(200))

	if _, err := Fit("m", s); err == nil {
		t.Fatal("a negative decode cost was accepted and would have sized timeouts")
	}
}

// Steps that never vary in shape cannot support three coefficients.
func TestFit_RefusesDegenerateSamples(t *testing.T) {
	shapes := make([][2]int, 0, 60)
	for i := 0; i < 60; i++ {
		shapes = append(shapes, [2]int{500, 5}) // identical every time
	}

	if _, err := Fit("m", synth(2400, 6.75, 800, shapes)); err == nil {
		t.Fatal("identical samples produced a fit")
	}
}

// Sizing from the SHAPE of the work beats scaling one scalar.
func TestProfile_PredictsFromWorkShape(t *testing.T) {
	p := Profile{FixedMS: 2400, MSPerCompletionToken: 6.75, MSPerToolCall: 800}

	small := p.PredictMS(200, 2)
	large := p.PredictMS(4000, 30)

	if small >= large {
		t.Fatalf("a bigger step predicted no longer: %.0f vs %.0f", small, large)
	}
	if want := 2400 + 6.75*4000 + 800*30; math.Abs(large-want) > 1 {
		t.Errorf("prediction = %.0f, want %.0f", large, want)
	}
}

func TestProfile_DecodeRateIsZeroWhenUnfitted(t *testing.T) {
	if got := (Profile{}).DecodeTokensPerSec(); got != 0 {
		t.Errorf("an unfitted profile reported %.1f tok/s", got)
	}
}

// Sample COUNT is not enough. Fifty steps that all generate about the same
// number of tokens pin the fixed cost and barely determine the slope — a
// confident-looking fit sized from noise (review-20260815-c93b).
func TestFit_RefusesWhenCompletionTokensBarelyVary(t *testing.T) {
	shapes := make([][2]int, 0, 200)
	for i := 0; i < 200; i++ {
		shapes = append(shapes, [2]int{1000 + i%3, 1 + (i*7)%40}) // CV ~ 0
	}

	_, err := Fit("m", synth(2400, 6.75, 800, shapes))
	if err == nil {
		t.Fatal("a fit was returned from samples whose token counts barely move")
	}
	if !strings.Contains(err.Error(), "vary too little") {
		t.Errorf("refusal does not name the cause: %v", err)
	}
}

// Collinearity inflates the VARIANCE of the decode coefficient without biasing
// it: the point estimate stays honest while its interval widens. A wide one
// must not quietly size a timeout.
func TestFit_RefusesADecodeRateThatIsReallyARange(t *testing.T) {
	// Same coefficients, but durations carry heavy noise, so the slope is
	// poorly determined even though the sample count is ample.
	shapes := variedShapes(200)
	s := synth(2400, 6.75, 800, shapes)
	// Deterministic pseudo-noise, uncorrelated with the regressors: a simple
	// LCG rather than an alternating sign, which would pattern-match the shape
	// generator and partly cancel in the fit.
	seed := uint64(12345)
	for i := range s {
		seed = seed*6364136223846793005 + 1442695040888963407
		s[i].DurationMS += int64(seed%400000) - 200000
	}

	_, err := Fit("m", s)
	if err == nil {
		t.Fatal("a decode rate with a huge standard error was accepted")
	}
	if !strings.Contains(err.Error(), "range, not a number") {
		t.Errorf("refusal does not explain the uncertainty: %v", err)
	}
}

// A clean fit must report a usable, tight standard error — otherwise the guard
// above would reject everything.
func TestFit_CleanDataReportsTightUncertainty(t *testing.T) {
	p, err := Fit("m", synth(2400, 6.75, 800, variedShapes(200)))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if p.DecodeUncertaintyRatio() > 0.01 {
		t.Errorf("noiseless data reported %.1f%% uncertainty", p.DecodeUncertaintyRatio()*100)
	}
}
