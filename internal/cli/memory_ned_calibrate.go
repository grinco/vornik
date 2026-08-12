package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/memory/ned"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/storage"
)

// NED calibration harness (chat memory-write design §7). A one-shot MEASUREMENT
// command: it samples historical human chat turns, runs the SHIPPED NED
// extract→resolve path (ned.Gate.Classify — the same billed code slice 4 uses)
// over the sample, and reports the resolver decision DISTRIBUTION
// (match/new/ambiguous/none) so the operator can decide whether shared-scope
// `remember` is usable (§6.2.1: `new` > 30% → do NOT ship shared as-is;
// `ambiguous` > 20% → tune the resolver).
//
// It is USABILITY calibration, not safety: the gate blocks every unresolved
// detected person by construction whether or not this has run. This only answers
// "is `new` so common the normal case is blocked".
//
// PRIVACY INVARIANT (design §7): the report is AGGREGATES ONLY. No chat content
// is ever printed, logged, or returned — sampled turn text flows only into
// Classify and is discarded. The command prints counts and rates, nothing else.

const (
	// nedCalNewBlockThreshold — `new` share of person-bearing turns above which
	// shared scope refuses the normal case, so it must NOT ship as designed
	// (§6.2.1 / §7).
	nedCalNewBlockThreshold = 0.30
	// nedCalAmbiguousTuneThreshold — `ambiguous` share above which the resolver
	// needs tuning (§7).
	nedCalAmbiguousTuneThreshold = 0.20
	// nedCalDominantCapFrac caps any single project at this fraction of the
	// total sample so the dominant project (janka, ~95% of turns) cannot make
	// the measurement a read of one person (§7).
	nedCalDominantCapFrac = 0.50
	// nedCalDefaultGraphModel mirrors the KG worker's code default for the
	// extractor/resolver stages (container_autonomy.go). The DEPLOYED
	// memory.graph.{extractor,resolver}_model governs and is read from config;
	// this is only the fallback when a key is unset (the deployed extractor is
	// the 120b model, not this default — hence reading config, not hardcoding).
	nedCalDefaultGraphModel = "openai.gpt-oss-20b-1:0"
)

var (
	nedCalSampleSize    int
	nedCalWindow        string
	nedCalMinPerStratum int
	nedCalSeed          string
	nedCalDryRun        bool
	nedCalJSON          bool
)

var memoryNEDCalibrateCmd = &cobra.Command{
	Use:   "ned-calibrate",
	Short: "Measure the shared-scope NED resolver decision distribution over historical chat",
	Long: `Sample historical human chat turns (chat_audit_log.user_message), run the
SHIPPED shared-scope NED extract→resolve path (the same billed code the chat
` + "`remember`" + ` tool uses) over the sample, and report the resolver decision
distribution — match / new / ambiguous / none (design §6.2.1, §7).

This is USABILITY calibration, not safety: the NED gate blocks every unresolved
detected person by construction regardless. This answers whether shared-scope
` + "`remember`" + ` is USABLE — i.e. whether the normal case is blocked.

Sampling frame (§7):
  - stratified by project; >=100 turns per stratum where available
  - the dominant project is capped at 50% of the total sample (janka holds
    ~95% of turns — an unstratified draw measures one person)
  - recent-only within --window
  - projects with <100 eligible turns are reported as a separate
    "insufficient_sample" bucket and are NOT screened or folded into the
    aggregate (review I4)

Cost: each screened turn is 1 extractor LLM call + (for person-bearing turns) a
resolver call that short-circuits ~70% without an LLM call. The measurement's own
spend is BILLED to task_llm_usage under source=chat_remember_ned (design D6.4).

Use --dry-run FIRST: it prints the sampling plan, per-stratum counts, and a
rough LLM-call estimate WITHOUT making a single LLM call, so cost is visible
before any spend.

The report is AGGREGATES ONLY — no chat content is ever printed.

Requires chat.provider=router with chat.router.bedrock.enabled=true (the KG
extractor/resolver models route to the bedrock sub-provider), and a postgres
database. AWS credentials must be present in the environment for the real run
(the systemd unit loads ~/.config/vornik/secrets/aws.env; a manual run must
source it).

Examples:
  vornikctl memory ned-calibrate --dry-run
  vornikctl memory ned-calibrate --dry-run --json
  vornikctl memory ned-calibrate --sample-size 800 --window 30d
  vornikctl memory ned-calibrate --sample-size 800 --window 30d --json`,
	RunE: runMemoryNEDCalibrate,
}

func init() {
	memoryNEDCalibrateCmd.Flags().IntVar(&nedCalSampleSize, "sample-size", 800, "Target number of turns to screen across all eligible strata")
	memoryNEDCalibrateCmd.Flags().StringVar(&nedCalWindow, "window", "30d", "Recent-only window (e.g. 30d, 720h) — only turns newer than now-window are eligible")
	memoryNEDCalibrateCmd.Flags().IntVar(&nedCalMinPerStratum, "min-per-stratum", 100, "Minimum eligible turns for a project to be a stratum; below this it is reported as insufficient_sample")
	memoryNEDCalibrateCmd.Flags().StringVar(&nedCalSeed, "seed", "vornik-ned-calibration", "Deterministic sampling seed — the same seed draws the same turns (reproducible re-runs)")
	memoryNEDCalibrateCmd.Flags().BoolVar(&nedCalDryRun, "dry-run", false, "Print the sampling plan + per-stratum counts + call estimate WITHOUT any LLM call")
	memoryNEDCalibrateCmd.Flags().BoolVar(&nedCalJSON, "json", false, "Emit JSON instead of the human-readable report")
	memoryCmd.AddCommand(memoryNEDCalibrateCmd)
}

// -----------------------------------------------------------------------------
// Injected seams — the sampling + tally logic is unit-testable without a DB or
// an LLM by faking these two interfaces.
// -----------------------------------------------------------------------------

// turnSource yields sampling metadata + deterministic samples from the historical
// chat store. The production implementation (chatAuditTurnSource) reads
// chat_audit_log; tests inject a fake with fixture rows.
type turnSource interface {
	// AvailableCounts returns, per project, the number of DISTINCT eligible
	// human turns newer than `since`. Projects with an empty id are excluded.
	AvailableCounts(ctx context.Context, since time.Time) (map[string]int, error)
	// SampleStratum returns up to n DISTINCT eligible human-turn TEXTS for one
	// project, selected deterministically by seed. The texts are consumed only
	// by the screener and never surface in the report.
	SampleStratum(ctx context.Context, projectID string, since time.Time, n int, seed string) ([]string, error)
}

// outcomeScreener classifies one turn into the §6.2.1 axis. *ned.Gate satisfies
// it (Classify), reusing the shipped, billed extract→resolve path.
type outcomeScreener interface {
	Classify(ctx context.Context, projectID, text string) (ned.Outcome, error)
}

// -----------------------------------------------------------------------------
// Deterministic sampling frame (§7) — pure, fully unit-testable.
// -----------------------------------------------------------------------------

type stratumPlan struct {
	Project   string `json:"project"`
	Available int    `json:"available"`
	Draw      int    `json:"draw"`
}

type samplePlan struct {
	Eligible     []stratumPlan `json:"eligible"`
	Insufficient []stratumPlan `json:"insufficient_sample"`
	CapMax       int           `json:"dominant_cap_max"`
	TotalDraw    int           `json:"total_draw"`
}

// planSample builds the stratified sampling plan (§7): >=minPerStratum per
// eligible stratum where available, the dominant project capped at dominantCap
// of sampleSize, and projects below minPerStratum split into a separate
// insufficient_sample bucket (never sampled, never folded into the aggregate —
// review I4). Deterministic: iteration is over sorted project names.
func planSample(counts map[string]int, sampleSize, minPerStratum int, dominantCap float64) samplePlan {
	var eligible []string
	var insufficient []stratumPlan
	for p, c := range counts {
		if strings.TrimSpace(p) == "" {
			// A turn with no project id cannot be entity-resolved (the resolver
			// needs a project scope for its catalog); exclude it entirely.
			continue
		}
		if c >= minPerStratum {
			eligible = append(eligible, p)
		} else {
			insufficient = append(insufficient, stratumPlan{Project: p, Available: c})
		}
	}
	sort.Strings(eligible)
	sort.Slice(insufficient, func(i, j int) bool { return insufficient[i].Project < insufficient[j].Project })

	capMax := int(float64(sampleSize) * dominantCap)
	if capMax < 1 {
		capMax = sampleSize
	}
	ceiling := func(p string) int {
		if c := counts[p]; c <= capMax {
			return c
		}
		return capMax
	}
	alloc := waterFill(eligible, sampleSize, minPerStratum, ceiling)

	plan := samplePlan{Insufficient: insufficient, CapMax: capMax}
	if plan.Insufficient == nil {
		plan.Insufficient = []stratumPlan{}
	}
	for _, p := range eligible {
		if alloc[p] == 0 {
			continue
		}
		plan.Eligible = append(plan.Eligible, stratumPlan{Project: p, Available: counts[p], Draw: alloc[p]})
		plan.TotalDraw += alloc[p]
	}
	if plan.Eligible == nil {
		plan.Eligible = []stratumPlan{}
	}
	return plan
}

// waterFill allocates `sampleSize` draws across the (sorted) eligible strata:
// phase 1 raises every stratum to the minPerStratum floor (bounded by its
// ceiling), spreading evenly by lowest current allocation; phase 2 distributes
// any remainder to the stratum with the largest remaining headroom. Both phases
// are ceiling-bounded, so the dominant cap and per-project availability are
// never exceeded. Deterministic: eligible is pre-sorted and ties resolve to the
// first (lowest-named) stratum.
func waterFill(eligible []string, sampleSize, minPerStratum int, ceiling func(string) int) map[string]int {
	alloc := make(map[string]int, len(eligible))
	for _, p := range eligible {
		alloc[p] = 0
	}
	for remaining := sampleSize; remaining > 0; remaining-- {
		pick := pickBelowFloor(eligible, alloc, minPerStratum, ceiling)
		if pick == "" {
			pick = pickLargestHeadroom(eligible, alloc, ceiling)
		}
		if pick == "" {
			break // no headroom anywhere; the sample is as large as it can be
		}
		alloc[pick]++
	}
	return alloc
}

// pickBelowFloor returns the eligible stratum with the smallest allocation that
// is still below both the floor and its ceiling, or "" if every stratum has met
// its floor (or is capped).
func pickBelowFloor(eligible []string, alloc map[string]int, minPerStratum int, ceiling func(string) int) string {
	pick := ""
	for _, p := range eligible {
		if alloc[p] >= minPerStratum || alloc[p] >= ceiling(p) {
			continue
		}
		if pick == "" || alloc[p] < alloc[pick] {
			pick = p
		}
	}
	return pick
}

// pickLargestHeadroom returns the eligible stratum with the most remaining
// capacity (ceiling - alloc), or "" if none has headroom left.
func pickLargestHeadroom(eligible []string, alloc map[string]int, ceiling func(string) int) string {
	pick, best := "", 0
	for _, p := range eligible {
		if head := ceiling(p) - alloc[p]; head > best {
			best, pick = head, p
		}
	}
	return pick
}

// -----------------------------------------------------------------------------
// Tally + report (§6.2.1 axis) — pure, unit-testable.
// -----------------------------------------------------------------------------

type outcomeTally struct {
	None      int `json:"none"`
	Match     int `json:"match"`
	New       int `json:"new"`
	Ambiguous int `json:"ambiguous"`
	Errors    int `json:"errors"`
}

func (t *outcomeTally) add(o outcomeTally) {
	t.None += o.None
	t.Match += o.Match
	t.New += o.New
	t.Ambiguous += o.Ambiguous
	t.Errors += o.Errors
}

// screened is every turn we attempted, including errors.
func (t outcomeTally) screened() int { return t.None + t.Match + t.New + t.Ambiguous + t.Errors }

// classified is every turn that produced a decision (errors excluded).
func (t outcomeTally) classified() int { return t.None + t.Match + t.New + t.Ambiguous }

// withPerson is the population that engages the gate — turns naming a person.
// The §7 ship/tune thresholds are read against THIS denominator, because `none`
// turns (no PERSON) are unaffected by the gate.
func (t outcomeTally) withPerson() int { return t.Match + t.New + t.Ambiguous }

type stratumReport struct {
	Project   string       `json:"project"`
	Available int          `json:"available"`
	Tally     outcomeTally `json:"tally"`
}

type calibrationReport struct {
	Window           string             `json:"window"`
	WindowSince      string             `json:"window_since"`
	AsOf             string             `json:"as_of"`
	Seed             string             `json:"seed"`
	ExtractorModel   string             `json:"extractor_model"`
	ResolverModel    string             `json:"resolver_model"`
	SampleSizeTarget int                `json:"sample_size_target"`
	MinPerStratum    int                `json:"min_per_stratum"`
	DominantCapFrac  float64            `json:"dominant_cap_frac"`
	DominantCapMax   int                `json:"dominant_cap_max"`
	TotalScreened    int                `json:"total_screened"`
	Overall          outcomeTally       `json:"overall"`
	PerStratum       []stratumReport    `json:"per_stratum"`
	Insufficient     []stratumPlan      `json:"insufficient_sample"`
	AllTurnsRates    map[string]float64 `json:"rates_all_turns"`
	WithPersonRates  map[string]float64 `json:"rates_with_person"`
	DecisionGuidance []string           `json:"decision_guidance"`
}

// buildReport assembles the aggregate report. It contains ONLY counts, rates,
// and guidance — never any turn content.
func buildReport(overall outcomeTally, strata []stratumReport, plan samplePlan, params calibrateParams, since time.Time) calibrationReport {
	classified := overall.classified()
	withPerson := overall.withPerson()
	allRates := map[string]float64{
		"none":      ratio(overall.None, classified),
		"match":     ratio(overall.Match, classified),
		"new":       ratio(overall.New, classified),
		"ambiguous": ratio(overall.Ambiguous, classified),
	}
	wpRates := map[string]float64{
		"match":     ratio(overall.Match, withPerson),
		"new":       ratio(overall.New, withPerson),
		"ambiguous": ratio(overall.Ambiguous, withPerson),
	}
	return calibrationReport{
		Window:           params.WindowLabel,
		WindowSince:      since.Format(time.RFC3339),
		AsOf:             params.AsOf.Format(time.RFC3339),
		Seed:             params.Seed,
		ExtractorModel:   params.ExtractorModel,
		ResolverModel:    params.ResolverModel,
		SampleSizeTarget: params.SampleSize,
		MinPerStratum:    params.MinPerStratum,
		DominantCapFrac:  params.DominantCap,
		DominantCapMax:   plan.CapMax,
		TotalScreened:    overall.screened(),
		Overall:          overall,
		PerStratum:       strata,
		Insufficient:     plan.Insufficient,
		AllTurnsRates:    allRates,
		WithPersonRates:  wpRates,
		DecisionGuidance: decisionGuidance(wpRates, withPerson),
	}
}

// decisionGuidance renders the §6.2.1 / §7 ship/tune verdict from the
// person-bearing rates. Both thresholds are evaluated; the guidance names the
// re-scope options when `new` blocks the design.
func decisionGuidance(wpRates map[string]float64, withPerson int) []string {
	if withPerson == 0 {
		return []string{"INCONCLUSIVE: no person-bearing turns were screened — the sample named nobody, so the resolver distribution cannot be read. Widen --window or raise --sample-size."}
	}
	var g []string
	newHigh := wpRates["new"] > nedCalNewBlockThreshold
	ambHigh := wpRates["ambiguous"] > nedCalAmbiguousTuneThreshold
	if newHigh {
		g = append(g,
			fmt.Sprintf("DO NOT ship shared scope as designed: new=%.1f%% of person-bearing turns exceeds the 30%% block threshold — the NED gate would refuse the normal case. Re-scope options (§6.2.1):", 100*wpRates["new"]),
			"  - allow `new` for PERSONAL scope only (write proceeds, no data-subject link; Art 17 falls back to full-text search), OR",
			"  - require the person be added to the entity graph before a shared write, OR",
			"  - keep shared-scope writes on the task path (route through a task).")
	}
	if ambHigh {
		g = append(g, fmt.Sprintf("TUNE the resolver: ambiguous=%.1f%% of person-bearing turns exceeds the 20%% threshold — the resolver is under-deciding; revisit the cosine/Levenshtein gate or the resolver prompt/model before shipping.", 100*wpRates["ambiguous"]))
	}
	if !newHigh && !ambHigh {
		g = append(g, fmt.Sprintf("SHIP shared scope end-to-end: new=%.1f%% (<=30%%) and ambiguous=%.1f%% (<=20%%) of person-bearing turns — the gate does not block the normal case.", 100*wpRates["new"], 100*wpRates["ambiguous"]))
	}
	return g
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// -----------------------------------------------------------------------------
// Orchestration
// -----------------------------------------------------------------------------

type calibrateParams struct {
	SampleSize     int
	MinPerStratum  int
	DominantCap    float64
	Window         time.Duration
	WindowLabel    string
	AsOf           time.Time
	Seed           string
	ExtractorModel string
	ResolverModel  string
	DryRun         bool
	JSON           bool
}

// runCalibration is the transport-free core: it plans the sample from the
// source's counts, and either prints the plan (dry-run, ZERO screener calls) or
// screens each drawn turn and reports the distribution. Fully unit-testable with
// a fake turnSource + fake outcomeScreener.
func runCalibration(ctx context.Context, src turnSource, scr outcomeScreener, params calibrateParams, out io.Writer) error {
	since := params.AsOf.Add(-params.Window)
	counts, err := src.AvailableCounts(ctx, since)
	if err != nil {
		return fmt.Errorf("count eligible turns: %w", err)
	}
	plan := planSample(counts, params.SampleSize, params.MinPerStratum, params.DominantCap)

	if params.DryRun {
		return printPlan(out, plan, params, since)
	}
	if scr == nil {
		return fmt.Errorf("no screener configured for a non-dry-run calibration")
	}

	strata := make([]stratumReport, 0, len(plan.Eligible))
	var overall outcomeTally
	for _, sp := range plan.Eligible {
		texts, err := src.SampleStratum(ctx, sp.Project, since, sp.Draw, params.Seed)
		if err != nil {
			return fmt.Errorf("sample stratum %q: %w", sp.Project, err)
		}
		var tally outcomeTally
		for _, txt := range texts {
			oc, err := scr.Classify(ctx, sp.Project, txt)
			if err != nil {
				// Do NOT surface the error text or the turn — an error may echo
				// the model's message; the content never leaves this loop. Count
				// it separately (never folded into the four rates).
				tally.Errors++
				continue
			}
			switch oc {
			case ned.OutcomeNone:
				tally.None++
			case ned.OutcomeMatch:
				tally.Match++
			case ned.OutcomeNew:
				tally.New++
			case ned.OutcomeAmbiguous:
				tally.Ambiguous++
			}
		}
		strata = append(strata, stratumReport{Project: sp.Project, Available: sp.Available, Tally: tally})
		overall.add(tally)
	}

	report := buildReport(overall, strata, plan, params, since)
	return printReport(out, report, params.JSON)
}

// -----------------------------------------------------------------------------
// Rendering (aggregates only — no content)
// -----------------------------------------------------------------------------

type planJSON struct {
	Mode             string     `json:"mode"`
	Window           string     `json:"window"`
	WindowSince      string     `json:"window_since"`
	AsOf             string     `json:"as_of"`
	Seed             string     `json:"seed"`
	ExtractorModel   string     `json:"extractor_model"`
	ResolverModel    string     `json:"resolver_model"`
	SampleSize       int        `json:"sample_size_target"`
	MinPerStratum    int        `json:"min_per_stratum"`
	DominantCapFrac  float64    `json:"dominant_cap_frac"`
	DominantCapMax   int        `json:"dominant_cap_max"`
	Plan             samplePlan `json:"plan"`
	EstCallsExpected int        `json:"est_llm_calls_expected"`
	EstCallsWorst    int        `json:"est_llm_calls_worst_case"`
}

func planEstimates(totalDraw int) (expected, worst int) {
	// Worst case: 2 LLM calls per turn (extractor + resolver, no short-circuit).
	// Expected: 1 extractor call per turn always, plus a resolver call on
	// person-bearing turns of which ~70% short-circuit without an LLM call.
	// Without knowing the person-bearing fraction up front we conservatively
	// bill the resolver on every turn at the 30% non-short-circuit rate.
	worst = totalDraw * 2
	expected = totalDraw + (totalDraw*30)/100
	return
}

func printPlan(out io.Writer, plan samplePlan, params calibrateParams, since time.Time) error {
	expected, worst := planEstimates(plan.TotalDraw)
	if params.JSON {
		pj := planJSON{
			Mode:             "dry-run",
			Window:           params.WindowLabel,
			WindowSince:      since.Format(time.RFC3339),
			AsOf:             params.AsOf.Format(time.RFC3339),
			Seed:             params.Seed,
			ExtractorModel:   params.ExtractorModel,
			ResolverModel:    params.ResolverModel,
			SampleSize:       params.SampleSize,
			MinPerStratum:    params.MinPerStratum,
			DominantCapFrac:  params.DominantCap,
			DominantCapMax:   plan.CapMax,
			Plan:             plan,
			EstCallsExpected: expected,
			EstCallsWorst:    worst,
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(pj)
	}

	_, _ = fmt.Fprintln(out, "NED calibration — SAMPLING PLAN (dry-run: no LLM calls made)")
	_, _ = fmt.Fprintln(out, "===========================================================")
	_, _ = fmt.Fprintf(out, "window:          %s (turns since %s)\n", params.WindowLabel, since.Format("2006-01-02 15:04 MST"))
	_, _ = fmt.Fprintf(out, "as-of:           %s\n", params.AsOf.Format("2006-01-02 15:04 MST"))
	_, _ = fmt.Fprintf(out, "seed:            %s\n", params.Seed)
	_, _ = fmt.Fprintf(out, "extractor model: %s\n", params.ExtractorModel)
	_, _ = fmt.Fprintf(out, "resolver model:  %s\n", params.ResolverModel)
	_, _ = fmt.Fprintf(out, "sample target:   %d   min/stratum: %d   dominant cap: %.0f%% (=%d turns)\n",
		params.SampleSize, params.MinPerStratum, 100*params.DominantCap, plan.CapMax)
	_, _ = fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STRATUM (project)\tAVAILABLE\tWILL DRAW")
	for _, sp := range plan.Eligible {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\n", sp.Project, sp.Available, sp.Draw)
	}
	_, _ = fmt.Fprintf(tw, "TOTAL\t\t%d\n", plan.TotalDraw)
	_ = tw.Flush()

	if len(plan.Insufficient) > 0 {
		_, _ = fmt.Fprintf(out, "\ninsufficient_sample (< %d eligible turns — NOT screened, NOT in the aggregate):\n", params.MinPerStratum)
		itw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(itw, "  project\tavailable")
		for _, sp := range plan.Insufficient {
			_, _ = fmt.Fprintf(itw, "  %s\t%d\n", sp.Project, sp.Available)
		}
		_ = itw.Flush()
	}

	_, _ = fmt.Fprintf(out, "\nrough LLM-call estimate for the real run (%d turns):\n", plan.TotalDraw)
	_, _ = fmt.Fprintf(out, "  expected: ~%d calls (1 extractor/turn + resolver on person-bearing turns, ~70%% short-circuit)\n", expected)
	_, _ = fmt.Fprintf(out, "  worst:     %d calls (2/turn, no short-circuit)\n", worst)
	_, _ = fmt.Fprintln(out, "\nspend lands in task_llm_usage under source=chat_remember_ned.")
	_, _ = fmt.Fprintln(out, "re-run without --dry-run to execute the measurement.")
	return nil
}

func printReport(out io.Writer, r calibrationReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}

	_, _ = fmt.Fprintln(out, "NED calibration — RESULT (aggregates only; no chat content)")
	_, _ = fmt.Fprintln(out, "==========================================================")
	_, _ = fmt.Fprintf(out, "window:          %s (turns since %s)\n", r.Window, r.WindowSince)
	_, _ = fmt.Fprintf(out, "seed:            %s\n", r.Seed)
	_, _ = fmt.Fprintf(out, "extractor model: %s\n", r.ExtractorModel)
	_, _ = fmt.Fprintf(out, "resolver model:  %s\n", r.ResolverModel)
	_, _ = fmt.Fprintf(out, "screened:        %d turns (%d classified, %d errors)\n", r.TotalScreened, r.Overall.classified(), r.Overall.Errors)
	_, _ = fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STRATUM\tAVAIL\tSCREEN\tNONE\tMATCH\tNEW\tAMBIG\tERR")
	for _, s := range r.PerStratum {
		t := s.Tally
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			s.Project, s.Available, t.screened(), t.None, t.Match, t.New, t.Ambiguous, t.Errors)
	}
	o := r.Overall
	_, _ = fmt.Fprintf(tw, "OVERALL\t\t%d\t%d\t%d\t%d\t%d\t%d\n",
		o.screened(), o.None, o.Match, o.New, o.Ambiguous, o.Errors)
	_ = tw.Flush()

	_, _ = fmt.Fprintln(out, "\nrates over ALL classified turns:")
	_, _ = fmt.Fprintf(out, "  none=%.1f%%  match=%.1f%%  new=%.1f%%  ambiguous=%.1f%%\n",
		100*r.AllTurnsRates["none"], 100*r.AllTurnsRates["match"], 100*r.AllTurnsRates["new"], 100*r.AllTurnsRates["ambiguous"])
	_, _ = fmt.Fprintf(out, "\nrates over PERSON-BEARING turns (%d turns — the gate-engaging population, drives the decision):\n", o.withPerson())
	_, _ = fmt.Fprintf(out, "  match=%.1f%%  new=%.1f%%  ambiguous=%.1f%%\n",
		100*r.WithPersonRates["match"], 100*r.WithPersonRates["new"], 100*r.WithPersonRates["ambiguous"])

	if len(r.Insufficient) > 0 {
		_, _ = fmt.Fprintf(out, "\ninsufficient_sample (< %d eligible turns — reported separately, NOT in the aggregate):\n", r.MinPerStratum)
		for _, sp := range r.Insufficient {
			_, _ = fmt.Fprintf(out, "  %s: %d turns available\n", sp.Project, sp.Available)
		}
	}

	_, _ = fmt.Fprintln(out, "\ndecision (§6.2.1 / §7):")
	for _, line := range r.DecisionGuidance {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Production wiring
// -----------------------------------------------------------------------------

func runMemoryNEDCalibrate(_ *cobra.Command, _ []string) error {
	window, err := parseWindow(nedCalWindow)
	if err != nil {
		return err
	}
	if nedCalSampleSize < 1 {
		return fmt.Errorf("--sample-size must be >= 1")
	}
	if nedCalMinPerStratum < 1 {
		return fmt.Errorf("--min-per-stratum must be >= 1")
	}

	cfg, cfgPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx := context.Background()
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "memory ned-calibrate")
	if err != nil {
		return err
	}

	params := calibrateParams{
		SampleSize:     nedCalSampleSize,
		MinPerStratum:  nedCalMinPerStratum,
		DominantCap:    nedCalDominantCapFrac,
		Window:         window,
		WindowLabel:    nedCalWindow,
		AsOf:           time.Now().UTC(),
		Seed:           nedCalSeed,
		ExtractorModel: resolvedGraphModel(cfg.Memory.Graph.ExtractorModel),
		ResolverModel:  resolvedGraphModel(cfg.Memory.Graph.ResolverModel),
		DryRun:         nedCalDryRun,
		JSON:           nedCalJSON,
	}

	src := &chatAuditTurnSource{db: db}

	var scr outcomeScreener
	if !params.DryRun {
		gate, err := buildCalibrationGate(ctx, cfg, cfgPath, db)
		if err != nil {
			return err
		}
		scr = gate
	}
	return runCalibration(ctx, src, scr, params, os.Stdout)
}

// resolvedGraphModel returns the configured KG-stage model, or the code default
// when the config key is unset (the DEPLOYED value governs — §7).
func resolvedGraphModel(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return nedCalDefaultGraphModel
}

// parseWindow accepts a Go duration (e.g. "720h") or a bare day count with a
// "d" suffix (e.g. "30d").
func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("--window is required")
	}
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid --window %q: use e.g. 30d or 720h", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --window %q: use e.g. 30d or 720h", s)
	}
	return d, nil
}

// buildCalibrationGate constructs a *ned.Gate that mirrors the daemon's
// buildChatMemoryNED (container_dispatcher.go): the DEPLOYED extractor/resolver
// models, the KG entity catalog, the memory embedder, and — critically — the
// SAME billing wiring (Usage + Pricing) so the measurement's own extract/resolve
// spend lands under source=chat_remember_ned.
//
// The KG models route to the bedrock sub-provider by `openai.*` prefix, so a
// single bedrock provider serves them (no need to reconstruct the full router).
func buildCalibrationGate(ctx context.Context, cfg *config.Config, cfgPath string, db *sql.DB) (*ned.Gate, error) {
	rcfg := cfg.Chat.Router
	if !strings.EqualFold(strings.TrimSpace(cfg.Chat.Provider), "router") || !rcfg.Bedrock.Enabled {
		return nil, fmt.Errorf("ned-calibrate requires chat.provider=router with chat.router.bedrock.enabled=true "+
			"(the KG extractor/resolver models route to the bedrock sub-provider); got provider=%q bedrock.enabled=%v",
			cfg.Chat.Provider, rcfg.Bedrock.Enabled)
	}
	if strings.TrimSpace(rcfg.Bedrock.Region) == "" {
		return nil, fmt.Errorf("chat.router.bedrock.region is required")
	}

	logger := zerolog.Nop()

	bedrockOpts := []chat.BedrockOption{chat.WithBedrockLogger(logger)}
	if cfg.Chat.Timeout != "" {
		if to, err := time.ParseDuration(cfg.Chat.Timeout); err == nil && to > 0 {
			bedrockOpts = append(bedrockOpts, chat.WithBedrockTimeout(to))
		}
	}
	maxTokens := rcfg.Bedrock.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.Chat.MaxTokens
	}
	if maxTokens > 0 {
		bedrockOpts = append(bedrockOpts, chat.WithBedrockMaxTokens(maxTokens))
	}
	provider, err := chat.NewBedrockProvider(ctx, rcfg.Bedrock.Region, rcfg.Bedrock.Model, bedrockOpts...)
	if err != nil {
		return nil, fmt.Errorf("build bedrock provider: %w", err)
	}

	extractorModel := resolvedGraphModel(cfg.Memory.Graph.ExtractorModel)
	resolverModel := resolvedGraphModel(cfg.Memory.Graph.ResolverModel)

	// Embedder — mirrors container_scheduler.go so the resolver's catalog
	// shortlist uses the SAME vector path as production. Best-effort: if memory
	// is disabled/misconfigured the embedder returns nothing and the resolver
	// falls back to a name-substring lookup (a documented fidelity caveat).
	var embedFn graph.EmbedFn
	if cfg.Memory.Enabled {
		embedder := memory.NewEmbedder(buildCalibrationEmbedConfig(cfg))
		// Bound in a closure rather than passed as a method value, because
		// graph.EmbedFn carries no scope: calibration is an operator-run tool,
		// so its embedding spend is infrastructure, not a tenant's.
		embedFn = func(ctx context.Context, projectID string, texts []string) ([][]float32, error) {
			return embedder.Embed(ctx,
				memory.EmbedScope{ProjectID: projectID, CallSite: memory.EmbedCallSiteKGResolve}, texts)
		}
	}

	entityRepo := postgres.NewKnowledgeEntityRepository(db)
	// The calibration harness bills its own measurement runs — the command's help
	// text promises exactly that ("the measurement's own spend is BILLED to
	// task_llm_usage under source=chat_remember_ned").
	gate := &ned.Gate{
		Extractor: graph.NewExtractor(provider, extractorModel),
		Resolver:  graph.NewResolver(provider, resolverModel, entityRepo, embedFn),
		Spend: llmspend.New(
			postgres.NewTaskLLMUsageRepository(db),
			loadCalibrationPricing(cfgPath),
			persistence.TaskLLMUsageSourceChatRememberNED,
			ned.RoleExtractor,
		),
	}
	return gate, nil
}

// buildCalibrationEmbedConfig maps the daemon config to a memory.Config for the
// embedder, mirroring container_scheduler.go's mapping (bedrock vs OpenAI-compat
// endpoint fallback to the resolved agent LLM).
func buildCalibrationEmbedConfig(cfg *config.Config) memory.Config {
	mc := memory.Config{
		Enabled:            true,
		EmbeddingProvider:  cfg.Memory.EmbeddingProvider,
		EmbeddingModel:     cfg.Memory.EmbeddingModel,
		BedrockRegion:      cfg.Memory.Bedrock.Region,
		EmbeddingDimension: cfg.Memory.EmbeddingDimension,
	}
	if strings.EqualFold(strings.TrimSpace(mc.EmbeddingProvider), "bedrock") {
		if mc.BedrockRegion == "" {
			mc.BedrockRegion = cfg.Chat.Router.Bedrock.Region
		}
		return mc
	}
	llm := cfg.ResolvedAgentLLM()
	mc.EmbeddingEndpoint = cfg.Memory.EmbeddingEndpoint
	if mc.EmbeddingEndpoint == "" {
		mc.EmbeddingEndpoint = llm.Endpoint
	}
	mc.EmbeddingAPIKey = cfg.Memory.EmbeddingAPIKey
	if mc.EmbeddingAPIKey == "" {
		mc.EmbeddingAPIKey = llm.APIKey
	}
	return mc
}

// loadCalibrationPricing loads the pricing table so billed rows carry cost_usd.
// Missing/invalid pricing is non-fatal (rows still record with cost 0) — a
// warning goes to stderr so the operator knows cost figures may be zero.
func loadCalibrationPricing(configPath string) *pricing.Table {
	candidates := make([]string, 0, 4)
	if env := strings.TrimSpace(os.Getenv("VORNIK_PRICING_PATH")); env != "" {
		candidates = append(candidates, env)
	}
	if configPath != "" {
		base := filepath.Dir(configPath)
		candidates = append(candidates,
			filepath.Join(base, "configs", "pricing.yaml"),
			filepath.Join(base, "pricing.yaml"),
		)
	}
	candidates = append(candidates, "configs/pricing.yaml")
	for _, c := range candidates {
		if info, err := os.Stat(c); err != nil || info.IsDir() {
			continue
		}
		if pt, err := pricing.Load(c); err == nil {
			return pt
		}
	}
	_, _ = fmt.Fprintln(os.Stderr, "warning: no pricing.yaml found — task_llm_usage rows will record cost_usd=0")
	return nil
}

// -----------------------------------------------------------------------------
// chat_audit_log turn source (postgres) — the historical human-chat store.
// Each row is one exchange; user_message is the human turn (design §7's ~23k
// turns / janka ~95%). project_id scopes the resolver; ts is the timestamp.
// -----------------------------------------------------------------------------

type chatAuditTurnSource struct{ db *sql.DB }

func (s *chatAuditTurnSource) AvailableCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, COUNT(DISTINCT user_message)
		FROM chat_audit_log
		WHERE btrim(user_message) <> ''
		  AND project_id <> ''
		  AND ts >= $1
		GROUP BY project_id`, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int)
	for rows.Next() {
		var project string
		var n int
		if err := rows.Scan(&project, &n); err != nil {
			return nil, err
		}
		out[project] = n
	}
	return out, rows.Err()
}

func (s *chatAuditTurnSource) SampleStratum(ctx context.Context, projectID string, since time.Time, n int, seed string) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	// DISTINCT turn texts, selected deterministically by md5(min(id)||seed) so
	// the same seed draws the same sample on every run. min(id) is a stable
	// per-text key; content is returned only to the screener.
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.user_message
		FROM (
			SELECT user_message, MIN(id) AS mid
			FROM chat_audit_log
			WHERE project_id = $1
			  AND btrim(user_message) <> ''
			  AND ts >= $2
			GROUP BY user_message
		) g
		ORDER BY md5(g.mid || $3)
		LIMIT $4`, projectID, since, seed, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	texts := make([]string, 0, n)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		texts = append(texts, t)
	}
	return texts, rows.Err()
}
