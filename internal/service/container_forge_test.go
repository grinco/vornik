package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

type fakeProjects map[string]*registry.Project

func (f fakeProjects) GetProject(id string) *registry.Project { return f[id] }

func writeKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newResolver(p projectGetter) *forgeProviderResolver {
	return &forgeProviderResolver{projects: p, cache: map[string]forge.ForgeProvider{}}
}

func TestForgeProviderResolver_BuildsAndCaches(t *testing.T) {
	ctx := context.Background()
	projs := fakeProjects{
		"gh": {GitHub: registry.ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: writeKey(t)}},
	}
	r := newResolver(projs)

	p1, err := r.ForgeProvider(ctx, "gh")
	if err != nil {
		t.Fatalf("resolve gh: %v", err)
	}
	if p1.Name() != forge.ProviderGitHub {
		t.Errorf("provider name = %s", p1.Name())
	}
	p2, err := r.ForgeProvider(ctx, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Error("resolver should cache the provider instance per project")
	}
}

func TestForgeProviderResolver_Errors(t *testing.T) {
	ctx := context.Background()
	r := newResolver(fakeProjects{
		"none": {}, // no forge configured
	})
	if _, err := r.ForgeProvider(ctx, "missing"); err == nil {
		t.Error("unknown project should error")
	}
	if _, err := r.ForgeProvider(ctx, "none"); err == nil {
		t.Error("project without forge config should error")
	}
	// nil project getter.
	if _, err := (&forgeProviderResolver{}).ForgeProvider(ctx, "x"); err == nil {
		t.Error("nil project registry should error")
	}
}

func TestForgeProviderResolver_NilCacheInit(t *testing.T) {
	// A resolver with a nil cache map must lazily initialise it, not panic.
	r := &forgeProviderResolver{projects: fakeProjects{
		"gh": {GitHub: registry.ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: writeKey(t)}},
	}}
	if _, err := r.ForgeProvider(context.Background(), "gh"); err != nil {
		t.Fatalf("resolve with nil cache: %v", err)
	}
}

func TestNewForgeResolver(t *testing.T) {
	if (&Container{}).newForgeResolver() != nil {
		t.Error("no registry → nil resolver")
	}
	c := &Container{Registry: &registry.Registry{}}
	if c.newForgeResolver() == nil {
		t.Error("with registry → resolver built")
	}
}

func TestForgePublishSource_RevParseError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// A workspace dir that isn't a git repo → rev-parse fails.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&forgePublishSource{workspacePath: ws}).PublishSource(context.Background(), &persistence.Task{ProjectID: "p"}); err == nil {
		t.Error("rev-parse on a non-git dir should error")
	}
}

func TestForgePublishSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()

	// Missing workspace / project → error.
	if _, _, err := (&forgePublishSource{}).PublishSource(ctx, &persistence.Task{ProjectID: "p"}); err == nil {
		t.Error("empty workspace path should error")
	}
	if _, _, err := (&forgePublishSource{workspacePath: "/ws"}).PublishSource(ctx, nil); err == nil {
		t.Error("nil task should error")
	}

	// Real repo → returns its HEAD sha.
	ws := t.TempDir()
	proj := filepath.Join(ws, "proj-1")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", proj}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("commit", "--allow-empty", "-m", "x")

	gitDir, sha, err := (&forgePublishSource{workspacePath: ws}).PublishSource(ctx, &persistence.Task{ProjectID: "proj-1"})
	if err != nil {
		t.Fatal(err)
	}
	if gitDir != proj {
		t.Errorf("gitDir = %s, want %s", gitDir, proj)
	}
	if len(sha) < 7 {
		t.Errorf("sha looks wrong: %q", sha)
	}
}

func TestForgeTaskPrompt(t *testing.T) {
	issue := forge.ForgeJob{Repo: "o/r", Number: 7, Title: "Fix the bug", Body: "details here"}
	got := forgeTaskPrompt(issue)
	if !strings.Contains(got, "issue #7 in o/r") || !strings.Contains(got, "Fix the bug") || !strings.Contains(got, "details here") {
		t.Errorf("issue prompt = %q", got)
	}
	pr := forge.ForgeJob{Repo: "o/r", Number: 12, Title: "A PR", IsChangeRequest: true}
	if got := forgeTaskPrompt(pr); !strings.Contains(got, "pull request #12") || !strings.Contains(got, "A PR") {
		t.Errorf("PR prompt = %q", got)
	}
	// No title/body → still a valid one-liner.
	if got := forgeTaskPrompt(forge.ForgeJob{Repo: "o/r", Number: 1}); !strings.Contains(got, "issue #1 in o/r") {
		t.Errorf("bare prompt = %q", got)
	}
}

func TestForgeWebhookClassifier(t *testing.T) {
	ctx := context.Background()
	projs := fakeProjects{
		"gh":   {GitHub: registry.ProjectGitHub{AppID: 1, InstallationID: 2, PrivateKeyPath: writeKey(t)}},
		"none": {},
	}
	c := &forgeWebhookClassifier{resolver: newResolver(projs)}

	issueBody := []byte(`{"action":"labeled","repository":{"full_name":"o/r","default_branch":"main"},"issue":{"number":7,"title":"T","body":"B","labels":[{"name":"bug"}]}}`)
	fj, prompt, ok := c.ClassifyWebhook(ctx, "gh", issueBody)
	if !ok {
		t.Fatal("labeled issue should classify ok")
	}
	if !strings.Contains(string(fj), `"number":7`) || !strings.Contains(string(fj), `"title":"T"`) {
		t.Errorf("forge_job = %s", fj)
	}
	if !strings.Contains(prompt, "issue #7 in o/r") || !strings.Contains(prompt, "T") {
		t.Errorf("prompt = %q", prompt)
	}

	// Non-forge event (issue closed) → ok=false.
	if _, _, ok := c.ClassifyWebhook(ctx, "gh", []byte(`{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":7}}`)); ok {
		t.Error("issue closed should not classify")
	}
	// Project with no forge configured → ok=false.
	if _, _, ok := c.ClassifyWebhook(ctx, "none", issueBody); ok {
		t.Error("project without forge should not classify")
	}
	// Nil classifier → ok=false (no panic).
	if _, _, ok := (*forgeWebhookClassifier)(nil).ClassifyWebhook(ctx, "gh", issueBody); ok {
		t.Error("nil classifier should be ok=false")
	}
}

func TestNewForgeClassifier(t *testing.T) {
	if (&Container{}).newForgeClassifier() != nil {
		t.Error("no registry → nil classifier")
	}
	if (&Container{Registry: &registry.Registry{}}).newForgeClassifier() == nil {
		t.Error("with registry → classifier built")
	}
}
