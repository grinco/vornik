# Reranker timeout budget

About eight percent of queries lose reranking to the timeout budget. The reranker is given a bounded slice of the overall recall deadline so a slow model cannot consume the whole request.
