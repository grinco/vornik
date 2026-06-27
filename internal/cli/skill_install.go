package cli

// `vornikctl skill install <source>` — fetches a SWARM-SKILL.md
// from local-path / HTTPS / git / handle, validates it, and
// materialises a namespaced workflow + swarm into the deployed
// configs tree.
//
// Reuses the import primitive from skill_import.go:
//   - ParseSwarmSkill / ValidateSwarmSkillMarkdown for the file;
//   - MarshalWorkflowMarkdown / MarshalSwarmMarkdown for the
//     materialised files;
//   - planSkillImport for conflict detection in the configs tree.
//
// The install layer adds: namespacing, the install ledger, the
// source-mirror copy (so `update` knows where to re-fetch from),
// and the opt-in telemetry POST.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

var (
	skillInstallRegistry          string
	skillInstallHandle            string
	skillInstallSkillName         string
	skillInstallForce             bool
	skillInstallDryRun            bool
	skillInstallAllowSourceChange bool

	skillInstallCmd = &cobra.Command{
		Use:   "install <source>",
		Short: "Install a SWARM-SKILL.md from a local path, HTTPS URL, git URL, or registry handle",
		Long: `Resolve a source to a SWARM-SKILL.md, materialise its workflow + roles
into the deployed configs tree under a namespaced ID prefix, and record
the install in the ledger.

Source forms:
  ./skill.md                       local file
  file:///abs/path/skill.md        local file (URL form)
  https://example.com/skill.md     direct HTTPS GET
  git+https://github.com/h/r.git   git clone, picks SKILL.md at root
  vadim/research                   resolved via the registry index

--handle / --skill let an operator override the (handle, skill) tuple
the ledger records when installing from a non-registry source where
the canonical identity isn't otherwise known.`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillInstall,
	}
)

func init() {
	skillInstallCmd.Flags().StringVar(&skillInstallRegistry, "registry", "", "Override registry index URL (defaults to VORNIK_SKILL_REGISTRY_URL or "+DefaultSkillRegistryURL+")")
	skillInstallCmd.Flags().StringVar(&skillInstallHandle, "handle", "", "Override ledger handle when installing from a non-registry source")
	skillInstallCmd.Flags().StringVar(&skillInstallSkillName, "skill", "", "Override ledger skill name when installing from a non-registry source")
	skillInstallCmd.Flags().BoolVar(&skillInstallForce, "force", false, "Re-install (overwrite materialised files) even when the ledger already has this skill")
	skillInstallCmd.Flags().BoolVar(&skillInstallAllowSourceChange, "allow-source-change", false, "Proceed even if the resolved source commit/content differs from what was pinned (supply-chain drift)")
	skillInstallCmd.Flags().BoolVar(&skillInstallDryRun, "dry-run", false, "Resolve + validate without writing anything")
	skillCmd.AddCommand(skillInstallCmd)
}

// skillSourceDrift reports whether re-installing `result` over an existing
// ledger entry would pull DIFFERENT source material than what's pinned — the
// supply-chain drift check (resolve-and-pin, Option A). Prefers the git
// commit SHA when both are known; always cross-checks the content checksum
// (covers non-git sources). Returns a human-readable detail for the
// warning/refusal.
func skillSourceDrift(prior SkillLedgerEntry, result *SkillSourceResult, newChecksum string) (bool, string) {
	if prior.SourceCommitSHA != "" && result.ResolvedSHA != "" && prior.SourceCommitSHA != result.ResolvedSHA {
		return true, fmt.Sprintf("commit %s → %s", abbreviateRevision(prior.SourceCommitSHA), abbreviateRevision(result.ResolvedSHA))
	}
	if prior.SourceFileChecksum != "" && newChecksum != "" && prior.SourceFileChecksum != newChecksum {
		return true, fmt.Sprintf("content %s → %s", abbreviateRevision(prior.SourceFileChecksum), abbreviateRevision(newChecksum))
	}
	return false, ""
}

func runSkillInstall(cmd *cobra.Command, args []string) error {
	source := args[0]
	idx := NewSkillIndexClient(skillInstallRegistry)
	result, err := ResolveSkillSource(source, idx)
	if err != nil {
		return err
	}

	handle, skillName, err := deriveSkillIdentity(source, skillInstallHandle, skillInstallSkillName, result.Bytes)
	if err != nil {
		return err
	}
	if err := validateSkillConfigID("handle", handle); err != nil {
		return err
	}
	if err := validateSkillConfigID("skill", skillName); err != nil {
		return err
	}

	// Validate before any write — the operator should see
	// validation errors before we touch the ledger or configs.
	report := registry.ValidateSwarmSkillMarkdown(result.Bytes, fmt.Sprintf("%s/%s", handle, skillName))
	if report.HasErrors() {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s/%s: validation failed\n", handle, skillName)
		for _, f := range report.Findings {
			if f.Severity == registry.SeverityError {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", f)
			}
		}
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("refusing to install — fix validation errors first")
	}

	skill, err := registry.ParseSwarmSkill(result.Bytes, fmt.Sprintf("%s/%s", handle, skillName))
	if err != nil {
		return err
	}
	if skill.Workflow == nil || len(skill.Roles) == 0 {
		return fmt.Errorf("skill has no vornik payload; --standard files cannot be installed")
	}

	configsDir := resolveConfigsDir("")
	if configsDir == "" {
		return fmt.Errorf("could not locate configs/ directory (set VORNIK_CONFIGS_DIR)")
	}

	ledgerPath, err := DefaultSkillLedgerPath()
	if err != nil {
		return err
	}
	ledger, err := LoadSkillLedger(ledgerPath)
	if err != nil {
		return err
	}
	priorIdx, priorExists := FindSkillEntry(ledger, handle, skillName)
	if priorExists && !skillInstallForce {
		return fmt.Errorf("skill %s/%s is already installed; pass --force to overwrite or use `vornikctl skill update`", handle, skillName)
	}
	// Supply-chain drift (resolve-and-pin, Option A): if re-installing over an
	// existing pin and the resolved commit/content differs, the upstream
	// changed under us. Surface it; refuse unless the operator explicitly
	// accepts (or it's an intentional `update`, which sets the flag).
	if priorExists {
		if drifted, detail := skillSourceDrift(ledger.Skills[priorIdx], result, skillSourceChecksum(result.Bytes)); drifted {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠ %s/%s source changed since it was pinned: %s\n", handle, skillName, detail)
			if !skillInstallAllowSourceChange {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return fmt.Errorf("refusing to install changed source — verify the update, then pass --allow-source-change (or use `vornikctl skill update`)")
			}
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "  proceeding (--allow-source-change / update)")
		}
	}

	// Rename workflow + role refs to namespaced IDs.
	applySkillInstallNamespace(skill, handle, skillName)

	namespacedSwarmID := SkillNamespacedSwarmID(handle, skillName)
	plan, err := planSkillImportWithSwarm(skill, configsDir, namespacedSwarmID)
	if err != nil {
		return err
	}
	if !skillInstallForce {
		if cs := plan.conflicts(); len(cs) > 0 {
			for _, c := range cs {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "conflict: %s\n", c)
			}
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return fmt.Errorf("install conflict — pass --force to overwrite")
		}
	}

	if skillInstallDryRun {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "would install %s/%s\n", handle, skillName)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  source: %s (rev %s)\n", result.SourceURL, result.Revision)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  workflow → %s\n", plan.WorkflowPath)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  swarm    → %s\n", plan.SwarmPath)
		return nil
	}

	// Write the source-mirror copy under
	// ~/.config/vornik/skills/<handle>/<skill>/ so `update`
	// can read the .source pointer.
	if err := writeSkillSourceMirror(handle, skillName, result); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plan.WorkflowPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plan.WorkflowPath, plan.WorkflowBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.WorkflowPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.SwarmPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plan.SwarmPath, plan.SwarmBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.SwarmPath, err)
	}

	// Ledger row records every file we wrote so remove +
	// update can clean up without rebuilding the namespace
	// prefix.
	entry := SkillLedgerEntry{
		Handle:             handle,
		Skill:              skillName,
		SkillVersion:       skill.Version,
		VornikSkillSchema:  registry.SwarmSkillSchemaVersion,
		InstalledAt:        time.Now().UTC(),
		Source:             result.SourceURL,
		SourceRevision:     result.Revision,
		SourceCommitSHA:    result.ResolvedSHA,
		SourceFileChecksum: skillSourceChecksum(result.Bytes),
		Materialised: SkillLedgerMaterialised{
			Workflows: []string{filepath.Base(plan.WorkflowPath)},
			Swarms:    []string{filepath.Base(plan.SwarmPath)},
		},
	}
	if i, exists := FindSkillEntry(ledger, handle, skillName); exists {
		ledger.Skills[i] = entry
	} else {
		ledger.Skills = append(ledger.Skills, entry)
	}
	if err := SaveSkillLedger(ledgerPath, ledger); err != nil {
		return err
	}

	postSkillTelemetry(idx.BaseURL(), entry, "install")

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "installed %s/%s @ %s\n", handle, skillName, skill.Version)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  workflow %s\n", plan.WorkflowPath)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  swarm    %s\n", plan.SwarmPath)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "next: vornikctl config reload\n")
	return nil
}

// deriveSkillIdentity picks the (handle, skill) tuple the
// ledger will record. Precedence:
//
//  1. Explicit --handle / --skill flags (always win).
//  2. <handle>/<skill> source form (parsed from the operator's arg).
//  3. SWARM-SKILL.md `name:` field — used as the skill name with
//     handle "local" so file/url-installs still get a row.
func deriveSkillIdentity(source, handleFlag, skillFlag string, body []byte) (handle, skill string, err error) {
	if handleFlag != "" && skillFlag != "" {
		return handleFlag, skillFlag, nil
	}
	if form, _ := ClassifySkillSource(source); form == SourceFormHandle {
		h, s, _, err := parseSkillHandle(source)
		if err != nil {
			return "", "", err
		}
		if handleFlag != "" {
			h = handleFlag
		}
		if skillFlag != "" {
			s = skillFlag
		}
		return h, s, nil
	}
	// Local / HTTPS / git: try to read the file's name: field.
	parsed, perr := registry.ParseSwarmSkill(body, "install")
	if perr != nil {
		return "", "", fmt.Errorf("derive identity: %w", perr)
	}
	h := handleFlag
	if h == "" {
		h = "local"
	}
	s := skillFlag
	if s == "" {
		s = parsed.Name
	}
	if s == "" {
		return "", "", fmt.Errorf("could not derive skill name from source; pass --skill to set one explicitly")
	}
	return h, s, nil
}

// applySkillInstallNamespace rewrites IDs so installed
// artifacts can never collide with operator-authored content.
// Mirrors `--rename-*` in skill_import but applied to every
// workflow + role automatically — registry installs always
// namespace.
func applySkillInstallNamespace(skill *registry.SwarmSkill, handle, skillName string) {
	original := skill.Workflow.ID
	skill.Workflow.ID = SkillNamespacedID(handle, skillName, original)
	roleRename := make(map[string]string, len(skill.Roles))
	for i := range skill.Roles {
		newName := SkillNamespacedID(handle, skillName, skill.Roles[i].Name)
		roleRename[skill.Roles[i].Name] = newName
		skill.Roles[i].Name = newName
	}
	if skill.Workflow.Steps != nil {
		for id, step := range skill.Workflow.Steps {
			if newName, ok := roleRename[step.Role]; ok {
				step.Role = newName
				skill.Workflow.Steps[id] = step
			}
		}
	}
}

// writeSkillSourceMirror stores a verbatim copy of the source
// SWARM-SKILL.md plus a `.source` pointer so `vornikctl skill
// update` knows where to re-fetch.
func writeSkillSourceMirror(handle, skill string, result *SkillSourceResult) error {
	root, err := DefaultSkillLedgerPath()
	if err != nil {
		return err
	}
	// installed-skills.yaml's parent dir is ~/.config/vornik;
	// the source mirror lives at <parent>/skills/<h>/<s>/.
	mirror := filepath.Join(filepath.Dir(root), "skills", handle, skill)
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", mirror, err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "SKILL.md"), result.Bytes, 0o600); err != nil {
		return fmt.Errorf("write source mirror: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mirror, ".source"), []byte(result.SourceURL+"\n"), 0o600); err != nil {
		return fmt.Errorf("write .source: %w", err)
	}
	return nil
}

// skillSourceChecksum returns the first 16 hex chars of sha256
// of the source bytes. Used by `update` to detect "nothing
// changed upstream"; pinned in the ledger.
func skillSourceChecksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}

// postSkillTelemetry POSTs to <registry>/install when the opt-in
// telemetry flag is on. Best-effort — telemetry failures NEVER
// surface as install errors, because a busted endpoint must not
// prevent operators from installing skills.
func postSkillTelemetry(registryURL string, entry SkillLedgerEntry, event string) {
	if !isSkillTelemetryEnabled() {
		return
	}
	if registryURL == "" {
		return
	}
	payload := map[string]any{
		"handle":         entry.Handle,
		"skill":          entry.Skill,
		"version":        entry.SkillVersion,
		"event":          event,
		"vornik_version": Version,
		"instance_id":    skillTelemetryInstanceID(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(registryURL, "/")+"/install", bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vornikctl-skill/1")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func isSkillTelemetryEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("VORNIK_SKILL_TELEMETRY")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// skillTelemetryInstanceID is the first 8 hex chars of
// sha256(hostname + YYYY-MM). Rotates monthly so per-operator
// tracking is infeasible; rough dedup still works inside one
// month.
func skillTelemetryInstanceID() string {
	host, _ := os.Hostname()
	salt := time.Now().UTC().Format("2006-01")
	sum := sha256.Sum256([]byte(host + "|" + salt))
	return hex.EncodeToString(sum[:])[:8]
}
