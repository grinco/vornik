package api

import (
	"context"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// featureDeps assembles a featuredoctor.Deps from the daemon's live
// components. All fields are nil-safe — the feature checks degrade
// gracefully when a component isn't wired.
func (s *Server) featureDeps() featuredoctor.Deps {
	var cr featuredoctor.ConfigReader
	if s.config != nil {
		cr = configGateReader{cfg: s.config}
	}

	var mp featuredoctor.ModelPinger
	if s.chatProvider != nil {
		mp = modelPingerAdapter{provider: s.chatProvider}
	}

	var tl featuredoctor.TaskLister
	if s.taskRepo != nil {
		tl = taskListerAdapter{repo: s.taskRepo}
	}

	// Derive the secrets directory from the admin-key path convention:
	// $HOME/.config/vornik/secrets (the documented location per MEMORY.md).
	secretsDir := defaultSecretsDir()

	return featuredoctor.Deps{
		Config:     cr,
		Instincts:  s.instinctRepo,
		Outcomes:   nil, // not currently exposed on Server; feature checks degrade gracefully
		Models:     mp,
		Embeddings: embeddingProberAdapter{},
		Tasks:      tl,
		Trading:    s.featureTradingProbe,
		SecretsDir: secretsDir,
		Logger:     s.logger,
	}
}

// embeddingProberAdapter implements featuredoctor.EmbeddingProber by
// performing a minimal embedding request against the dedicated embedding
// endpoint (memory.embedding_endpoint) — the surface embeddings actually
// use, which the chat-provider catalog never lists. Reachability is
// proven by getting a non-empty vector back, matching what the memory
// subsystem does at runtime. Embedder.Embed degrades to (nil, nil) on any
// network/HTTP error, so an empty result means "not reachable".
type embeddingProberAdapter struct{}

func (embeddingProberAdapter) ProbeEmbedding(ctx context.Context, endpoint, apiKey, model string) bool {
	if endpoint == "" || model == "" {
		return false
	}
	emb := memory.NewEmbedder(memory.Config{
		EmbeddingEndpoint: endpoint,
		EmbeddingAPIKey:   apiKey,
		EmbeddingModel:    model,
	})
	vecs, _ := emb.Embed(ctx, []string{"vornik embedding reachability probe"})
	return len(vecs) > 0 && len(vecs[0]) > 0
}

// defaultSecretsDir returns the operator-conventional secrets directory
// ($HOME/.config/vornik/secrets). Used by featureDeps when the server
// hasn't had a secrets path injected.
func defaultSecretsDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ".config/vornik/secrets"
	}
	p, err := filepath.Abs(filepath.Join(home, ".config", "vornik", "secrets"))
	if err != nil {
		return filepath.Join(home, ".config", "vornik", "secrets")
	}
	return p
}

// configGateReader adapts *config.Config to featuredoctor.ConfigReader.
type configGateReader struct {
	cfg *config.Config
}

func (r configGateReader) GateValue(key string) (any, bool) {
	return config.LookupByPath(r.cfg, key)
}

// modelPingerAdapter implements featuredoctor.ModelPinger by consulting
// the daemon's chat provider's model catalog. Reachable returns true
// when the model ID appears in the discovered catalog.
//
// Fallback note: if model discovery fails, the model is considered
// not-reachable — the operator should diagnose and fix the discovery
// issue rather than the feature doctor assuming it's fine.
type modelPingerAdapter struct {
	provider chat.Provider
}

func (m modelPingerAdapter) Reachable(ctx context.Context, modelID string) bool {
	if m.provider == nil || modelID == "" {
		return false
	}
	// Try the aggregating path first (Router / QueuedProvider / LoggingProvider
	// wrapping a Router), then the single-provider ModelLister path.
	// *chat.Router implements chat.ModelAggregator, so the interface branch
	// covers it — a direct *chat.Router type-assertion is redundant.
	var models []chat.ModelInfo
	if agg, ok := m.provider.(chat.ModelAggregator); ok {
		if result, ok2 := agg.ListModelsAggregated(ctx); ok2 {
			for _, ms := range result.Providers {
				models = append(models, ms...)
			}
		} else if ms, err := agg.ListModels(ctx); err == nil {
			models = ms
		}
	} else if lister, ok := m.provider.(chat.ModelLister); ok {
		if ms, err := lister.ListModels(ctx); err == nil {
			models = ms
		}
	}
	for _, info := range models {
		if info.ID == modelID {
			return true
		}
	}
	return false
}

// taskListerAdapter implements featuredoctor.TaskLister via
// persistence.TaskRepository.
type taskListerAdapter struct {
	repo persistence.TaskRepository
}

func (t taskListerAdapter) HasActiveTasks(ctx context.Context) (bool, error) {
	if t.repo == nil {
		return false, nil
	}
	// Count RUNNING tasks.
	sRunning := persistence.TaskStatusRunning
	nRunning, err := t.repo.Count(ctx, persistence.TaskFilter{Status: &sRunning})
	if err != nil {
		return false, err
	}
	if nRunning > 0 {
		return true, nil
	}
	// Count LEASED tasks.
	sLeased := persistence.TaskStatusLeased
	nLeased, err := t.repo.Count(ctx, persistence.TaskFilter{Status: &sLeased})
	if err != nil {
		return false, err
	}
	return nLeased > 0, nil
}
