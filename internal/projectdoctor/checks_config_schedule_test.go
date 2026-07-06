package projectdoctor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestCheckConfigValid(t *testing.T) {
	d := New(Deps{})
	// Resolve error => red.
	got := d.checkConfigValid(nil, errFake("swarmId not found"))
	if got.Status != StatusRed || !got.Required {
		t.Fatalf("resolve error: got %+v", got)
	}
	if got.FixHref == "" {
		t.Error("config_valid red must offer a fix link")
	}
	// Resolves => green.
	got = d.checkConfigValid(&registry.Project{ID: "p"}, nil)
	if got.Status != StatusGreen {
		t.Fatalf("clean resolve: got %+v", got)
	}
}

func TestCheckSchedule(t *testing.T) {
	d := New(Deps{})
	// Autonomy off => neutral, required=false.
	off := &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: false}}
	if got := d.checkSchedule(off); got.Status != StatusNeutral || got.Required {
		t.Fatalf("autonomy off: got %+v", got)
	}
	// Enabled + valid interval => green with next-fire detail.
	ok := &registry.Project{Autonomy: registry.ProjectAutonomy{
		Enabled: true, Mode: "llm", PollInterval: "4h", Goal: "do things",
	}}
	if got := d.checkSchedule(ok); got.Status != StatusGreen {
		t.Fatalf("valid schedule: got %+v", got)
	}
	// Enabled + unparseable interval => red.
	bad := &registry.Project{Autonomy: registry.ProjectAutonomy{
		Enabled: true, PollInterval: "soon", Goal: "x",
	}}
	if got := d.checkSchedule(bad); got.Status != StatusRed {
		t.Fatalf("bad interval: got %+v", got)
	}
	// Cron mode without a goal => red (cron fires the goal verbatim).
	cron := &registry.Project{Autonomy: registry.ProjectAutonomy{
		Enabled: true, Mode: registry.AutonomyModeCron, PollInterval: "24h", Goal: "",
	}}
	if got := d.checkSchedule(cron); got.Status != StatusRed {
		t.Fatalf("cron without goal: got %+v", got)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
