package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// managerCovFakeInspect returns a fake-podman script that emits a single
// inspect record with the given status/name/labels, and otherwise exits 1.
func managerCovFakeInspect(t *testing.T) string {
	t.Helper()
	return writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "inspect" ]]; then
  cat <<'JSON'
[{"Id":"abc123","Name":"/vornik-proj-worker","Image":"alpine:latest","State":{"Status":"running","ExitCode":0},"Config":{"Labels":{"vornik.projectId":"proj","vornik.role":"worker","vornik.taskId":"task-1"}}}]
JSON
  exit 0
fi
exit 1
`)
}

func TestInspectContainer_ParsesRunningWithLabels(t *testing.T) {
	m := &Manager{podmanPath: managerCovFakeInspect(t)}

	c, err := m.InspectContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("InspectContainer() error = %v", err)
	}
	if c.ID != "abc123" {
		t.Errorf("ID = %q, want abc123", c.ID)
	}
	if c.Name != "vornik-proj-worker" {
		t.Errorf("Name = %q, want leading slash stripped", c.Name)
	}
	if c.Status != StatusRunning {
		t.Errorf("Status = %q, want running", c.Status)
	}
	if c.ProjectID != "proj" || c.Role != "worker" || c.TaskID != "task-1" {
		t.Errorf("labels not mapped: %+v", c)
	}
}

func TestInspectContainer_NoSuchContainerReturnsNotFound(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "Error: no such container abc" >&2
exit 125
`)}

	_, err := m.InspectContainer(context.Background(), "abc")
	if err == nil {
		t.Fatal("expected error")
	}
	var notFound *ContainerNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ContainerNotFoundError, got %T: %v", err, err)
	}
}

func TestInspectContainer_EmptyArrayReturnsNotFound(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo '[]'
exit 0
`)}

	_, err := m.InspectContainer(context.Background(), "gone")
	var notFound *ContainerNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ContainerNotFoundError for empty inspect, got %v", err)
	}
}

func TestInspectContainer_BadJSONReturnsParseError(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo 'not json'
exit 0
`)}

	_, err := m.InspectContainer(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "failed to parse podman inspect output") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestListContainers_DefaultFilterParsesEntries(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "ps" ]]; then
  cat <<'JSON'
[{"ID":"c1","Names":["/vornik-a"],"Image":"img","State":"running","ExitCode":0,"Labels":{"vornik.projectId":"p","vornik.role":"r","vornik.taskId":"t"}},
 {"ID":"c2","Names":["vornik-b"],"Image":"img","State":"Exited","ExitCode":3,"Labels":{}}]
JSON
  exit 0
fi
exit 1
`)}

	cs, err := m.ListContainers(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListContainers() error = %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("len = %d, want 2", len(cs))
	}
	if cs[0].Status != StatusRunning || cs[0].Name != "vornik-a" {
		t.Errorf("c1 unexpected: %+v", cs[0])
	}
	if cs[0].ProjectID != "p" || cs[0].Role != "r" || cs[0].TaskID != "t" {
		t.Errorf("c1 labels not mapped: %+v", cs[0])
	}
	if cs[1].Status != StatusExited || cs[1].ExitCode != 3 {
		t.Errorf("c2 unexpected: %+v", cs[1])
	}
}

func TestListContainers_EmptyOutputReturnsEmptySlice(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
exit 0
`)}

	cs, err := m.ListContainers(context.Background(), map[string]string{"vornik.role": "coder"})
	if err != nil {
		t.Fatalf("ListContainers() error = %v", err)
	}
	if len(cs) != 0 {
		t.Fatalf("want empty slice, got %d", len(cs))
	}
}

func TestListContainers_PodmanFailureSurfacesError(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "boom" >&2
exit 7
`)}

	_, err := m.ListContainers(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "podman ps failed") {
		t.Fatalf("expected ps failure, got %v", err)
	}
}

func TestGetContainerByTask_FoundAndNotFound(t *testing.T) {
	found := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "ps" ]]; then
  echo '[{"ID":"c1","Names":["/x"],"Image":"img","State":"running","Labels":{"vornik.taskId":"task-9"}}]'
  exit 0
fi
exit 1
`)}
	c, err := found.GetContainerByTask(context.Background(), "task-9")
	if err != nil {
		t.Fatalf("GetContainerByTask() error = %v", err)
	}
	if c.ID != "c1" {
		t.Errorf("ID = %q, want c1", c.ID)
	}

	empty := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "ps" ]]; then echo '[]'; exit 0; fi
exit 1
`)}
	_, err = empty.GetContainerByTask(context.Background(), "missing")
	var notFound *ContainerNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ContainerNotFoundError, got %v", err)
	}
}

func TestGetContainersByProjectAndRole_BuildFilters(t *testing.T) {
	// The fake echoes its own argv as JSON-ish container names so the test
	// can assert the label filters were threaded through to podman ps.
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "ps" ]]; then
  if [[ "$*" == *"vornik.projectId=proj"* ]]; then
    echo '[{"ID":"byproj","Names":["/x"],"Image":"img","State":"running"}]'
    exit 0
  fi
  echo '[]'
  exit 0
fi
exit 1
`)}

	byProj, err := m.GetContainersByProject(context.Background(), "proj")
	if err != nil {
		t.Fatalf("GetContainersByProject() error = %v", err)
	}
	if len(byProj) != 1 || byProj[0].ID != "byproj" {
		t.Fatalf("project filter not applied: %+v", byProj)
	}

	byRole, err := m.GetContainersByRole(context.Background(), "proj", "coder")
	if err != nil {
		t.Fatalf("GetContainersByRole() error = %v", err)
	}
	if len(byRole) != 1 {
		t.Fatalf("role filter result = %+v", byRole)
	}
}

func TestRemoveContainer_SuccessAndAlreadyGone(t *testing.T) {
	ok := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "rm" ]]; then exit 0; fi
exit 1
`)}
	if err := ok.RemoveContainer(context.Background(), "c1", true); err != nil {
		t.Fatalf("RemoveContainer() error = %v", err)
	}

	gone := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "Error: no such container c2" >&2
exit 1
`)}
	if err := gone.RemoveContainer(context.Background(), "c2", false); err != nil {
		t.Fatalf("RemoveContainer() should treat 'no such container' as success, got %v", err)
	}
}

func TestRemoveContainer_RealFailureSurfacesError(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "container is in use" >&2
exit 2
`)}
	err := m.RemoveContainer(context.Background(), "c3", false)
	if err == nil || !strings.Contains(err.Error(), "podman rm failed") {
		t.Fatalf("expected rm failure, got %v", err)
	}
}

func TestStopContainer_NotFoundAndFailure(t *testing.T) {
	gone := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "Error: no such container" >&2
exit 125
`)}
	err := gone.StopContainer(context.Background(), "c1", true)
	var notFound *ContainerNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ContainerNotFoundError, got %v", err)
	}

	failing := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "daemon down" >&2
exit 1
`)}
	err = failing.StopContainer(context.Background(), "c1", false)
	if err == nil || !strings.Contains(err.Error(), "podman stop failed") {
		t.Fatalf("expected stop failure, got %v", err)
	}
}

func TestStopContainer_Success(t *testing.T) {
	m := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "stop" ]]; then exit 0; fi
exit 1
`)}
	if err := m.StopContainer(context.Background(), "c1", true); err != nil {
		t.Fatalf("StopContainer() error = %v", err)
	}
}

func TestPullImage_SuccessAndFailure(t *testing.T) {
	ok := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "pull" ]]; then exit 0; fi
exit 1
`)}
	if err := ok.PullImage(context.Background(), "alpine:latest"); err != nil {
		t.Fatalf("PullImage() error = %v", err)
	}

	bad := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "manifest unknown" >&2
exit 1
`)}
	err := bad.PullImage(context.Background(), "nope:latest")
	if err == nil || !strings.Contains(err.Error(), "podman pull failed") {
		t.Fatalf("expected pull failure, got %v", err)
	}
}

func TestLogs_TailSuccessNotFoundAndFailure(t *testing.T) {
	ok := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "logs" ]]; then
  # Assert --tail was threaded through when tail > 0.
  if [[ "$*" == *"--tail 5"* ]]; then echo "hello log"; exit 0; fi
  echo "no tail flag"; exit 0
fi
exit 1
`)}
	out, err := ok.Logs(context.Background(), "c1", 5)
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if !strings.Contains(out, "hello log") {
		t.Errorf("Logs output = %q, want tail flag applied", out)
	}

	gone := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "Error: no such container" >&2
exit 125
`)}
	_, err = gone.Logs(context.Background(), "c2", 0)
	var notFound *ContainerNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ContainerNotFoundError, got %v", err)
	}

	failing := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
echo "logs unavailable" >&2
exit 1
`)}
	_, err = failing.Logs(context.Background(), "c3", 0)
	if err == nil || !strings.Contains(err.Error(), "podman logs failed") {
		t.Fatalf("expected logs failure, got %v", err)
	}
}

func TestIsAvailable_TrueAndFalse(t *testing.T) {
	avail := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
if [[ "$1" == "version" ]]; then echo '{"Client":{"Version":"5.0.0"}}'; exit 0; fi
exit 1
`)}
	if !avail.IsAvailable() {
		t.Error("IsAvailable() = false, want true")
	}

	down := &Manager{podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
exit 1
`)}
	if down.IsAvailable() {
		t.Error("IsAvailable() = true, want false")
	}
}

func TestPodmanPath_ReturnsConfiguredPath(t *testing.T) {
	m := &Manager{podmanPath: "/usr/bin/podman"}
	if got := m.PodmanPath(); got != "/usr/bin/podman" {
		t.Errorf("PodmanPath() = %q, want /usr/bin/podman", got)
	}
}
