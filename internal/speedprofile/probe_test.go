package speedprofile

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodingEndpoint simulates a server with a FIXED per-request overhead plus a
// per-token decode cost, honouring max_tokens. Both are what the two-point
// probe must tell apart.
func decodingEndpoint(t *testing.T, fixed time.Duration, perToken time.Duration, extra func(call int) time.Duration) *httptest.Server {
	t.Helper()
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)
		d := fixed + perToken*time.Duration(req.MaxTokens)
		if extra != nil {
			d += extra(call)
		}
		call++
		time.Sleep(d)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]int{"completion_tokens": req.MaxTokens},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The property the whole two-point design exists for: a large per-request fixed
// cost must NOT drag the reported decode rate.
//
// Dividing total elapsed by tokens folds it in and understates the model. On the
// first real run that produced 174 tok/s against a fitted 210 — idle slower than
// loaded, which is impossible.
func TestProbe_SlopeCancelsPerRequestOverhead(t *testing.T) {
	// 1ms/token = 1000 tok/s decode, behind a punishing 400ms fixed cost.
	srv := decodingEndpoint(t, 400*time.Millisecond, 500*time.Microsecond, nil)

	got, err := Probe(context.Background(), srv.Client(), ProbeOptions{
		Endpoint: srv.URL, Model: "m", Samples: 3, ShortTokens: 20, LongTokens: 200,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// 0.5ms/token = 2000 tok/s true decode. Naive total/tokens on the long call
	// would read ~400 tok/s; the slope recovers far more of the truth.
	if got.MedianTokensPerSec < 1200 {
		t.Errorf("decode = %.0f tok/s; the fixed cost was folded into the rate rather than "+
			"cancelled by the slope", got.MedianTokensPerSec)
	}
	if got.FixedMS < 200 {
		t.Errorf("fixed = %.0f ms, want it to surface the ~400ms overhead", got.FixedMS)
	}
}

// The first call pays model load and cold-cache costs no steady-state step ever
// pays again.
func TestProbe_DiscardsTheWarmUpCall(t *testing.T) {
	srv := decodingEndpoint(t, 10*time.Millisecond, time.Millisecond, func(call int) time.Duration {
		if call < 2 { // the warm-up pair
			return 300 * time.Millisecond
		}
		return 0
	})

	got, err := Probe(context.Background(), srv.Client(), ProbeOptions{
		Endpoint: srv.URL, Model: "m", Samples: 2, ShortTokens: 20, LongTokens: 200,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got.Samples != 2 {
		t.Errorf("reported %d samples, want 2", got.Samples)
	}
	if got.MedianTokensPerSec < 700 {
		t.Errorf("median %.0f tok/s suggests the warm-up was counted", got.MedianTokensPerSec)
	}
}

// One scheduling hiccup on a shared host must not define the result.
func TestProbe_MedianResistsASingleSlowCall(t *testing.T) {
	srv := decodingEndpoint(t, 10*time.Millisecond, time.Millisecond, func(call int) time.Duration {
		if call == 4 { // one measured long call stalls
			return 500 * time.Millisecond
		}
		return 0
	})

	got, err := Probe(context.Background(), srv.Client(), ProbeOptions{
		Endpoint: srv.URL, Model: "m", Samples: 3, ShortTokens: 20, LongTokens: 200,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got.MedianTokensPerSec < 700 {
		t.Errorf("median %.0f tok/s was dragged by the outlier", got.MedianTokensPerSec)
	}
	if got.MinTokensPerSec >= got.MedianTokensPerSec {
		t.Error("the spread does not reflect the slow call; the hiccup is hidden rather than shown")
	}
}

// A slope needs two genuinely different lengths.
func TestProbe_RefusesEqualLengths(t *testing.T) {
	srv := decodingEndpoint(t, time.Millisecond, time.Millisecond, nil)

	_, err := Probe(context.Background(), srv.Client(), ProbeOptions{
		Endpoint: srv.URL, Model: "m", Samples: 1, ShortTokens: 100, LongTokens: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "slope is undefined") {
		t.Fatalf("want a refusal about the slope, got: %v", err)
	}
}

func TestProbe_SurfacesAnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := Probe(context.Background(), srv.Client(), ProbeOptions{
		Endpoint: srv.URL, Model: "m", Samples: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("want an error naming the status, got: %v", err)
	}
}

// The gap between a probe and a fit is the deployment's contention — the reason
// the two are recorded separately and never averaged.
func TestContentionRatio(t *testing.T) {
	probe := ProbeResult{MedianTokensPerSec: 8000}
	fitted := Profile{MSPerCompletionToken: 1000.0 / 200} // 200 tok/s under load

	if got := ContentionRatio(probe, fitted); got < 39 || got > 41 {
		t.Errorf("ratio = %.1f, want ~40 (8000 idle vs 200 under load)", got)
	}
	if got := ContentionRatio(ProbeResult{}, fitted); got != 0 {
		t.Errorf("a missing probe produced a ratio of %.1f", got)
	}
}
