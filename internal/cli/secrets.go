package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/storage"
)

var (
	secretsScanProject string
	secretsScanSince   string
	secretsScanApply   bool
	secretsScanJSON    bool
	secretsScanRules   string
	secretsScanSample  int
	secretsPatternJSON bool
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Inspect secret-leak detection patterns and scan historical rows",
	Long: `Operator surface for the secret-leak detection engine (secret-leak Phase 3).

  list-patterns   print the effective detection pattern corpus
  scan-history    retro-scan historical audit + prompt rows for secret-shaped values
`,
}

var secretsListPatternsCmd = &cobra.Command{
	Use:   "list-patterns",
	Short: "Print the effective secret-detection pattern corpus",
	Long: `Print the patterns currently in force: the curated defaults, minus any
listed in secrets.patterns.disable, plus secrets.patterns.custom. This is the
exact set buildSecretsDetector compiles, so what you see here is what the
daemon actually scans with.`,
	RunE: runSecretsListPatterns,
}

var secretsScanHistoryCmd = &cobra.Command{
	Use:   "scan-history",
	Short: "Retro-scan historical tool-audit rows for secret-shaped values",
	Long: `Scan the historical audit + prompt stores for secret-shaped values and, with --apply, redact
them in place. The tool_audit checkpoint defaults to REDACT, so a current daemon
cleans rows as it writes them — but rows written before that default changed, or
under an explicit checkpoints.tool_audit: detect override, or by a writer that
predates the redaction seam, can still hold raw secrets. This is the retro-scan
that cleans them.

Other operational tables are NOT scanned: webhook_events stores only a payload
HASH, task_llm_usage stores token COUNTS, and artifacts are redacted at write
time — none persist raw secret-bearing text to retro-redact.

Defaults to a dry-run (counts findings, mutates nothing). --apply rewrites the
matching rows with typed [REDACTED:type] markers, records the counts to
secret_redaction_audit (source=scan), and writes one admin_audit row naming the
operator + scope. Re-running --apply is safe: already-redacted rows no longer
match, so a second pass is a no-op (though it writes a fresh admin_audit row —
run dry-run first). Postgres-only.

WHICH RULES GET REDACTED. --rules defaults to "strong": the typed,
prefix-anchored credential patterns only. The heuristic rules (entropy,
generic_kv) are high-recall and fire on hashes, opaque ids and base64 that in an
audit table are frequently the substance of the record rather than a secret in
it — and the rewrite is irreversible. Counting and reporting always cover every
rule; only the write-back set narrows. --rules all restores the historical
behaviour of redacting everything.

DECIDING ABOUT A HEURISTIC RULE. --sample N prints up to N masked examples per
rule so you can see what a rule actually matched before you rewrite it. The
matched bytes are NEVER printed: each example carries the value's length, a
correlation token salted per run (so an archived sample cannot later be used to
confirm whether a given secret was present), and the surrounding text with every
overlapping finding masked. Sampling reads raw historical secrets, so like
--apply it writes an admin_audit row.

Examples:
  vornikctl secrets scan-history                          # dry-run, all projects
  vornikctl secrets scan-history --project janka --since 30d
  vornikctl secrets scan-history --sample 20 --rules entropy   # what does entropy match?
  vornikctl secrets scan-history --project janka --apply  # redact the strong rules
  vornikctl secrets scan-history --project janka --apply --rules all  # everything
`,
	RunE: runSecretsScanHistory,
}

func init() {
	secretsListPatternsCmd.Flags().BoolVar(&secretsPatternJSON, "json", false, "emit machine-readable JSON")
	secretsScanHistoryCmd.Flags().StringVar(&secretsScanProject, "project", "", "operate on a single project (default: all)")
	secretsScanHistoryCmd.Flags().StringVar(&secretsScanSince, "since", "", "only scan rows newer than this (e.g. 30d, 720h); default: all history")
	secretsScanHistoryCmd.Flags().BoolVar(&secretsScanApply, "apply", false, "redact matching rows in place (default: dry-run)")
	secretsScanHistoryCmd.Flags().BoolVar(&secretsScanJSON, "json", false, "emit machine-readable JSON")
	secretsScanHistoryCmd.Flags().StringVar(&secretsScanRules, "rules", ruleSpecStrong,
		`which rules to REDACT: "strong" (typed credential patterns only; default), "all", or a comma-separated list of rule names`)
	secretsScanHistoryCmd.Flags().IntVar(&secretsScanSample, "sample", 0,
		"print up to N masked examples per rule (matched values are never printed); this is a privileged read and is recorded to admin_audit")
	secretsCmd.AddCommand(secretsListPatternsCmd)
	secretsCmd.AddCommand(secretsScanHistoryCmd)
	rootCmd.AddCommand(secretsCmd)
}

// effectivePatternsFromConfig resolves the in-force corpus the same way
// buildSecretsDetector does, via the shared secrets.EffectivePatterns
// helper so CLI + daemon never drift.
func effectivePatternsFromConfig(cfg config.SecretsConfig) []secrets.Pattern {
	custom := make([]secrets.Pattern, 0, len(cfg.Patterns.Custom))
	for _, c := range cfg.Patterns.Custom {
		custom = append(custom, secrets.Pattern{Name: c.Name, Regex: c.Regex, Description: c.Description})
	}
	return secrets.EffectivePatterns(cfg.Patterns.Disable, custom)
}

func runSecretsListPatterns(_ *cobra.Command, _ []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	patterns := effectivePatternsFromConfig(cfg.Secrets)

	if secretsPatternJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(patterns)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "NAME\tDESCRIPTION\tREGEX\n")
	for _, p := range patterns {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Description, p.Regex)
	}
	_ = tw.Flush()
	if !cfg.Secrets.Entropy.Disabled {
		_, _ = fmt.Fprintf(os.Stdout, "\n(plus entropy detection: minLen=%d minBits=%g)\n",
			cfg.Secrets.Entropy.MinLen, cfg.Secrets.Entropy.MinBits)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n%d pattern(s) in force.\n", len(patterns))
	return nil
}

// scanHistoryResult is the per-run summary (dry-run and apply).
//
// CountsByType covers EVERY finding; SelectedByType and ExcludedByType split it
// by the --rules selection. Both halves are always reported: a scoped run that
// printed only its selection would let "nothing was redacted" render
// identically to "nothing was found", which is the coverage-boundary defect
// retired elsewhere in this codebase on 2026-08-27.
type scanHistoryResult struct {
	RowsScanned    int            `json:"rows_scanned"`
	RowsMatched    int            `json:"rows_matched"`
	Applied        bool           `json:"applied"`
	Rules          string         `json:"rules"`
	CountsByType   map[string]int `json:"counts_by_type"`
	SelectedByType map[string]int `json:"selected_by_type"`
	ExcludedByType map[string]int `json:"excluded_by_type"`
	Samples        []string       `json:"samples,omitempty"`
}

func runSecretsScanHistory(_ *cobra.Command, _ []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Secrets.Enabled {
		return fmt.Errorf("secrets detection is disabled (secrets.enabled=false); enable it before scanning")
	}

	var since time.Time
	if secretsScanSince != "" {
		d, derr := time.ParseDuration(normalizeDuration(secretsScanSince))
		if derr != nil {
			return fmt.Errorf("invalid --since %q: %w", secretsScanSince, derr)
		}
		since = time.Now().Add(-d)
	}

	custom := make([]secrets.Pattern, 0, len(cfg.Secrets.Patterns.Custom))
	for _, c := range cfg.Secrets.Patterns.Custom {
		custom = append(custom, secrets.Pattern{Name: c.Name, Regex: c.Regex, Description: c.Description})
	}
	corpus := secrets.EffectivePatterns(cfg.Secrets.Patterns.Disable, custom)
	selection, err := parseRuleSelection(secretsScanRules, corpus)
	if err != nil {
		return err
	}
	if secretsScanSample < 0 {
		return fmt.Errorf("--sample must be >= 0, got %d", secretsScanSample)
	}
	detector, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:        corpus,
		Allowlist:       append(secrets.DefaultAllowlist(), cfg.Secrets.Allowlist...),
		EntropyDisabled: cfg.Secrets.Entropy.Disabled,
		EntropyMinLen:   cfg.Secrets.Entropy.MinLen,
		EntropyMinBits:  cfg.Secrets.Entropy.MinBits,
	})
	if err != nil {
		return fmt.Errorf("build detector: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "secrets scan-history")
	if err != nil {
		return err
	}

	// One pass per target. tool_audit_log first because it is the oldest and
	// largest; the two content-addressed prompt stores last, since re-keying a
	// body is the only operation here that moves rows rather than rewriting
	// them (chat-audit design §5).
	result, err := scanToolAuditHistory(ctx, db, detector, secretsScanProject, since, secretsScanApply, selection, secretsScanSample)
	if err != nil {
		return err
	}
	targets := []scanTargetResult{{table: "tool_audit_log", result: result}}

	chatRes, err := scanChatAuditHistory(ctx, db, detector, secretsScanProject, since, secretsScanApply, selection, secretsScanSample)
	if err != nil {
		return err
	}
	targets = append(targets, scanTargetResult{table: "chat_audit_log", result: chatRes})

	for _, store := range contentStores {
		storeRes, serr := scanContentStore(ctx, db, store, detector, since, secretsScanApply, selection, secretsScanSample)
		if serr != nil {
			return serr
		}
		targets = append(targets, scanTargetResult{table: store.table, result: storeRes})
	}
	// The admin-audit rows and the JSON envelope below report the whole run,
	// so the totals are the sum over targets rather than tool_audit_log's
	// counts standing in for everything (which is what they used to do).
	for _, t := range targets[1:] {
		result.RowsScanned += t.result.RowsScanned
		result.RowsMatched += t.result.RowsMatched
	}

	if secretsScanSample > 0 {
		// Sampling READS raw historical secrets. The Phase-3 design rested its
		// accountability on --apply being the only path that does; adding a
		// look-but-do-not-touch surface without extending the record would give
		// the control a hole while leaving it looking unchanged.
		audit := postgres.NewAdminAuditRepository(db)
		_ = audit.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: cliPrincipal(),
			Source:    "cli",
			Action:    "secrets.scan-history.sample",
			Target:    secretsScanProject,
			After: fmt.Sprintf(`{"rules":%q,"sample":%d,"rows_scanned":%d,"rows_matched":%d}`,
				selection.spec, secretsScanSample, result.RowsScanned, result.RowsMatched),
		})
	}

	if secretsScanApply && result.RowsMatched > 0 {
		// Accountability: who ran a privileged retro-scan that read raw
		// historical secrets, and over what scope (design §M3 / the
		// secret_view_audit reinterpretation).
		audit := postgres.NewAdminAuditRepository(db)
		_ = audit.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: cliPrincipal(),
			Source:    "cli",
			Action:    "secrets.scan-history.apply",
			Target:    secretsScanProject,
			// The rule scope belongs in the record: without it a later reader
			// cannot tell a scoped purge from a full one, and that difference
			// is exactly what --rules introduces.
			After: fmt.Sprintf(`{"rows_scanned":%d,"rows_matched":%d,"rules":%q}`,
				result.RowsScanned, result.RowsMatched, selection.spec),
		})
	}

	if secretsScanJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		out := struct {
			scanHistoryResult
			Targets map[string]scanHistoryResult `json:"targets"`
		}{scanHistoryResult: result, Targets: map[string]scanHistoryResult{}}
		for _, t := range targets {
			out.Targets[t.table] = t.result
		}
		return enc.Encode(out)
	}
	mode := fmt.Sprintf("DRY-RUN (nothing changed; re-run with --apply to redact the %s rules)", selection.spec)
	if result.Applied {
		mode = fmt.Sprintf("APPLIED (rows redacted under --rules %s)", selection.spec)
	}
	for _, t := range targets {
		// Every target is printed, INCLUDING the ones with nothing in them: a
		// store that is silently absent from the report is indistinguishable
		// from a store that is clean, which is the confusion this whole seam
		// exists to remove.
		_, _ = fmt.Fprintf(os.Stdout, "%s: scanned %d row(s), %d contained secret-shaped value(s)\n",
			t.table, t.result.RowsScanned, t.result.RowsMatched)
		if t.result.RowsMatched == 0 {
			continue
		}
		_, _ = fmt.Fprintf(os.Stdout, "  selected (--rules %s):\n", selection.spec)
		printCounts(t.result.SelectedByType, "    none\n")
		if len(t.result.ExcludedByType) > 0 {
			_, _ = fmt.Fprintf(os.Stdout, "  NOT selected (add --rules all to include):\n")
			printCounts(t.result.ExcludedByType, "")
		}
		if len(t.result.Samples) > 0 {
			_, _ = fmt.Fprintf(os.Stdout, "\n  samples (matched values are never printed; tok= is salted per run):\n")
			for _, line := range t.result.Samples {
				_, _ = fmt.Fprintln(os.Stdout, line)
			}
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s\n", mode)
	return nil
}

// scanTargetResult pairs a store with what the scan found in it, so the report
// names its sources instead of presenting one table's numbers as the whole.
type scanTargetResult struct {
	table  string
	result scanHistoryResult
}

// printCounts renders a finding-type histogram in a stable order. emptyNote is
// printed when there is nothing — never silence, so an empty selection cannot
// read like a clean table.
func printCounts(counts map[string]int, emptyNote string) {
	if len(counts) == 0 {
		if emptyNote != "" {
			_, _ = fmt.Fprint(os.Stdout, emptyNote)
		}
		return
	}
	types := make([]string, 0, len(counts))
	for ft := range counts {
		types = append(types, ft)
	}
	sort.Strings(types)
	for _, ft := range types {
		_, _ = fmt.Fprintf(os.Stdout, "    %-18s %d\n", ft, counts[ft])
	}
}

// scanToolAuditHistory scans (and with apply=true redacts) tool_input +
// tool_output on tool_audit_log. Each redacted row records its per-type
// counts to secret_redaction_audit with source="scan".
func scanToolAuditHistory(ctx context.Context, db *sql.DB, detector secrets.Detector, project string, since time.Time, apply bool, sel ruleSelection, sampleN int) (scanHistoryResult, error) {
	res := scanHistoryResult{
		Applied: apply, Rules: sel.spec,
		CountsByType: map[string]int{}, SelectedByType: map[string]int{}, ExcludedByType: map[string]int{},
	}
	// The salt lives only for this call, so the sample it renders stops being a
	// confirmation oracle the moment the process exits.
	salt := newRunSalt()
	sampled := map[string]int{}

	q := `SELECT id, project_id, task_id, execution_id, tool_name, tool_input, tool_output FROM tool_audit_log WHERE 1=1`
	args := []any{}
	if project != "" {
		args = append(args, project)
		q += fmt.Sprintf(" AND project_id = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, since)
		q += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	q += " ORDER BY created_at ASC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query tool_audit_log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []toolAuditHit
	for rows.Next() {
		var id, projectID, taskID, execID, tool, in, out string
		if err := rows.Scan(&id, &projectID, &taskID, &execID, &tool, &in, &out); err != nil {
			return res, fmt.Errorf("scan row: %w", err)
		}
		res.RowsScanned++
		inF := detector.Scan([]byte(in))
		outF := detector.Scan([]byte(out))
		if len(inF) == 0 && len(outF) == 0 {
			continue
		}
		res.RowsMatched++
		countBySelection(&res, sel, inF, outF)
		if sampleN > 0 {
			collectSamples(&res, salt, sampled, sampleN, sel, id, tool, in, inF)
			collectSamples(&res, salt, sampled, sampleN, sel, id, tool, out, outF)
		}
		if h, ok := hitForSelection(sel, id, projectID, taskID, execID, in, out, inF, outF); ok {
			hits = append(hits, h)
		}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	if err := applyToolAuditRedactions(ctx, db, hits); err != nil {
		return res, err
	}
	return res, nil
}

// countBySelection records every finding, split by whether the --rules
// selection will write it back. Both halves are reported, so a scoped run
// cannot let "nothing redacted" read like "nothing found".
func countBySelection(res *scanHistoryResult, sel ruleSelection, inF, outF []secrets.Finding) {
	for ft, n := range secrets.CountByType(append(append([]secrets.Finding{}, inF...), outF...)) {
		res.CountsByType[ft] += n
		if sel.selects(ft) {
			res.SelectedByType[ft] += n
		} else {
			res.ExcludedByType[ft] += n
		}
	}
}

// hitForSelection builds the queued rewrite for one row, redacting ONLY the
// selected findings. A row that matched but has nothing selected is not queued:
// rewriting it would be a no-op UPDATE and a misleading audit row.
func hitForSelection(sel ruleSelection, id, projectID, taskID, execID, in, out string, inF, outF []secrets.Finding) (toolAuditHit, bool) {
	selIn := selectFindings(inF, sel)
	selOut := selectFindings(outF, sel)
	if len(selIn) == 0 && len(selOut) == 0 {
		return toolAuditHit{}, false
	}
	h := toolAuditHit{id: id, projectID: projectID, taskID: taskID, execID: execID, counts: map[string]int{}}
	for ft, n := range secrets.CountByType(append(append([]secrets.Finding{}, selIn...), selOut...)) {
		h.counts[ft] += n
	}
	if len(selIn) > 0 {
		h.newInput = string(secrets.Redact([]byte(in), selIn))
		h.rewriteInput = true
	}
	if len(selOut) > 0 {
		h.newOutput = string(secrets.Redact([]byte(out), selOut))
		h.rewriteOutput = true
	}
	return h, true
}

// collectSamples renders up to sampleN masked examples per rule. It samples the
// SELECTED rules — the operator is deciding whether to apply them — and never
// emits a matched byte; see maskedContext.
func collectSamples(res *scanHistoryResult, salt []byte, sampled map[string]int, sampleN int, sel ruleSelection, rowID, tool, text string, findings []secrets.Finding) {
	for _, f := range findings {
		if !sel.selects(f.Type) || sampled[f.Type] >= sampleN {
			continue
		}
		sampled[f.Type]++
		res.Samples = append(res.Samples, sampleLine(salt, rowID, tool, text, findings, f))
	}
}

// toolAuditHit is one matched tool_audit_log row queued for redaction.
type toolAuditHit struct {
	id, projectID, taskID, execID string
	newInput, newOutput           string
	rewriteInput, rewriteOutput   bool
	counts                        map[string]int
}

// applyToolAuditRedactions rewrites each matched row in place and records
// its per-type counts to secret_redaction_audit (source="scan").
func applyToolAuditRedactions(ctx context.Context, db *sql.DB, hits []toolAuditHit) error {
	auditRepo := postgres.NewSecretRedactionAuditRepository(db)
	for _, h := range hits {
		var err error
		switch {
		case h.rewriteInput && h.rewriteOutput:
			_, err = db.ExecContext(ctx, `UPDATE tool_audit_log SET tool_input=$1, tool_output=$2 WHERE id=$3`, h.newInput, h.newOutput, h.id)
		case h.rewriteInput:
			_, err = db.ExecContext(ctx, `UPDATE tool_audit_log SET tool_input=$1 WHERE id=$2`, h.newInput, h.id)
		case h.rewriteOutput:
			_, err = db.ExecContext(ctx, `UPDATE tool_audit_log SET tool_output=$1 WHERE id=$2`, h.newOutput, h.id)
		}
		if err != nil {
			return fmt.Errorf("redact row %s: %w", h.id, err)
		}
		events := make([]persistence.SecretRedactionEvent, 0, len(h.counts))
		for ft, n := range h.counts {
			events = append(events, persistence.SecretRedactionEvent{
				ProjectID: h.projectID, TaskID: h.taskID, ExecutionID: h.execID,
				Checkpoint: secrets.CheckpointToolAudit, FindingType: ft, Count: n, Source: "scan",
			})
		}
		if err := auditRepo.Record(ctx, events); err != nil {
			return fmt.Errorf("record audit for row %s: %w", h.id, err)
		}
	}
	return nil
}

// normalizeDuration lets operators write "30d" as a shorthand for 720h;
// Go's time.ParseDuration has no day unit.
func normalizeDuration(s string) string {
	if n := len(s); n > 1 && s[n-1] == 'd' {
		if days, err := parsePositiveInt(s[:n-1]); err == nil {
			return fmt.Sprintf("%dh", days*24)
		}
	}
	return s
}

func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

// cliPrincipal identifies the operator running a CLI mutation for the
// admin_audit row. Best-effort: the OS user, prefixed to distinguish
// from UI/API principals.
func cliPrincipal() string {
	if u := os.Getenv("USER"); u != "" {
		return "cli:" + u
	}
	return "cli:unknown"
}
