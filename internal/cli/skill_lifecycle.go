package cli

// `vornikctl skill list / remove / info / update` — the
// lifecycle quartet that operates on the install ledger.
//
// list:   tabular dump of the ledger; --json passes through raw.
// remove: drops materialised files + source mirror + ledger row.
// info:   ledger row + on-disk source-mirror frontmatter summary.
// update: re-runs the install path against the recorded source.
//
// All four share one file because each is a thin wrapper around
// the ledger helpers; spreading them across separate files would
// just create import churn.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

// --- list -----------------------------------------------------

var (
	skillListJSON bool

	skillListCmd = &cobra.Command{
		Use:   "list",
		Short: "List installed skills",
		RunE:  runSkillList,
	}
)

func init() {
	skillListCmd.Flags().BoolVar(&skillListJSON, "json", false, "Output as JSON")
	skillCmd.AddCommand(skillListCmd)
}

func runSkillList(cmd *cobra.Command, args []string) error {
	ledger, err := loadLedgerForCommand()
	if err != nil {
		return err
	}
	if skillListJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(ledger)
	}
	if len(ledger.Skills) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No skills installed.")
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Install one via: vornikctl skill install <source>")
		return nil
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "HANDLE\tSKILL\tVERSION\tREVISION\tINSTALLED")
	for _, e := range ledger.Skills {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Handle, e.Skill, e.SkillVersion, abbreviateRevision(e.SourceRevision), e.InstalledAt.Format("2006-01-02 15:04"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nTotal: %d\n", len(ledger.Skills))
	return nil
}

// abbreviateRevision shortens long SHA strings for the list
// view; tags and pseudo-revisions ("local"/"https") pass through.
func abbreviateRevision(rev string) string {
	if len(rev) >= 40 {
		return rev[:8]
	}
	return rev
}

// --- info -----------------------------------------------------

var (
	skillInfoJSON bool

	skillInfoCmd = &cobra.Command{
		Use:   "info <handle>/<skill>",
		Short: "Show detail about an installed skill",
		Args:  cobra.ExactArgs(1),
		RunE:  runSkillInfo,
	}
)

func init() {
	skillInfoCmd.Flags().BoolVar(&skillInfoJSON, "json", false, "Output as JSON")
	skillCmd.AddCommand(skillInfoCmd)
}

func runSkillInfo(cmd *cobra.Command, args []string) error {
	handle, skillName, err := splitInstalledSkillRef(args[0])
	if err != nil {
		return err
	}
	ledger, err := loadLedgerForCommand()
	if err != nil {
		return err
	}
	i, ok := FindSkillEntry(ledger, handle, skillName)
	if !ok {
		return fmt.Errorf("skill %s/%s is not installed (run `vornikctl skill list` to see what is)", handle, skillName)
	}
	entry := ledger.Skills[i]

	// Read the source mirror to surface the live frontmatter
	// (description, license, author) instead of re-typing them
	// in the ledger.
	mirror, _ := readSkillSourceMirror(handle, skillName)

	if skillInfoJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"entry":  entry,
			"mirror": mirror,
		})
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s/%s\n", entry.Handle, entry.Skill)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  version:   %s\n", entry.SkillVersion)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  source:    %s\n", entry.Source)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  revision:  %s\n", entry.SourceRevision)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  installed: %s\n", entry.InstalledAt.Format("2006-01-02 15:04:05"))
	if mirror != nil {
		if mirror.Description != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  description: %s\n", mirror.Description)
		}
		if mirror.Author != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  author:    %s\n", mirror.Author)
		}
		if mirror.License != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  license:   %s\n", mirror.License)
		}
	}
	if len(entry.Materialised.Workflows) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  workflows: %s\n", strings.Join(entry.Materialised.Workflows, ", "))
	}
	if len(entry.Materialised.Swarms) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  swarms:    %s\n", strings.Join(entry.Materialised.Swarms, ", "))
	}
	return nil
}

// --- remove ---------------------------------------------------

var (
	skillRemoveKeepMirror bool

	skillRemoveCmd = &cobra.Command{
		Use:     "remove <handle>/<skill>",
		Aliases: []string{"rm", "uninstall"},
		Short:   "Remove an installed skill (deletes materialised files + ledger row)",
		Args:    cobra.ExactArgs(1),
		RunE:    runSkillRemove,
	}
)

func init() {
	skillRemoveCmd.Flags().BoolVar(&skillRemoveKeepMirror, "keep-mirror", false, "Keep the source mirror copy (default: delete)")
	skillCmd.AddCommand(skillRemoveCmd)
}

func runSkillRemove(cmd *cobra.Command, args []string) error {
	handle, skillName, err := splitInstalledSkillRef(args[0])
	if err != nil {
		return err
	}
	ledger, err := loadLedgerForCommand()
	if err != nil {
		return err
	}
	i, ok := FindSkillEntry(ledger, handle, skillName)
	if !ok {
		return fmt.Errorf("skill %s/%s is not installed", handle, skillName)
	}
	entry := ledger.Skills[i]

	configsDir := resolveConfigsDir("")
	if configsDir == "" {
		return fmt.Errorf("could not locate configs/ directory (set VORNIK_CONFIGS_DIR)")
	}

	// Track every removal attempt so the operator gets a
	// complete report even when one file is already missing.
	var removed, missed []string
	for _, name := range entry.Materialised.Workflows {
		p := filepath.Join(configsDir, "workflows", name)
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		} else if os.IsNotExist(err) {
			missed = append(missed, p)
		} else {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	for _, name := range entry.Materialised.Swarms {
		p := filepath.Join(configsDir, "swarms", name)
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		} else if os.IsNotExist(err) {
			missed = append(missed, p)
		} else {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}

	if !skillRemoveKeepMirror {
		if mirrorDir, err := skillSourceMirrorDir(handle, skillName); err == nil {
			_ = os.RemoveAll(mirrorDir)
		}
	}

	RemoveSkillEntry(ledger, i)
	ledgerPath, err := DefaultSkillLedgerPath()
	if err != nil {
		return err
	}
	if err := SaveSkillLedger(ledgerPath, ledger); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s/%s\n", handle, skillName)
	for _, p := range removed {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", p)
	}
	for _, p := range missed {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  (already gone) %s\n", p)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "next: vornikctl config reload\n")
	return nil
}

// --- update ---------------------------------------------------

var (
	skillUpdateAll bool

	skillUpdateCmd = &cobra.Command{
		Use:   "update [<handle>/<skill>]",
		Short: "Re-fetch the source for an installed skill and re-materialise",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runSkillUpdate,
	}
)

func init() {
	skillUpdateCmd.Flags().BoolVar(&skillUpdateAll, "all", false, "Update every installed skill")
	skillCmd.AddCommand(skillUpdateCmd)
}

func runSkillUpdate(cmd *cobra.Command, args []string) error {
	ledger, err := loadLedgerForCommand()
	if err != nil {
		return err
	}
	if len(ledger.Skills) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Nothing installed.")
		return nil
	}

	var targets []SkillLedgerEntry
	switch {
	case skillUpdateAll:
		targets = append(targets, ledger.Skills...)
	case len(args) == 1:
		handle, skillName, err := splitInstalledSkillRef(args[0])
		if err != nil {
			return err
		}
		i, ok := FindSkillEntry(ledger, handle, skillName)
		if !ok {
			return fmt.Errorf("skill %s/%s is not installed", handle, skillName)
		}
		targets = append(targets, ledger.Skills[i])
	default:
		return fmt.Errorf("specify <handle>/<skill> or pass --all")
	}

	// Sort so the output order is stable and tests stay
	// deterministic across map-iteration randomness in the
	// install path.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Handle != targets[j].Handle {
			return targets[i].Handle < targets[j].Handle
		}
		return targets[i].Skill < targets[j].Skill
	})

	anyErr := false
	for _, entry := range targets {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "updating %s/%s …\n", entry.Handle, entry.Skill)
		if err := updateOneSkill(cmd, entry); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  failed: %v\n", err)
			anyErr = true
		}
	}
	if anyErr {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("one or more updates failed")
	}
	return nil
}

func updateOneSkill(cmd *cobra.Command, entry SkillLedgerEntry) error {
	// Re-run the install path with --force so the existing
	// row is overwritten. Source is the ledger's recorded URL.
	source := entry.Source
	if source == "" {
		return fmt.Errorf("ledger row %s/%s has no source URL; cannot update", entry.Handle, entry.Skill)
	}
	prevHandle := skillInstallHandle
	prevSkill := skillInstallSkillName
	prevForce := skillInstallForce
	prevAllowSrc := skillInstallAllowSourceChange
	defer func() {
		skillInstallHandle = prevHandle
		skillInstallSkillName = prevSkill
		skillInstallForce = prevForce
		skillInstallAllowSourceChange = prevAllowSrc
	}()
	skillInstallHandle = entry.Handle
	skillInstallSkillName = entry.Skill
	skillInstallForce = true
	// `update` is an INTENTIONAL re-pull of the recorded source, so source
	// drift is expected, not a supply-chain surprise — allow it (the install
	// path still prints the old→new warning so the operator sees the change).
	skillInstallAllowSourceChange = true
	return runSkillInstall(cmd, []string{source})
}

// --- shared helpers -------------------------------------------

// loadLedgerForCommand picks the standard ledger path and loads
// it, surfacing a consistent "couldn't find ledger" message
// from every lifecycle command.
func loadLedgerForCommand() (*SkillLedger, error) {
	path, err := DefaultSkillLedgerPath()
	if err != nil {
		return nil, err
	}
	return LoadSkillLedger(path)
}

// splitInstalledSkillRef accepts `<handle>/<skill>` and rejects
// `<handle>/<skill>@<version>` — version pinning is only
// meaningful for install / update sources, not for already-
// installed lookups.
func splitInstalledSkillRef(ref string) (handle, skill string, err error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("reference must be <handle>/<skill> (got %q)", ref)
	}
	handle, skill = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if handle == "" || skill == "" {
		return "", "", fmt.Errorf("reference %q requires both handle and skill", ref)
	}
	return handle, skill, nil
}

// skillSourceMirrorDir returns ~/.config/vornik/skills/<h>/<s>/.
func skillSourceMirrorDir(handle, skill string) (string, error) {
	ledgerPath, err := DefaultSkillLedgerPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(ledgerPath), "skills", handle, skill), nil
}

// readSkillSourceMirror parses the on-disk mirror copy of the
// SWARM-SKILL.md so `info` can show description / license /
// author without re-fetching the source. Returns nil + nil when
// the mirror is missing (older installs may not have one).
func readSkillSourceMirror(handle, skill string) (*registry.SwarmSkill, error) {
	dir, err := skillSourceMirrorDir(handle, skill)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return nil, nil
	}
	parsed, err := registry.ParseSwarmSkill(data, "SKILL.md")
	if err != nil {
		return nil, nil
	}
	return parsed, nil
}
