package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		Config:         cr,
		Instincts:      s.instinctRepo,
		Outcomes:       nil, // not currently exposed on Server; feature checks degrade gracefully
		Models:         mp,
		Embeddings:     embeddingProberAdapter{},
		Tasks:          tl,
		Trading:        s.featureTradingProbe,
		SecretsDir:     secretsDir,
		RoleLibraryDir: resolveConfigsDirBestEffort(s.setupConfigPath),
		Logger:         s.logger,
	}
}

// resolveConfigsDirBestEffort derives the daemon's configs root from
// its resolved config.yaml path (<dir-of-config.yaml>/configs — the
// primary candidate internal/service's resolveRegistryConfigDir also
// tries first). A best-effort mirror rather than a shared helper: the
// full resolver (env override + hasRegistryLayout probing + a bare
// "configs" fallback) lives in internal/service and isn't reachable
// from internal/api without introducing an import; this is accurate
// for the standard deployment layout the composer feature-doctor
// prereq needs (does role-library/ exist and parse), and degrades to
// "" (prereq reports not-ok) when setupConfigPath is unset.
func resolveConfigsDirBestEffort(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "configs")
}

// embeddingProberAdapter implements featuredoctor.EmbeddingProber by
// performing a minimal embedding request against the dedicated embedding
// endpoint (memory.embedding_endpoint) — the surface embeddings actually
// use, which the chat-provider catalog never lists. Reachability is
// proven by getting a non-empty vector back, matching what the memory
// subsystem does at runtime. Embedder.Embed degrades to (nil, nil) on any
// network/HTTP error, so an empty result means "not reachable".
type embeddingProberAdapter struct{}

func (embeddingProberAdapter) ProbeEmbedding(ctx context.Context, cfg memory.Config) bool {
	emb := memory.NewEmbedder(cfg)
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

// modelPingerAdapter implements featuredoctor.ModelPinger for chat models.
// It first consults the daemon's model catalog; when catalog discovery is
// incomplete, it falls back to the same per-model completion path runtime
// callers use (for example instinct.distiller with chat.WithModel). Some
// providers, notably Bedrock deployments without ListFoundationModels/static
// catalog coverage, can complete successfully while returning no list rows.
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
	return m.probeCompletion(ctx, modelID)
}

func (m modelPingerAdapter) probeCompletion(ctx context.Context, modelID string) bool {
	provider := m.provider
	if provider == nil || modelID == "" {
		return false
	}
	if overridable, ok := provider.(chat.ModelOverridable); ok {
		provider = overridable.WithModel(modelID)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := provider.Complete(ctx, []chat.Message{
		{Role: "system", Content: "You are a model reachability probe. Reply with only ok."},
		{Role: "user", Content: "ok"},
	})
	if err != nil || resp == nil {
		return false
	}
	if len(resp.Choices) == 0 {
		return false
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content) != ""
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
