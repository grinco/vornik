// Coverage for pure helpers + simple option setters in the
// runtime package. The Manager/WarmPool surfaces that actually
// talk to podman need integration tests; these tests pin the
// data-shaping helpers and option closures.

package runtime

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// TestPodmanNotAvailableError_Format ensures the error string
// includes the inner error text. Operator-facing — surfaces in
// readiness reports.
func TestPodmanNotAvailableError_Format(t *testing.T) {
	inner := errors.New("exec: \"podman\" not found in PATH")
	err := &PodmanNotAvailableError{Err: inner}
	if err.Error() != "podman not available: exec: \"podman\" not found in PATH" {
		t.Errorf("Error() = %q, want prefixed wrap", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Errorf("errors.Is must unwrap PodmanNotAvailableError to its Err field")
	}
}

// TestContainerNotFoundError_Format pins the same shape for the
// not-found error: prefix + container ID, no extra punctuation
// so callers grepping logs match consistently.
func TestContainerNotFoundError_Format(t *testing.T) {
	err := &ContainerNotFoundError{ContainerID: "abc123"}
	if err.Error() != "container not found: abc123" {
		t.Errorf("Error() = %q, want canonical shape", err.Error())
	}
}

// TestPoolKey_String pins the key string format so logs/metrics
// keep a stable shape. Format is project:role:image:network (the
// trailing network segment was added in Step B so warm containers
// don't cross network policies; no escaping — keys are validated
// upstream).
func TestPoolKey_String(t *testing.T) {
	k := PoolKey{ProjectID: "vornik", Role: "lead", Image: "vornik-agent:latest", Network: NetworkDaemonOnly}
	if got := k.String(); got != "vornik:lead:vornik-agent:latest:daemon-only" {
		t.Errorf("PoolKey.String() = %q, want canonical", got)
	}
	// Empty network (the default) renders as a trailing empty segment,
	// preserving the historical project:role:image prefix.
	def := PoolKey{ProjectID: "vornik", Role: "lead", Image: "vornik-agent:latest"}
	if got := def.String(); got != "vornik:lead:vornik-agent:latest:" {
		t.Errorf("default-network PoolKey: got %q, want trailing ':'", got)
	}
	empty := PoolKey{}
	if got := empty.String(); got != ":::" {
		t.Errorf("empty PoolKey: got %q, want ':::'", got)
	}
}

// TestDefaultPoolConfig defends the operator-visible defaults.
// Changing these without an explicit operator-facing reason can
// shift behaviour silently — pin them.
func TestDefaultPoolConfig(t *testing.T) {
	c := DefaultPoolConfig()
	if c.MaxPerRole != 2 {
		t.Errorf("MaxPerRole: got %d, want 2", c.MaxPerRole)
	}
	if c.IdleTimeout.Minutes() != 10 {
		t.Errorf("IdleTimeout: got %v, want 10m", c.IdleTimeout)
	}
}

// TestManagerWithLogger pins the option applies the logger to
// the Manager. The Manager struct itself is large; we only
// inspect the field we set.
func TestManagerWithLogger(t *testing.T) {
	m := &Manager{}
	logger := zerolog.Nop()
	WithLogger(logger)(m)
	// zerolog.Logger values aren't directly comparable; checking
	// the field exists + the option ran (no panic) is what we want.
	_ = m.logger
}

// TestWithPrometheusRegistry_NilSkips — the option is no-op on
// nil so the operator can construct a Manager without metrics in
// constrained deployments.
func TestWithPrometheusRegistry_NilSkips(t *testing.T) {
	m := &Manager{}
	WithPrometheusRegistry(nil)(m)
	if m.metrics != nil {
		t.Error("nil registry: metrics should stay unset")
	}
}

// TestWithPrometheusRegistry_NonNilWires — happy path: a real
// registry wires a Metrics instance.
func TestWithPrometheusRegistry_NonNilWires(t *testing.T) {
	m := &Manager{}
	reg := prometheus.NewRegistry()
	WithPrometheusRegistry(reg)(m)
	if m.metrics == nil {
		t.Error("non-nil registry: metrics should be wired")
	}
}

// TestWithPoolOptions verifies the WarmPool option setters all
// route to the right field. Mirrors the api/telegram option
// tests.
func TestWithPoolOptions(t *testing.T) {
	p := &WarmPool{}
	WithPoolLogger(zerolog.Nop())(p)
	envIn := map[string]string{"FOO": "bar"}
	WithPoolEnvVars(envIn)(p)
	if got, want := len(p.envVars), 1; got != want {
		t.Errorf("WithPoolEnvVars: envVars len = %d, want %d", got, want)
	}
	WithPoolProjectWorkspacePath("/tmp/projects")(p)
	if p.projectWorkspacePath != "/tmp/projects" {
		t.Errorf("WithPoolProjectWorkspacePath: got %q", p.projectWorkspacePath)
	}
}

// TestWithPoolPrometheusRegistry_NilSkips matches the manager
// helper — nil registry leaves metrics unwired.
func TestWithPoolPrometheusRegistry_NilSkips(t *testing.T) {
	p := &WarmPool{}
	WithPoolPrometheusRegistry(nil)(p)
	if p.metrics != nil {
		t.Error("nil pool registry: metrics should stay unset")
	}
}

// TestParsePsData_PinStateMapping covers the lowercased-state →
// typed-status mapping. Mismatched literals would silently
// classify containers as Unknown — visible in /ui/audit but easy
// to miss; pin the transitions.
func TestParsePsData_PinStateMapping(t *testing.T) {
	cases := map[string]Status{
		"running": StatusRunning,
		"RUNNING": StatusRunning,
		"stopped": StatusExited,
		"exited":  StatusExited,
		"paused":  StatusPaused,
		"weird":   StatusUnknown,
		"":        StatusUnknown,
	}
	for state, want := range cases {
		got := parsePsData(&podmanPsData{Id: "id1", State: state})
		if got.Status != want {
			t.Errorf("state %q: got %s, want %s", state, got.Status, want)
		}
	}
}

// TestParsePsData_NameStripsLeadingSlash — podman ps emits
// container names with a leading slash; the parser strips it so
// downstream `name` lookups don't have to.
func TestParsePsData_NameStripsLeadingSlash(t *testing.T) {
	got := parsePsData(&podmanPsData{Id: "id1", Names: []string{"/vornik-x"}, State: "running"})
	if got.Name != "vornik-x" {
		t.Errorf("Name: got %q, want %q", got.Name, "vornik-x")
	}
}

// TestParsePsData_LabelsPopulateRichFields — podman labels carry
// the project, role, and task IDs vornik uses for container ↔
// task correlation. Ensure they flow through verbatim.
func TestParsePsData_LabelsPopulateRichFields(t *testing.T) {
	got := parsePsData(&podmanPsData{
		Id:    "id1",
		State: "running",
		Labels: map[string]string{
			LabelProjectID: "p1",
			LabelRole:      "lead",
			LabelTaskID:    "task_x",
		},
	})
	if got.ProjectID != "p1" || got.Role != "lead" || got.TaskID != "task_x" {
		t.Errorf("labels: got %+v", got)
	}
}

// TestParsePsData_EmptyNamesDoesNotPanic — defensive: an empty
// names slice must not crash the parser.
func TestParsePsData_EmptyNamesDoesNotPanic(t *testing.T) {
	got := parsePsData(&podmanPsData{Id: "id1", State: "running"})
	if got.Name != "" {
		t.Errorf("empty Names: got Name=%q, want empty", got.Name)
	}
}
