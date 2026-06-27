package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewUsesConfiguredPodmanAndOptions(t *testing.T) {
	podmanPath := writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "version" ]]; then
  echo '{"Client":{"Version":"5.0.0"}}'
  exit 0
fi
exit 1
`)

	manager, err := New(
		WithPodmanPath(podmanPath),
		WithDefaultTimeout(2*time.Second),
		WithUserNSMode(" keep-id "),
		WithAllowHostUserns(true),
		WithRunAsUser(" 1000:1000 "),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if manager.podmanPath != podmanPath {
		t.Fatalf("New() podmanPath = %q, want %q", manager.podmanPath, podmanPath)
	}
	if manager.defaultTimeout != 2*time.Second {
		t.Fatalf("New() defaultTimeout = %s", manager.defaultTimeout)
	}
	if manager.userNSMode != "keep-id" {
		t.Fatalf("New() userNSMode = %q", manager.userNSMode)
	}
	if !manager.allowHostUserns {
		t.Fatal("New() did not apply WithAllowHostUserns")
	}
	if manager.runAsUser != "1000:1000" {
		t.Fatalf("New() runAsUser = %q", manager.runAsUser)
	}
}

func TestNewWrapsPodmanVersionFailure(t *testing.T) {
	podmanPath := writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "version" ]]; then
  echo "podman socket unavailable"
  exit 42
fi
exit 1
`)

	_, err := New(WithPodmanPath(podmanPath))
	if err == nil {
		t.Fatal("expected New() error")
	}
	var unavailable *PodmanNotAvailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected PodmanNotAvailableError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "podman version check failed") || !strings.Contains(err.Error(), "podman socket unavailable") {
		t.Fatalf("expected version failure output in error, got %q", err.Error())
	}
}

func TestStartContainerRetriesKeepIDOnRootlessUserNSError(t *testing.T) {
	manager := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "run" ]]; then
  shift
  for arg in "$@"; do
    if [[ "$arg" == "keep-id" ]]; then
      echo "container-123"
      exit 0
    fi
  done
  printf '%s\n' 'Error: invalid internal status, unable to create a new pause process: cannot set up namespace using "/usr/bin/newuidmap": exit status 1'
  exit 125
fi
echo '{"Client":{"Version":"5.0.0"}}'
`)}

	containerID, err := manager.StartContainer(context.Background(), &ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "worker",
		TaskID:    "task-1",
	})
	if err != nil {
		t.Fatalf("StartContainer() error = %v", err)
	}
	if containerID != "container-123" {
		t.Fatalf("unexpected container ID %q", containerID)
	}
}

func TestStartContainerUsesConfiguredUserNSMode(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "args.log")
	manager := &Manager{
		podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "run" ]]; then
  printf '%s\n' "$@" > "`+logPath+`"
  echo "container-456"
  exit 0
fi
echo '{"Client":{"Version":"5.0.0"}}'
`),
		userNSMode: "host",
	}

	_, err := manager.StartContainer(context.Background(), &ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "worker",
		TaskID:    "task-2",
	})
	if err != nil {
		t.Fatalf("StartContainer() error = %v", err)
	}

	argsBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read args log: %v", err)
	}
	args := string(argsBytes)
	if !strings.Contains(args, "--userns") || !strings.Contains(args, "host") {
		t.Fatalf("expected --userns host in args, got %q", args)
	}
}

func TestStartContainerReturnsActionableRootlessUserNSError(t *testing.T) {
	manager := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "run" ]]; then
  printf '%s\n' 'newuidmap: write to uid_map failed: Operation not permitted'
  exit 125
fi
echo '{"Client":{"Version":"5.0.0"}}'
`)}

	_, err := manager.StartContainer(context.Background(), &ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "worker",
		TaskID:    "task-3",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rootless podman user namespace setup failed") {
		t.Fatalf("expected actionable rootless message, got %q", msg)
	}
	if !strings.Contains(msg, "runtime.userns_mode: host") {
		t.Fatalf("expected runtime.userns_mode guidance, got %q", msg)
	}
}

// TestBuildPreImageArgsSetsHardeningFlags locks in the container hardening
// flags. Any regression that drops no-new-privileges or the full capability
// drop should fail this test before it reaches production.
func TestBuildPreImageArgsSetsHardeningFlags(t *testing.T) {
	m := &Manager{}
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "worker",
		TaskID:    "task-harden",
	})

	joined := strings.Join(args, " ")
	wantPairs := [][2]string{
		{"--security-opt", "no-new-privileges"},
		{"--cap-drop", "ALL"},
	}
	for _, pair := range wantPairs {
		want := pair[0] + " " + pair[1]
		if !strings.Contains(joined, want) {
			t.Errorf("buildPreImageArgs missing %q; got %q", want, joined)
		}
	}

	// Without WithRunAsUser the manager must not emit --user. The image's
	// USER directive owns the identity in that mode; injecting --user would
	// break rootless images that rely on their own uid mapping.
	if strings.Contains(joined, "--user ") {
		t.Errorf("buildPreImageArgs unexpectedly set --user when runAsUser is empty: %q", joined)
	}
}

// TestBuildPreImageArgsNetworkModes pins the per-role network policy
// (mitigation plan §7.1 step A). The default (empty) mode must append
// NO --network flag so rootless podman keeps its slirp4netns default —
// changing that would be a breaking change for every existing role.
// The explicit modes each map to a specific --network value.
func TestBuildPreImageArgsNetworkModes(t *testing.T) {
	cases := []struct {
		name      string
		mode      NetworkMode
		wantFlag  bool
		wantValue string
	}{
		{"default omits flag", NetworkDefault, false, ""},
		{"none", NetworkNone, true, "none"},
		{"host", NetworkHost, true, "host"},
		// Step B: daemon-only now maps to --network=none (the daemon is
		// reached over a bind-mounted socket, see the dedicated tests),
		// NOT the old egress-permitting slirp4netns placeholder.
		{"daemon-only", NetworkDaemonOnly, true, "none"},
		// egress = explicit permissive: omit the flag (isolated egress).
		{"egress", NetworkEgress, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{}
			args := m.buildPreImageArgs(&ContainerConfig{
				Image:     "alpine:latest",
				ProjectID: "proj",
				Role:      "worker",
				TaskID:    "task-net",
				Network:   tc.mode,
			})
			joined := strings.Join(args, " ")
			hasFlag := strings.Contains(joined, "--network")
			if hasFlag != tc.wantFlag {
				t.Fatalf("mode %q: --network present=%v want=%v; args=%q", tc.mode, hasFlag, tc.wantFlag, joined)
			}
			if tc.wantFlag && !strings.Contains(joined, "--network "+tc.wantValue) {
				t.Fatalf("mode %q: missing --network %s; args=%q", tc.mode, tc.wantValue, joined)
			}
		})
	}
}

// TestContainerConfigValidateNetworkMode confirms Validate rejects
// unknown network modes and accepts the four valid ones.
func TestContainerConfigValidateNetworkMode(t *testing.T) {
	// `host` is a valid MODE; the deny-by-default policy gate is orthogonal
	// (covered by TestContainerConfig_Validate). Opt in here so this test
	// asserts mode-validity, not the policy.
	t.Setenv("VORNIK_ALLOW_NETWORK_HOST", "1")
	base := func(n NetworkMode) *ContainerConfig {
		return &ContainerConfig{Image: "alpine", ProjectID: "p", Role: "r", TaskID: "t", Network: n}
	}
	for _, ok := range []NetworkMode{NetworkDefault, NetworkHost, NetworkNone, NetworkDaemonOnly, NetworkEgress} {
		if err := base(ok).Validate(); err != nil {
			t.Errorf("valid network %q rejected: %v", ok, err)
		}
	}
	if err := base("bridge").Validate(); err == nil {
		t.Error("expected unknown network mode 'bridge' to be rejected")
	}
}

// TestBuildPreImageArgsWithProjectGitDir locks in the extra bind mount the
// runtime emits when ProjectDir is a git worktree. Without this mount the
// worktree's .git file (which holds an absolute host path) can't be
// resolved inside the container and every git command fails with "not a
// git repository".
func TestBuildPreImageArgsWithProjectGitDir(t *testing.T) {
	m := &Manager{}
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:         "alpine:latest",
		ProjectID:     "proj",
		Role:          "coder",
		TaskID:        "task-git",
		ProjectDir:    "/host/projects/acme/.worktrees/task-git",
		ProjectGitDir: "/host/projects/acme/.git",
	})

	// Read-only mount: agent container reads .git for object storage
	// + ref lookup, but writes happen host-side via mergeWorktree.
	// Tightened from rw to ro by the 2026-05-05 container-isolation
	// security commit.
	wantMount := "/host/projects/acme/.git:/host/projects/acme/.git:ro,z"
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--volume" && args[i+1] == wantMount {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected volume mount %q when ProjectGitDir is set; got %v", wantMount, args)
	}
}

// TestBuildPreImageArgsNoProjectGitDir ensures the extra mount is only
// added when explicitly configured — non-worktree projects shouldn't see
// a spurious --volume flag that would point at a missing directory.
func TestBuildPreImageArgsNoProjectGitDir(t *testing.T) {
	m := &Manager{}
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:      "alpine:latest",
		ProjectID:  "proj",
		Role:       "coder",
		TaskID:     "task-plain",
		ProjectDir: "/host/projects/acme",
	})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, ".git:/") && strings.Contains(joined, ".git:rw,z") {
		t.Errorf("buildPreImageArgs added a .git bind mount when ProjectGitDir was empty; args: %q", joined)
	}
}

// TestBuildPreImageArgsWithRunAsUser ensures the configured --user value
// reaches the podman arg list. This is the operator's escape hatch for
// images that still default to root.
func TestBuildPreImageArgsWithRunAsUser(t *testing.T) {
	m := &Manager{runAsUser: "1000:1000"}
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "worker",
		TaskID:    "task-runas",
	})

	// We scan arg pairs because the flag and value live in separate
	// positional slots: ["--user", "1000:1000"].
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--user" && args[i+1] == "1000:1000" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --user 1000:1000 in args, got %v", args)
	}
}

func TestBuildPreImageArgsAddsManagedLabel(t *testing.T) {
	m := &Manager{}
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "worker",
		TaskID:    "task-managed",
	})

	want := "--label vornik.managed=true"
	if !strings.Contains(strings.Join(args, " "), want) {
		t.Fatalf("expected %q in args, got %v", want, args)
	}
}

func TestRedactPodmanArgsRedactsEnvValues(t *testing.T) {
	args := []string{
		"run",
		"--env", "VORNIK_LLM_API_KEY=secret-key",
		"--env", "PATH=/usr/bin",
		"--label", "vornik.managed=true",
	}

	got := redactPodmanArgs(args)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "secret-key") || strings.Contains(joined, "/usr/bin") {
		t.Fatalf("redacted args leaked env value: %v", got)
	}
	if !strings.Contains(joined, "VORNIK_LLM_API_KEY=<redacted>") {
		t.Fatalf("expected key name with redacted value, got %v", got)
	}

	original := strings.Join(args, " ")
	if !strings.Contains(original, "secret-key") {
		t.Fatalf("redactPodmanArgs mutated input slice: %v", args)
	}
}

func TestRootlessAndPauseErrorDetection(t *testing.T) {
	cases := []struct {
		name         string
		output       string
		wantRootless bool
		wantPause    bool
	}{
		{"newuidmap", "newuidmap: write to uid_map failed", true, false},
		{"newgidmap", "newgidmap: write to gid_map failed", true, false},
		{"pause process", "unable to create a new pause process: cannot set up namespace", true, true},
		{"internal status", "invalid internal status: /usr/bin/newuidmap failed", true, true},
		{"ordinary failure", "image not found", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRootlessUserNSError([]byte(tc.output)); got != tc.wantRootless {
				t.Fatalf("isRootlessUserNSError() = %v, want %v", got, tc.wantRootless)
			}
			if got := isPauseProcessError([]byte(tc.output)); got != tc.wantPause {
				t.Fatalf("isPauseProcessError() = %v, want %v", got, tc.wantPause)
			}
		})
	}
}

func TestEnrichStartErrorAddsGuidanceOnlyForRootlessFailures(t *testing.T) {
	base := errors.New("exit status 125")
	rootless := enrichStartError(base, []byte("newuidmap: write to uid_map failed"))
	if !errors.Is(rootless, base) {
		t.Fatalf("rootless enriched error does not wrap base: %v", rootless)
	}
	if !strings.Contains(rootless.Error(), "runtime.userns_mode: host") {
		t.Fatalf("expected rootless guidance, got %q", rootless.Error())
	}

	ordinary := enrichStartError(base, []byte("image not found"))
	if !errors.Is(ordinary, base) {
		t.Fatalf("ordinary enriched error does not wrap base: %v", ordinary)
	}
	if strings.Contains(ordinary.Error(), "runtime.userns_mode: host") {
		t.Fatalf("ordinary error unexpectedly included rootless guidance: %q", ordinary.Error())
	}
}

func writeFakePodman(t *testing.T, script string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake podman script: %v", err)
	}
	return path
}

// TestWaitForExit_TimeoutSurfacesContextError pins the 2026-05-15
// ibkr-trader fix: when the WaitForExit timeout fires, the os/exec
// layer SIGKILLs the podman wait subprocess and CombinedOutput
// reports "signal: killed". Pre-fix that bare error reached
// executor.shouldRetry, which couldn't distinguish it from a
// transient runtime failure and wasted a within-execution retry on
// every step timeout. Post-fix WaitForExit surfaces ctx.Err()
// (context.DeadlineExceeded) so shouldRetry's existing
// errors.Is(ctx.DeadlineExceeded) branch correctly skips the
// within-exec retry; the task-level retry then gets a fresh
// wall-clock budget.
func TestWaitForExit_TimeoutSurfacesContextError(t *testing.T) {
	// Fake podman whose `wait` subcommand sleeps far past the
	// caller's timeout — the parent ctx will fire and the os/exec
	// layer will SIGKILL this script. The `exec sleep` form replaces
	// bash with sleep so the kill closes stdout/stderr promptly and
	// CombinedOutput unblocks (bash without `exec` would keep the FDs
	// open via the inherited sleep child).
	manager := &Manager{
		podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "wait" ]]; then
  exec sleep 30
fi
exit 1
`),
	}

	_, err := manager.WaitForExit(context.Background(), "container-x", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected error after deadline; got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded in error chain; got %v", err)
	}
	if !strings.Contains(err.Error(), "podman wait timed out") {
		t.Errorf("error message should mention the timeout origin, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "signal: killed") {
		t.Errorf("error message must NOT leak the os/exec SIGKILL string (that's what tripped shouldRetry), got %q", err.Error())
	}
}

// TestWaitForExit_RuntimeErrorStillReturnedAsPodmanFailure guards
// against over-correction: when the subprocess fails for reasons
// OTHER than context expiry, the original "podman wait failed"
// classification must still surface — those are the genuinely
// transient runtime failures the within-execution retry path is
// designed for.
func TestWaitForExit_RuntimeErrorStillReturnedAsPodmanFailure(t *testing.T) {
	manager := &Manager{
		podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "wait" ]]; then
  echo "Error: no container with name or ID \"container-x\" found" >&2
  exit 125
fi
exit 1
`),
	}

	// Use a generous timeout so ctx never expires; the error comes
	// from the fake podman returning a non-zero exit.
	_, err := manager.WaitForExit(context.Background(), "container-x", 5*time.Second)
	if err == nil {
		t.Fatal("expected error from failing podman wait")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("non-timeout failures must NOT report DeadlineExceeded: %v", err)
	}
	if !strings.Contains(err.Error(), "podman wait failed") {
		t.Errorf("expected 'podman wait failed' classification, got %q", err.Error())
	}
}

// TestBuildPreImageArgsDaemonOnlySocket pins the Step-B daemon-only
// wiring: with a daemon socket configured, the container gets
// --network=none, --security-opt label=disable (required for the agent
// to connect() to the host socket under SELinux), the socket bind mount,
// and VORNIK_API_URL / VORNIK_LLM_ENDPOINT / VORNIK_MEM_URL rewritten to
// the in-container socket path so it reaches the daemon with zero egress.
func TestBuildPreImageArgsDaemonOnlySocket(t *testing.T) {
	m := &Manager{daemonSocketPath: "/run/host/vornik.sock"}
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:     "alpine:latest",
		ProjectID: "proj",
		Role:      "lead",
		TaskID:    "task-do",
		Network:   NetworkDaemonOnly,
		EnvVars: map[string]string{
			"VORNIK_API_URL":      "http://host.containers.internal:8080",
			"VORNIK_LLM_ENDPOINT": "http://host.containers.internal:8080/api/v1",
			"VORNIK_MEM_URL":      "http://host.containers.internal:8080",
			"VORNIK_PROJECT_ID":   "proj",
		},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--network none",
		"--security-opt label=disable",
		"--volume /run/host/vornik.sock:" + DaemonOnlySocketContainerPath + ":ro",
		"--env VORNIK_API_URL=unix://" + DaemonOnlySocketContainerPath,
		"--env VORNIK_LLM_ENDPOINT=unix://" + DaemonOnlySocketContainerPath + "/api/v1",
		"--env VORNIK_MEM_URL=unix://" + DaemonOnlySocketContainerPath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("daemon-only args missing %q; got %q", want, joined)
		}
	}
	// The original TCP endpoints must NOT survive the rewrite.
	if strings.Contains(joined, "host.containers.internal") {
		t.Errorf("daemon-only must rewrite host.containers.internal endpoints; got %q", joined)
	}
	// Unrelated env passes through untouched.
	if !strings.Contains(joined, "--env VORNIK_PROJECT_ID=proj") {
		t.Errorf("daemon-only dropped unrelated env; got %q", joined)
	}
}

// TestBuildPreImageArgsDaemonOnlyFailClosed verifies that daemon-only
// with NO configured socket still removes the network (--network=none)
// and does NOT mount a socket, disable SELinux, or rewrite endpoints —
// failing closed (no daemon access) rather than open (silent egress).
func TestBuildPreImageArgsDaemonOnlyFailClosed(t *testing.T) {
	m := &Manager{} // no daemonSocketPath
	args := m.buildPreImageArgs(&ContainerConfig{
		Image: "alpine:latest", ProjectID: "p", Role: "r", TaskID: "t",
		Network: NetworkDaemonOnly,
		EnvVars: map[string]string{"VORNIK_API_URL": "http://host.containers.internal:8080"},
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--network none") {
		t.Errorf("fail-closed must still set --network none; got %q", joined)
	}
	for _, unwanted := range []string{"--volume " + "", "label=disable", "unix://"} {
		if unwanted != "" && strings.Contains(joined, unwanted) {
			t.Errorf("fail-closed must not emit %q; got %q", unwanted, joined)
		}
	}
	// Endpoint must be left as-is (no socket to rewrite to).
	if !strings.Contains(joined, "--env VORNIK_API_URL=http://host.containers.internal:8080") {
		t.Errorf("fail-closed must leave endpoint unchanged; got %q", joined)
	}
}

// TestDaemonOnlyEnv unit-tests the endpoint rewrite in isolation.
func TestDaemonOnlyEnv(t *testing.T) {
	in := map[string]string{
		"VORNIK_API_URL":      "http://host.containers.internal:8080",
		"VORNIK_LLM_ENDPOINT": "http://host.containers.internal:8080/api/v1",
		"VORNIK_MEM_URL":      "http://host.containers.internal:8080",
		"OTHER":               "keep",
	}
	// Non-daemon-only: passthrough (same map).
	if got := daemonOnlyEnv(in, NetworkHost, "/run/x.sock"); &got == &in && len(got) != len(in) {
		t.Fatal("host mode should pass env through")
	}
	if got := daemonOnlyEnv(in, NetworkHost, "/run/x.sock"); got["VORNIK_API_URL"] != in["VORNIK_API_URL"] {
		t.Errorf("host mode rewrote VORNIK_API_URL: %q", got["VORNIK_API_URL"])
	}
	// daemon-only without socket: passthrough.
	if got := daemonOnlyEnv(in, NetworkDaemonOnly, ""); got["VORNIK_API_URL"] != in["VORNIK_API_URL"] {
		t.Errorf("no-socket must not rewrite: %q", got["VORNIK_API_URL"])
	}
	// daemon-only with socket: rewrite the three daemon endpoints.
	got := daemonOnlyEnv(in, NetworkDaemonOnly, "/run/x.sock")
	want := map[string]string{
		"VORNIK_API_URL":      "unix://" + DaemonOnlySocketContainerPath,
		"VORNIK_LLM_ENDPOINT": "unix://" + DaemonOnlySocketContainerPath + "/api/v1",
		"VORNIK_MEM_URL":      "unix://" + DaemonOnlySocketContainerPath,
		"OTHER":               "keep",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("daemonOnlyEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
	// Must not mutate the input map.
	if in["VORNIK_API_URL"] != "http://host.containers.internal:8080" {
		t.Error("daemonOnlyEnv mutated the caller's map")
	}
}

// TestNetworkDaemonOnlyPodmanArg confirms the enum maps to none (fail
// closed), never the old egress-permitting placeholder.
func TestNetworkDaemonOnlyPodmanArg(t *testing.T) {
	v, ok := NetworkDaemonOnly.podmanNetworkArg()
	if !ok || v != "none" {
		t.Fatalf("daemon-only podmanNetworkArg = (%q,%v), want (\"none\",true)", v, ok)
	}
}

// TestPoolKeyStringIncludesNetwork ensures the network policy is part of
// the warm-pool key so a daemon-only container is never reused for a
// host role (and vice-versa).
func TestPoolKeyStringIncludesNetwork(t *testing.T) {
	a := PoolKey{ProjectID: "p", Role: "r", Image: "img", Network: NetworkDaemonOnly}
	b := PoolKey{ProjectID: "p", Role: "r", Image: "img", Network: NetworkHost}
	if a.String() == b.String() {
		t.Fatalf("pool keys with different Network must differ: %q == %q", a.String(), b.String())
	}
	if !strings.Contains(a.String(), "daemon-only") {
		t.Errorf("pool key string should encode the network mode: %q", a.String())
	}
}

// TestBuildPreImageArgsDefaultNetwork covers runtime.default_network:
// a role with no explicit Network inherits the manager default, while
// an explicit role Network always wins (the opt-out path). This is how
// "daemon-only by default for all roles" works without per-role config.
func TestBuildPreImageArgsDefaultNetwork(t *testing.T) {
	// Default daemon-only + a socket: an unset-Network role gets the full
	// daemon-only treatment (--network none + label=disable + socket mount).
	m := &Manager{daemonSocketPath: "/run/host/vornik.sock", defaultNetwork: NetworkDaemonOnly}
	args := strings.Join(m.buildPreImageArgs(&ContainerConfig{
		Image: "alpine", ProjectID: "p", Role: "lead", TaskID: "t",
		// Network intentionally unset → inherits the default.
		EnvVars: map[string]string{"VORNIK_API_URL": "http://host.containers.internal:8080"},
	}), " ")
	for _, want := range []string{
		"--network none", "--security-opt label=disable",
		"--volume /run/host/vornik.sock:" + DaemonOnlySocketContainerPath + ":ro",
		"--env VORNIK_API_URL=unix://" + DaemonOnlySocketContainerPath,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("default-network daemon-only: missing %q; got %q", want, args)
		}
	}

	// Explicit role Network overrides the default (opt-out): a build role
	// asking for host must NOT be forced to daemon-only.
	hostArgs := strings.Join(m.buildPreImageArgs(&ContainerConfig{
		Image: "alpine", ProjectID: "p", Role: "coder", TaskID: "t",
		Network: NetworkHost,
		EnvVars: map[string]string{"VORNIK_API_URL": "http://host.containers.internal:8080"},
	}), " ")
	if !strings.Contains(hostArgs, "--network host") {
		t.Errorf("explicit network:host must override the daemon-only default; got %q", hostArgs)
	}
	if strings.Contains(hostArgs, "label=disable") || strings.Contains(hostArgs, "unix://") {
		t.Errorf("host role must not get daemon-only socket wiring; got %q", hostArgs)
	}

	// No default set (legacy): an unset-Network role appends no --network
	// flag (permissive), preserving historical behaviour.
	legacy := &Manager{}
	legacyArgs := strings.Join(legacy.buildPreImageArgs(&ContainerConfig{
		Image: "alpine", ProjectID: "p", Role: "r", TaskID: "t",
	}), " ")
	if strings.Contains(legacyArgs, "--network") {
		t.Errorf("no default + unset role network must omit --network; got %q", legacyArgs)
	}
}

func TestWithDefaultNetworkInvalidFallsClosed(t *testing.T) {
	m := &Manager{daemonSocketPath: "/run/host/vornik.sock"}
	WithDefaultNetwork("daemononly")(m)
	if m.defaultNetwork != NetworkDaemonOnly {
		t.Fatalf("invalid default network should fall closed to daemon-only, got %q", m.defaultNetwork)
	}
	args := strings.Join(m.buildPreImageArgs(&ContainerConfig{
		Image: "alpine", ProjectID: "p", Role: "lead", TaskID: "t",
		EnvVars: map[string]string{"VORNIK_API_URL": "http://host.containers.internal:8080"},
	}), " ")
	if !strings.Contains(args, "--network none") || !strings.Contains(args, "unix://"+DaemonOnlySocketContainerPath) {
		t.Fatalf("invalid default network did not produce daemon-only args: %q", args)
	}
}
