package service

import (
	"os"
	"path/filepath"
	"testing"
)

// isolatedConfigPath returns a config-file path whose sibling registry tree is EMPTY
// but structurally valid, so a container test cannot pick up the ambient one.
//
// WHY THIS EXISTS. NewContainer's second argument is the config FILE path, and the
// registry is resolved relative to it — `<dir-of-config>/configs` — before falling
// through to `~/.config/vornik/configs`. Container tests passing "" therefore had no
// anchor and loaded the DEVELOPER'S OR OPERATOR'S LIVE PROJECTS. Their own output
// showed it: `registry loaded config_dir=/home/.../.config/vornik/configs` with a real
// project_count.
//
// So those tests passed or failed on the machine's configuration rather than on the
// code. It surfaced on 2026-07-30 when a real project gained a `slack:` block: eight
// container tests began failing with `signing_secret_env ... is unset or empty`,
// because the live config names a secret the test process does not hold. None of the
// tests was about Slack.
//
// All three subdirectories are created because the resolver only accepts a candidate
// tree containing projects/, swarms/ AND workflows/ — a partial tree is skipped
// silently and resolution falls through to the ambient one, which is the exact trap
// this helper exists to close.
func isolatedConfigPath(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(root, "configs", sub), 0o755); err != nil {
			t.Fatalf("isolatedConfigPath: create %s: %v", sub, err)
		}
	}
	// VORNIK_CONFIGS_DIR OUTRANKS the config-path anchor, so it must be pointed at the
	// isolated tree too — otherwise a developer or operator shell that exports it (this
	// host does) leaks the live registry into the test regardless of the path passed to
	// NewContainer. That is precisely how this defect stayed invisible: the anchor
	// looked sufficient and the env var silently won. t.Setenv restores it afterwards.
	t.Setenv("VORNIK_CONFIGS_DIR", filepath.Join(root, "configs"))

	// The file need not exist — only its directory anchors the fallback chain.
	return filepath.Join(root, "config.yaml")
}
