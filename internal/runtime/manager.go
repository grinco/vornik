// Package runtime provides Podman container lifecycle management for vornik.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// PodmanNotAvailableError is returned when podman is not found or not running.
type PodmanNotAvailableError struct {
	Err error
}

func (e *PodmanNotAvailableError) Error() string {
	return fmt.Sprintf("podman not available: %v", e.Err)
}

func (e *PodmanNotAvailableError) Unwrap() error {
	return e.Err
}

// ContainerNotFoundError is returned when a container is not found.
type ContainerNotFoundError struct {
	ContainerID string
}

func (e *ContainerNotFoundError) Error() string {
	return fmt.Sprintf("container not found: %s", e.ContainerID)
}

// Manager controls local Podman agent runtimes.
// It is stateless - all state is derived from Podman via labels.
type Manager struct {
	// podmanPath is the path to the podman binary.
	podmanPath string

	// defaultTimeout is the default timeout for podman operations.
	defaultTimeout time.Duration

	// userNSMode is the configured Podman user namespace mode.
	userNSMode string

	// allowHostUserns controls whether fallback to --userns=host is permitted.
	allowHostUserns bool

	// runAsUser is the value passed to podman --user for every container it
	// starts. Empty means trust the image's USER directive. A typical value
	// is "1000:1000" to guarantee non-root even if an image regresses.
	runAsUser string

	// daemonSocketPath is the host-side path to the daemon's unix
	// socket (server.unix_socket). When set, NetworkDaemonOnly
	// containers bind-mount it at DaemonOnlySocketContainerPath and
	// reach the daemon over it with no network device. Empty means
	// daemon-only roles still run --network=none but lose daemon
	// access (fail closed) — a loud misconfiguration, logged at start.
	daemonSocketPath string

	// defaultNetwork is the network policy applied to roles that don't
	// set ContainerConfig.Network explicitly (runtime.default_network).
	// Empty = the historical permissive default (no --network flag).
	// Set to NetworkDaemonOnly to make zero-egress the default for all
	// roles, with network:host as the per-role opt-out.
	defaultNetwork NetworkMode

	// metrics holds the Prometheus metrics for the runtime.
	metrics *Metrics

	logger zerolog.Logger
}

// ManagerOption is a functional option for configuring the Manager.
type ManagerOption func(*Manager)

// WithPodmanPath sets a custom path to the podman binary.
func WithPodmanPath(path string) ManagerOption {
	return func(m *Manager) {
		m.podmanPath = path
	}
}

// WithDefaultTimeout sets the default timeout for podman operations.
func WithDefaultTimeout(d time.Duration) ManagerOption {
	return func(m *Manager) {
		m.defaultTimeout = d
	}
}

// WithUserNSMode sets the Podman user namespace mode for started containers.
func WithUserNSMode(mode string) ManagerOption {
	return func(m *Manager) {
		m.userNSMode = strings.TrimSpace(mode)
	}
}

// WithAllowHostUserns sets whether fallback to --userns=host is permitted.
func WithAllowHostUserns(allow bool) ManagerOption {
	return func(m *Manager) {
		m.allowHostUserns = allow
	}
}

// WithRunAsUser sets the value passed as podman --user for every container.
// Empty (the default) means trust the image's USER directive.
func WithRunAsUser(user string) ManagerOption {
	return func(m *Manager) {
		m.runAsUser = strings.TrimSpace(user)
	}
}

// WithDaemonSocketPath sets the host path to the daemon's unix socket,
// bind-mounted into NetworkDaemonOnly containers so they reach the
// daemon with no network device. Empty (the default) leaves daemon-only
// containers without daemon access (fail closed).
func WithDaemonSocketPath(path string) ManagerOption {
	return func(m *Manager) {
		m.daemonSocketPath = strings.TrimSpace(path)
	}
}

// WithDefaultNetwork sets the network policy for roles that don't
// specify one (runtime.default_network). An invalid non-empty value
// fails closed to daemon-only: a typo must not silently restore
// permissive container egress.
func WithDefaultNetwork(mode string) ManagerOption {
	return func(m *Manager) {
		nm := NetworkMode(strings.TrimSpace(mode))
		if nm == NetworkDefault {
			return
		}
		if !ValidNetworkMode(nm) {
			m.logger.Warn().Str("default_network", mode).
				Msg("runtime.default_network is not a valid network mode; falling back to daemon-only")
			m.defaultNetwork = NetworkDaemonOnly
			return
		}
		m.defaultNetwork = nm
	}
}

// WithMetrics sets the metrics instance for the manager.
func WithMetrics(metrics *Metrics) ManagerOption {
	return func(m *Manager) {
		m.metrics = metrics
	}
}

// WithLogger sets the logger used for Podman command diagnostics.
func WithLogger(logger zerolog.Logger) ManagerOption {
	return func(m *Manager) {
		m.logger = logger
	}
}

// WithPrometheusRegistry creates metrics with the given Prometheus registry.
// This is a convenience option that creates a new Metrics instance.
func WithPrometheusRegistry(registry *prometheus.Registry) ManagerOption {
	return func(m *Manager) {
		if registry != nil {
			m.metrics = NewMetrics(registry)
		}
	}
}

// New creates a new runtime Manager.
// It discovers podman from PATH if not explicitly configured.
func New(opts ...ManagerOption) (*Manager, error) {
	m := &Manager{
		defaultTimeout: 60 * time.Second,
		logger:         zerolog.Nop(),
	}

	for _, opt := range opts {
		opt(m)
	}

	// Discover podman binary
	if m.podmanPath == "" {
		path, err := exec.LookPath("podman")
		if err != nil {
			return nil, &PodmanNotAvailableError{Err: err}
		}
		m.podmanPath = path
	}

	// Verify podman is working
	if err := m.verifyPodmanAvailable(); err != nil {
		return nil, err
	}

	return m, nil
}

// verifyPodmanAvailable checks that podman is functional.
func (m *Manager) verifyPodmanAvailable() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.podmanPath, "version", "--format", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		m.logger.Error().
			Err(err).
			Str("command", m.podmanPath+" version --format json").
			Str("output", strings.TrimSpace(string(output))).
			Msg("podman availability check failed")
		return &PodmanNotAvailableError{Err: fmt.Errorf("podman version check failed: %w - output: %s", err, string(output))}
	}
	return nil
}

// recordPodmanError is a helper to record podman errors with nil safety.
func (m *Manager) recordPodmanError(operation string) {
	if m.metrics != nil {
		m.metrics.RecordPodmanError(operation)
	}
}

// buildPreImageArgs builds the podman run argument list up to (but not
// including) the image name and any userns flags, so callers can splice
// those in before appending the image.
func (m *Manager) buildPreImageArgs(config *ContainerConfig) []string {
	args := []string{"run", "--detach", "--replace"}

	// Container name
	args = append(args, "--name", ContainerName(config.ProjectID, config.Role, config.TaskID))

	// Labels
	labels := StandardLabelSet(config.ProjectID, config.Role, config.TaskID)
	for k, v := range labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	// Resolve the effective network policy: a role's explicit
	// ContainerConfig.Network wins; an empty value falls back to the
	// daemon's configured default (runtime.default_network, via
	// WithDefaultNetwork). This is how "daemon-only by default" works
	// without annotating every role — an operator opts a role OUT with
	// an explicit network:host (e.g. a build role that needs egress).
	effectiveNetwork := config.Network
	if effectiveNetwork == "" {
		effectiveNetwork = m.defaultNetwork
	}

	// Environment variables. For NetworkDaemonOnly the container has no
	// network device, so the daemon + LLM endpoints (both target the
	// daemon) are rewritten to the bind-mounted unix socket.
	for k, v := range daemonOnlyEnv(config.EnvVars, effectiveNetwork, m.daemonSocketPath) {
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}

	// Resource limits
	if config.CPUQuota > 0 {
		args = append(args, "--cpu-quota", strconv.FormatInt(config.CPUQuota, 10))
	}
	if config.MemoryLimit > 0 {
		args = append(args, "--memory", strconv.FormatInt(config.MemoryLimit, 10))
	}

	// Volume mounts for agent runtime contract.
	// Input is read-only for ephemeral containers (host writes task.json,
	// container only reads). Warm containers need write access to remove
	// the .ready sentinel file that acknowledges each injected task.
	if config.InputDir != "" {
		inputMode := "ro"
		if config.LifecyclePolicy == PolicyWarm {
			inputMode = "rw"
		}
		args = append(args, "--volume", fmt.Sprintf("%s:/app/input:%s,Z", config.InputDir, inputMode))
	}
	if config.OutputDir != "" {
		args = append(args, "--volume", fmt.Sprintf("%s:/app/output:rw,Z", config.OutputDir))
	}
	if config.WorkspaceDir != "" {
		args = append(args, "--volume", fmt.Sprintf("%s:/app/workspace:rw,Z", config.WorkspaceDir))
	}
	if config.ProjectDir != "" {
		// Use :z (shared) not :Z (private) — the project dir is shared
		// across containers for the same project. :Z would relabel it
		// with a private MCS category that blocks subsequent containers.
		args = append(args, "--volume", fmt.Sprintf("%s:/app/workspace/project:rw,z", config.ProjectDir))
	}
	if config.ProjectGitDir != "" {
		// Bind-mount the project's .git at its original host path.
		// A git worktree's .git file contains an absolute host path like
		// "gitdir: /var/lib/vornik/workspaces/<project>/.git/worktrees/<branch>";
		// without this mount the agent container can't run git commands
		// from inside the worktree because that path doesn't exist in
		// the container's filesystem. Shared label (:z) — multiple
		// containers from the same project may share the main .git dir
		// for object storage and cross-worktree refs.
		args = append(args, "--volume", fmt.Sprintf("%s:%s:ro,z", config.ProjectGitDir, config.ProjectGitDir))
	}

	// Working directory (agent runtime contract)
	args = append(args, "--workdir", "/app/workspace")

	// Security hardening. Agent containers run arbitrary LLM-directed code,
	// so we strip every Linux capability and block privilege gain by default.
	args = append(args,
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
	)
	// Force a non-root identity when the operator configured runtime.run_as_user.
	// Empty (the default) trusts the image's own USER directive so images that
	// already run as non-root (or depend on rootless uid mapping) keep working.
	if m.runAsUser != "" {
		args = append(args, "--user", m.runAsUser)
	}

	// Per-role network policy (see ContainerConfig.Network). The default
	// (NetworkDefault) appends nothing, preserving rootless podman's
	// slirp4netns egress so no existing role breaks. Roles can opt into
	// none / host / daemon-only. See https://docs.vornik.io
	// finding #1 and the mitigation plan §7.1.
	//
	// see LLD § https://docs.vornik.io § 5
	// "Tenancy and Project Isolation".
	if val, ok := effectiveNetwork.podmanNetworkArg(); ok {
		args = append(args, "--network", val)
	}

	// daemon-only: bind-mount the daemon unix socket so the agent can
	// still reach the daemon (MCP + LLM) despite --network=none. The env
	// rewrite above points VORNIK_API_URL / VORNIK_LLM_ENDPOINT at it.
	//
	// --security-opt label=disable is required under SELinux (enforcing):
	// a unix-socket connect() is authorised as `connectto` against the
	// LISTENING process's domain (the daemon), NOT the socket file's
	// label — so relabeling the mount (:z/:Z) does NOT grant access
	// (verified on Fedora/netavark). label=disable is the idiomatic
	// podman approach for host-socket access (same as docker.sock). It
	// is an acceptable trade here: the egress threat is fully handled by
	// --network=none, and the container is still no-network + non-root
	// (--user) + scoped read-only mounts; SELinux confinement is dropped
	// only to reach the one daemon socket.
	//
	// If no socket is configured we omit the mount and leave the
	// container at --network=none (fail closed): better no daemon access
	// than silent egress.
	if effectiveNetwork == NetworkDaemonOnly && m.daemonSocketPath != "" {
		args = append(args,
			"--security-opt", "label=disable",
			"--volume", fmt.Sprintf("%s:%s:ro", m.daemonSocketPath, DaemonOnlySocketContainerPath))
	}

	return args
}

// daemonOnlyEnv returns the env map to inject. For NetworkDaemonOnly
// with a configured socket it returns a copy with the daemon + LLM
// endpoints rewritten to the bind-mounted unix socket; otherwise it
// returns env unchanged. Both endpoints target the daemon in the
// default deployment shape (VORNIK_API_URL → daemon API,
// VORNIK_LLM_ENDPOINT → runtime.agent_llm.endpoint = the daemon), so
// both move to the socket. The bridge appends its own /api/v1 path, so
// VORNIK_API_URL carries only the socket; the agent entrypoint expects
// the LLM base path, so VORNIK_LLM_ENDPOINT keeps the /api/v1 suffix.
func daemonOnlyEnv(env map[string]string, mode NetworkMode, socketPath string) map[string]string {
	if mode != NetworkDaemonOnly || socketPath == "" {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	sockURL := "unix://" + DaemonOnlySocketContainerPath
	if _, ok := out["VORNIK_API_URL"]; ok {
		out["VORNIK_API_URL"] = sockURL
	}
	if _, ok := out["VORNIK_LLM_ENDPOINT"]; ok {
		out["VORNIK_LLM_ENDPOINT"] = sockURL + "/api/v1"
	}
	// VORNIK_MEM_URL is a daemon base the entrypoint appends /api/v1/...
	// to (built-in memory_search tool), so it carries only the socket.
	if _, ok := out["VORNIK_MEM_URL"]; ok {
		out["VORNIK_MEM_URL"] = sockURL
	}
	return out
}

func redactPodmanArgs(args []string) []string {
	redacted := make([]string, len(args))
	copy(redacted, args)
	for i := 0; i < len(redacted); i++ {
		if redacted[i] != "--env" || i+1 >= len(redacted) {
			continue
		}
		key, _, ok := strings.Cut(redacted[i+1], "=")
		if !ok {
			redacted[i+1] = "<redacted>"
			continue
		}
		redacted[i+1] = key + "=<redacted>"
		i++
	}
	return redacted
}

// StartContainer starts a new agent container with the given configuration.
// Returns the container ID on success.
//
// When userNSMode is explicitly configured, it is used directly with no
// fallback. When unconfigured, StartContainer tries progressively less
// isolated user namespace modes: default → keep-id → host. This handles
// SELinux hosts and systems without /etc/subuid entries where newuidmap
// fails under systemd's NoNewPrivileges (host mode bypasses newuidmap
// entirely and works on any Linux host).
func (m *Manager) StartContainer(ctx context.Context, config *ContainerConfig) (string, error) {
	if err := config.Validate(); err != nil {
		return "", fmt.Errorf("invalid container config: %w", err)
	}

	start := time.Now()
	preImageArgs := m.buildPreImageArgs(config)

	// If the operator explicitly chose a userns mode, honour it with a single
	// attempt (no auto-fallback — they know what they want).  If the failure
	// is the pause-process issue, try "podman system migrate" once and retry.
	if m.userNSMode != "" {
		args := make([]string, 0, len(preImageArgs)+3)
		args = append(args, preImageArgs...)
		args = append(args, "--userns", m.userNSMode, config.Image)
		id, err := m.runStartAttempt(ctx, config, args, m.userNSMode, start)
		if err != nil && m.tryMigrateOnPauseError(ctx, []byte(err.Error())) {
			return m.runStartAttempt(ctx, config, args, m.userNSMode, start)
		}
		return id, err
	}

	// Auto-fallback chain for rootless Podman on hosts where newuidmap may
	// fail (SELinux, missing subuid, systemd NoNewPrivileges on the helper).
	type usernsTry struct {
		userns string // flag value, empty = default (no --userns flag)
		label  string // human-readable for logs
	}
	tries := []usernsTry{
		{"", "default"},
		{"keep-id", "keep-id"},
	}
	if m.allowHostUserns {
		tries = append(tries, usernsTry{"host", "host"})
	}

	for i, try := range tries {
		args := make([]string, 0, len(preImageArgs)+3)
		args = append(args, preImageArgs...)
		if try.userns != "" {
			args = append(args, "--userns", try.userns)
		}
		args = append(args, config.Image)

		if i == 0 {
			m.logger.Info().
				Str("task_id", config.TaskID).
				Str("project_id", config.ProjectID).
				Str("image", config.Image).
				Strs("args", redactPodmanArgs(args)).
				Msg("starting podman container")
		}

		output, err := m.runPodmanCommand(ctx, args)
		if err != nil {
			if i < len(tries)-1 && isRootlessUserNSError(output) {
				m.logger.Warn().
					Str("task_id", config.TaskID).
					Str("project_id", config.ProjectID).
					Str("image", config.Image).
					Str("retry_userns", tries[i+1].label).
					Msg("podman run hit rootless user namespace setup failure, retrying with --userns=" + tries[i+1].userns)
				continue
			}
			// Last userns mode exhausted — if the failure is a stale
			// pause process, run "podman system migrate" and retry the
			// last mode once more before giving up.
			if m.tryMigrateOnPauseError(ctx, output) {
				retryArgs := make([]string, 0, len(preImageArgs)+3)
				retryArgs = append(retryArgs, preImageArgs...)
				if try.userns != "" {
					retryArgs = append(retryArgs, "--userns", try.userns)
				}
				retryArgs = append(retryArgs, config.Image)
				return m.runStartAttempt(ctx, config, retryArgs, try.label+" (post-migrate)", start)
			}
			m.recordPodmanError("start")
			m.logger.Error().
				Err(err).
				Str("task_id", config.TaskID).
				Str("project_id", config.ProjectID).
				Str("image", config.Image).
				Str("output", strings.TrimSpace(string(output))).
				Msg("podman run failed")
			return "", fmt.Errorf("podman run failed: %w", enrichStartError(err, output))
		}

		containerID := strings.TrimSpace(string(output))
		if containerID == "" {
			m.recordPodmanError("start")
			m.logger.Error().
				Str("task_id", config.TaskID).
				Str("project_id", config.ProjectID).
				Str("image", config.Image).
				Msg("podman run returned empty container id")
			return "", fmt.Errorf("podman run did not return a container ID")
		}

		m.logger.Info().
			Str("task_id", config.TaskID).
			Str("project_id", config.ProjectID).
			Str("container_id", containerID).
			Str("userns", try.label).
			Msg("podman container started")

		if m.metrics != nil {
			m.metrics.RecordContainerStarted(config.ProjectID, time.Since(start).Seconds())
		}

		return containerID, nil
	}

	return "", fmt.Errorf("podman run failed: all user namespace modes exhausted")
}

// runStartAttempt executes a single podman run attempt and handles success/failure logging.
func (m *Manager) runStartAttempt(ctx context.Context, config *ContainerConfig, args []string, userns string, start time.Time) (string, error) {
	m.logger.Info().
		Str("task_id", config.TaskID).
		Str("project_id", config.ProjectID).
		Str("image", config.Image).
		Strs("args", redactPodmanArgs(args)).
		Msg("starting podman container")

	output, err := m.runPodmanCommand(ctx, args)
	if err != nil {
		m.recordPodmanError("start")
		m.logger.Error().
			Err(err).
			Str("task_id", config.TaskID).
			Str("project_id", config.ProjectID).
			Str("image", config.Image).
			Str("output", strings.TrimSpace(string(output))).
			Msg("podman run failed")
		return "", fmt.Errorf("podman run failed: %w", enrichStartError(err, output))
	}

	containerID := strings.TrimSpace(string(output))
	if containerID == "" {
		m.recordPodmanError("start")
		m.logger.Error().
			Str("task_id", config.TaskID).
			Str("project_id", config.ProjectID).
			Str("image", config.Image).
			Msg("podman run returned empty container id")
		return "", fmt.Errorf("podman run did not return a container ID")
	}

	m.logger.Info().
		Str("task_id", config.TaskID).
		Str("project_id", config.ProjectID).
		Str("container_id", containerID).
		Str("userns", userns).
		Msg("podman container started")

	if m.metrics != nil {
		m.metrics.RecordContainerStarted(config.ProjectID, time.Since(start).Seconds())
	}

	return containerID, nil
}

func (m *Manager) runPodmanCommand(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	return cmd.CombinedOutput()
}

func isRootlessUserNSError(output []byte) bool {
	msg := strings.ToLower(string(output))
	return strings.Contains(msg, "newuidmap") ||
		strings.Contains(msg, "newgidmap") ||
		strings.Contains(msg, "unable to create a new pause process") ||
		strings.Contains(msg, "cannot set up namespace")
}

// isPauseProcessError returns true when the failure is Podman's
// infrastructure pause process, not the container itself.  The pause
// process holds the rootless user namespace; if its state is stale or
// the namespace setup is broken, a "podman system migrate" can repair it.
func isPauseProcessError(output []byte) bool {
	msg := strings.ToLower(string(output))
	return strings.Contains(msg, "unable to create a new pause process") ||
		(strings.Contains(msg, "invalid internal status") && strings.Contains(msg, "newuidmap"))
}

// tryMigrateOnPauseError runs "podman system migrate" when the output
// indicates a broken pause process.  Returns true if migration ran (caller
// should retry the container start).
func (m *Manager) tryMigrateOnPauseError(ctx context.Context, output []byte) bool {
	if !isPauseProcessError(output) {
		return false
	}

	m.logger.Warn().Msg("detected stale podman pause process, running 'podman system migrate' to recover")

	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(migrateCtx, m.podmanPath, "system", "migrate")
	migrateOut, err := cmd.CombinedOutput()
	if err != nil {
		m.logger.Error().
			Err(err).
			Str("output", strings.TrimSpace(string(migrateOut))).
			Msg("podman system migrate failed")
		return false
	}

	m.logger.Info().Msg("podman system migrate succeeded, retrying container start")
	return true
}

func enrichStartError(err error, output []byte) error {
	msg := strings.TrimSpace(string(output))
	if !isRootlessUserNSError(output) {
		return fmt.Errorf("%w - output: %s", err, msg)
	}

	return fmt.Errorf(
		"%w - rootless podman user namespace setup failed; vornik retried with --userns=keep-id. Fix podman with `podman system migrate`, verify /etc/subuid and /etc/subgid for this user, or set `runtime.userns_mode: host` as a compatibility fallback - output: %s",
		err,
		msg,
	)
}

// StopContainer stops a running container gracefully.
// If force is true, it sends SIGKILL instead of SIGTERM.
func (m *Manager) StopContainer(ctx context.Context, containerID string, force bool) error {
	start := time.Now()

	args := []string{"stop"}
	if force {
		args = append(args, "--time", "0") // No grace period
	} else {
		args = append(args, "--time", "10") // 10 second grace period
	}
	args = append(args, containerID)

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if container doesn't exist
		if strings.Contains(string(output), "no such container") {
			return &ContainerNotFoundError{ContainerID: containerID}
		}
		m.recordPodmanError("stop")
		return fmt.Errorf("podman stop failed: %w - output: %s", err, string(output))
	}

	// Record metrics - we need to get the project ID from container inspection
	// For now, we record without project_id (empty string) since we don't have it
	if m.metrics != nil {
		m.metrics.RecordContainerStopped("", time.Since(start).Seconds())
	}

	return nil
}

// RemoveContainer removes a stopped container.
// If force is true, it removes even if running.
func (m *Manager) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, containerID)

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if container doesn't exist
		if strings.Contains(string(output), "no such container") {
			return nil // Already gone
		}
		m.recordPodmanError("remove")
		m.logger.Error().
			Err(err).
			Str("container_id", containerID).
			Str("output", strings.TrimSpace(string(output))).
			Msg("podman rm failed")
		return fmt.Errorf("podman rm failed: %w - output: %s", err, string(output))
	}

	return nil
}

// InspectContainer gets detailed information about a container.
func (m *Manager) InspectContainer(ctx context.Context, containerID string) (*Container, error) {
	args := []string{"inspect", "--format", "json", containerID}

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "no such container") {
			return nil, &ContainerNotFoundError{ContainerID: containerID}
		}
		m.recordPodmanError("inspect")
		return nil, fmt.Errorf("podman inspect failed: %w - output: %s", err, string(output))
	}

	// Parse inspect output
	var inspectData []podmanInspectData
	if err := json.Unmarshal(output, &inspectData); err != nil {
		return nil, fmt.Errorf("failed to parse podman inspect output: %w", err)
	}

	if len(inspectData) == 0 {
		return nil, &ContainerNotFoundError{ContainerID: containerID}
	}

	return parseInspectData(&inspectData[0]), nil
}

// podmanInspectData represents the JSON structure from podman inspect.
type podmanInspectData struct {
	Id     string              `json:"Id"`
	Name   string              `json:"Name"`
	Image  string              `json:"Image"`
	State  podmanInspectState  `json:"State"`
	Config podmanInspectConfig `json:"Config"`
}

type podmanInspectState struct {
	Status   string `json:"Status"`
	ExitCode int    `json:"ExitCode"`
}

type podmanInspectConfig struct {
	Labels map[string]string `json:"Labels"`
}

func parseInspectData(data *podmanInspectData) *Container {
	// Strip leading / from container name
	name := strings.TrimPrefix(data.Name, "/")

	// Extract labels
	container := &Container{
		ID:       data.Id,
		Name:     name,
		Image:    data.Image,
		ExitCode: data.State.ExitCode,
	}

	// Parse status
	switch data.State.Status {
	case "running":
		container.Status = StatusRunning
	case "stopped", "exited":
		container.Status = StatusExited
	case "paused":
		container.Status = StatusPaused
	default:
		container.Status = StatusUnknown
	}

	// Extract labels
	if data.Config.Labels != nil {
		container.ProjectID = data.Config.Labels[LabelProjectID]
		container.Role = data.Config.Labels[LabelRole]
		container.TaskID = data.Config.Labels[LabelTaskID]
	}

	return container
}

// ListContainers lists all containers matching the given filters.
// If no filters are provided, lists all vornik-managed containers.
func (m *Manager) ListContainers(ctx context.Context, filters map[string]string) ([]*Container, error) {
	args := []string{"ps", "--all", "--format", "json"}

	// Default to showing all vornik-managed containers
	if filters == nil {
		args = append(args, "--filter", "label="+LabelManaged+"="+LabelValueTrue)
	} else {
		for k, v := range filters {
			args = append(args, "--filter", fmt.Sprintf("label=%s=%s", k, v))
		}
	}

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		m.recordPodmanError("list")
		return nil, fmt.Errorf("podman ps failed: %w - output: %s", err, string(output))
	}

	// Parse output
	var psData []podmanPsData
	if len(output) > 0 {
		if err := json.Unmarshal(output, &psData); err != nil {
			return nil, fmt.Errorf("failed to parse podman ps output: %w", err)
		}
	}

	containers := make([]*Container, 0, len(psData))
	for i := range psData {
		containers = append(containers, parsePsData(&psData[i]))
	}

	return containers, nil
}

// podmanPsData represents the JSON structure from podman ps --format json.
type podmanPsData struct {
	Id       string            `json:"ID"`
	Names    []string          `json:"Names"`
	Image    string            `json:"Image"`
	State    string            `json:"State"`
	ExitCode int               `json:"ExitCode"`
	Labels   map[string]string `json:"Labels"`
}

func parsePsData(data *podmanPsData) *Container {
	name := ""
	if len(data.Names) > 0 {
		name = strings.TrimPrefix(data.Names[0], "/")
	}

	container := &Container{
		ID:       data.Id,
		Name:     name,
		Image:    data.Image,
		ExitCode: data.ExitCode,
	}

	// Parse state
	switch strings.ToLower(data.State) {
	case "running":
		container.Status = StatusRunning
	case "stopped", "exited":
		container.Status = StatusExited
	case "paused":
		container.Status = StatusPaused
	default:
		container.Status = StatusUnknown
	}

	// Extract labels
	if data.Labels != nil {
		container.ProjectID = data.Labels[LabelProjectID]
		container.Role = data.Labels[LabelRole]
		container.TaskID = data.Labels[LabelTaskID]
	}

	return container
}

// GetContainerByTask finds a container by its task ID.
func (m *Manager) GetContainerByTask(ctx context.Context, taskID string) (*Container, error) {
	containers, err := m.ListContainers(ctx, map[string]string{LabelTaskID: taskID})
	if err != nil {
		return nil, err
	}

	if len(containers) == 0 {
		return nil, &ContainerNotFoundError{ContainerID: "task:" + taskID}
	}

	return containers[0], nil
}

// GetContainersByProject finds all containers for a project.
func (m *Manager) GetContainersByProject(ctx context.Context, projectID string) ([]*Container, error) {
	return m.ListContainers(ctx, map[string]string{LabelProjectID: projectID})
}

// GetContainersByRole finds all containers for a role within a project.
func (m *Manager) GetContainersByRole(ctx context.Context, projectID, role string) ([]*Container, error) {
	return m.ListContainers(ctx, map[string]string{
		LabelProjectID: projectID,
		LabelRole:      role,
	})
}

// WaitForExit waits for a container to exit and returns its exit code.
// Returns an error if the timeout is reached.
func (m *Manager) WaitForExit(ctx context.Context, containerID string, timeout time.Duration) (int, error) {
	ctx, cancel := contextWithOptionalTimeout(ctx, timeout)
	defer cancel()

	// Use podman wait to wait for container exit
	args := []string{"wait", containerID}
	m.logger.Info().
		Str("container_id", containerID).
		Dur("timeout", timeout).
		Msg("waiting for podman container exit")

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// podman wait exits with the container's own exit code, so a non-zero
		// status is expected when the container fails. Try to parse the output
		// as the container's exit code before treating this as a podman error.
		if exitCode, parseErr := strconv.Atoi(strings.TrimSpace(string(output))); parseErr == nil {
			m.logger.Info().
				Str("container_id", containerID).
				Int("exit_code", exitCode).
				Msg("podman container exited with non-zero code")
			return exitCode, nil
		}
		// When ctx is done, the os/exec layer SIGKILLs the podman wait
		// subprocess and CombinedOutput reports "signal: killed" — which
		// the executor's shouldRetry can't distinguish from a transient
		// container failure, so it wastes a within-execution retry that
		// eats the workflow's wall-clock budget (observed 2026-05-15 on
		// ibkr-trader: strategist hit its 6m step timeout, retry ate the
		// remaining wall-clock, every task-level attempt collapsed the
		// same way). Surfacing ctx.Err() first preserves
		// context.DeadlineExceeded / context.Canceled in the error chain,
		// which shouldRetry already checks for FIRST — within-execution
		// retry skipped, task-level retry gets a fresh wall-clock budget.
		if ctxErr := ctx.Err(); ctxErr != nil {
			m.recordPodmanError("wait_timeout")
			m.logger.Warn().
				Err(ctxErr).
				Str("container_id", containerID).
				Dur("timeout", timeout).
				Str("subprocess_error", err.Error()).
				Msg("podman wait timed out — surfacing context error")
			return -1, fmt.Errorf("podman wait timed out after %s: %w", timeout, ctxErr)
		}
		m.recordPodmanError("wait")
		m.logger.Error().
			Err(err).
			Str("container_id", containerID).
			Str("output", strings.TrimSpace(string(output))).
			Msg("podman wait failed")
		return -1, fmt.Errorf("podman wait failed: %w", err)
	}

	// podman wait returns the exit code on stdout
	exitCode, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		m.logger.Error().
			Err(err).
			Str("container_id", containerID).
			Str("output", strings.TrimSpace(string(output))).
			Msg("failed to parse podman wait output")
		return -1, fmt.Errorf("failed to parse podman wait output: %w", err)
	}

	m.logger.Info().
		Str("container_id", containerID).
		Int("exit_code", exitCode).
		Msg("podman container exited")

	return exitCode, nil
}

func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

// PullImage pulls a container image.
func (m *Manager) PullImage(ctx context.Context, image string) error {
	args := []string{"pull", image}

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		m.recordPodmanError("pull")
		return fmt.Errorf("podman pull failed: %w - output: %s", err, string(output))
	}

	return nil
}

// IsAvailable checks if podman is available and functional.
func (m *Manager) IsAvailable() bool {
	return m.verifyPodmanAvailable() == nil
}

// PodmanPath returns the path to the podman binary being used.
func (m *Manager) PodmanPath() string {
	return m.podmanPath
}

// Logs retrieves logs from a container.
func (m *Manager) Logs(ctx context.Context, containerID string, tail int) (string, error) {
	args := []string{"logs"}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	args = append(args, containerID)

	cmd := exec.CommandContext(ctx, m.podmanPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "no such container") {
			return "", &ContainerNotFoundError{ContainerID: containerID}
		}
		m.recordPodmanError("logs")
		return "", fmt.Errorf("podman logs failed: %w", err)
	}

	return string(output), nil
}
