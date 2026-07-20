package chat

// TDD for the router-level non-swarm model fallback
// (https://docs.vornik.io).
// FallbackProvider retries ONCE on a configured per-model twin when the inner
// provider returns MODEL_UNHEALTHY, scoped out via WithoutModelFallback(ctx).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// fbFake is a fake inner Provider. results maps a model name to the error it
// returns (nil = success); calls records the sequence of models actually
// invoked (shared across WithModel clones so the test sees the retry hop).
type fbFake struct {
	model   string
	results map[string]error
	calls   *[]string
}

func (f *fbFake) do() (*ChatResponse, error) {
	*f.calls = append(*f.calls, f.model)
	if err := f.results[f.model]; err != nil {
		return nil, err
	}
	return &ChatResponse{Model: "ok:" + f.model}, nil
}
func (f *fbFake) Complete(context.Context, []Message) (*ChatResponse, error) { return f.do() }
func (f *fbFake) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return f.do()
}
func (f *fbFake) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return f.do()
}
func (f *fbFake) Model() string       { return f.model }
func (f *fbFake) SetMetrics(*Metrics) {}
func (f *fbFake) WithModel(m string) Provider {
	return &fbFake{model: m, results: f.results, calls: f.calls}
}

func unhealthy(model string) error {
	return &ModelUnhealthyError{Route: "ollama_cloud", Model: model, State: "open"}
}

// infra429 is a raw exhausted-upstream gateway error (weekly-limit 429) — the
// class that reaches the FallbackProvider BEFORE the breaker trips (low-volume
// model that never hits MinSamples, or a HALF_OPEN probe). IsUpstreamInfraError
// classifies it as "provider down".
func infra429(msg string) error {
	return &GatewayError{Status: 429, Message: msg}
}

func newFB(primaryModel string, results map[string]error, fallbacks map[string]string) (*FallbackProvider, *[]string) {
	calls := &[]string{}
	inner := &fbFake{model: primaryModel, results: results, calls: calls}
	return NewFallbackProvider(inner, fallbacks, zerolog.Nop()), calls
}

func TestFallbackProvider(t *testing.T) {
	ctx := context.Background()
	fallbacks := map[string]string{"glm-5.2": "zai.glm-5"}

	t.Run("unhealthy primary + mapping retries the twin", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": unhealthy("glm-5.2")}, fallbacks)
		resp, err := fp.Complete(ctx, nil)
		if err != nil {
			t.Fatalf("want fallback success, got err %v", err)
		}
		if resp.Model != "ok:zai.glm-5" {
			t.Errorf("want fallback result, got %q", resp.Model)
		}
		if len(*calls) != 2 || (*calls)[0] != "glm-5.2" || (*calls)[1] != "zai.glm-5" {
			t.Errorf("want [glm-5.2 zai.glm-5], got %v", *calls)
		}
	})

	t.Run("raw upstream 429 on primary falls back (breaker not yet open)", func(t *testing.T) {
		// The low-volume / half-open-probe case: a raw exhausted 429 that never
		// tripped the circuit must STILL fail over to the twin.
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": infra429("weekly usage limit")}, fallbacks)
		resp, err := fp.Complete(ctx, nil)
		if err != nil {
			t.Fatalf("raw upstream error must fall back, got err %v", err)
		}
		if resp.Model != "ok:zai.glm-5" {
			t.Errorf("want twin result, got %q", resp.Model)
		}
		if len(*calls) != 2 || (*calls)[0] != "glm-5.2" || (*calls)[1] != "zai.glm-5" {
			t.Errorf("want [glm-5.2 zai.glm-5], got %v", *calls)
		}
	})

	t.Run("raw upstream error respects WithoutModelFallback", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": infra429("down")}, fallbacks)
		_, err := fp.Complete(WithoutModelFallback(ctx), nil)
		if !IsUpstreamInfraError(err) || len(*calls) != 1 {
			t.Errorf("marker must skip raw-infra fallback too: err=%v calls=%v", err, *calls)
		}
	})

	t.Run("raw upstream error is single hop when twin also fails", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{
			"glm-5.2":   infra429("down"),
			"zai.glm-5": infra429("down"),
		}, fallbacks)
		_, err := fp.Complete(ctx, nil)
		if !IsUpstreamInfraError(err) || len(*calls) != 2 {
			t.Errorf("want single hop then surface twin error: err=%v calls=%v", err, *calls)
		}
	})

	t.Run("no mapping returns the error, no retry", func(t *testing.T) {
		fp, calls := newFB("gpt-oss:20b", map[string]error{"gpt-oss:20b": unhealthy("gpt-oss:20b")}, fallbacks)
		_, err := fp.Complete(ctx, nil)
		if !IsModelUnhealthy(err) {
			t.Fatalf("want MODEL_UNHEALTHY, got %v", err)
		}
		if len(*calls) != 1 {
			t.Errorf("want 1 call (no retry), got %v", *calls)
		}
	})

	t.Run("fallback==primary is dropped", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": unhealthy("glm-5.2")}, map[string]string{"glm-5.2": "glm-5.2"})
		_, err := fp.Complete(ctx, nil)
		if !IsModelUnhealthy(err) || len(*calls) != 1 {
			t.Errorf("want no retry (fallback==primary), err=%v calls=%v", err, *calls)
		}
	})

	t.Run("fallback also unhealthy surfaces its error, single hop", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{
			"glm-5.2":   unhealthy("glm-5.2"),
			"zai.glm-5": unhealthy("zai.glm-5"),
		}, fallbacks)
		_, err := fp.Complete(ctx, nil)
		if !IsModelUnhealthy(err) {
			t.Fatalf("want MODEL_UNHEALTHY from fallback, got %v", err)
		}
		if len(*calls) != 2 {
			t.Errorf("want exactly 2 calls (single hop), got %v", *calls)
		}
	})

	t.Run("WithoutModelFallback marker skips the fallback", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": unhealthy("glm-5.2")}, fallbacks)
		_, err := fp.Complete(WithoutModelFallback(ctx), nil)
		if !IsModelUnhealthy(err) || len(*calls) != 1 {
			t.Errorf("marker must skip fallback: err=%v calls=%v", err, *calls)
		}
	})

	t.Run("success passes through, no retry", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": nil}, fallbacks)
		if _, err := fp.Complete(ctx, nil); err != nil || len(*calls) != 1 {
			t.Errorf("success must pass through: err=%v calls=%v", err, *calls)
		}
	})

	t.Run("non-unhealthy error passes through, no retry", func(t *testing.T) {
		shape := errors.New("bad json")
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": shape}, fallbacks)
		if _, err := fp.Complete(ctx, nil); err != shape || len(*calls) != 1 {
			t.Errorf("shape error must pass through: err=%v calls=%v", err, *calls)
		}
	})

	t.Run("WithModel clone consults the map by the failed model", func(t *testing.T) {
		fp, calls := newFB("router-default", map[string]error{"glm-5.2": unhealthy("glm-5.2")}, fallbacks)
		pinned := fp.WithModel("glm-5.2")
		resp, err := pinned.Complete(ctx, nil)
		if err != nil || resp.Model != "ok:zai.glm-5" {
			t.Fatalf("pinned clone must fall back to twin: err=%v resp=%v", err, resp)
		}
		if len(*calls) != 2 || (*calls)[0] != "glm-5.2" || (*calls)[1] != "zai.glm-5" {
			t.Errorf("want [glm-5.2 zai.glm-5], got %v", *calls)
		}
	})

	t.Run("empty map is inert", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": unhealthy("glm-5.2")}, nil)
		if _, err := fp.Complete(ctx, nil); !IsModelUnhealthy(err) || len(*calls) != 1 {
			t.Errorf("empty map must be inert: err=%v calls=%v", err, *calls)
		}
	})

	t.Run("SetMetrics(nil) is safe and fallback still works", func(t *testing.T) {
		fp, calls := newFB("glm-5.2", map[string]error{"glm-5.2": unhealthy("glm-5.2")}, fallbacks)
		fp.SetMetrics(nil) // must not panic
		if _, err := fp.Complete(ctx, nil); err != nil || len(*calls) != 2 {
			t.Errorf("nil metrics must be safe + still fall back: err=%v calls=%v", err, *calls)
		}
	})
}

// routingFake models the router's prefix routing: the router (route=="") selects
// a route by the model prefix on WithModel; a DESCENDED leaf keeps its fixed
// route on WithModel (modeling "descent loses re-routing"). ollama_cloud is
// "down" (returns MODEL_UNHEALTHY); bedrock succeeds. This is what exposes the
// bug where the fallback retried on the descended inner instead of the root.
type routingFake struct {
	route string
	model string
	raw   bool // ollama_cloud returns a raw 429 instead of MODEL_UNHEALTHY
	calls *[]string
}

func routeOf(model string) string {
	if strings.HasPrefix(model, "openai.") {
		return "bedrock"
	}
	return "ollama_cloud"
}
func (r *routingFake) do() (*ChatResponse, error) {
	*r.calls = append(*r.calls, r.route+"/"+r.model)
	if r.route == "ollama_cloud" {
		if r.raw {
			return nil, &GatewayError{Status: 429, Message: "weekly usage limit"}
		}
		return nil, &ModelUnhealthyError{Route: r.route, Model: r.model, State: "open"}
	}
	return &ChatResponse{Model: "ok:" + r.route + "/" + r.model}, nil
}
func (r *routingFake) Complete(context.Context, []Message) (*ChatResponse, error) { return r.do() }
func (r *routingFake) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return r.do()
}
func (r *routingFake) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return r.do()
}
func (r *routingFake) Model() string       { return r.model }
func (r *routingFake) SetMetrics(*Metrics) {}
func (r *routingFake) WithModel(m string) Provider {
	if r.route == "" { // router: route by prefix
		return &routingFake{route: routeOf(m), model: m, raw: r.raw, calls: r.calls}
	}
	return &routingFake{route: r.route, model: m, raw: r.raw, calls: r.calls} // descended leaf keeps its route
}

// TestFallbackProvider_ReRoutesTwinThroughRoot pins the fix: when a caller does
// WithModel(primary) THEN Complete (narrator/titler/etc.), the fallback must
// re-route the twin by ITS prefix through the ROOT router (→ bedrock), not pin
// it onto the primary's already-descended route (→ ollama_cloud, still down).
func TestFallbackProvider_ReRoutesTwinThroughRoot(t *testing.T) {
	calls := &[]string{}
	router := &routingFake{route: "", model: "router-default", calls: calls}
	fp := NewFallbackProvider(router, map[string]string{"gpt-oss:20b": "openai.gpt-oss-20b-1:0"}, zerolog.Nop())
	pinned := fp.WithModel("gpt-oss:20b") // descends inner into the ollama_cloud route
	resp, err := pinned.Complete(context.Background(), nil)
	if err != nil {
		t.Fatalf("fallback must succeed by re-routing the twin to bedrock; err=%v calls=%v", err, *calls)
	}
	if resp.Model != "ok:bedrock/openai.gpt-oss-20b-1:0" {
		t.Errorf("twin must land on bedrock; got %q calls=%v", resp.Model, *calls)
	}
	if len(*calls) != 2 || (*calls)[1] != "bedrock/openai.gpt-oss-20b-1:0" {
		t.Errorf("fallback must re-route via root to bedrock (not stay on ollama_cloud); calls=%v", *calls)
	}
}

// TestFallbackProvider_ReRoutesTwinThroughRoot_RawUpstreamError is the same
// root-reroute guarantee for the RAW-upstream trigger: a pinned caller whose
// primary route returns a raw 429 (circuit never tripped) must still fail the
// twin over to bedrock through the preserved root — not re-pin it onto the
// dead ollama_cloud route. This is the low-volume/half-open path the
// 2026-07-18 weekly-limit outage exposed.
func TestFallbackProvider_ReRoutesTwinThroughRoot_RawUpstreamError(t *testing.T) {
	calls := &[]string{}
	router := &routingFake{route: "", model: "router-default", raw: true, calls: calls}
	fp := NewFallbackProvider(router, map[string]string{"gpt-oss:20b": "openai.gpt-oss-20b-1:0"}, zerolog.Nop())
	pinned := fp.WithModel("gpt-oss:20b") // descends inner into the ollama_cloud route
	resp, err := pinned.Complete(context.Background(), nil)
	if err != nil {
		t.Fatalf("raw-upstream fallback must re-route the twin to bedrock; err=%v calls=%v", err, *calls)
	}
	if resp.Model != "ok:bedrock/openai.gpt-oss-20b-1:0" {
		t.Errorf("twin must land on bedrock; got %q calls=%v", resp.Model, *calls)
	}
	if len(*calls) != 2 || (*calls)[1] != "bedrock/openai.gpt-oss-20b-1:0" {
		t.Errorf("raw-upstream fallback must re-route via root to bedrock; calls=%v", *calls)
	}
}
