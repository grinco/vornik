package featuredoctor

import (
	"context"

	"vornik.io/vornik/internal/version"
)

func memoryRAGFeature() Feature {
	modelReach := func(name, key string) Prereq {
		return Prereq{
			Name: name,
			Check: func(ctx context.Context, d Deps) PrereqResult {
				v, _ := d.Config.GateValue(key)
				id, _ := v.(string)
				if id == "" {
					return PrereqResult{OK: true, Detail: key + " unset; using default"}
				}
				if d.Models == nil {
					return PrereqResult{OK: false, Fixable: false,
						Detail: id + " not wired (model pinger unavailable)"}
				}
				if d.Models.Reachable(ctx, id) {
					return PrereqResult{OK: true, Detail: id + " reachable"}
				}
				return PrereqResult{OK: false, Fixable: false,
					Detail:      id + " not reachable",
					Remediation: "ensure " + key + " (" + id + ") is served before enabling"}
			},
		}
	}
	// embeddingReach probes the embedding model where it is actually
	// served. Embeddings route to memory.embedding_endpoint when one is
	// configured — a different surface from the chat-provider catalog
	// that ModelPinger consults. Probing the chat catalog mis-reports a
	// locally-served embedding model (e.g. bge-m3 on a local Ollama) as
	// "not reachable" (incident 2026-06-12). Only when no dedicated
	// endpoint is set do embeddings fall back to the agent/chat endpoint,
	// where the chat-catalog check is the right surface.
	embeddingReach := Prereq{
		Name: "embedding model reachable",
		Check: func(ctx context.Context, d Deps) PrereqResult {
			v, _ := d.Config.GateValue("memory.embedding_model")
			id, _ := v.(string)
			if id == "" {
				return PrereqResult{OK: true, Detail: "memory.embedding_model unset; using default"}
			}

			ev, _ := d.Config.GateValue("memory.embedding_endpoint")
			endpoint, _ := ev.(string)
			if endpoint != "" {
				if d.Embeddings == nil {
					return PrereqResult{OK: false, Fixable: false,
						Detail: id + " not wired (embedding prober unavailable)"}
				}
				kv, _ := d.Config.GateValue("memory.embedding_api_key")
				key, _ := kv.(string)
				if d.Embeddings.ProbeEmbedding(ctx, endpoint, key, id) {
					return PrereqResult{OK: true, Detail: id + " reachable at " + endpoint}
				}
				return PrereqResult{OK: false, Fixable: false,
					Detail: id + " not reachable at " + endpoint,
					Remediation: "ensure memory.embedding_model (" + id +
						") is served at memory.embedding_endpoint (" + endpoint + ")"}
			}

			// No dedicated endpoint → embeddings use the agent/chat
			// endpoint; the chat-provider catalog is the right surface.
			if d.Models == nil {
				return PrereqResult{OK: false, Fixable: false,
					Detail: id + " not wired (model pinger unavailable)"}
			}
			if d.Models.Reachable(ctx, id) {
				return PrereqResult{OK: true, Detail: id + " reachable"}
			}
			return PrereqResult{OK: false, Fixable: false,
				Detail:      id + " not reachable",
				Remediation: "ensure memory.embedding_model (" + id + ") is served before enabling"}
		},
	}
	return Feature{
		ID:      "memory-rag",
		Title:   "Memory consolidation + RAG caches",
		Summary: "LLM consolidation, response cache, and embedding cache for project memory.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/memory-rag.md",
		Edition: version.EditionCommunity,
		Apply:   RestartRequired,
		Gates: []Gate{
			{Key: "memory.llm_consolidate_enabled", EnableTo: true},
			{Key: "memory.response_cache_enabled", EnableTo: true},
			{Key: "memory.embedding_cache_enabled", EnableTo: true},
		},
		Prereqs: []Prereq{
			embeddingReach,
			modelReach("consolidation model reachable", "memory.llm_consolidate_model"),
		},
		Verify: func(ctx context.Context, d Deps) PrereqResult {
			// Read-only coherence check; richer verify (embeddings present
			// on recent chunks) wired when the memory repo is in Deps.
			return PrereqResult{OK: true, Detail: "gates coherent"}
		},
	}
}
