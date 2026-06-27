package registry

import "testing"

// TestNormalizedAutonomyMode pins the defaulting + case
// normalisation contract that the autonomy manager relies on to
// dispatch ticks. Anything that doesn't match cron/backlog must
// fall back to llm so an empty or typo'd YAML doesn't break the
// existing LLM-driven projects.
func TestNormalizedAutonomyMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
	}{
		{"empty_defaults_to_llm", "", AutonomyModeLLM},
		{"whitespace_defaults_to_llm", "   ", AutonomyModeLLM},
		{"explicit_llm", "llm", AutonomyModeLLM},
		{"uppercase_cron", "CRON", AutonomyModeCron},
		{"mixedcase_backlog", "Backlog", AutonomyModeBacklog},
		{"unknown_falls_back_to_llm", "manual", AutonomyModeLLM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Project{Autonomy: ProjectAutonomy{Mode: c.mode}}
			if got := p.NormalizedAutonomyMode(); got != c.want {
				t.Errorf("NormalizedAutonomyMode(mode=%q) = %q, want %q", c.mode, got, c.want)
			}
		})
	}
}

func TestNormalizedAutonomyMode_NilProject(t *testing.T) {
	var p *Project
	if got := p.NormalizedAutonomyMode(); got != AutonomyModeLLM {
		t.Errorf("nil project must return llm, got %q", got)
	}
}

// TestResolveBacklogFilePath asserts the safety contract for the
// backlog file: defaults to BACKLOG.md when unset, rejects
// absolute paths and `..` traversal so an operator typo can't
// point autonomy at host files.
func TestResolveBacklogFilePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty_defaults", "", "BACKLOG.md"},
		{"whitespace_defaults", "  ", "BACKLOG.md"},
		{"custom_kept", "queue.md", "queue.md"},
		{"subdir_kept", "ops/queue.md", "ops/queue.md"},
		{"trims_whitespace", "  queue.md  ", "queue.md"},
		{"absolute_rejected", "/tmp/queue.md", ""},
		{"parent_rejected", "../queue.md", ""},
		{"nested_traversal_rejected", "ops/../../escape.md", ""},
		{"clean_collapses_dot", "./BACKLOG.md", "BACKLOG.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Project{Autonomy: ProjectAutonomy{BacklogFilePath: c.in}}
			if got := p.ResolveBacklogFilePath(); got != c.want {
				t.Errorf("ResolveBacklogFilePath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestResolveBacklogFilePath_NilProject(t *testing.T) {
	var p *Project
	if got := p.ResolveBacklogFilePath(); got != "" {
		t.Errorf("nil project must return empty path, got %q", got)
	}
}

// TestResolveCronTaskType walks the fallback chain: explicit
// field → first allowed type → "task". Pins the rule operators
// rely on when they configure cron-mode projects with only the
// allowedTaskTypes list set.
func TestResolveCronTaskType(t *testing.T) {
	cases := []struct {
		name        string
		cronType    string
		allowedList []string
		want        string
	}{
		{"explicit_wins", "place_orders", []string{"research"}, "place_orders"},
		{"empty_falls_to_allowed", "", []string{"research", "exec"}, "research"},
		{"empty_with_blank_allowed", "", []string{"", "research"}, "research"},
		{"fully_empty_defaults_task", "", nil, "task"},
		{"trims_whitespace", "  place  ", []string{"x"}, "place"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Project{Autonomy: ProjectAutonomy{
				CronTaskType:     c.cronType,
				AllowedTaskTypes: c.allowedList,
			}}
			if got := p.ResolveCronTaskType(); got != c.want {
				t.Errorf("ResolveCronTaskType = %q, want %q", got, c.want)
			}
		})
	}
}
