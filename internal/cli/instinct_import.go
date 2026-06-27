package cli

// `vornikctl instinct import <file>` — parse + validate a portable
// instinct frontmatter file (the LLD frontmatter shape produced by
// `vornikctl instinct export`).
//
// Import is the read side of the cross-deployment sharing primitive. It
// parses the file, validates each entry's enum fields, and reports what
// would be imported. It does NOT mutate the daemon: there is no
// instinct-create write path in this slice (instincts are mined by the
// leader-elected worker, not hand-injected), so import is verify-only.
// This keeps the round-trip honest (export → import re-derives the same
// in-memory shape) and the unit read/inspect only.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	instinctImportDryRun bool
	instinctImportJSON   bool

	instinctImportCmd = &cobra.Command{
		Use:   "import <file>",
		Short: "Parse + validate a portable instinct frontmatter file",
		Long: `Import reads a SWARM instinct frontmatter file (as produced by
'vornikctl instinct export'), validates every entry, and reports the
parsed instincts.

Verify-only: this slice has no instinct-create write path (instincts are
mined by the daemon's extraction worker, not hand-injected), so import
never mutates the daemon — it confirms the file is well-formed and shows
what it carries.`,
		Args: cobra.ExactArgs(1),
		RunE: runInstinctImport,
	}
)

func init() {
	instinctImportCmd.Flags().BoolVar(&instinctImportDryRun, "dry-run", false, "Parse + validate only (default behaviour; kept for symmetry)")
	instinctImportCmd.Flags().BoolVar(&instinctImportJSON, "json", false, "Output the parsed instincts as JSON")
	instinctCmd.AddCommand(instinctImportCmd)
}

func runInstinctImport(cmd *cobra.Command, args []string) error {
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("instinct import: read %s: %w", args[0], err)
	}
	doc, err := parseInstinctFrontmatter(raw)
	if err != nil {
		return fmt.Errorf("instinct import: %w", err)
	}
	if err := validateInstinctFrontmatter(doc); err != nil {
		return fmt.Errorf("instinct import: %w", err)
	}
	if instinctImportJSON {
		// Re-emit as the wire shape so the round-trip is observable.
		entries := frontmatterToInstincts(doc)
		out := instinctListResponse{Instincts: entries}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "validated %d instincts from %s\n", len(doc.Instincts), args[0])
		return writeJSON(cmd, out)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Validated %d instinct(s) from %s (format v%d).\n",
		len(doc.Instincts), args[0], doc.Version)
	for _, e := range doc.Instincts {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  - [%s/%s] %s — %s\n",
			e.Domain, e.Scope, truncate(e.Action, 60), e.Status)
	}
	return nil
}

// parseInstinctFrontmatter decodes the YAML document. Pure helper so
// the round-trip test can exercise it directly.
func parseInstinctFrontmatter(raw []byte) (instinctFrontmatter, error) {
	var doc instinctFrontmatter
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return instinctFrontmatter{}, fmt.Errorf("invalid YAML: %w", err)
	}
	if doc.Version == 0 {
		// Be forgiving: a missing version defaults to the current one
		// rather than failing — older exports may predate the field.
		doc.Version = instinctFrontmatterVersion
	}
	if doc.Version != instinctFrontmatterVersion {
		return instinctFrontmatter{}, fmt.Errorf("unsupported frontmatter version %d (this build understands v%d)",
			doc.Version, instinctFrontmatterVersion)
	}
	return doc, nil
}

// instinctValidDomains / Scopes / Statuses mirror the persistence enum
// constants (duplicated so the CLI doesn't import internal/persistence).
var (
	instinctValidDomains  = map[string]bool{"recovery": true, "cost": true, "quality": true, "retrieval": true, "workflow": true}
	instinctValidScopes   = map[string]bool{"project": true, "global": true}
	instinctValidStatuses = map[string]bool{"candidate": true, "active": true, "promoted": true, "retired": true, "": true}
)

// validateInstinctFrontmatter checks each entry's required + enum fields.
// Reports the first problem with the offending index so a malformed file
// is easy to fix.
func validateInstinctFrontmatter(doc instinctFrontmatter) error {
	if len(doc.Instincts) == 0 {
		return fmt.Errorf("file carries no instincts")
	}
	for i, e := range doc.Instincts {
		if e.Domain == "" || !instinctValidDomains[e.Domain] {
			return fmt.Errorf("instinct[%d]: invalid or missing domain %q", i, e.Domain)
		}
		if e.Scope == "" || !instinctValidScopes[e.Scope] {
			return fmt.Errorf("instinct[%d]: invalid or missing scope %q", i, e.Scope)
		}
		if !instinctValidStatuses[e.Status] {
			return fmt.Errorf("instinct[%d]: invalid status %q", i, e.Status)
		}
		if e.Action == "" {
			return fmt.Errorf("instinct[%d]: action is required", i)
		}
		// A project-scoped instinct must name a project; a global one
		// must not (mirrors the schema's '' project_id for global).
		if e.Scope == "project" && e.ProjectID == "" {
			return fmt.Errorf("instinct[%d]: project scope requires project_id", i)
		}
		if e.Scope == "global" && e.ProjectID != "" {
			return fmt.Errorf("instinct[%d]: global scope must not carry project_id (got %q)", i, e.ProjectID)
		}
	}
	return nil
}

// frontmatterToInstincts converts the parsed document back to the wire
// entry shape, re-encoding the nested trigger map into the trigger_json
// string. The inverse of instinctsToFrontmatter — the round-trip test
// asserts export→import→export is stable.
func frontmatterToInstincts(doc instinctFrontmatter) []instinctEntry {
	out := make([]instinctEntry, 0, len(doc.Instincts))
	for _, e := range doc.Instincts {
		entry := instinctEntry{
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
		if len(e.Trigger) > 0 {
			entry.Trigger = marshalTriggerMap(e.Trigger)
		}
		out = append(out, entry)
	}
	return out
}
