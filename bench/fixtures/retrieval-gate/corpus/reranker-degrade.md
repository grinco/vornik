# Reranker degradation

The reranker must never fail a search: on timeout or transport error it degrades to the fused RRF ordering. That silence is by design, which is why a dropped rerank request went unnoticed across 151,818 recorded operations.
