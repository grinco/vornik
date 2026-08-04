package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRootCmd_Exists(t *testing.T) {
	assert.NotNil(t, rootCmd)
	// The CLI binary is vornikctl; the daemon (cmd/vornik) does not
	// use this cobra tree. Keep the Use field aligned with the binary
	// name so help output renders `vornikctl …` instead of `vornik …`.
	assert.Equal(t, "vornikctl", rootCmd.Use)
	assert.Equal(t, "vornik operator CLI", rootCmd.Short)
	assert.Contains(t, rootCmd.Long, "vornik")
}

func TestVersionCmd_Exists(t *testing.T) {
	assert.NotNil(t, versionCmd)
	assert.Equal(t, "version", versionCmd.Use)
	assert.Equal(t, "Print the version number", versionCmd.Short)
}

func TestRootCmd_HasSubcommands(t *testing.T) {
	cmds := rootCmd.Commands()
	assert.GreaterOrEqual(t, len(cmds), 1, "root should have at least one subcommand")
}

func TestVersionCmd_Output(t *testing.T) {
	buf := new(bytes.Buffer)
	versionCmd.SetOut(buf)
	versionCmd.Run(versionCmd, nil)

	assert.Contains(t, buf.String(), "vornikctl version")
	assert.Contains(t, buf.String(), Version)
}

func TestRootCmd_Execute(t *testing.T) {
	// Execute with help to avoid running anything
	rootCmd.SetArgs([]string{"--help"})
	err := Execute()
	assert.NoError(t, err)
}

func TestVersionCmd_Execute(t *testing.T) {
	rootCmd.SetArgs([]string{"version"})
	err := Execute()
	assert.NoError(t, err)
}

func TestRootCmd_UnknownCommand(t *testing.T) {
	rootCmd.SetArgs([]string{"unknown"})
	err := Execute()
	assert.Error(t, err)
}

func TestInit(t *testing.T) {
	// Test that init() was called and subcommands are registered
	cmds := rootCmd.Commands()
	var foundVersion bool
	for _, cmd := range cmds {
		if cmd.Use == "version" {
			foundVersion = true
			break
		}
	}
	assert.True(t, foundVersion, "version command should be registered")
}

func TestVersionCommandIncludesEdition(t *testing.T) {
	SetVersion("1.2.3")
	SetEdition("enterprise")
	t.Cleanup(func() { SetEdition("") }) // reset shared package state

	var out bytes.Buffer
	versionCmd.SetOut(&out)
	versionCmd.Run(versionCmd, nil)

	got := out.String()
	if !strings.Contains(got, "enterprise edition") {
		t.Errorf("version output %q does not mention the enterprise edition", got)
	}
	if !strings.Contains(got, "1.2.3") {
		t.Errorf("version output %q missing version", got)
	}
}

// REGRESSION 2026-08-03: the Makefile stamps -X main.BuildDate, but main() only
// called SetVersion + SetEdition, so BuildDate kept its "unknown" default —
// every release build printed "built unknown" and a `vornikctl report` body
// could not distinguish two builds of the same tag.
func TestSetBuildDate_ReachesTheVersionLine(t *testing.T) {
	orig := BuildDate
	t.Cleanup(func() { BuildDate = orig })

	SetBuildDate("2026-08-03T09:14:00Z")

	var out bytes.Buffer
	versionCmd.SetOut(&out)
	versionCmd.Run(versionCmd, nil)

	if !strings.Contains(out.String(), "2026-08-03T09:14:00Z") {
		t.Errorf("version output %q missing the stamped build date", out.String())
	}
}

// An unstamped ldflag must not blank out the honest fallback.
func TestSetBuildDate_EmptyKeepsTheFallback(t *testing.T) {
	orig := BuildDate
	t.Cleanup(func() { BuildDate = orig })

	BuildDate = "2026-08-03T09:14:00Z"
	SetBuildDate("")

	if BuildDate != "2026-08-03T09:14:00Z" {
		t.Errorf("BuildDate = %q, an empty stamp must not overwrite it", BuildDate)
	}
}

func TestVersionCommandDefaultsToCommunity(t *testing.T) {
	SetEdition("") // unstamped
	t.Cleanup(func() { SetEdition("") })

	var out bytes.Buffer
	versionCmd.SetOut(&out)
	versionCmd.Run(versionCmd, nil)

	if !strings.Contains(out.String(), "community edition") {
		t.Errorf("unstamped build should report community edition, got %q", out.String())
	}
}
