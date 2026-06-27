package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRenderCLI_EmitsCommandsAndFlags(t *testing.T) {
	root := &cobra.Command{Use: "vornikctl", Short: "root short"}
	sub := &cobra.Command{Use: "task", Short: "manage tasks", Run: func(*cobra.Command, []string) {}}
	sub.Flags().String("priority", "normal", "task priority")
	hidden := &cobra.Command{Use: "secret", Hidden: true, Run: func(*cobra.Command, []string) {}}
	internalCmd := &cobra.Command{Use: "admin", Short: "internal admin", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(sub, hidden, internalCmd)

	out := renderCLI(root, map[string]bool{"task": true})
	if !strings.Contains(out, "## vornikctl task") {
		t.Errorf("expected task command heading; got:\n%s", out)
	}
	if !strings.Contains(out, "`--priority`") || !strings.Contains(out, "task priority") {
		t.Errorf("expected priority flag row; got:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("hidden command must not appear; got:\n%s", out)
	}
	if strings.Contains(out, "## vornikctl admin") {
		t.Errorf("non-allowlisted top-level command must not appear; got:\n%s", out)
	}
}

func TestRenderConfig_OnlyDocTaggedFields(t *testing.T) {
	type Nested struct {
		Enabled bool   `yaml:"enabled" doc:"Turn the thing on."`
		Secret  string `yaml:"secret"` // no doc tag => excluded
	}
	type Cfg struct {
		API      Nested `yaml:"api"`
		Internal string `yaml:"internal_only"` // no doc tag => excluded
	}

	out := renderConfig(reflect.TypeOf(Cfg{}))
	if !strings.Contains(out, "`api.enabled`") {
		t.Errorf("doc-tagged nested key must appear; got:\n%s", out)
	}
	if !strings.Contains(out, "Turn the thing on.") {
		t.Errorf("doc description must appear; got:\n%s", out)
	}
	if strings.Contains(out, "api.secret") || strings.Contains(out, "internal_only") {
		t.Errorf("untagged fields must be excluded (deny-by-default); got:\n%s", out)
	}
	if !strings.Contains(out, "## api") {
		t.Errorf("expected section grouping by top-level key; got:\n%s", out)
	}
}
