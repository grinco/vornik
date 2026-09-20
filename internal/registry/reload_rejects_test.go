package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Incident 2026-09-10, ~30 minutes of production outage (loader-validator
// agreement design §10, §12). A new optional project key, `autonomy.feeds`,
// was written into the DEPLOYED config tree of a running daemon whose binary
// predated the field. Project YAML decodes with KnownFields(true), so the
// unknown key is fatal to the whole document; loadProjects skips a file it
// cannot parse; and the reload then promoted a config set with the `assistant`
// project simply ABSENT. Autonomy, chat and email went dark together, and the
// only signal was a line on stderr.
//
// §11 made that legible. §12 makes it not happen: a reload that would drop a
// project is refused, and the running configuration keeps serving.
//
// Note this is a RELOAD test, not a startup test. The outage came through
// hot-reload — the daemon never restarted — so a startup gate would not have
// fired.

// projectTree writes a minimal loadable project tree and returns its dir.
func projectTree(t *testing.T, projectYAML map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	// The swarm a project references must exist: a dangling swarmId makes
	// stripInvalidProjects remove the project, which would look exactly like
	// the drop this test is about and mask it.
	const workflow = "---\nworkflowId: \"w1\"\ndisplayName: \"W\"\nentrypoint: \"go\"\nsteps:\n  go:\n    type: \"agent\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n\n## Prompts\n\n### go\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, "workflows", "w1.md"), []byte(workflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	const swarm = "---\nswarmId: \"s1\"\ndisplayName: \"S\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "s1.md"), []byte(swarm), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	for name, body := range projectYAML {
		if err := os.WriteFile(filepath.Join(dir, "projects", name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const goodProject = `projectId: "keeper"
displayName: "Keeper"
swarmId: "s1"
defaultWorkflowId: "w1"
`

// The key this binary does not know — the 2026-09-10 shape exactly: a config
// written ahead of the binary that owns the field.
const aheadOfBinaryProject = `projectId: "keeper"
displayName: "Keeper"
swarmId: "s1"
defaultWorkflowId: "w1"
autonomy:
  feeds_from_a_newer_release: true
`

// A reload whose tree drops a project must be refused, and the project the
// daemon is already serving must survive it.
func TestReload_RefusesATreeThatWouldDropAProject(t *testing.T) {
	dir := projectTree(t, map[string]string{"keeper.yaml": goodProject})
	r := New()
	if err := r.Load(dir); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if r.GetProject("keeper") == nil {
		t.Fatal("fixture did not load: keeper is absent before the reload")
	}

	// The deploy-ordering mistake: the key lands while this binary is running.
	if err := os.WriteFile(filepath.Join(dir, "projects", "keeper.yaml"),
		[]byte(aheadOfBinaryProject), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	err := r.Reload()

	if err == nil {
		t.Fatal("reload accepted a tree that drops a project; the running daemon " +
			"would lose it exactly as on 2026-09-10")
	}
	if r.GetProject("keeper") == nil {
		t.Error("the running configuration was replaced anyway — the project the " +
			"daemon was serving is gone, which is the outage this prevents")
	}
	// §11's diagnosis has to survive into the refusal, or the operator gets
	// "reload failed" and none of what makes it actionable.
	if !strings.Contains(err.Error(), "keeper.yaml") {
		t.Errorf("refusal does not name the offending file: %v", err)
	}
}

// A clean reload must still apply. The gate must not become a reason edits
// stop landing.
func TestReload_AcceptsACleanTree(t *testing.T) {
	dir := projectTree(t, map[string]string{"keeper.yaml": goodProject})
	r := New()
	if err := r.Load(dir); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	renamed := strings.Replace(goodProject, `displayName: "Keeper"`, `displayName: "Renamed"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "projects", "keeper.yaml"),
		[]byte(renamed), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if err := r.Reload(); err != nil {
		t.Fatalf("clean reload refused: %v", err)
	}
	p := r.GetProject("keeper")
	if p == nil {
		t.Fatal("keeper vanished after a clean reload")
	}
	if p.DisplayName != "Renamed" {
		t.Errorf("displayName = %q, want the reloaded value — the gate blocked a good edit", p.DisplayName)
	}
}

// Adding a project is not dropping one: a tree that gains a file and rejects
// nothing must activate.
func TestReload_AcceptsAnAddedProject(t *testing.T) {
	dir := projectTree(t, map[string]string{"keeper.yaml": goodProject})
	r := New()
	if err := r.Load(dir); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	second := strings.ReplaceAll(goodProject, "keeper", "second")
	if err := os.WriteFile(filepath.Join(dir, "projects", "second.yaml"),
		[]byte(second), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}

	if err := r.Reload(); err != nil {
		t.Fatalf("reload with an added project refused: %v", err)
	}
	if r.GetProject("second") == nil {
		t.Error("the added project did not activate")
	}
}

// The INITIAL load is deliberately unchanged: at boot there is no
// last-known-good to keep, so refusing would mean refusing to start — a
// separate policy question (§12). A degraded boot is not silent: §11's
// project_config_skew doctor check reports it at ERROR.
func TestLoad_StillStartsDegradedWhenAFileIsRejected(t *testing.T) {
	dir := projectTree(t, map[string]string{
		"keeper.yaml": goodProject,
		"broken.yaml": strings.ReplaceAll(aheadOfBinaryProject, "keeper", "broken"),
	})
	r := New()

	err := r.Load(dir)

	if r.GetProject("keeper") == nil {
		t.Error("a rejected sibling prevented a good project from loading at boot")
	}
	if r.GetProject("broken") != nil {
		t.Error("the rejected project loaded anyway — the decoder is not strict")
	}
	_ = err // a boot-time rejection may or may not surface as an error; not this test's subject
}
