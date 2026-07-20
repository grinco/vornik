package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// TestProjectPermissions_APIProvidersUnmarshal covers query_api
// provider-discovery design §4.2: a project's permissions.api_providers
// list unmarshals into ProjectPermissions.APIProviders.
func TestProjectPermissions_APIProvidersUnmarshal(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	if err := os.Mkdir(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yamlSrc := `projectId: "janka"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
permissions:
  api_providers:
    - "maps"
    - "headmatch-ats"
`
	if err := os.WriteFile(filepath.Join(projectsDir, "janka.yaml"), []byte(yamlSrc), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	projects, err := LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}

	p := projects["janka"]
	if p == nil {
		t.Fatal("expected project \"janka\" to load")
	}
	want := []string{"maps", "headmatch-ats"}
	if len(p.Permissions.APIProviders) != len(want) {
		t.Fatalf("APIProviders = %v, want %v", p.Permissions.APIProviders, want)
	}
	for i, v := range want {
		if p.Permissions.APIProviders[i] != v {
			t.Errorf("APIProviders[%d] = %q, want %q", i, p.Permissions.APIProviders[i], v)
		}
	}
}

// TestProjectPermissions_APIProvidersUnsetIsNil pins the empty=all
// convention's data-layer contract: omitting permissions.api_providers
// entirely leaves the field nil (not an empty-but-non-nil slice), matching
// how AllowedTools/AllowedProjects already behave elsewhere.
func TestProjectPermissions_APIProvidersUnsetIsNil(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	if err := os.Mkdir(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yamlSrc := `projectId: "no-allowlist"
swarmId: "test-swarm"
defaultWorkflowId: "test-workflow"
`
	if err := os.WriteFile(filepath.Join(projectsDir, "no-allowlist.yaml"), []byte(yamlSrc), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	projects, err := LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}

	p := projects["no-allowlist"]
	if p == nil {
		t.Fatal("expected project \"no-allowlist\" to load")
	}
	if p.Permissions.APIProviders != nil {
		t.Errorf("APIProviders = %#v, want nil (unset ⇒ nil, empty=all convention)", p.Permissions.APIProviders)
	}
}

// TestWarnUnknownAPIProviders_WarnsOnUnknownName covers §4.3 of the
// query_api provider-discovery design: a project's api_providers naming a
// provider absent from gateway.providers logs a WARNING via the caller's
// zerolog.Logger, but never returns an error or panics — the daemon must
// still boot on a stale/typo'd allowlist entry.
func TestWarnUnknownAPIProviders_WarnsOnUnknownName(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.WarnLevel)

	projects := []*Project{
		{
			ID:          "janka",
			Permissions: ProjectPermissions{APIProviders: []string{"maps", "headmatch-ats"}},
		},
	}
	knownProviders := map[string]bool{"maps": true}

	WarnUnknownAPIProviders(logger, projects, knownProviders)

	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("headmatch-ats")) {
		t.Fatalf("expected a warning naming the unknown provider %q, got log: %s", "headmatch-ats", out)
	}
	if !bytes.Contains(buf.Bytes(), []byte("janka")) {
		t.Fatalf("expected the warning to name the offending project %q, got log: %s", "janka", out)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"maps"`)) {
		t.Errorf("registered provider %q should not be warned about, got log: %s", "maps", out)
	}
}

// TestWarnUnknownAPIProviders_NoWarnWhenAllKnown pins the no-noise case: an
// allowlist naming only registered providers produces no log output.
func TestWarnUnknownAPIProviders_NoWarnWhenAllKnown(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.WarnLevel)

	projects := []*Project{
		{
			ID:          "janka",
			Permissions: ProjectPermissions{APIProviders: []string{"maps"}},
		},
	}
	knownProviders := map[string]bool{"maps": true}

	WarnUnknownAPIProviders(logger, projects, knownProviders)

	if buf.Len() != 0 {
		t.Errorf("expected no warning when every allowlisted provider is known, got: %s", buf.String())
	}
}

// TestWarnUnknownAPIProviders_EmptyAllowlistNoWarn pins the empty=all
// convention: a project with an unset/empty allowlist never warns,
// regardless of what's registered.
func TestWarnUnknownAPIProviders_EmptyAllowlistNoWarn(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.WarnLevel)

	projects := []*Project{
		{ID: "no-allowlist"},
	}
	knownProviders := map[string]bool{"maps": true}

	WarnUnknownAPIProviders(logger, projects, knownProviders)

	if buf.Len() != 0 {
		t.Errorf("expected no warning for an empty/unset allowlist, got: %s", buf.String())
	}
}

// TestWarnUnknownAPIProviders_NilSafe pins that the diagnostic never panics
// on a nil projects map, nil entries, or nil knownProviders — this is a
// load-time warning, not a gate, so it must degrade gracefully.
func TestWarnUnknownAPIProviders_NilSafe(_ *testing.T) {
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.WarnLevel)

	WarnUnknownAPIProviders(logger, nil, nil)

	projects := []*Project{nil}
	WarnUnknownAPIProviders(logger, projects, nil)
}
