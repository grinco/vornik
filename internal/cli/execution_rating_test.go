package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ratingHTTPStub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestExecutionRateCommand_Wiring(t *testing.T) {
	sub := map[string]bool{}
	for _, c := range executionCmd.Commands() {
		sub[c.Name()] = true
	}
	if !sub["rate"] {
		t.Errorf("execution is missing the rate subcommand: %v", sub)
	}
}

func TestExecutionRate_PostsTheVerdictAndReason(t *testing.T) {
	var method, path string
	var body map[string]any
	ratingHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"execution_id":"exec_1","rater_id":"op_1","verdict":"down",
			"reason":"thin","created_at":"2026-09-07T10:00:00Z","updated_at":"2026-09-07T10:00:00Z"}`)
	})

	if err := runExecutionRate("exec_1", "down", "thin", false); err != nil {
		t.Fatalf("rate: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if path != "/api/v1/executions/exec_1/rating" {
		t.Errorf("path = %s", path)
	}
	if body["verdict"] != "down" || body["reason"] != "thin" {
		t.Errorf("body = %v", body)
	}
	// The rater is NOT in the body — the daemon resolves it from the caller's
	// credentials. A CLI that sent one would be asking to be impersonated.
	if _, present := body["rater_id"]; present {
		t.Error("the CLI sent a rater_id; identity is the daemon's to resolve")
	}
}

// --clear is DELETE, not a POST of some empty verdict.
func TestExecutionRate_ClearSendsDelete(t *testing.T) {
	var method, path string
	ratingHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := runExecutionRate("exec_1", "", "", true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", method)
	}
	if path != "/api/v1/executions/exec_1/rating" {
		t.Errorf("path = %s", path)
	}
}

// The verdict set is closed at the CLI too, so a typo costs a round trip
// rather than a confusing 400 from the daemon.
func TestExecutionRate_RefusesAVerdictThatIsNotUpOrDown(t *testing.T) {
	called := false
	ratingHTTPStub(t, func(http.ResponseWriter, *http.Request) { called = true })

	err := runExecutionRate("exec_1", "meh", "", false)
	if err == nil {
		t.Fatal("accepted a verdict that is neither up nor down")
	}
	if !strings.Contains(err.Error(), "up") || !strings.Contains(err.Error(), "down") {
		t.Errorf("the refusal does not name the allowed values: %v", err)
	}
	if called {
		t.Error("a bad verdict still reached the daemon")
	}
}

// --clear and a verdict together are contradictory intents; refusing beats
// silently preferring one.
func TestExecutionRate_RefusesClearTogetherWithAVerdict(t *testing.T) {
	called := false
	ratingHTTPStub(t, func(http.ResponseWriter, *http.Request) { called = true })

	if err := runExecutionRate("exec_1", "up", "", true); err == nil {
		t.Fatal("accepted --clear alongside a verdict")
	}
	if called {
		t.Error("a contradictory invocation still reached the daemon")
	}
}

// A 401 is the deployment-shape refusal from the design §4: the caller has no
// resolvable identity. The CLI must say what to do about it, not just relay a
// status code.
func TestExecutionRate_401ExplainsTheDeploymentRequirement(t *testing.T) {
	ratingHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"UNAUTHORIZED","message":"a rating needs an identified rater"}}`)
	})

	err := runExecutionRate("exec_1", "up", "", false)
	if err == nil {
		t.Fatal("a 401 was not surfaced as an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "key") {
		t.Errorf("the 401 does not point at per-operator keys: %v", err)
	}
}
