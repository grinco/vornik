// Package runtime provides container models for the runtime manager.
package runtime

import (
	"fmt"
	"os"
	"strings"
)

// networkHostAllowed reports whether `network: host` roles are permitted.
// Host networking shares the daemon host's network namespace, defeating the
// per-task sandbox isolation, so it's DENIED BY DEFAULT — a role can't request
// it just by setting it in swarm YAML. An operator opts in explicitly via
// VORNIK_ALLOW_NETWORK_HOST=1 (a deliberate, host-level trust decision).
func networkHostAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORNIK_ALLOW_NETWORK_HOST"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// LifecyclePolicy defines how containers are managed after task completion.
type LifecyclePolicy string

const (
	// PolicyEphemeral means the container is stopped and removed after task completion.
	PolicyEphemeral LifecyclePolicy = "ephemeral"

	// PolicyWarm means the container may be reused for future tasks.
	PolicyWarm LifecyclePolicy = "warm"
)

// Status represents the current state of a container.
type Status string

const (
	StatusRunning  Status = "running"
	StatusStopped  Status = "stopped"
	StatusExited   Status = "exited"
	StatusPaused   Status = "paused"
	StatusUnknown  Status = "unknown"
	StatusNotFound Status = "not_found"
)

// Container represents a managed agent container.
type Container struct {
	// ID is the container ID (full or short form from podman).
	ID string `json:"id"`

	// Name is the container name following vornik naming convention.
	Name string `json:"name"`

	// Image is the container image reference.
	Image string `json:"image"`

	// Status is the current container status.
	Status Status `json:"status"`

	// ProjectID is the project this container belongs to.
	ProjectID string `json:"projectId"`

	// Role is the agent role (e.g., "coder", "tester").
	Role string `json:"role"`

	// TaskID is the task this container is running.
	TaskID string `json:"taskId"`

	// ExitCode is the container exit code (if exited).
	ExitCode int `json:"exitCode,omitempty"`
}

// ContainerConfig defines the configuration for starting a new container.
type ContainerConfig struct {
	// Image is the container image to run.
	Image string `json:"image"`

	// ProjectID is the project this container belongs to.
	ProjectID string `json:"projectId"`

	// Role is the agent role.
	Role string `json:"role"`

	// TaskID is the task this container will run.
	TaskID string `json:"taskId"`

	// LifecyclePolicy determines container reuse behavior.
	LifecyclePolicy LifecyclePolicy `json:"lifecyclePolicy,omitempty"`

	// EnvVars are environment variables to inject into the container.
	EnvVars map[string]string `json:"envVars,omitempty"`

	// CPUQuota is the CPU quota in microseconds (100000 = 1 CPU).
	// 0 means no limit.
	CPUQuota int64 `json:"cpuQuota,omitempty"`

	// MemoryLimit is the memory limit in bytes.
	// 0 means no limit.
	MemoryLimit int64 `json:"memoryLimit,omitempty"`

	// InputDir is the host path for /app/input mount.
	InputDir string `json:"inputDir,omitempty"`

	// OutputDir is the host path for /app/output mount.
	OutputDir string `json:"outputDir,omitempty"`

	// WorkspaceDir is the host path for /app/workspace mount.
	WorkspaceDir string `json:"workspaceDir,omitempty"`

	// ProjectDir is the host path for /app/workspace/project mount (persistent per-project workspace).
	ProjectDir string `json:"projectDir,omitempty"`

	// ProjectGitDir is the host path of the project's main .git directory.
	// When set, the runtime adds an extra bind mount at the same host path
	// inside the container. This is needed when ProjectDir points at a git
	// worktree: the worktree's .git file holds an absolute host path to
	// <projectRoot>/.git/worktrees/<branch>, and without a matching mount
	// git commands from inside the container fail with "not a git repository".
	// Leave empty for non-git or non-worktree workspaces.
	ProjectGitDir string `json:"projectGitDir,omitempty"`

	// TimeoutSeconds is the maximum execution time.
	// 0 means no timeout (handled by executor).
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// Network selects the container's network policy. Empty
	// (NetworkDefault) preserves the historical behavior: no --network
	// flag is appended and rootless podman falls back to slirp4netns
	// with outbound egress. Roles can opt into stricter modes.
	//
	// This field is the Step-A half of the agent-container network
	// policy rollout (see https://docs.vornik.io finding #1
	// and the mitigation plan §7.1). Step A is additive: the default
	// stays permissive so no existing role breaks. Step B flips
	// no-egress roles to NetworkDaemonOnly once the mechanism bakes.
	// See https://docs.vornik.io § 5.
	Network NetworkMode `json:"network,omitempty"`
}

// NetworkMode is the per-container network policy.
type NetworkMode string

const (
	// NetworkDefault appends no --network flag, preserving rootless
	// podman's default (slirp4netns with outbound egress). This is the
	// backward-compatible default for roles that don't set a policy.
	NetworkDefault NetworkMode = ""
	// NetworkHost shares the host network namespace (--network=host).
	NetworkHost NetworkMode = "host"
	// NetworkNone disables networking entirely (--network=none). The
	// container cannot reach the daemon or any external host.
	NetworkNone NetworkMode = "none"
	// NetworkDaemonOnly gives the container NO network device
	// (--network=none) and instead bind-mounts the daemon's unix socket
	// so the agent can still reach the daemon for MCP tool calls AND LLM
	// completions (both legs target the daemon — see
	// runtime.agent_llm.endpoint), while having zero route to the
	// internet. This is the Step-B realisation (mitigation plan §7.1
	// step B): on rootless podman there is no network flag that allows
	// the host daemon but blocks the internet (slirp4netns/pasta
	// host-loopback grants both), so true egress restriction requires
	// removing the network device and reaching the daemon over a socket.
	//
	// Precondition: daemon-only is correct only when the agent's daemon
	// + LLM endpoints both point at the daemon (the default deployment
	// shape). A role whose LLM endpoint is a direct external URL must
	// use NetworkHost instead — daemon-only would cut its LLM access.
	// The Manager rewrites VORNIK_API_URL / VORNIK_LLM_ENDPOINT to the
	// mounted socket and fails closed (still --network=none) if no
	// daemon socket is configured.
	NetworkDaemonOnly NetworkMode = "daemon-only"
	// NetworkEgress is the explicit "permissive egress, isolated
	// namespace" policy — functionally the same as appending no
	// --network flag (rootless pasta/slirp4netns: NAT outbound to the
	// internet AND daemon reachability via host.containers.internal, but
	// the container keeps its OWN network namespace and does NOT share
	// host services the way NetworkHost does). Its purpose is to let a
	// role opt OUT of a daemon-only default (runtime.default_network)
	// when it genuinely needs the internet — e.g. build/coder roles
	// running `pip install`, `go mod download`, npm, `git fetch` —
	// without the blast radius of host-namespace sharing. Distinct from
	// NetworkDefault ("") which now means "inherit the configured
	// default"; NetworkEgress always means "permissive egress".
	NetworkEgress NetworkMode = "egress"
)

// DaemonOnlySocketContainerPath is the in-container path at which the
// Manager bind-mounts the daemon's unix socket for NetworkDaemonOnly
// containers. The filename ends in ".sock" so consumers (mcp-bridge,
// the agent entrypoint) can split a "unix://<path>/<httpPath>" endpoint
// deterministically. Kept in sync with the daemon's server.unix_socket
// host path, which is bind-mounted here.
const DaemonOnlySocketContainerPath = "/run/vornik/vornik.sock"

// podmanNetworkArg returns the --network value for this mode, and a
// bool indicating whether a --network flag should be appended at all.
// NetworkDefault returns ("", false) so callers omit the flag and
// preserve podman's rootless default. Unknown values are treated as
// NetworkDefault (omit the flag) — Validate rejects them earlier, so
// reaching here with a bad value means a programmatic caller bypassed
// validation; failing open to current behavior is safer than emitting
// a malformed argv.
func (n NetworkMode) podmanNetworkArg() (string, bool) {
	switch n {
	case NetworkHost:
		return "host", true
	case NetworkNone:
		return "none", true
	case NetworkDaemonOnly:
		// No network device. Daemon reachability is provided out-of-band
		// by the Manager bind-mounting the daemon unix socket (see
		// DaemonOnlySocketContainerPath). Failing to "none" here means a
		// caller that bypasses the Manager's socket wiring still gets
		// zero egress rather than the old egress-permitting placeholder.
		return "none", true
	case NetworkEgress:
		// Explicit permissive egress: omit the flag (rootless pasta/
		// slirp4netns default) — internet + daemon, isolated namespace.
		return "", false
	default:
		return "", false
	}
}

// ValidNetworkMode reports whether s is a recognized network policy.
func ValidNetworkMode(s NetworkMode) bool {
	switch s {
	case NetworkDefault, NetworkHost, NetworkNone, NetworkDaemonOnly, NetworkEgress:
		return true
	default:
		return false
	}
}

// ContainerName generates the standard container name following vornik naming convention.
// Format: vornik-<project>-<role>-<taskId>
func ContainerName(projectID, role, taskID string) string {
	// Sanitize components to be valid container names (lowercase, alphanumeric, dashes)
	project := sanitizeNamePart(projectID)
	rolePart := sanitizeNamePart(role)
	task := sanitizeNamePart(taskID)
	return fmt.Sprintf("vornik-%s-%s-%s", project, rolePart, task)
}

// sanitizeNamePart converts a string to a valid container name component.
// Container names must be lowercase, alphanumeric with dashes allowed.
func sanitizeNamePart(s string) string {
	// Convert to lowercase
	result := strings.ToLower(s)

	// Replace invalid characters with dashes
	var builder strings.Builder
	for _, r := range result {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		} else {
			builder.WriteRune('-')
		}
	}

	// Trim leading/trailing dashes and collapse multiple dashes
	result = builder.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	result = strings.Trim(result, "-")

	// Ensure non-empty
	if result == "" {
		return "unknown"
	}

	// Truncate if too long (container names have a 64-character limit including prefix)
	if len(result) > 50 {
		result = result[:50]
	}

	return result
}

// Validate checks that the container config has all required fields.
func (c *ContainerConfig) Validate() error {
	c.Image = strings.TrimSpace(c.Image)
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	c.Role = strings.TrimSpace(c.Role)
	c.TaskID = strings.TrimSpace(c.TaskID)
	if c.Image == "" {
		return fmt.Errorf("image is required")
	}
	if strings.HasPrefix(c.Image, "-") {
		return fmt.Errorf("image must not start with '-'")
	}
	if c.ProjectID == "" {
		return fmt.Errorf("projectId is required")
	}
	if c.Role == "" {
		return fmt.Errorf("role is required")
	}
	if c.TaskID == "" {
		return fmt.Errorf("taskId is required")
	}
	if !ValidNetworkMode(c.Network) {
		return fmt.Errorf("network must be one of '', host, none, daemon-only, egress (got %q)", c.Network)
	}
	// Sandbox-escape guard: `network: host` is denied by default (it shares
	// the daemon host's netns). Requires an explicit operator opt-in so an
	// untrusted/templated role can't break isolation via swarm YAML.
	if c.Network == NetworkHost && !networkHostAllowed() {
		return fmt.Errorf("network: host is disabled by default (sandbox isolation); set VORNIK_ALLOW_NETWORK_HOST=1 to permit it for role %q", c.Role)
	}
	return nil
}

// DefaultContainerConfig returns a ContainerConfig with sensible defaults.
func DefaultContainerConfig() *ContainerConfig {
	return &ContainerConfig{
		LifecyclePolicy: PolicyEphemeral,
	}
}
