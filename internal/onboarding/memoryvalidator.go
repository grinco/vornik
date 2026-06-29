package onboarding

import (
	"context"
	"errors"
	"strings"
	"time"

	"vornik.io/vornik/internal/memory"
)

// MemoryConfigProposal is the memory/RAG input the validator tests against
// the real embedding endpoint. Like ChatConfigProposal these are PROPOSED
// values — the daemon's loaded config is never consulted.
type MemoryConfigProposal struct {
	Enabled            bool   `json:"enabled"`
	EmbeddingEndpoint  string `json:"embedding_endpoint"`
	EmbeddingAPIKey    string `json:"embedding_api_key"`
	EmbeddingModel     string `json:"embedding_model"`
	EmbeddingDimension int    `json:"embedding_dimension"`
}

// MemoryValidationResult is the setup-guide memory decision payload shared
// by the UI and the memory validate/commit endpoints. EmbeddingOK is the
// single blocking signal for commit when memory is enabled.
type MemoryValidationResult struct {
	// Skipped is true when the proposal disables memory: there is
	// nothing to probe and commit is always allowed.
	Skipped bool `json:"skipped"`
	// EmbeddingOK is true when the endpoint returned a non-empty vector
	// for the probe text — the authoritative reachability gate.
	EmbeddingOK bool `json:"embedding_ok"`
	// ReturnedDimension is the length of the vector the endpoint actually
	// produced, 0 when the probe failed. The UI offers it as the value to
	// store so the operator doesn't have to know the model's dimension.
	ReturnedDimension int `json:"returned_dimension"`
	// DimensionMatches is true when the declared EmbeddingDimension equals
	// ReturnedDimension. Advisory: a mismatch is the classic cause of
	// pgvector insert failures, but the operator may be intentionally
	// re-declaring (the returned value is authoritative).
	DimensionMatches bool           `json:"dimension_matches"`
	Failures         []CheckFailure `json:"failures,omitempty"`
}

// embedProbe returns the first embedding vector for a fixed probe string,
// or an error. The production factory wraps memory.NewEmbedder; tests inject
// a fake.
type embedProbe func(ctx context.Context, endpoint, apiKey, model string) ([]float32, error)

// MemoryValidatorInterface is the seam the API handlers depend on so they
// can be tested with a stub instead of a real validator.
type MemoryValidatorInterface interface {
	Validate(ctx context.Context, p MemoryConfigProposal) MemoryValidationResult
}

// MemoryValidator tests proposed memory/embedding config against the real
// embedding endpoint.
type MemoryValidator struct {
	probe   embedProbe
	timeout time.Duration
}

// NewMemoryValidator returns a validator that probes the real embedding
// endpoint via memory.NewEmbedder with a 15s timeout.
func NewMemoryValidator() MemoryValidator {
	return MemoryValidator{
		probe: func(ctx context.Context, endpoint, apiKey, model string) ([]float32, error) {
			emb := memory.NewEmbedder(memory.Config{
				EmbeddingEndpoint: endpoint,
				EmbeddingAPIKey:   apiKey,
				EmbeddingModel:    model,
			})
			vecs, err := emb.Embed(ctx, []string{"vornik embedding reachability probe"})
			if err != nil {
				return nil, err
			}
			if len(vecs) == 0 || len(vecs[0]) == 0 {
				return nil, errors.New("embedding endpoint returned no vector")
			}
			return vecs[0], nil
		},
		timeout: 15 * time.Second,
	}
}

// NewMemoryValidatorWithProbe is the test constructor.
func NewMemoryValidatorWithProbe(p embedProbe, timeout time.Duration) MemoryValidator {
	return MemoryValidator{probe: p, timeout: timeout}
}

// Validate probes the embedding endpoint and returns a structured result.
// When the proposal disables memory it short-circuits to Skipped — there is
// nothing to validate and commit will simply persist memory.enabled=false.
func (v MemoryValidator) Validate(ctx context.Context, p MemoryConfigProposal) MemoryValidationResult {
	if !p.Enabled {
		return MemoryValidationResult{Skipped: true}
	}

	result := MemoryValidationResult{}
	if strings.TrimSpace(p.EmbeddingEndpoint) == "" || strings.TrimSpace(p.EmbeddingModel) == "" {
		result.Failures = append(result.Failures, CheckFailure{
			Name:        "embedding_incomplete",
			Severity:    "blocking",
			Message:     "embedding endpoint or model is empty",
			Remediation: "Enter the embedding base URL and model, or disable memory.",
		})
		return result
	}

	probeCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	vec, err := v.probe(probeCtx, p.EmbeddingEndpoint, p.EmbeddingAPIKey, p.EmbeddingModel)
	if err != nil {
		name := "embedding_unreachable"
		remediation := "Check the embedding base URL and that it is reachable from the daemon host."
		if isAuthError(err) {
			name = "embedding_key_rejected"
			remediation = "The embedding API key was rejected. Verify the key has access to this endpoint."
		} else if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			name = "timeout"
			remediation = "Check network latency or the embedding endpoint's responsiveness."
		}
		result.Failures = append(result.Failures, CheckFailure{
			Name:        name,
			Severity:    "blocking",
			Message:     err.Error(),
			Remediation: remediation,
		})
		return result
	}

	result.EmbeddingOK = true
	result.ReturnedDimension = len(vec)
	result.DimensionMatches = p.EmbeddingDimension == len(vec)
	if p.EmbeddingDimension > 0 && !result.DimensionMatches {
		result.Failures = append(result.Failures, CheckFailure{
			Name:     "dimension_mismatch",
			Severity: "advisory",
			Message: "declared embedding_dimension does not match the vector the model returned " +
				"(declared vs returned differ)",
			Remediation: "Use the returned dimension — a mismatch is the classic cause of pgvector insert failures.",
		})
	}
	return result
}
