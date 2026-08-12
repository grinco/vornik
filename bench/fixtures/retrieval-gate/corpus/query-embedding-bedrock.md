# Query embedding on Bedrock

Searcher.embedQueryWithTimeout gated on an empty embedding endpoint. Bedrock sets no endpoint, only a model id and a region, so the query vector was nil and hybrid search ran with no semantic arm. Ask the embedder whether it is configured instead of re-deriving that from one transport's settings.
