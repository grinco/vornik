package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/storage"
)

// llmReclassifyResponse + runLLMReclassifyLoop live in this file too.

var (
	reclassifyProject   string
	reclassifyDryRun    bool
	reclassifyJSON      bool
	reclassifyUseLLM    bool
	reclassifyLLMOnly   bool
	reclassifyBatchSize int
)

var memoryReclassifyCmd = &cobra.Command{
	Use:   "reclassify",
	Short: "Re-derive content_class for unclassified chunks",
	Long: `Walk every chunk in --project whose content_class is 'unclassified'
(or empty) and re-derive the class.

Default (deterministic): apply ClassifyByRole to each chunk's
producer_role. Chunks whose producer_role is empty or maps to
ClassUnclassified are left alone.

--use-llm: chunks the deterministic pass leaves unclassified are
sent to the LLM classifier (POST /api/v1/memory/reclassify-llm).
Requires the daemon to be running with memory.classifier.enabled=true
in config. Costs one LLM call per chunk; the CLI loops batches
until the queue drains.

--llm-only: SKIP the deterministic pass entirely. Useful when an
operator has manually reset chunks to 'unclassified' specifically
to force LLM reclassification — without this flag the deterministic
pass would immediately re-stamp them based on producer_role, leaving
nothing for the LLM to see. Implies --use-llm. Requires the daemon.

The deterministic mapping is the canonical Phase-2 table
(researcher → research, analyst → spec, reviewer → decision,
coder/etc. → commit_msg, tester/etc. → diagnostic, lead → spec,
vision → external_fetch, strategist → spec, risk-officer → decision,
executor → commit_msg).

Examples:
  vornikctl memory reclassify --project assistant --dry-run
  vornikctl memory reclassify --project assistant
  vornikctl memory reclassify --project assistant --use-llm
  vornikctl memory reclassify --project assistant --llm-only
  vornikctl memory reclassify --project assistant --use-llm --batch-size 5
  vornikctl memory reclassify --project assistant --json`,
	RunE: runMemoryReclassify,
}

func init() {
	memoryReclassifyCmd.Flags().StringVarP(&reclassifyProject, "project", "p", "", "Project ID (required)")
	memoryReclassifyCmd.Flags().BoolVar(&reclassifyDryRun, "dry-run", false, "Report what would change without writing")
	memoryReclassifyCmd.Flags().BoolVar(&reclassifyJSON, "json", false, "Emit JSON summary instead of human-readable output")
	memoryReclassifyCmd.Flags().BoolVar(&reclassifyUseLLM, "use-llm", false, "After the deterministic pass, send remaining unclassified chunks to the LLM classifier")
	memoryReclassifyCmd.Flags().BoolVar(&reclassifyLLMOnly, "llm-only", false, "Skip the deterministic pass; send every unclassified chunk straight to the LLM classifier. Implies --use-llm.")
	memoryReclassifyCmd.Flags().IntVar(&reclassifyBatchSize, "batch-size", 10, "Chunks per LLM batch when --use-llm is set (1-50)")
	_ = memoryReclassifyCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryReclassifyCmd)
}

// reclassifyPlan groups roles by their target class so the writer
// makes one UPDATE per class instead of one per role. Surfaces to the
// dry-run + final summary the way operators expect to read it.
type reclassifyPlan struct {
	Class            memory.ContentClass
	Roles            []string
	TTL              time.Duration
	UnclassifiedRows int
}

// PlanReport is the JSON-friendly shape emitted by --json. Stable
// keys so downstream automation can grep it.
type PlanReport struct {
	Project        string        `json:"project"`
	DryRun         bool          `json:"dryRun"`
	PerClass       []ClassReport `json:"perClass"`
	StuckUnknown   int           `json:"stuckUnknownRoles"`
	StuckNoRole    int           `json:"stuckNoRole"`
	TotalReclassed int           `json:"totalReclassed"`
}

// ClassReport is one row in PlanReport.PerClass. RowsUpdated is the
// authoritative count after writing (or the projected count in
// --dry-run).
type ClassReport struct {
	Class       string   `json:"class"`
	Roles       []string `json:"roles"`
	TTLSeconds  int64    `json:"ttlSeconds"`
	RowsUpdated int      `json:"rowsUpdated"`
}

func runMemoryReclassify(_ *cobra.Command, _ []string) error {
	if reclassifyProject == "" {
		return fmt.Errorf("--project is required")
	}

	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()

	db, err := requirePostgresDB(backend, "memory reclassify")
	if err != nil {
		return err
	}
	repo := memory.NewRepository(db)
	return runReclassifyFlow(
		ctx, repo,
		reclassifyProject,
		reclassifyDryRun, reclassifyJSON,
		reclassifyUseLLM, reclassifyLLMOnly,
		reclassifyBatchSize,
		os.Stdout,
		runLLMReclassifyLoop,
	)
}

// llmReclassifyRunner is the signature of runLLMReclassifyLoop,
// extracted so runReclassifyFlow can be unit-tested with a stub
// runner that records invocations without needing a daemon.
type llmReclassifyRunner func(projectID string, dryRun, asJSON bool, batchSize int, out *os.File) error

// runReclassifyFlow orchestrates the two-phase reclassify based on
// the operator's flag combination. Extracted from runMemoryReclassify
// so the flag-driven branching can be tested without mocking config
// loading, the Postgres connection, or the HTTP daemon.
//
// Branching truth table:
//
//	         | llm-only=F | llm-only=T
//	---------+------------+-----------
//	useLLM=F | det only   | LLM only (det skipped)
//	useLLM=T | det + LLM  | LLM only (det skipped)
//
// --llm-only is the operator's escape hatch when a manual reset
// (e.g. resetting commit_msg → unclassified to force LLM
// reclassification of previously role-classified chunks) would
// otherwise be undone instantly by the deterministic role-map.
// Without this flag, `executor → commit_msg` (or any other role
// mapping) re-stamps the rows in a single UPDATE before the LLM
// ever sees them.
func runReclassifyFlow(
	ctx context.Context,
	repo reclassifyRepo,
	projectID string,
	dryRun, asJSON, useLLM, llmOnly bool,
	batchSize int,
	out *os.File,
	runLLM llmReclassifyRunner,
) error {
	if !llmOnly {
		if err := doMemoryReclassify(ctx, repo, projectID, dryRun, asJSON, out); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintln(out, "skipping deterministic pass (--llm-only); every unclassified chunk goes to the LLM.")
	}
	if !useLLM && !llmOnly {
		return nil
	}
	// LLM pass — POST to the daemon's /api/v1/memory/reclassify-llm
	// endpoint, which routes through the chat router and produces
	// usage rows attributed to the "memory_classifier" role.
	return runLLM(projectID, dryRun, asJSON, batchSize, out)
}

// llmReclassifyResponse mirrors api.MemoryClassifyBackfillResult on
// the client side. Kept here so the CLI stays a self-contained tool.
type llmReclassifyResponse struct {
	Processed int      `json:"processed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"`
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"`
}

// runLLMReclassifyLoop drives the LLM-backfill batch-by-batch. The
// CLI loops because the API caps batchSize at 50 — a project with
// hundreds of unclassified chunks needs multiple round-trips.
// Counters accumulate across batches so the final summary reflects
// the whole run.
func runLLMReclassifyLoop(projectID string, dryRun, asJSON bool, batchSize int, out *os.File) error {
	if batchSize < 1 {
		batchSize = 1
	}
	if batchSize > 50 {
		batchSize = 50
	}
	// Probe Remaining first so the operator sees scope before any
	// LLM cost is incurred. Returns 0 + clear message when nothing
	// to do — saves an unneeded round-trip on a clean queue.
	probePath := fmt.Sprintf("/api/v1/memory/reclassify-llm?project=%s&count=true", projectID)
	probe, err := postJSON(probePath, nil)
	if err != nil {
		return fmt.Errorf("llm probe: %w", err)
	}
	var probed llmReclassifyResponse
	if err := json.Unmarshal(probe, &probed); err != nil {
		return fmt.Errorf("llm probe parse: %w", err)
	}
	if dryRun {
		if asJSON {
			_, err := out.Write(probe)
			return err
		}
		_, _ = fmt.Fprintf(out, "\nLLM classifier would process %d remaining unclassified chunks.\n", probed.Remaining)
		_, _ = fmt.Fprintln(out, "(dry-run: no LLM calls were made)")
		return nil
	}
	if probed.Remaining == 0 {
		_, _ = fmt.Fprintln(out, "\nno chunks left for the LLM classifier — deterministic pass cleared them all.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nLLM classifying %d chunks (batch size %d)...\n", probed.Remaining, batchSize)
	totals := llmReclassifyResponse{}
	prevRemaining := probed.Remaining
	stalled := 0
	for {
		path := fmt.Sprintf("/api/v1/memory/reclassify-llm?project=%s&batch_size=%d", projectID, batchSize)
		raw, err := postJSON(path, nil)
		if err != nil {
			return fmt.Errorf("llm batch: %w", err)
		}
		var batch llmReclassifyResponse
		if err := json.Unmarshal(raw, &batch); err != nil {
			return fmt.Errorf("llm batch parse: %w", err)
		}
		totals.Processed += batch.Processed
		totals.Succeeded += batch.Succeeded
		totals.Failed += batch.Failed
		totals.Skipped += batch.Skipped
		totals.Remaining = batch.Remaining
		totals.Errors = append(totals.Errors, batch.Errors...)

		_, _ = fmt.Fprintf(out, "  batch: %d processed (%d ok, %d skip, %d fail); %d remaining\n",
			batch.Processed, batch.Succeeded, batch.Skipped, batch.Failed, batch.Remaining)

		if batch.Remaining == 0 {
			break
		}
		// Stall guard: if the queue doesn't shrink across two
		// consecutive batches, the LLM is rejecting every chunk and
		// looping wastes spend. Bail out with a clear message.
		if batch.Remaining >= prevRemaining {
			stalled++
		} else {
			stalled = 0
		}
		if stalled >= 2 {
			_, _ = fmt.Fprintln(out, "  (stalled — LLM not making progress; aborting loop)")
			break
		}
		prevRemaining = batch.Remaining
	}

	if asJSON {
		return json.NewEncoder(out).Encode(totals)
	}
	_, _ = fmt.Fprintf(out, "\nLLM pass done: %d chunks classified, %d skipped (model unsure), %d failed.\n",
		totals.Succeeded, totals.Skipped, totals.Failed)
	if totals.Remaining > 0 {
		_, _ = fmt.Fprintf(out, "%d chunks still unclassified after the LLM pass.\n", totals.Remaining)
	}
	return nil
}

// reclassifyRepo narrows the dependency surface so the unit test can
// supply a stub without standing up a real Postgres. *memory.Repository
// satisfies it via its existing methods.
type reclassifyRepo interface {
	CountUnclassifiedByRole(ctx context.Context, projectID string) (map[string]int, error)
	ReclassifyUnclassifiedByRoles(ctx context.Context, projectID, newClass string, roles []string, ttl time.Duration) (int, error)
}

// doMemoryReclassify is the body of `vornikctl memory reclassify`,
// extracted for testability. Takes a writer so tests can capture
// output instead of touching stdout.
func doMemoryReclassify(ctx context.Context, repo reclassifyRepo, projectID string, dryRun, asJSON bool, out *os.File) error {
	counts, err := repo.CountUnclassifiedByRole(ctx, projectID)
	if err != nil {
		return fmt.Errorf("count unclassified: %w", err)
	}
	report := PlanReport{Project: projectID, DryRun: dryRun}
	if len(counts) == 0 {
		report.PerClass = []ClassReport{}
		return printReclassifyReport(out, report, asJSON, true)
	}

	// Group counts by their target class. Empty role and unknown
	// roles (those that map back to ClassUnclassified) are tracked
	// separately so the operator sees what's stuck.
	planByClass := make(map[memory.ContentClass]*reclassifyPlan)
	for role, n := range counts {
		if role == "" {
			report.StuckNoRole += n
			continue
		}
		class, policy := memory.ClassifyByRole(role)
		if class == memory.ClassUnclassified {
			report.StuckUnknown += n
			continue
		}
		p, ok := planByClass[class]
		if !ok {
			p = &reclassifyPlan{Class: class, TTL: policy.TTL}
			planByClass[class] = p
		}
		p.Roles = append(p.Roles, role)
		p.UnclassifiedRows += n
	}

	// Stable ordering: emit class buckets alphabetically and the
	// roles inside each bucket sorted too. Predictable output is
	// important for both --json consumers and operator eyeballs.
	classes := make([]memory.ContentClass, 0, len(planByClass))
	for c := range planByClass {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })

	for _, c := range classes {
		p := planByClass[c]
		sort.Strings(p.Roles)
		row := ClassReport{
			Class:      string(c),
			Roles:      p.Roles,
			TTLSeconds: int64(p.TTL.Seconds()),
		}
		if dryRun {
			row.RowsUpdated = p.UnclassifiedRows
		} else {
			updated, err := repo.ReclassifyUnclassifiedByRoles(ctx, projectID, string(c), p.Roles, p.TTL)
			if err != nil {
				return fmt.Errorf("reclassify %s: %w", c, err)
			}
			row.RowsUpdated = updated
		}
		report.PerClass = append(report.PerClass, row)
		report.TotalReclassed += row.RowsUpdated
	}

	return printReclassifyReport(out, report, asJSON, false)
}

// printReclassifyReport renders the report in either human or JSON
// form. Split out from doMemoryReclassify so the write logic stays
// terse and the formatting is independently testable.
func printReclassifyReport(out *os.File, report PlanReport, asJSON, empty bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	if empty {
		_, _ = fmt.Fprintf(out, "no unclassified chunks in project %q.\n", report.Project)
		return nil
	}
	verb := "would reclassify"
	if !report.DryRun {
		verb = "reclassified"
	}
	_, _ = fmt.Fprintf(out, "%s %d chunks in project %q:\n\n", verb, report.TotalReclassed, report.Project)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CLASS\tROWS\tTTL\tROLES")
	for _, row := range report.PerClass {
		ttlStr := "—"
		if row.TTLSeconds > 0 {
			ttlStr = (time.Duration(row.TTLSeconds) * time.Second).String()
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%v\n", row.Class, row.RowsUpdated, ttlStr, row.Roles)
	}
	_ = tw.Flush()
	if report.StuckNoRole > 0 || report.StuckUnknown > 0 {
		_, _ = fmt.Fprintf(out, "\nstuck (left unclassified):\n")
		if report.StuckNoRole > 0 {
			_, _ = fmt.Fprintf(out, "  - %d chunks with no producer_role\n", report.StuckNoRole)
		}
		if report.StuckUnknown > 0 {
			_, _ = fmt.Fprintf(out, "  - %d chunks with an unknown role (not in roleClassMap)\n", report.StuckUnknown)
		}
	}
	if report.DryRun {
		_, _ = fmt.Fprintln(out, "\n(dry-run: no changes written. re-run without --dry-run to apply.)")
	}
	return nil
}
