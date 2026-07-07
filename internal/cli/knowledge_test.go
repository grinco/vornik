package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func knowledgeHTTPStub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestKnowledgeSetGlobal_PostsGlobalTrue(t *testing.T) {
	var method, path string
	var body map[string]any
	knowledgeHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(knowledgeGlobalOutput{ID: "skill_1", Name: "trace-hang", IsGlobal: true})
	})
	out, err := captureStdoutFn(t, func() error { return runKnowledgeSetGlobal("skill_1", true) })
	if err != nil {
		t.Fatalf("set-global: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/api/v1/skills/skill_1/global" {
		t.Errorf("path = %q", path)
	}
	if body["global"] != true {
		t.Errorf("body global = %v, want true", body["global"])
	}
	if !strings.Contains(out, "GLOBAL") {
		t.Errorf("output should report GLOBAL reach; got %q", out)
	}
}

func TestKnowledgeSetProject_PostsGlobalFalse(t *testing.T) {
	var body map[string]any
	knowledgeHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(knowledgeGlobalOutput{ID: "skill_1", Name: "trace-hang", IsGlobal: false})
	})
	out, err := captureStdoutFn(t, func() error { return runKnowledgeSetGlobal("skill_1", false) })
	if err != nil {
		t.Fatalf("set-project: %v", err)
	}
	if body["global"] != false {
		t.Errorf("body global = %v, want false", body["global"])
	}
	if !strings.Contains(out, "project-only") {
		t.Errorf("output should report project-only reach; got %q", out)
	}
}

func TestKnowledgeSetGlobal_ServerError(t *testing.T) {
	knowledgeHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "NOT_FOUND", "message": "skill not found"}})
	})
	if err := runKnowledgeSetGlobal("ghost", true); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestKnowledgeCommand_Wiring(t *testing.T) {
	sub := map[string]bool{}
	for _, c := range knowledgeCmd.Commands() {
		sub[c.Name()] = true
	}
	if !sub["set-global"] || !sub["set-project"] {
		t.Fatalf("knowledge command missing leaves: %v", sub)
	}
}
