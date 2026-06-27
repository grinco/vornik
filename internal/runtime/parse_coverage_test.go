package runtime

import (
	"testing"
)

// TestParseInspectData_MapsStatusBranches — pin every status
// branch in the switch. A future Podman version adds a new
// status (e.g. "removing") and we silently classify it as
// "unknown" — that's the desired behaviour, but the test
// surfaces the addition to a reviewer.
func TestParseInspectData_MapsStatusBranches(t *testing.T) {
	cases := []struct {
		in   string
		want Status
	}{
		{"running", StatusRunning},
		{"stopped", StatusExited},
		{"exited", StatusExited},
		{"paused", StatusPaused},
		{"", StatusUnknown},
		{"something-new-podman-added", StatusUnknown},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			d := &podmanInspectData{
				Id: "abc", Name: "/vornik-test", Image: "vornik-agent:latest",
				State: podmanInspectState{Status: c.in, ExitCode: 0},
			}
			got := parseInspectData(d)
			if got.Status != c.want {
				t.Errorf("status %q → %q, want %q", c.in, got.Status, c.want)
			}
		})
	}
}

// TestParseInspectData_StripsNamePrefix — podman returns
// "/container-name" with a leading slash; we drop it so logs +
// UI render the bare name.
func TestParseInspectData_StripsNamePrefix(t *testing.T) {
	d := &podmanInspectData{Name: "/vornik-task-123"}
	got := parseInspectData(d)
	if got.Name != "vornik-task-123" {
		t.Errorf("Name = %q, want stripped", got.Name)
	}
	// Already-stripped name shouldn't be over-stripped.
	d2 := &podmanInspectData{Name: "no-slash"}
	got2 := parseInspectData(d2)
	if got2.Name != "no-slash" {
		t.Errorf("Name = %q, want unchanged", got2.Name)
	}
}

// TestParseInspectData_ExtractsLabels — the project / role /
// task association comes from container labels. Pin the mapping
// so a label-name rename surfaces here rather than in production.
func TestParseInspectData_ExtractsLabels(t *testing.T) {
	d := &podmanInspectData{
		Id: "abc",
		Config: podmanInspectConfig{
			Labels: map[string]string{
				LabelProjectID: "assistant",
				LabelRole:      "researcher",
				LabelTaskID:    "task_20260516_abc",
			},
		},
	}
	got := parseInspectData(d)
	if got.ProjectID != "assistant" || got.Role != "researcher" || got.TaskID != "task_20260516_abc" {
		t.Errorf("labels not extracted: %+v", got)
	}
}

// TestParseInspectData_NilLabelsSafe — defensive: containers
// created by something OTHER than vornik may not carry our
// labels; the parser must not panic on a nil map.
func TestParseInspectData_NilLabelsSafe(t *testing.T) {
	d := &podmanInspectData{Id: "abc", Config: podmanInspectConfig{Labels: nil}}
	got := parseInspectData(d)
	if got.ProjectID != "" || got.Role != "" || got.TaskID != "" {
		t.Errorf("expected empty labels on nil map, got %+v", got)
	}
}

// TestParseInspectData_CarriesIDExitCode — the obvious echo
// fields. ExitCode in particular matters for the executor's
// failure-classification path.
func TestParseInspectData_CarriesIDExitCode(t *testing.T) {
	d := &podmanInspectData{
		Id:    "ctr-abc",
		Image: "vornik-agent:latest",
		State: podmanInspectState{Status: "exited", ExitCode: 137},
	}
	got := parseInspectData(d)
	if got.ID != "ctr-abc" || got.Image != "vornik-agent:latest" || got.ExitCode != 137 {
		t.Errorf("inspect fields not echoed: %+v", got)
	}
}
