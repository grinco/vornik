package cli

// `vornikctl skill` — portable SWARM-SKILL.md surface.
//
// One command-tree, four leaves:
//
//   - export   read live registry state, emit SWARM-SKILL.md bytes
//   - import   materialise a SWARM-SKILL.md into the deployed config tree
//   - validate run the SKILL.md-shape validator over a file or dir
//
// The commands sit on top of internal/registry's parser, serialiser,
// and validator — no business logic lives in this file. Splitting the
// parent + each subcommand into their own files keeps each ≤200
// lines and makes the test files a clear 1:1 map.

import (
	"github.com/spf13/cobra"
)

var (
	skillCmd = &cobra.Command{
		Use:   "skill",
		Short: "Export, import, and validate portable SWARM-SKILL.md files",
		Long: `Portable SKILL.md interop layer.

A SWARM-SKILL.md is a single Markdown file that bundles a workflow
plus the roles its steps reference, with YAML frontmatter shaped
after the agentskills.io SKILL.md spec. The file is publishable
(one file, one curl) and ingestable (one vornikctl call materialises
the workflow + roles into the deployed config tree).`,
	}
)

func init() {
	rootCmd.AddCommand(skillCmd)
}
