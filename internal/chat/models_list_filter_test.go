package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freeSuffixFilter is the predicate the OpenRouter wiring uses when
// free_only is set: keep only `:free` model IDs.
func freeSuffixFilter(m ModelInfo) bool { return strings.HasSuffix(m.ID, ":free") }

// TestClient_WithModelListFilter_LiveFetch verifies the filter drops
// non-matching IDs from a live /models response.
func TestClient_WithModelListFilter_LiveFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"deepseek/deepseek-r1:free"},
			{"id":"openai/gpt-4o"},
			{"id":"meta-llama/llama-3.3-70b-instruct:free"}
		]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "or-key", "m", WithModelListFilter(freeSuffixFilter))
	models, err := client.ListModels(context.Background())
	require.NoError(t, err)

	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	assert.ElementsMatch(t, []string{
		"deepseek/deepseek-r1:free",
		"meta-llama/llama-3.3-70b-instruct:free",
	}, ids)
}

// TestClient_WithModelListFilter_Nil verifies a nil filter is a no-op —
// every model passes through (existing behaviour unchanged).
func TestClient_WithModelListFilter_Nil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"a:free"},{"id":"b"}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "or-key", "m", WithModelListFilter(nil))
	models, err := client.ListModels(context.Background())
	require.NoError(t, err)
	assert.Len(t, models, 2)
}

// TestClient_WithModelListFilter_StaticPath verifies the filter is also
// applied to the static-list short-circuit (WithStaticModelList), not just
// the live fetch.
func TestClient_WithModelListFilter_StaticPath(t *testing.T) {
	client := NewClient("https://example.com", "k", "m",
		WithStaticModelList([]ModelInfo{
			{ID: "x/y:free"},
			{ID: "x/z"},
		}),
		WithModelListFilter(freeSuffixFilter),
	)
	models, err := client.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "x/y:free", models[0].ID)
}
