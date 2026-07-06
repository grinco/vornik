package projectdoctor

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/registry"
)

// ProjectResolver resolves a project's committed config + swarm +
// workflow. Satisfied by *registry.Registry.
type ProjectResolver interface {
	ResolveProjectConfig(id string) (*registry.Project, *registry.Swarm, *registry.Workflow, error)
}

// MCPSnapshotSource yields the daemon's cached MCP server reachability
// snapshot. Satisfied by *mcp.Registry (non-blocking, async-refreshed).
type MCPSnapshotSource interface {
	Snapshot(ctx context.Context) []mcp.ServerSnapshot
}

// ModelPinger performs an authoritative completion ping against the
// daemon's configured chat model.
type ModelPinger interface {
	Ping(ctx context.Context) error
}

// SecretReader reports whether a secret is present in the running
// process environment.
type SecretReader interface {
	Has(name string) bool
}

// SecretWriter makes a secret live now (os.Setenv) and persists it
// for next boot (env file).
type SecretWriter interface {
	Set(name, value string) error
}

// SmokeStatus is the last (or in-flight) smoke run for a project.
type SmokeStatus struct {
	TaskID  string
	Status  Status
	Detail  string
	USD     float64
	Running bool
}

// SmokeRunner triggers a smoke task and reports its latest state.
type SmokeRunner interface {
	Trigger(ctx context.Context, projectID, prompt string) (taskID string, err error)
	Latest(projectID string) (SmokeStatus, bool)
}

// Deps is the doctor's narrow read/act surface. All fields are
// interfaces so tests inject fakes. A nil field degrades that check
// to unknown rather than panicking.
type Deps struct {
	Registry     ProjectResolver
	MCP          MCPSnapshotSource
	Model        ModelPinger
	Secrets      SecretReader
	SecretWriter SecretWriter
	Smoke        SmokeRunner
	Logger       zerolog.Logger
}

// Doctor diagnoses one project's readiness.
type Doctor struct {
	deps Deps
}

// New builds a Doctor over the given deps.
func New(deps Deps) *Doctor { return &Doctor{deps: deps} }

// Run resolves the project once and runs every check, returning a
// full Report. A project that does not resolve still produces a
// report whose config_valid check is red (the other checks are
// skipped since they need a resolved project).
func (d *Doctor) Run(ctx context.Context, projectID string) Report {
	proj, err := d.resolve(projectID)
	rep := Report{ProjectID: projectID}
	if err != nil || proj == nil {
		stub := &registry.Project{ID: projectID}
		rep.Checks = []CheckResult{d.checkConfigValid(stub, orResolveErr(err))}
		rep.Complete = ComputeComplete(rep.Checks)
		return rep
	}
	rep.Checks = []CheckResult{
		d.checkConfigValid(proj, nil),
		d.checkSecrets(proj),
		d.checkModel(ctx),
		d.checkMCP(ctx, proj),
		d.checkSchedule(proj),
		d.checkSmoke(proj),
	}
	rep.Complete = ComputeComplete(rep.Checks)
	return rep
}

// RunOne runs a single check by key (for the per-check re-run
// endpoint). Returns an error for an unknown key. config_valid is
// runnable even when the project does not resolve (it reports the
// resolve error); every other key needs a resolved project.
func (d *Doctor) RunOne(ctx context.Context, projectID, key string) (CheckResult, error) {
	proj, err := d.resolve(projectID)
	if key == "config_valid" {
		stub := proj
		if stub == nil {
			stub = &registry.Project{ID: projectID}
		}
		return d.checkConfigValid(stub, orResolveErr(err)), nil
	}
	if err != nil || proj == nil {
		return CheckResult{}, fmt.Errorf("project %q does not resolve: %w", projectID, orResolveErr(err))
	}
	switch key {
	case "secrets":
		return d.checkSecrets(proj), nil
	case "model":
		return d.checkModel(ctx), nil
	case "mcp":
		return d.checkMCP(ctx, proj), nil
	case "schedule":
		return d.checkSchedule(proj), nil
	case "smoke":
		return d.checkSmoke(proj), nil
	default:
		return CheckResult{}, fmt.Errorf("unknown check %q", key)
	}
}

// QuickStatus is a cheap completeness signal for the project-list
// badge: it runs only the non-network checks (config_valid, secrets,
// schedule) so listing N projects doesn't fire N model pings / MCP
// probes. A project that is quick-incomplete is definitely
// incomplete; one that is quick-complete may still have a red model
// or mcp check visible on the full setup page.
func (d *Doctor) QuickStatus(projectID string) bool {
	proj, err := d.resolve(projectID)
	if err != nil || proj == nil {
		return false
	}
	checks := []CheckResult{
		d.checkConfigValid(proj, nil),
		d.checkSecrets(proj),
		d.checkSchedule(proj),
	}
	return ComputeComplete(checks)
}

// TriggerSmoke enqueues a smoke task for the project using its
// autonomy goal (or a minimal probe when it has none). Returns the
// new task id.
func (d *Doctor) TriggerSmoke(ctx context.Context, projectID string) (string, error) {
	if d.deps.Smoke == nil {
		return "", fmt.Errorf("smoke runs unavailable")
	}
	// Idempotency guard: if a smoke task is already in flight for this
	// project, return it instead of spawning another token-spending run
	// (the UI button's disabled state is client-side only). Final-review
	// finding.
	if last, ok := d.deps.Smoke.Latest(projectID); ok && last.Running {
		return last.TaskID, nil
	}
	proj, err := d.resolve(projectID)
	if err != nil || proj == nil {
		return "", fmt.Errorf("project %q does not resolve: %w", projectID, orResolveErr(err))
	}
	return d.deps.Smoke.Trigger(ctx, projectID, smokePrompt(proj))
}

// SetSecret stores a secret value (live + persisted) via the writer,
// but only when the project declares that secret name in
// Permissions.Secrets (exact case-sensitive match — env var names are
// case-sensitive). Without this gate, setting a name the project
// hasn't declared silently no-ops from the operator's point of view:
// the secrets check only iterates declared names, so the write is
// invisible and the value never surfaces anywhere. Companion review
// finding (2026-07-04).
func (d *Doctor) SetSecret(projectID, name, value string) error {
	if d.deps.SecretWriter == nil {
		return fmt.Errorf("secret writer unavailable")
	}
	proj, err := d.resolve(projectID)
	if err != nil || proj == nil {
		return fmt.Errorf("project %q does not resolve: %w", projectID, orResolveErr(err))
	}
	declared := false
	for _, n := range proj.Permissions.Secrets {
		if n == name {
			declared = true
			break
		}
	}
	if !declared {
		return fmt.Errorf("secret %q is not declared by project %q", name, projectID)
	}
	return d.deps.SecretWriter.Set(name, value)
}

// smokePrompt is the project's autonomy goal when set, else a
// minimal end-to-end probe that still exercises the swarm + model.
func smokePrompt(proj *registry.Project) string {
	if g := proj.Autonomy.Goal; g != "" {
		return g
	}
	return "Reply with exactly: OK"
}

func (d *Doctor) resolve(projectID string) (*registry.Project, error) {
	if d.deps.Registry == nil {
		return nil, fmt.Errorf("registry unavailable")
	}
	proj, _, _, err := d.deps.Registry.ResolveProjectConfig(projectID)
	return proj, err
}

func orResolveErr(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("project not found")
}
