package cli

// `vornikctl instinct export` — pull matching instincts out of the
// running daemon and emit a portable file in the LLD frontmatter shape.
//
// The file is the cross-deployment sharing primitive: a YAML document
// with a top-level `instincts:` list, each entry carrying the
// persistence shape. The structured `trigger` is emitted as a nested
// YAML map (it round-trips to/from the trigger_json column at this
// boundary — the LLD's frontmatter `trigger` maps to trigger_json).
//
// Export is read-only against the daemon (GET /api/v1/instincts).

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// instinctFrontmatter is the portable document shape. `Version` lets a
// future reader detect format drift; the list mirrors the persistence
// shape with `trigger` decoded from trigger_json into a nested map.
type instinctFrontmatter struct {
	Version   int                        `yaml:"version" json:"version"`
	Instincts []instinctFrontmatterEntry `yaml:"instincts" json:"instincts"`
}

// instinctFrontmatterEntry is one instinct in the portable file. Trigger
// is a decoded map so the file is human-readable; derived/volatile fields
// (confidence, counts, timestamps) are carried for provenance but are
// recomputed on the importing side from evidence, never trusted verbatim.
type instinctFrontmatterEntry struct {
	ID              string         `yaml:"id,omitempty" json:"id,omitempty"`
	Scope           string         `yaml:"scope" json:"scope"`
	ProjectID       string         `yaml:"project_id,omitempty" json:"project_id,omitempty"`
	Domain          string         `yaml:"domain" json:"domain"`
	TriggerKey      string         `yaml:"trigger_key,omitempty" json:"trigger_key,omitempty"`
	Trigger         map[string]any `yaml:"trigger,omitempty" json:"trigger,omitempty"`
	Action          string         `yaml:"action" json:"action"`
	Confidence      float64        `yaml:"confidence,omitempty" json:"confidence,omitempty"`
	SupportCount    int            `yaml:"support_count,omitempty" json:"support_count,omitempty"`
	ContradictCount int            `yaml:"contradict_count,omitempty" json:"contradict_count,omitempty"`
	Source          string         `yaml:"source,omitempty" json:"source,omitempty"`
	Status          string         `yaml:"status,omitempty" json:"status,omitempty"`
	DistillModel    string         `yaml:"distill_model,omitempty" json:"distill_model,omitempty"`
}

// instinctFrontmatterVersion is the current portable-file format version.
const instinctFrontmatterVersion = 1

var (
	instinctExportOutput  string
	instinctExportDomain  string
	instinctExportScope   string
	instinctExportProject string
	instinctExportStatus  string
	instinctExportMinConf float64
	instinctExportLimit   int

	instinctExportCmd = &cobra.Command{
		Use:   "export",
		Short: "Export matching instincts to a portable frontmatter file",
		Long: `Export pulls instincts matching the filter out of the running daemon
and writes them in the LLD frontmatter shape (a YAML document with a
top-level instincts: list). The structured trigger is emitted as a
nested map; it round-trips to the trigger_json column on import.

  vornikctl instinct export --domain recovery -o recovery-instincts.yaml`,
		RunE: runInstinctExport,
	}
)

func init() {
	instinctExportCmd.Flags().StringVarP(&instinctExportOutput, "output", "o", "", "Write to file instead of stdout")
	instinctExportCmd.Flags().StringVar(&instinctExportDomain, "domain", "", "Filter by domain")
	instinctExportCmd.Flags().StringVar(&instinctExportScope, "scope", "", "Filter by scope")
	instinctExportCmd.Flags().StringVar(&instinctExportProject, "project", "", "Filter by project ID")
	instinctExportCmd.Flags().StringVar(&instinctExportStatus, "status", "", "Filter by status")
	instinctExportCmd.Flags().Float64Var(&instinctExportMinConf, "min-confidence", 0, "Only instincts with confidence >= this (0-1)")
	instinctExportCmd.Flags().IntVarP(&instinctExportLimit, "limit", "n", 1000, "Maximum rows to export (1-1000)")
	instinctCmd.AddCommand(instinctExportCmd)
}

func runInstinctExport(cmd *cobra.Command, _ []string) error {
	path := instinctListQuery(instinctExportDomain, instinctExportScope, instinctExportProject,
		instinctExportStatus, instinctExportMinConf, instinctExportLimit)
	rows, err := fetchInstincts(path)
	if err != nil {
		return fmt.Errorf("instinct export: %w", err)
	}
	doc, err := instinctsToFrontmatter(rows)
	if err != nil {
		return fmt.Errorf("instinct export: %w", err)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("instinct export: marshal failed: %w", err)
	}
	if instinctExportOutput == "" {
		_, err := cmd.OutOrStdout().Write(out)
		return err
	}
	if err := os.WriteFile(instinctExportOutput, out, 0o644); err != nil {
		return fmt.Errorf("instinct export: write %s: %w", instinctExportOutput, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d instincts, %d bytes)\n", instinctExportOutput, len(rows), len(out))
	return nil
}

// instinctsToFrontmatter converts the wire entries to the portable
// document, decoding the raw trigger_json string into a nested map.
// Pure — no I/O, the round-trip test exercises it directly.
func instinctsToFrontmatter(rows []instinctEntry) (instinctFrontmatter, error) {
	doc := instinctFrontmatter{
		Version:   instinctFrontmatterVersion,
		Instincts: make([]instinctFrontmatterEntry, 0, len(rows)),
	}
	for _, e := range rows {
		entry := instinctFrontmatterEntry{
			ID:              e.ID,
			Scope:           e.Scope,
			ProjectID:       e.ProjectID,
			Domain:          e.Domain,
			TriggerKey:      e.TriggerKey,
			Action:          e.Action,
			Confidence:      e.Confidence,
			SupportCount:    e.SupportCount,
			ContradictCount: e.ContradictCount,
			Source:          e.Source,
			Status:          e.Status,
			DistillModel:    e.DistillModel,
		}
		if e.Trigger != "" {
			var trig map[string]any
			if err := json.Unmarshal([]byte(e.Trigger), &trig); err != nil {
				return instinctFrontmatter{}, fmt.Errorf("instinct %s: invalid trigger_json: %w", e.ID, err)
			}
			entry.Trigger = trig
		}
		doc.Instincts = append(doc.Instincts, entry)
	}
	return doc, nil
}
