package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/executor"
)

// FetchDiffHandler implements the "forge.fetch_diff" system step: fetch a change
// request's diff daemon-side and pass it to the next step as the result message,
// so the reviewer agent never needs forge CLI / network access. Deterministic.
type FetchDiffHandler struct {
	resolver ProviderResolver
}

// NewFetchDiffHandler wires the handler.
func NewFetchDiffHandler(resolver ProviderResolver) *FetchDiffHandler {
	return &FetchDiffHandler{resolver: resolver}
}

// Name implements executor.SystemHandler.
func (h *FetchDiffHandler) Name() string { return "forge.fetch_diff" }

// Execute implements executor.SystemHandler. The result carries both a `message`
// (the diff, so the next agent step receives it as its prior-step context) and a
// `diff` field for any structured consumer.
func (h *FetchDiffHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	const name = "forge.fetch_diff"
	if h == nil || h.resolver == nil {
		return executor.SystemStepResult{}, errors.New(name + ": handler is missing required dependencies (resolver)")
	}
	job, err := forgeJobFromTask(in.Task, name)
	if err != nil {
		return executor.SystemStepResult{}, err
	}
	provider, err := h.resolver.ForgeProvider(ctx, in.Task.ProjectID)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: resolve provider: %w", name, err)
	}
	diff, err := provider.FetchDiff(ctx, job.Repo, job.Number)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: fetch diff for %s#%d: %w", name, job.Repo, job.Number, err)
	}
	out, _ := json.Marshal(map[string]any{
		"message": string(diff),
		"diff":    string(diff),
		"repo":    job.Repo,
		"number":  job.Number,
	})
	return executor.SystemStepResult{Result: out}, nil
}
