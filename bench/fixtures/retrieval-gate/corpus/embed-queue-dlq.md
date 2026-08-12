# Embed queue dead letters

A chunk that cannot be embedded after retries moves to memory_embed_dlq with a retry_after backoff of ten minutes doubling to a twenty-four hour cap. The DLQ is replayed at the head of each worker tick.
