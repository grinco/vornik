package agentbench

import "testing"

// The endpoint's /v1/models does not carry max_model_len on this vLLM build,
// so the window is discovered by asking the server to do something impossible
// and reading the limit out of its refusal. Measured 2026-08-16 against
// Qwen/Qwen3.8-27B-FP8; this is the verbatim body.
func TestParseMaxModelLen(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{
			name: "vllm refusal, verbatim",
			body: `{"error":{"message":"max_tokens=99000000 cannot be greater than max_model_len=max_total_tokens=32768. Please request fewer output tokens. (parameter=max_tokens, value=99000000)","type":"BadRequestError","param":"max_tokens","code":400}}`,
			want: 32768,
			ok:   true,
		},
		{
			// The operator can raise this deployment; the parser must not be
			// tuned to one value or one order of magnitude.
			name: "a reconfigured, much larger window",
			body: `{"error":{"message":"max_tokens=99000000 cannot be greater than max_model_len=max_total_tokens=1010000."}}`,
			want: 1010000,
			ok:   true,
		},
		{
			// Some builds phrase it with only max_model_len.
			name: "max_model_len alone",
			body: `{"error":{"message":"This model's maximum context length is max_model_len=131072 tokens."}}`,
			want: 131072,
			ok:   true,
		},
		{
			name: "unrelated error yields nothing",
			body: `{"error":{"message":"Unauthorized"}}`,
			ok:   false,
		},
		{
			name: "empty body yields nothing",
			body: "",
			ok:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMaxModelLen(tc.body)
			if ok != tc.ok {
				t.Fatalf("ok: got %v want %v (body=%q)", ok, tc.ok, tc.body)
			}
			if ok && got != tc.want {
				t.Errorf("window: got %d want %d", got, tc.want)
			}
		})
	}
}

// Configured ABOVE discovered is the dangerous direction: the agent budgets a
// conversation the model cannot hold and overflows mid-step. 14 of the 73
// failures in the 2026-08-16 long-horizon arm were exactly this — the model
// inherited context_size 100000 against a real 32768.
func TestCheckConfiguredWindow_OverEstimateIsFatal(t *testing.T) {
	v := CheckConfiguredWindow("Qwen/Qwen3.8-27B-FP8", 100000, 32768)
	if v.Verdict != WindowOverEstimate {
		t.Fatalf("verdict: got %v want WindowOverEstimate", v.Verdict)
	}
	if !v.Fatal {
		t.Error("an over-estimate must abort the run: it overflows mid-step")
	}
}

// Configured well BELOW discovered wastes capability but is safe, and may be
// deliberate — an operator constraining the arm. Warn, never abort.
func TestCheckConfiguredWindow_UnderEstimateWarnsOnly(t *testing.T) {
	v := CheckConfiguredWindow("Qwen/Qwen3.8-27B-FP8", 32768, 1000000)
	if v.Verdict != WindowUnderEstimate {
		t.Fatalf("verdict: got %v want WindowUnderEstimate", v.Verdict)
	}
	if v.Fatal {
		t.Error("an under-estimate must not abort — it may be a deliberate constraint")
	}
}

// Equal, or within the tolerance band, is simply fine.
func TestCheckConfiguredWindow_MatchIsClean(t *testing.T) {
	v := CheckConfiguredWindow("m", 32768, 32768)
	if v.Verdict != WindowOK || v.Fatal {
		t.Fatalf("exact match must be clean, got %v fatal=%v", v.Verdict, v.Fatal)
	}
}

// A model with NO configured entry inherits the global, which is the silent
// failure this whole check exists to surface. It must be reported, not passed.
func TestCheckConfiguredWindow_ZeroConfiguredIsReported(t *testing.T) {
	v := CheckConfiguredWindow("m", 0, 32768)
	if v.Verdict != WindowUnconfigured {
		t.Fatalf("verdict: got %v want WindowUnconfigured", v.Verdict)
	}
	if !v.Fatal {
		t.Error("an unconfigured model silently inherits the global — that is the 2026-07-12 trap")
	}
}

// Discovery failing is not the same as discovering a problem. A probe that
// cannot reach the server must not be reported as a window mismatch.
func TestCheckConfiguredWindow_NoDiscoveryIsInconclusive(t *testing.T) {
	v := CheckConfiguredWindow("m", 32768, 0)
	if v.Verdict != WindowUndiscovered || v.Fatal {
		t.Fatalf("undiscovered must be inconclusive and non-fatal, got %v fatal=%v", v.Verdict, v.Fatal)
	}
}
