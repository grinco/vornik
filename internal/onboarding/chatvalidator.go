package onboarding

import (
	"context"
	"errors"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
)

// ChatConfigProposal is the chat-config input the validator tests
// against the real endpoint. These are PROPOSED credentials — the
// daemon's loaded config is never consulted.
type ChatConfigProposal struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
}

// CheckFailure is one structured validation finding with remediation
// text the UI renders inline.
type CheckFailure struct {
	Name        string `json:"name"`
	Severity    string `json:"severity"` // "blocking" | "advisory"
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// ChatValidationResult is the setup-guide chat decision payload shared
// by the UI and the validate/commit API endpoints. PingOK is the
// single blocking signal for commit.
type ChatValidationResult struct {
	EndpointOK   bool             `json:"endpoint_ok"`
	ModelsListed bool             `json:"models_listed"`
	ModelKnown   bool             `json:"model_known"`
	PingOK       bool             `json:"ping_ok"`
	ModelOptions []chat.ModelInfo `json:"model_options,omitempty"`
	Failures     []CheckFailure   `json:"failures,omitempty"`
}

// chatReachable is the minimal client surface Validate needs. The
// production factory returns a *chat.Client; tests return a fake.
type chatReachable interface {
	ListModels(ctx context.Context) ([]chat.ModelInfo, error)
	PingCompletion(ctx context.Context) error
}

// chatClientFactory builds a chatReachable from proposed credentials.
type chatClientFactory func(endpoint, apiKey, model string) chatReachable

// ChatValidatorInterface is the seam the API handlers depend on so
// they can be tested with a stub instead of a real validator.
type ChatValidatorInterface interface {
	Validate(ctx context.Context, p ChatConfigProposal) ChatValidationResult
}

// ChatValidator tests proposed chat credentials against the real
// endpoint. It is a pure function of (proposal, factory behavior).
type ChatValidator struct {
	factory chatClientFactory
	timeout time.Duration
}

// NewChatValidator returns a validator that builds real *chat.Client
// instances with a 10s timeout per check.
func NewChatValidator() ChatValidator {
	return ChatValidator{
		factory: func(endpoint, apiKey, model string) chatReachable {
			return chat.NewClient(endpoint, apiKey, model, chat.WithTimeout(10*time.Second))
		},
		timeout: 10 * time.Second,
	}
}

// NewChatValidatorWithFactory is the test constructor.
func NewChatValidatorWithFactory(fn chatClientFactory, timeout time.Duration) ChatValidator {
	return ChatValidator{factory: fn, timeout: timeout}
}

// ListModels returns the models the proposed endpoint + key can see,
// WITHOUT the invocation (ping) check. It powers the setup form's
// "Fetch models" button so the operator can pick a model from a dropdown
// before committing — they shouldn't have to know the exact model ID by
// heart. Returns an error (not a structured result) so the handler maps
// it to a simple inline message; the authoritative pass/fail gate stays
// with Validate's ping.
func (v ChatValidator) ListModels(ctx context.Context, endpoint, apiKey string) ([]chat.ModelInfo, error) {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("endpoint or API key is empty")
	}
	client := v.factory(endpoint, apiKey, "")
	listCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	return client.ListModels(listCtx)
}

// Validate runs the list + ping checks and returns a structured result.
// PingOK is the single blocking signal: ModelKnown is advisory because
// some providers don't list every invocable model.
func (v ChatValidator) Validate(ctx context.Context, p ChatConfigProposal) ChatValidationResult {
	result := ChatValidationResult{}

	if strings.TrimSpace(p.Endpoint) == "" || strings.TrimSpace(p.APIKey) == "" {
		result.Failures = append(result.Failures, CheckFailure{
			Name:        "endpoint_unreachable",
			Severity:    "blocking",
			Message:     "endpoint or API key is empty",
			Remediation: "Enter the chat base URL and API key.",
		})
		return result
	}

	client := v.factory(p.Endpoint, p.APIKey, p.Model)

	// --- List check ---
	listCtx, listCancel := context.WithTimeout(ctx, v.timeout)
	defer listCancel()
	models, err := client.ListModels(listCtx)
	if err != nil {
		if errors.Is(listCtx.Err(), context.DeadlineExceeded) {
			result.Failures = append(result.Failures, CheckFailure{
				Name:        "timeout",
				Severity:    "blocking",
				Message:     "endpoint did not respond within the validation timeout",
				Remediation: "Check network latency or raise the chat timeout.",
			})
			return result
		}
		if isAuthError(err) {
			result.Failures = append(result.Failures, CheckFailure{
				Name:        "key_rejected",
				Severity:    "blocking",
				Message:     err.Error(),
				Remediation: "The API key was rejected. Verify the key and that it has access to this endpoint.",
			})
		} else {
			result.Failures = append(result.Failures, CheckFailure{
				Name:        "endpoint_unreachable",
				Severity:    "blocking",
				Message:     err.Error(),
				Remediation: "Check the base URL and that the endpoint is reachable from the daemon host.",
			})
		}
		return result
	}
	result.EndpointOK = true
	result.ModelsListed = true
	result.ModelOptions = models
	for _, m := range models {
		if m.ID == p.Model {
			result.ModelKnown = true
			break
		}
	}
	if !result.ModelKnown {
		result.Failures = append(result.Failures, CheckFailure{
			Name:        "model_unknown",
			Severity:    "advisory",
			Message:     "model not present in the endpoint's model list",
			Remediation: "Confirm the model ID spelling or pick from the list. The ping below is the authoritative check.",
		})
	}

	// --- PingCompletion check (the authoritative gate) ---
	if failure := pingCheck(ctx, client, v.timeout); failure != nil {
		result.Failures = append(result.Failures, *failure)
		return result
	}
	result.PingOK = true
	return result
}

// pingCheck runs the PingCompletion probe and returns a blocking
// CheckFailure on failure, or nil on success.
func pingCheck(ctx context.Context, client chatReachable, timeout time.Duration) *CheckFailure {
	pingCtx, pingCancel := context.WithTimeout(ctx, timeout)
	defer pingCancel()
	if err := client.PingCompletion(pingCtx); err != nil {
		if errors.Is(pingCtx.Err(), context.DeadlineExceeded) {
			return &CheckFailure{
				Name:        "timeout",
				Severity:    "blocking",
				Message:     "model invocation did not complete within the validation timeout",
				Remediation: "Check network latency or raise the chat timeout.",
			}
		}
		return &CheckFailure{
			Name:        "ping_failed",
			Severity:    "blocking",
			Message:     err.Error(),
			Remediation: "Model is listed but the key can't invoke it. Check billing, quota, and model-access permissions for this key.",
		}
	}
	return nil
}

// isAuthError recognizes 401/403-style rejections from the chat client.
func isAuthError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(strings.ToLower(msg), "unauthorized") ||
		strings.Contains(strings.ToLower(msg), "invalid api key") ||
		strings.Contains(strings.ToLower(msg), "permission")
}
