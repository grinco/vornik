package runtime

import "testing"

// TestQualifyAgentImage pins the fail-safe that keeps a bare vornik-agent
// short-name (which rootless podman can't resolve headless) from reaching a
// `podman run`. Recurring config drift (control-plane applies mirroring a
// swarm file drafted against a bare-image tree) re-introduced the unqualified
// form 3× — this primitive normalises it wherever it leaks in.
func TestQualifyAgentImage(t *testing.T) {
	cases := map[string]string{
		"vornik-agent:latest":                "ghcr.io/grinco/vornik-agent:latest",
		"vornik-agent":                       "ghcr.io/grinco/vornik-agent",
		"vornik-agent:2026.7.4":              "ghcr.io/grinco/vornik-agent:2026.7.4",
		"vornik-agent@sha256:abc":            "ghcr.io/grinco/vornik-agent@sha256:abc",
		"  vornik-agent:latest  ":            "ghcr.io/grinco/vornik-agent:latest", // trimmed
		"ghcr.io/grinco/vornik-agent:latest": "ghcr.io/grinco/vornik-agent:latest", // already qualified — unchanged
		"docker.io/library/golang:1.25":      "docker.io/library/golang:1.25",      // other registry — unchanged
		"ghcr.io/x/vornik-agent:latest":      "ghcr.io/x/vornik-agent:latest",      // registry-qualified — unchanged
		"":                                   "",                                   // empty — unchanged (Validate rejects separately)
		"some-other-image:latest":            "some-other-image:latest",            // not our agent — unchanged
	}
	for in, want := range cases {
		if got := QualifyAgentImage(in); got != want {
			t.Errorf("QualifyAgentImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestValidateQualifiesBareAgentImage confirms Validate() normalises a bare
// agent image on the config it will launch, so the spawn uses localhost/….
func TestValidateQualifiesBareAgentImage(t *testing.T) {
	c := &ContainerConfig{
		Image:     "vornik-agent:latest",
		ProjectID: "p1",
		Role:      "worker",
		TaskID:    "task_x",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Image != "ghcr.io/grinco/vornik-agent:latest" {
		t.Errorf("Validate did not qualify the image: got %q", c.Image)
	}
}
