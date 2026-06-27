package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
)

// A webhook-profile node must mount the public webhook ingress but NOT the
// data-plane API. We assert by status code: an unmounted route on the mux
// falls through to 404, a mounted one does not.
func TestSetupRoutes_WebhookProfileMountsWebhooksNotAPI(t *testing.T) {
	srv := &Server{} // minimal; handlers that need deps will 4xx/5xx, not 404, which is the signal
	cfg := &config.Config{Node: config.NodeConfig{
		Profile: "webhook",
		Relay:   config.RelayConfig{Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca"},
	}}
	h := SetupRoutes(srv, cfg)

	// webhook ingress is mounted → NOT 404
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/some-project", nil))
	if rw.Code == http.StatusNotFound {
		t.Fatalf("webhook route must be mounted on a webhook node, got 404")
	}

	// a data-plane API route is NOT mounted → 404
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if rw2.Code != http.StatusNotFound {
		t.Fatalf("data-plane API must NOT be mounted on a webhook node, got %d", rw2.Code)
	}

	// health stays mounted on every node → NOT 404
	rw3 := httptest.NewRecorder()
	h.ServeHTTP(rw3, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rw3.Code == http.StatusNotFound {
		t.Fatalf("/livez must be mounted on every node, got 404")
	}

	// "/" OllamaRoot sink is in the ServeAPI region; on a webhook node it must
	// fall through to a clean 404, never a panic/500.
	rw4 := httptest.NewRecorder()
	h.ServeHTTP(rw4, httptest.NewRequest(http.MethodGet, "/", nil))
	if rw4.Code != http.StatusNotFound {
		t.Fatalf("GET / on a webhook node must be 404, got %d", rw4.Code)
	}
}

// An empty / default config (no Node.Profile) resolves to the "all" profile and
// must mount both the data-plane API and the webhook ingress routes.
func TestSetupRoutes_DefaultProfileMountsEverything(t *testing.T) {
	srv := &Server{}                        // minimal; handlers that need deps will 4xx/5xx, not 404
	h := SetupRoutes(srv, &config.Config{}) // empty Node → "all" → all caps on

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodPost, "/api/v1/webhooks/some-project"},
		{http.MethodGet, "/livez"},
	} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(tc.method, tc.path, nil))
		if rw.Code == http.StatusNotFound {
			t.Fatalf("default (all) profile must mount %s, got 404", tc.path)
		}
	}
}

// A "ui" profile node serves only the health endpoint; neither the data-plane
// API nor the webhook ingress are mounted (the UI is served from the
// container_http mount, not NewRouter).
func TestSetupRoutes_UIProfileMountsNeitherAPINorWebhooks(t *testing.T) {
	srv := &Server{} // minimal; handlers that need deps will 4xx/5xx, not 404
	h := SetupRoutes(srv, &config.Config{Node: config.NodeConfig{Profile: "ui"}})

	// data-plane + webhook both unmounted → 404
	for _, path := range []string{"/api/v1/projects", "/api/v1/webhooks/some-project"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, path, nil))
		if rw.Code != http.StatusNotFound {
			t.Fatalf("ui profile must NOT mount %s, got %d", path, rw.Code)
		}
	}

	// health still mounted on every node
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rw.Code == http.StatusNotFound {
		t.Fatal("ui profile must still mount /livez")
	}
}
