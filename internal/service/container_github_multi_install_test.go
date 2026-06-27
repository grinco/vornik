package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
)

// signedReq signs a webhook body with the given secret + event +
// delivery headers, mirroring the helper in internal/github tests.
func signedReq(secret, event, delivery string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", sig)
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", delivery)
	return r
}

// multiInstallProjectPair returns two projects ready for multi-
// installation channel construction, sharing one webhook secret +
// one PEM file but with distinct installation_ids and repos.
func multiInstallProjectPair(t *testing.T) (*registry.Project, *registry.Project, string) {
	t.Helper()
	pemPath := writePEM(t, filepath.Join(t.TempDir(), "key.pem"))

	pA := inboundOnlyProject("project-A")
	pA.GitHubApp.RepoAllowlist = []string{"acme/api"}
	pA.GitHubApp.TaskLabels = []string{"vornik-task"}
	pA.GitHubApp.AppID = 1
	pA.GitHubApp.InstallationID = 100
	pA.GitHubApp.PrivateKeyPath = pemPath

	pB := inboundOnlyProject("project-B")
	pB.GitHubApp.RepoAllowlist = []string{"widgets/web"}
	pB.GitHubApp.TaskLabels = []string{"vornik-task"}
	pB.GitHubApp.AppID = 2
	pB.GitHubApp.InstallationID = 200
	pB.GitHubApp.PrivateKeyPath = pemPath

	return pA, pB, pemPath
}

// TestBuildGitHubChannel_MultiInstall_RoutesSessions — boot the
// channel from two projects, fire one webhook per installation,
// assert the session-store project resolution maps each session
// to the right project.
func TestBuildGitHubChannel_MultiInstall_RoutesSessions(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pA, pB, _ := multiInstallProjectPair(t)

	ch, enabled, err := buildGitHubChannel([]*registry.Project{pA, pB})
	if err != nil {
		t.Fatalf("buildGitHubChannel: %v", err)
	}
	if ch == nil || len(enabled) != 2 {
		t.Fatalf("ch=%v enabled=%v, want non-nil + 2 projects", ch, enabled)
	}

	// Fire two issue_comment.created webhooks @vornik to seed
	// sessions on each installation. The channel needs a Receiver
	// to actually dispatch — but we just want recordSession to
	// fire, so a no-op receiver is enough.
	noopRx := noopReceiver{}
	go func() {
		_ = ch.Start(context.Background(), noopRx)
	}()

	bodyA := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 100},
		"issue": {"number": 5, "title": "issue A"},
		"comment": {"id": 1, "body": "@vornik hi", "user": {"login": "vadim"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedReq("shhh", "issue_comment", "d-A", bodyA))

	bodyB := []byte(`{
		"action": "created",
		"repository": {"full_name": "widgets/web"},
		"sender": {"login": "alice"},
		"installation": {"id": 200},
		"issue": {"number": 7, "title": "issue B"},
		"comment": {"id": 2, "body": "@vornik hi", "user": {"login": "alice"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedReq("shhh", "issue_comment", "d-B", bodyB))

	// Session store with the channel as resolver — Load should
	// return the right ActiveProject per session.
	store := newGitHubSessionStoreWithResolver(nil, ch)
	sessA, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/5"})
	if err != nil {
		t.Fatalf("Load A: %v", err)
	}
	if sessA.ActiveProject != "project-A" {
		t.Errorf("session A ActiveProject = %q, want project-A", sessA.ActiveProject)
	}
	sessB, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "widgets/web#issues/7"})
	if err != nil {
		t.Fatalf("Load B: %v", err)
	}
	if sessB.ActiveProject != "project-B" {
		t.Errorf("session B ActiveProject = %q, want project-B", sessB.ActiveProject)
	}

	// Unknown sessions resolve to the empty project (won't lift
	// any project-scoped tools).
	sessU, _ := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "nobody/none#issues/1"})
	if sessU.ActiveProject != "" {
		t.Errorf("unknown session ActiveProject = %q, want empty", sessU.ActiveProject)
	}
}

// TestGitHubSessionStore_ResolverWiring_FullRegistry — with a
// real registry + a resolver that maps two sessions to two
// projects, Load assembles the per-project AllowedProjects and
// LeadSystemPrompt independently for each session.
func TestGitHubSessionStore_ResolverWiring_FullRegistry(t *testing.T) {
	reg := registry.New()
	// Re-use the existing loadFullRegistry helper layout to set
	// up "p-1" with a lead role; add a second project that
	// references the same swarm/workflow.
	regDir := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		full := regDir + "/" + rel
		if err := writeDirFile(full, body); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mustWrite("workflows/w-1.md", `---
workflowId: "w-1"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "do the thing"
---
`)
	mustWrite("swarms/s-1.md", `---
swarmId: "s-1"
displayName: "test swarm"
leadRole: "lead"
roles:
  - name: "lead"
    description: "the leader"
    systemPrompt: "You are the lead role."
    runtime:
      image: "vornik/test-runtime:latest"
---
`)
	mustWrite("projects/p-a.yaml", `projectId: "project-A"
swarmId: "s-1"
defaultWorkflowId: "w-1"
`)
	mustWrite("projects/p-b.yaml", `projectId: "project-B"
swarmId: "s-1"
defaultWorkflowId: "w-1"
`)
	if err := reg.Load(regDir); err != nil {
		t.Fatalf("registry.Load: %v", err)
	}

	resolver := mapResolver{
		"acme/api#issues/5":    "project-A",
		"widgets/web#issues/7": "project-B",
	}
	store := newGitHubSessionStoreWithResolver(reg, resolver)

	sessA, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/5"})
	if err != nil {
		t.Fatalf("Load A: %v", err)
	}
	if sessA.ActiveProject != "project-A" {
		t.Errorf("session A ActiveProject = %q, want project-A", sessA.ActiveProject)
	}
	if len(sessA.AllowedProjects) != 1 || sessA.AllowedProjects[0] != "project-A" {
		t.Errorf("session A AllowedProjects = %v, want [project-A]", sessA.AllowedProjects)
	}
	if sessA.LeadSystemPrompt == "" {
		t.Error("session A LeadSystemPrompt empty; expected BuildLeadSystemPrompt output")
	}

	sessB, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "widgets/web#issues/7"})
	if err != nil {
		t.Fatalf("Load B: %v", err)
	}
	if sessB.ActiveProject != "project-B" {
		t.Errorf("session B ActiveProject = %q, want project-B", sessB.ActiveProject)
	}
	if len(sessB.AllowedProjects) != 1 || sessB.AllowedProjects[0] != "project-B" {
		t.Errorf("session B AllowedProjects = %v, want [project-B]", sessB.AllowedProjects)
	}

	// A session that the resolver doesn't recognise returns an
	// empty ActiveProject and an empty AllowedProjects list — the
	// dispatcher safely falls back to no project context.
	sessU, _ := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "unknown/repo#issues/1"})
	if sessU.ActiveProject != "" {
		t.Errorf("unknown session ActiveProject = %q, want empty", sessU.ActiveProject)
	}
	if len(sessU.AllowedProjects) != 0 {
		t.Errorf("unknown session AllowedProjects = %v, want empty", sessU.AllowedProjects)
	}
}

// TestNewGitHubSessionStoreWithResolver_NilResolverIsSafe — passing
// nil for the resolver falls back to the empty constantResolver
// so Load returns ActiveProject == "" rather than nil-deref'ing.
func TestNewGitHubSessionStoreWithResolver_NilResolverIsSafe(t *testing.T) {
	store := newGitHubSessionStoreWithResolver(nil, nil)
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "x"})
	if err != nil {
		t.Fatalf("Load with nil resolver: %v", err)
	}
	if sess.ActiveProject != "" {
		t.Errorf("ActiveProject = %q, want empty for nil resolver", sess.ActiveProject)
	}
}

// TestBuildGitHubChannel_MultiInstall_SecondProjectBadPEM —
// when one of the secondary projects has a bogus private_key_path,
// boot fails with a project-scoped error message.
func TestBuildGitHubChannel_MultiInstall_SecondProjectBadPEM(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pA, pB, _ := multiInstallProjectPair(t)
	pB.GitHubApp.PrivateKeyPath = "/nonexistent/key.pem"
	_, _, err := buildGitHubChannel([]*registry.Project{pA, pB})
	if err == nil || !strings.Contains(err.Error(), "project-B") {
		t.Errorf("err = %v, want project-B-scoped failure", err)
	}
}

// TestBuildGitHubChannel_MultiInstall_FirstProjectBadPEM —
// when the FIRST enabled project has a bogus PEM, the multi-
// install path's "base config" resolution fails — boot aborts
// with the first project's error.
func TestBuildGitHubChannel_MultiInstall_FirstProjectBadPEM(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pA, pB, _ := multiInstallProjectPair(t)
	pA.GitHubApp.PrivateKeyPath = "/nonexistent/keyA.pem"
	_, _, err := buildGitHubChannel([]*registry.Project{pA, pB})
	if err == nil || !strings.Contains(err.Error(), "project-A") {
		t.Errorf("err = %v, want project-A-scoped failure", err)
	}
}

// TestBuildGitHubChannel_MultiInstall_APIBaseURLMismatch —
// two enabled projects pointing at different GitHub Enterprise
// API endpoints can't share one channel; surface at boot.
func TestBuildGitHubChannel_MultiInstall_APIBaseURLMismatch(t *testing.T) {
	t.Setenv("GH_TEST_SECRET", "shhh")
	pA, pB, _ := multiInstallProjectPair(t)
	pA.GitHubApp.APIBaseURL = "https://github.example.com/api/v3"
	pB.GitHubApp.APIBaseURL = "https://github.other.example/api/v3"
	_, _, err := buildGitHubChannel([]*registry.Project{pA, pB})
	if err == nil || !strings.Contains(err.Error(), "APIBaseURL") {
		t.Errorf("err = %v, want APIBaseURL-mismatch failure", err)
	}
}

// mapResolver implements projectResolver via a static lookup
// table — used by the test above to feed the store both
// recognised and unknown session IDs.
type mapResolver map[string]string

func (m mapResolver) ProjectForSession(s string) string { return m[s] }

// noopReceiver discards every message — used by tests that
// exercise the inbound path without caring about dispatcher
// invocation.
type noopReceiver struct{}

func (noopReceiver) Receive(_ context.Context, _ conversation.ChannelMessage) error { return nil }

// Compile-time interface assertions guard against drift.
var (
	_ projectResolver         = (*mapResolver)(nil)
	_ conversation.Receiver   = (*noopReceiver)(nil)
	_ dispatcher.SessionStore = (*githubSessionStore)(nil)
)
