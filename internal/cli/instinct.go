package cli

// `vornikctl instinct` — operator surface over the continuous-learning
// instinct layer (LLD: continuous-learning-instinct-layer-design.md).
//
//   vornikctl instinct list   [--domain D] [--scope S] [--project P]
//                            [--status ST] [--min-confidence F]
//                            [--limit N] [--json]
//   vornikctl instinct show   <id> [--json]
//   vornikctl instinct retire <id> [--json]
//   vornikctl instinct export [filters] [-o FILE]
//   vornikctl instinct import <file> [--dry-run]
//
// list / show / retire hit the daemon's /api/v1/instincts surface.
// export reads the daemon and writes a portable frontmatter file (the
// LLD frontmatter shape); import parses + validates such a file
// offline. All daemon-side operations are read / inspect / retire only.

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

var instinctCmd = &cobra.Command{
	Use:   "instinct",
	Short: "Inspect, retire, export, and import learned instincts",
	Long: `Browse the continuous-learning instinct layer.

Instincts are confidence-scored learned patterns ("in situation T,
action A held") mined from the audit spine. They are advisory: surfaced
as evidence behind their own gates, never auto-applied.

list / show / retire operate against the running daemon's
/api/v1/instincts surface. export / import are the cross-deployment
sharing primitive — export pulls matching instincts into a portable
frontmatter file; import parses + validates such a file.`,
}

func init() {
	rootCmd.AddCommand(instinctCmd)
}

// instinctEntry mirrors api.InstinctJSON. Kept local so the CLI doesn't
// import the api package (separate binary).
type instinctEntry struct {
	ID              string  `json:"id"`
	Scope           string  `json:"scope"`
	ProjectID       string  `json:"project_id,omitempty"`
	Domain          string  `json:"domain"`
	TriggerKey      string  `json:"trigger_key"`
	Trigger         string  `json:"trigger,omitempty"`
	Action          string  `json:"action"`
	Confidence      float64 `json:"confidence"`
	SupportCount    int     `json:"support_count"`
	ContradictCount int     `json:"contradict_count"`
	Source          string  `json:"source"`
	Status          string  `json:"status"`
	DistillModel    string  `json:"distill_model,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	LastSeenAt      string  `json:"last_seen_at"`
}

type instinctListResponse struct {
	Instincts []instinctEntry `json:"instincts"`
}

type instinctShowResponse struct {
	Instinct instinctEntry `json:"instinct"`
}

// writeJSON pretty-prints v to the command's stdout. Small shared
// helper for the instinct subcommands' --json output.
func writeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// marshalTriggerMap re-encodes a decoded trigger map back to the
// compact JSON string the trigger_json column / wire shape carry.
// Returns "" on a nil/empty map or a marshal error (the trigger is
// optional, so a degenerate value is simply dropped).
func marshalTriggerMap(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
