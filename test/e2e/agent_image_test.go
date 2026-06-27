//go:build e2e
// +build e2e

// Package e2e_test contains end-to-end tests that shell out to real
// podman and build real container images. They are gated with the
// `e2e` build tag so `go test ./...` in development is unaffected.
//
// Run with:
//
//	make test-e2e
package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// agentImageTag is the local image tag the smoke test builds and
	// runs. It's deliberately distinct from :latest so a developer's
	// working image is not clobbered.
	agentImageTag = "localhost/vornik-agent:e2e-smoke"

	// Canned task.json that the entrypoint can parse without talking
	// to an LLM. The presence of /app/input/CANCEL short-circuits
	// main() into the cancellation path, so the LLM endpoint is
	// never contacted.
	cannedTaskJSON = `{
  "taskId": "smoke-test-task",
  "swarm": {"role": "coder"},
  "workflow": {"stepId": "smoke"},
  "context": {"prompt": "noop"}
}`
)

// TestAgentImageSmokeBuildsAndRunsAsNonRoot rebuilds images/vornik-agent
// and exercises the entrypoint end-to-end against a canned task. It
// confirms three things that matter post-hardening:
//
//  1. The image still builds after the USER directive landed.
//  2. The container process runs as uid 1000 (not root).
//  3. The agent can read /app/input, write /app/output, and write
//     artifacts into /app/workspace — i.e. the chown in the
//     Containerfile actually covers every path the entrypoint touches.
//
// Uses the CANCEL sentinel to avoid needing a real LLM endpoint.
func TestAgentImageSmokeBuildsAndRunsAsNonRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("agent image smoke test requires Linux + podman")
	}
	requirePodman(t)

	repoRoot := findRepoRoot(t)

	// Build the image using the current process's uid/gid so bind
	// mounts work under --userns=host later in the test — this
	// mirrors what `make build-agent` does for a local operator.
	// The test is deterministic across developers and CI because we
	// always use os.Getuid/os.Getgid rather than hardcoding 1000.
	uid := os.Getuid()
	gid := os.Getgid()
	buildCmd := exec.Command(
		"podman", "build",
		"-f", "images/vornik-agent/Containerfile",
		"--build-arg", fmt.Sprintf("VORNIK_UID=%d", uid),
		"--build-arg", fmt.Sprintf("VORNIK_GID=%d", gid),
		"-t", agentImageTag,
		".",
	)
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("podman build failed: %v\n%s", err, out)
	}

	// Identity probe — run `id` as the image's default user and
	// assert it matches the uid we built for and is not root.
	// Runs before the task smoke test so a USER regression fails
	// fast with an unambiguous error.
	idOut, err := exec.Command(
		"podman", "run", "--rm",
		"--entrypoint", "id",
		agentImageTag,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("podman run id failed: %v\n%s", err, idOut)
	}
	wantUID := fmt.Sprintf("uid=%d", uid)
	if !strings.Contains(string(idOut), wantUID) {
		t.Fatalf("agent image must run as %s (the build uid); got: %s", wantUID, idOut)
	}
	if uid != 0 && strings.Contains(string(idOut), "uid=0(") {
		t.Fatalf("agent image is still running as root: %s", idOut)
	}

	// Real task run. We set up a host-side workspace that mirrors
	// what the runtime manager mounts at /app/input, /app/output,
	// and /app/workspace.
	scratch := t.TempDir()
	inputDir := filepath.Join(scratch, "input")
	outputDir := filepath.Join(scratch, "output")
	workspaceDir := filepath.Join(scratch, "workspace")
	for _, d := range []string{inputDir, outputDir, workspaceDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Canned task plus a CANCEL sentinel. The entrypoint sees
	// CANCEL before touching the LLM and writes a CANCELLED
	// result.json.
	if err := os.WriteFile(filepath.Join(inputDir, "task.json"), []byte(cannedTaskJSON), 0o644); err != nil {
		t.Fatalf("write task.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "CANCEL"), []byte("1"), 0o644); err != nil {
		t.Fatalf("write CANCEL: %v", err)
	}

	runCmd := exec.Command(
		"podman", "run", "--rm",
		// Match production runtime flags. If any of these start
		// blocking legitimate agent behaviour we want the test to
		// surface it, not paper over it.
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		// --userns=keep-id maps the container's uid (set by
		// VORNIK_UID at build time above) back to the invoking
		// process's host uid. Without this, in rootless podman the
		// container's non-zero uid is routed through subuids and
		// writes land as a uid the operator doesn't own — which
		// breaks every bind mount.
		// --userns=host is only correct when the image runs as
		// uid 0; see manager.go's auto-fallback logic for the
		// production path.
		"--userns", fmt.Sprintf("keep-id:uid=%d,gid=%d", uid, gid),
		"--volume", fmt.Sprintf("%s:/app/input:rw,Z", inputDir),
		"--volume", fmt.Sprintf("%s:/app/output:rw,Z", outputDir),
		"--volume", fmt.Sprintf("%s:/app/workspace:rw,Z", workspaceDir),
		agentImageTag,
	)
	runCmd.Env = append(os.Environ(),
		// The entrypoint inspects these but short-circuits on
		// CANCEL before actually calling out, so dummy values are
		// fine.
		"VORNIK_LLM_ENDPOINT=http://127.0.0.1:1",
		"VORNIK_LLM_MODEL=noop",
	)
	// Cap wall-clock time in case the entrypoint regresses and the
	// cancel short-circuit no longer fires.
	done := make(chan error, 1)
	go func() {
		out, err := runCmd.CombinedOutput()
		if err != nil {
			done <- fmt.Errorf("podman run failed: %v\n%s", err, out)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		_ = runCmd.Process.Kill()
		t.Fatal("podman run exceeded 60s — cancel short-circuit likely broken")
	}

	// Assert the container produced a well-formed CANCELLED result.
	resultBytes, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatalf("result.json missing after run: %v", err)
	}
	var result struct {
		Status      string `json:"status"`
		Message     string `json:"message"`
		Diagnostics struct {
			ExitCode int `json:"exitCode"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("result.json malformed: %v\n%s", err, resultBytes)
	}
	if result.Status != "CANCELLED" {
		t.Fatalf("expected status=CANCELLED, got %q; body=%s", result.Status, resultBytes)
	}

	// The artifact directory must exist and hold the per-step
	// response file — this proves the non-root user could create
	// and write into /app/workspace/artifacts/out/. In the cancel
	// path the entrypoint short-circuits before reading
	// workflow.stepId from task.json, so STEP_ID stays at its
	// initialisation default "unknown" and the artifact is named
	// accordingly.
	artifactPath := filepath.Join(workspaceDir, "artifacts", "out", "unknown-response.md")
	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("expected per-step artifact at %s: %v", artifactPath, err)
	}
}

// requirePodman skips the test if podman is not on PATH. E2E tests
// must be opt-in; developers without podman shouldn't be surprised by
// test failures when they run make test-e2e in another context.
func requirePodman(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available on PATH")
	}
}

// findRepoRoot walks up from the test file until it finds go.mod, so
// the test works regardless of which directory `go test` was invoked
// from. Keeps the test self-contained rather than depending on the
// caller's working directory.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod walking up from working directory")
		}
		dir = parent
	}
}
