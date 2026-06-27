package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.Enabled, "Enabled should default to false")
	assert.Empty(t, cfg.EmbeddingEndpoint, "EmbeddingEndpoint should default to empty")
	assert.Empty(t, cfg.EmbeddingAPIKey, "EmbeddingAPIKey should default to empty")
	assert.Empty(t, cfg.EmbeddingModel, "EmbeddingModel should default to empty")
	assert.Equal(t, 1536, cfg.EmbeddingDimension, "EmbeddingDimension should default to 1536")
	assert.Equal(t, 512, cfg.ChunkTokens, "ChunkTokens should default to 512")
	assert.Equal(t, 64, cfg.ChunkOverlap, "ChunkOverlap should default to 64")
	assert.Equal(t, 2, cfg.WorkerConcurrency, "WorkerConcurrency should default to 2")
}

func TestConfigCanBeModified(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = "https://api.example.com/v1"
	cfg.EmbeddingAPIKey = "test-key"
	cfg.EmbeddingModel = "text-embedding-3-small"
	cfg.EmbeddingDimension = 3072
	cfg.ChunkTokens = 1024
	cfg.ChunkOverlap = 128
	cfg.WorkerConcurrency = 4

	assert.True(t, cfg.Enabled)
	assert.Equal(t, "https://api.example.com/v1", cfg.EmbeddingEndpoint)
	assert.Equal(t, "test-key", cfg.EmbeddingAPIKey)
	assert.Equal(t, "text-embedding-3-small", cfg.EmbeddingModel)
	assert.Equal(t, 3072, cfg.EmbeddingDimension)
	assert.Equal(t, 1024, cfg.ChunkTokens)
	assert.Equal(t, 128, cfg.ChunkOverlap)
	assert.Equal(t, 4, cfg.WorkerConcurrency)
}

func TestDefaultConfigReturnsIndependentValues(t *testing.T) {
	first := DefaultConfig()
	second := DefaultConfig()

	first.ChunkTokens = 2048
	first.WorkerConcurrency = 8

	assert.Equal(t, 512, second.ChunkTokens)
	assert.Equal(t, 2, second.WorkerConcurrency)
}
