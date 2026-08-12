# Embedding cache key

The cache is keyed on content hash and model. Keying on the raw content hash alone evicted nothing, because two models produce different vectors for identical text and the entry for the other model survived.
