# Embedding dimension

The schema pins vector(1024). A model whose native width differs cannot be truncated to fit, because the OpenAI-compatible embed request carries only model and input with no dimensions field.
