package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// fakeChatClient implements chatReachable for unit tests.
type fakeChatClient struct {
	models  []chat.ModelInfo
	listErr error
	pingErr error
}

func (f fakeChatClient) ListModels(context.Context) ([]chat.ModelInfo, error) {
	return f.models, f.listErr
}
func (f fakeChatClient) PingCompletion(context.Context) error { return f.pingErr }

func newFakeFactory(c fakeChatClient) chatClientFactory {
	return func(_, _, _ string) chatReachable { return c }
}

func hasFailure(r ChatValidationResult, name string) bool {
	for _, f := range r.Failures {
		if f.Name == name {
			return true
		}
	}
	return false
}

func TestValidate_AllChecksPass(t *testing.T) {
	v := NewChatValidatorWithFactory(newFakeFactory(fakeChatClient{
		models:  []chat.ModelInfo{{ID: "gpt-4.1"}, {ID: "gpt-4.1-mini"}},
		pingErr: nil,
	}), time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://chat.example", APIKey: "k", Model: "gpt-4.1",
	})
	if !r.EndpointOK || !r.ModelsListed || !r.ModelKnown || !r.PingOK {
		t.Fatalf("expected all checks pass, got %+v", r)
	}
	if len(r.Failures) != 0 {
		t.Fatalf("expected no failures, got %v", r.Failures)
	}
	if len(r.ModelOptions) != 2 {
		t.Fatalf("expected 2 model options, got %d", len(r.ModelOptions))
	}
}

func TestValidate_EndpointUnreachable(t *testing.T) {
	v := NewChatValidatorWithFactory(newFakeFactory(fakeChatClient{
		listErr: errors.New("dial tcp: connection refused"),
	}), time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://nope", APIKey: "k", Model: "m",
	})
	if r.EndpointOK || r.PingOK {
		t.Fatalf("expected endpoint down + ping skipped, got %+v", r)
	}
	if !hasFailure(r, "endpoint_unreachable") {
		t.Fatalf("expected endpoint_unreachable failure, got %v", r.Failures)
	}
	for _, f := range r.Failures {
		if f.Severity != "blocking" {
			t.Errorf("endpoint_unreachable must be blocking, got %q", f.Severity)
		}
	}
}

func TestValidate_KeyRejected(t *testing.T) {
	v := NewChatValidatorWithFactory(newFakeFactory(fakeChatClient{
		listErr: errors.New("401 Unauthorized: invalid api key"),
	}), time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://chat.example", APIKey: "bad", Model: "m",
	})
	if !hasFailure(r, "key_rejected") {
		t.Fatalf("expected key_rejected, got %v", r.Failures)
	}
	if r.PingOK {
		t.Fatal("ping must not pass when key is rejected")
	}
}

func TestValidate_ModelUnknown_PingStillPasses(t *testing.T) {
	v := NewChatValidatorWithFactory(newFakeFactory(fakeChatClient{
		models:  []chat.ModelInfo{{ID: "other-model"}},
		pingErr: nil,
	}), time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://chat.example", APIKey: "k", Model: "not-listed",
	})
	if !r.ModelsListed || r.ModelKnown {
		t.Fatalf("expected listed but model unknown, got %+v", r)
	}
	if !hasFailure(r, "model_unknown") {
		t.Fatalf("expected model_unknown advisory, got %v", r.Failures)
	}
	if !r.PingOK {
		t.Fatal("ping passes → commit must be allowed (PingOK is the gate)")
	}
	for _, f := range r.Failures {
		if f.Name == "model_unknown" && f.Severity != "advisory" {
			t.Errorf("model_unknown must be advisory, got %q", f.Severity)
		}
	}
}

func TestValidate_ModelUnknown_PingFails(t *testing.T) {
	v := NewChatValidatorWithFactory(newFakeFactory(fakeChatClient{
		models:  []chat.ModelInfo{{ID: "other"}},
		pingErr: errors.New("404 model not found"),
	}), time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://chat.example", APIKey: "k", Model: "nope",
	})
	if r.PingOK {
		t.Fatal("ping must fail")
	}
	if !hasFailure(r, "ping_failed") {
		t.Fatalf("expected ping_failed (blocking), got %v", r.Failures)
	}
}

func TestValidate_PingFails_ListOK(t *testing.T) {
	v := NewChatValidatorWithFactory(newFakeFactory(fakeChatClient{
		models:  []chat.ModelInfo{{ID: "gpt-4.1"}},
		pingErr: errors.New("403 model access denied: billing required"),
	}), time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://chat.example", APIKey: "k", Model: "gpt-4.1",
	})
	if !r.ModelKnown {
		t.Fatal("model should be known from listing")
	}
	if r.PingOK {
		t.Fatal("ping must fail (quota/billing)")
	}
	if !hasFailure(r, "ping_failed") {
		t.Fatalf("expected ping_failed, got %v", r.Failures)
	}
	for _, f := range r.Failures {
		if f.Name == "ping_failed" && f.Severity != "blocking" {
			t.Errorf("ping_failed must be blocking, got %q", f.Severity)
		}
	}
}

func TestValidate_EmptyProposal_ShortCircuits(t *testing.T) {
	called := false
	factory := func(_, _, _ string) chatReachable {
		called = true
		return fakeChatClient{}
	}
	v := NewChatValidatorWithFactory(factory, time.Second)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "", APIKey: "", Model: "m",
	})
	if called {
		t.Fatal("validator must not construct a client for an empty proposal")
	}
	if r.PingOK || !hasFailure(r, "endpoint_unreachable") {
		t.Fatalf("expected endpoint_unreachable on empty proposal, got %+v", r)
	}
}

func TestValidate_Timeout(t *testing.T) {
	// A client whose ListModels blocks past the validator timeout.
	blocking := blockingClient{}
	v := NewChatValidatorWithFactory(func(_, _, _ string) chatReachable { return blocking }, 50*time.Millisecond)
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "http://chat.example", APIKey: "k", Model: "m",
	})
	if !hasFailure(r, "timeout") {
		t.Fatalf("expected timeout failure, got %v", r.Failures)
	}
	if r.PingOK {
		t.Fatal("ping must not pass on timeout")
	}
}

// blockingClient never returns from ListModels until the context is cancelled.
type blockingClient struct{}

func (blockingClient) ListModels(ctx context.Context) ([]chat.ModelInfo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingClient) PingCompletion(context.Context) error { return nil }

func TestNewChatValidator_RealFactoryBuildsClient(t *testing.T) {
	// Smoke test: the production factory returns a non-nil reachable
	// without panicking. We do not exercise it against the network here.
	v := NewChatValidator()
	if v.timeout != 10*time.Second {
		t.Errorf("default timeout = %v, want 10s", v.timeout)
	}
	r := v.Validate(context.Background(), ChatConfigProposal{
		Endpoint: "", APIKey: "", Model: "",
	})
	if !strings.Contains(strings.Join(reasonTexts(r), " "), "endpoint") {
		t.Fatalf("empty proposal should short-circuit, got %+v", r)
	}
}

func reasonTexts(r ChatValidationResult) []string {
	out := make([]string, 0, len(r.Failures))
	for _, f := range r.Failures {
		out = append(out, f.Message)
	}
	return out
}

// TestChatConfigProposal_JSONDecodesSnakeCaseKeys guards the JSON contract
// the setup validate handler relies on: encoding/json must populate APIKey
// from the snake_case `api_key` request key. Without explicit json tags,
// case-folding turns APIKey into `apikey`, which does NOT match `api_key`,
// and the key is silently dropped on every validate request.
func TestChatConfigProposal_JSONDecodesSnakeCaseKeys(t *testing.T) {
	raw := `{"endpoint":"http://chat.example","api_key":"sk-real","model":"gpt-4.1"}`
	var p ChatConfigProposal
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Endpoint != "http://chat.example" {
		t.Errorf("Endpoint = %q, want %q", p.Endpoint, "http://chat.example")
	}
	if p.APIKey != "sk-real" {
		t.Errorf("APIKey = %q, want %q (snake_case api_key did not decode)", p.APIKey, "sk-real")
	}
	if p.Model != "gpt-4.1" {
		t.Errorf("Model = %q, want %q", p.Model, "gpt-4.1")
	}
}
