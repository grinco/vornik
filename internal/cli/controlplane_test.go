package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func cpHTTPStub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestControlPlaneCommand_Wiring(t *testing.T) {
	sub := map[string]bool{}
	for _, c := range controlPlaneCmd.Commands() {
		sub[c.Name()] = true
	}
	for _, want := range []string{"proposals", "show", "approve", "reject", "propose"} {
		if !sub[want] {
			t.Errorf("control-plane missing subcommand %q: %v", want, sub)
		}
	}
	if controlPlaneCmd.Aliases[0] != "cp" {
		t.Errorf("expected alias cp, got %v", controlPlaneCmd.Aliases)
	}
}

func TestCPPropose_PostsCorrectly(t *testing.T) {
	var method, path string
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cpProposalWire{ID: "cpp_1", Title: "t", Status: "DRAFT"})
	})
	cpProposeKind, cpProposeScope, cpProposeTitle = "config", "project", "bump timeout"
	out, err := captureStdoutFn(t, func() error { return runCPPropose(nil, nil) })
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if method != http.MethodPost || path != "/api/v1/operator/proposals" {
		t.Errorf("wrong request: %s %s", method, path)
	}
	if body["kind"] != "config" || body["title"] != "bump timeout" {
		t.Errorf("wrong body: %v", body)
	}
	if !strings.Contains(out, "cpp_1") {
		t.Errorf("output missing id: %q", out)
	}
}

func TestCPDecide_PostsDecision(t *testing.T) {
	var body map[string]any
	cpHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_ = json.NewEncoder(w).Encode(cpProposalWire{ID: "cpp_1", Status: "APPROVED", Approver: "vadim"})
	})
	cpActor = "vadim"
	out, err := captureStdoutFn(t, func() error { return runCPDecide("cpp_1", "approve") })
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if body["decision"] != "approve" || body["actor"] != "vadim" {
		t.Errorf("wrong decide body: %v", body)
	}
	if !strings.Contains(out, "APPROVED") {
		t.Errorf("output missing status: %q", out)
	}
}

func TestCPProposals_ServerError(t *testing.T) {
	cpHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "NOT_FOUND", "message": "x"}})
	})
	if err := runCPShow("ghost"); err == nil {
		t.Fatal("expected error on 404")
	}
}
