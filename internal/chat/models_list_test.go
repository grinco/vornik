package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_ListModels_StaticModels(t *testing.T) {
	staticModels := []ModelInfo{
		{ID: "model-1", Source: "static", OwnedBy: "org"},
		{ID: "model-2", Source: "static", OwnedBy: "org"},
	}
	c := NewClient("https://example.com", "key", "default", WithStaticModelList(staticModels))

	models, err := c.ListModels(t.Context())
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "model-1", models[0].ID)
	assert.Equal(t, "model-2", models[1].ID)

	// Verify that returned slice is a copy, not the original
	models[0].ID = "mutated"
	models2, err := c.ListModels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "model-1", models2[0].ID, "original static slice should not be mutated")
}

func TestClient_ListModels_EmptyEndpoint(t *testing.T) {
	c := NewClient("", "key", "default")
	models, err := c.ListModels(t.Context())
	require.Error(t, err)
	assert.Nil(t, models)
}

// TestClient_ListModels_ExplicitEmptyStatic locks in the Vertex
// pattern: WithStaticModelList with an empty slice must skip the live
// /v1/models fetch. Without this, an operator with no pricing.yaml
// entries for Vertex would see a 404 HTML page in their model-list
// output instead of a clean empty result.
func TestClient_ListModels_ExplicitEmptyStatic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("live /v1/models must not be called when WithStaticModelList was passed (even with an empty slice); got %s %s", r.Method, r.URL.Path)
		http.Error(w, "should not have been called", http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(server.URL, "key", "default", WithStaticModelList(nil))
	models, err := c.ListModels(t.Context())
	require.NoError(t, err)
	assert.Empty(t, models)
}

func TestClient_ListModels_OpenAIFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))

		resp := map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{"id": "gpt-4", "object": "model", "created": 1234567890, "owned_by": "openai"},
				{"id": "gpt-3.5-turbo", "object": "model", "created": 1234567891, "owned_by": "openai"},
				{"id": "", "object": "model", "created": 1234567892, "owned_by": "openai"}, // should be filtered
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", "default")
	models, err := c.ListModels(t.Context())
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "gpt-4", models[0].ID)
	assert.Equal(t, "live", models[0].Source)
	assert.Equal(t, "openai", models[0].OwnedBy)
	assert.Equal(t, int64(1234567890), models[0].Created)
	assert.Equal(t, "gpt-3.5-turbo", models[1].ID)
}

func TestClient_ListModels_BareArrayFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := []map[string]interface{}{
			{"id": "custom-1", "created": 111, "owned_by": "vendor"},
			{"id": "custom-2", "created": 222, "owned_by": "vendor"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", "default")
	models, err := c.ListModels(t.Context())
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "custom-1", models[0].ID)
	assert.Equal(t, "custom-2", models[1].ID)
}

func TestClient_ListModels_Non2xxResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><body><h1>404 Not Found</h1>
<p>The requested URL was not found on this server.</p>
</body></html>`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", "default")
	models, err := c.ListModels(t.Context())
	require.Error(t, err)
	assert.Nil(t, models)
	assert.Contains(t, err.Error(), "HTTP 404")
	assert.Contains(t, err.Error(), "Not Found")
}

func TestClient_ListModels_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"invalid": json`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-key", "default")
	models, err := c.ListModels(t.Context())
	require.Error(t, err)
	assert.Nil(t, models)
	assert.Contains(t, err.Error(), "parse models response")
}

func TestCompactErrorPreview(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		expected string
	}{
		{
			name:     "collapses whitespace",
			input:    "Hello\n\tWorld  Test",
			max:      100,
			expected: "Hello World Test",
		},
		{
			name:     "truncates to max",
			input:    "The quick brown fox jumps over the lazy dog",
			max:      20,
			expected: "The quick brown fox …",
		},
		{
			name:     "empty input",
			input:    "",
			max:      100,
			expected: "",
		},
		{
			name:     "only whitespace",
			input:    "   \n\t  \r  ",
			max:      100,
			expected: " ",
		},
		{
			name:     "exact length",
			input:    "12345",
			max:      5,
			expected: "12345",
		},
		{
			name:     "one over max",
			input:    "123456",
			max:      5,
			expected: "12345…",
		},
		{
			name:     "complex HTML",
			input:    "<html>\n<body>\n  <h1>Error</h1>\n</body>\n</html>",
			max:      30,
			expected: "<html> <body> <h1>Error</h1> <…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := compactErrorPreview(tt.input, tt.max)
			assert.Equal(t, tt.expected, result)
		})
	}
}
