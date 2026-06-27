package cli

// Coverage sweep for `vornikctl {project,swarm,workflow} {list,show}` and
// the fetchJSON / postJSON / prettyPrintJSON helpers. httptest harness;
// captureStdoutFunc from blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunProjectList_TableSorted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(projectListResp{
			Projects: []projectSummary{
				{ProjectID: "zeta", DisplayName: "Z", SwarmID: "s", DefaultWorkflowID: "w", AutonomyEnabled: true},
				{ProjectID: "alpha", DisplayName: "A", SwarmID: "s2", DefaultWorkflowID: "w2"},
			},
			Total: 2,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	registryJSON = false
	out, err := captureStdoutFunc(t, func() error { return runProjectList(projectListCmd, nil) })
	if err != nil {
		t.Fatalf("runProjectList: %v", err)
	}
	// alpha must sort before zeta.
	if strings.Index(out, "alpha") > strings.Index(out, "zeta") {
		t.Errorf("projects not sorted:\n%s", out)
	}
	if !strings.Contains(out, "on") || !strings.Contains(out, "off") || !strings.Contains(out, "Total: 2") {
		t.Errorf("project list output: %s", out)
	}
}

func TestRunProjectList_JSONPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"projectId":"p1"}],"total":1}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	registryJSON = true
	defer func() { registryJSON = false }()
	out, err := captureStdoutFunc(t, func() error { return runProjectList(projectListCmd, nil) })
	if err != nil {
		t.Fatalf("runProjectList json: %v", err)
	}
	if !strings.Contains(out, "p1") {
		t.Errorf("json passthrough output: %s", out)
	}
}

func TestRunProjectShow_PrettyPrints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/janka/config" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"projectId":"janka","autonomy":{"enabled":true}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	out, err := captureStdoutFunc(t, func() error { return runProjectShow(projectShowCmd, []string{"janka"}) })
	if err != nil {
		t.Fatalf("runProjectShow: %v", err)
	}
	if !strings.Contains(out, "janka") || !strings.Contains(out, "\"enabled\": true") {
		t.Errorf("show output: %s", out)
	}
}

func TestRunSwarmList_AndShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/swarms":
			_ = json.NewEncoder(w).Encode(swarmListResp{
				Swarms: []swarmSummary{{SwarmID: "sw1", DisplayName: "D", LeadRole: "lead", Roles: []string{"a", "b"}}},
				Total:  1,
			})
		case "/api/v1/swarms/sw1":
			_, _ = w.Write([]byte(`{"swarmId":"sw1"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	registryJSON = false

	out, err := captureStdoutFunc(t, func() error { return runSwarmList(swarmListCmd, nil) })
	if err != nil {
		t.Fatalf("runSwarmList: %v", err)
	}
	if !strings.Contains(out, "sw1") || !strings.Contains(out, "lead") {
		t.Errorf("swarm list output: %s", out)
	}

	out, err = captureStdoutFunc(t, func() error { return runSwarmShow(swarmShowCmd, []string{"sw1"}) })
	if err != nil {
		t.Fatalf("runSwarmShow: %v", err)
	}
	if !strings.Contains(out, "sw1") {
		t.Errorf("swarm show output: %s", out)
	}
}

func TestRunWorkflowList_AndShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/workflows":
			_ = json.NewEncoder(w).Encode(workflowListResp{
				Workflows: []workflowSummary{{WorkflowID: "wf1", DisplayName: "WF", Steps: []string{"s1", "s2", "s3"}}},
				Total:     1,
			})
		case "/api/v1/workflows/wf1":
			_, _ = w.Write([]byte(`{"workflowId":"wf1"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	registryJSON = false

	out, err := captureStdoutFunc(t, func() error { return runWorkflowList(workflowListCmd, nil) })
	if err != nil {
		t.Fatalf("runWorkflowList: %v", err)
	}
	if !strings.Contains(out, "wf1") || !strings.Contains(out, "3") {
		t.Errorf("workflow list output: %s", out)
	}

	out, err = captureStdoutFunc(t, func() error { return runWorkflowShow(workflowShowCmd, []string{"wf1"}) })
	if err != nil {
		t.Fatalf("runWorkflowShow: %v", err)
	}
	if !strings.Contains(out, "wf1") {
		t.Errorf("workflow show output: %s", out)
	}
}

func TestRunProjectList_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	registryJSON = false
	_, err := captureStdoutFunc(t, func() error { return runProjectList(projectListCmd, nil) })
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestPostJSON_SuccessAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "nope"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	raw, err := postJSON("/ok", map[string]string{"a": "b"})
	if err != nil || !strings.Contains(string(raw), "ok") {
		t.Fatalf("postJSON ok: %v %s", err, raw)
	}
	if _, err := postJSON("/bad", nil); err == nil {
		t.Fatal("expected error from postJSON on 400")
	}
}

func TestPrettyPrintJSON_NonJSONFallback(t *testing.T) {
	out, err := captureStdoutFunc(t, func() error { return prettyPrintJSON([]byte("plain text not json")) })
	if err != nil {
		t.Fatalf("prettyPrintJSON fallback: %v", err)
	}
	if !strings.Contains(out, "plain text not json") {
		t.Errorf("fallback output: %s", out)
	}
}
