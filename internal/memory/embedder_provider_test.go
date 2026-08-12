package memory

import "testing"

// The resolved embedder has to be OBSERVABLE, not just configured.
//
// A benchmark labelled its runs with the embedding model an operator typed on the
// command line, because nothing in the product reported the one actually in use.
// Two providers then produced byte-identical retrieval metrics — impossible for two
// different embedders — and there was no way to tell which vectors had been queried.
// Model() already existed for cache keying; Provider() completes the pair so the
// transport is reportable too.

func TestEmbedder_ReportsProviderAndModel(t *testing.T) {
	e := &Embedder{cfg: Config{
		EmbeddingProvider: "bedrock",
		EmbeddingModel:    "cohere.embed-v4:0",
		BedrockRegion:     "eu-central-1",
	}}
	if got := e.Provider(); got != "bedrock" {
		t.Errorf("Provider() = %q, want bedrock", got)
	}
	if got := e.Model(); got != "cohere.embed-v4:0" {
		t.Errorf("Model() = %q, want cohere.embed-v4:0", got)
	}
}

// An unconfigured embedder must report nothing rather than a default, so a caller
// cannot mistake "not wired" for a working provider.
func TestEmbedder_UnconfiguredReportsEmpty(t *testing.T) {
	e := &Embedder{}
	if got := e.Provider(); got != "" {
		t.Errorf("Provider() = %q on an unconfigured embedder, want empty", got)
	}
	if got := e.Model(); got != "" {
		t.Errorf("Model() = %q on an unconfigured embedder, want empty", got)
	}
}

// Empty provider means the default transport, and it must SAY so rather than
// returning an empty string that reads as "not configured".
func TestEmbedder_DefaultProviderIsNamed(t *testing.T) {
	e := &Embedder{cfg: Config{
		EmbeddingModel:    "text-embedding-3-small",
		EmbeddingEndpoint: "http://localhost:1234/v1/embeddings",
	}}
	if got := e.Provider(); got != "openai" {
		t.Errorf("Provider() = %q, want the default named as openai", got)
	}
}
