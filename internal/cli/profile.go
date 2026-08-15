package cli

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/speedprofile"
)

var (
	profileWindow    string
	profileDSN       string
	profileEndpoint  string
	profileAPIKeyEnv string
	profileModel     string
	profileSamples   int
	profileShort     int
	profileLong      int
	profileSuggest   bool
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Measure how fast each model generates, separately from how long its tools take",
	Long: "Measure how fast each configured model generates, separately from how long\n" +
		"its tools take.\n\n" +
		"Every step timeout in Vornik is absolute wall-clock, calibrated on whatever\n" +
		"hardware the defaults were chosen on. Self-hosted deployments differ by three\n" +
		"orders of magnitude in inference speed, so sizing a budget needs a number for\n" +
		"how fast this model is HERE.\n\n" +
		"The obvious number — completion tokens over step duration — is wrong: it folds\n" +
		"container start and tool execution into the model's rate, so adding one slow\n" +
		"tool would appear to make the model slower. This fits\n\n" +
		"    duration = fixed + perToken*completion_tokens + perToolCall*tool_calls\n\n" +
		"over steps already recorded, so the model's rate and its tools' cost come out\n" +
		"as separate terms. A rising per-tool cost is a TOOL problem and must never buy\n" +
		"the model more time.\n\n" +
		"Reads only. Nothing here changes a timeout — LLD 6.2's scaling is blocked on a\n" +
		"slow-hardware measurement, which this command exists to make possible.",
	RunE: runProfile,
}

func init() {
	profileCmd.Flags().StringVar(&profileWindow, "window", "24 hours",
		"how far back to read steps (postgres interval)")
	profileCmd.Flags().StringVar(&profileDSN, "dsn", "",
		"database to read (defaults to $VORNIK_PROFILE_DSN, then $VORNIK_BENCH_DSN)")
	profileCmd.Flags().StringVar(&profileEndpoint, "probe-endpoint", "",
		"OpenAI-compatible base URL to PROBE directly (e.g. http://host:8000/v1)")
	profileCmd.Flags().StringVar(&profileAPIKeyEnv, "probe-key-env", "VORNIK_PROBE_API_KEY",
		"env var holding the probe endpoint's bearer token")
	profileCmd.Flags().StringVar(&profileModel, "probe-model", "",
		"model id to probe (required with --probe-endpoint)")
	profileCmd.Flags().IntVar(&profileSamples, "probe-samples", 3,
		"measured probe pairs, excluding a discarded warm-up")
	profileCmd.Flags().IntVar(&profileShort, "probe-short-tokens", 32,
		"short generation length; the slope against --probe-long-tokens cancels fixed cost")
	profileCmd.Flags().IntVar(&profileLong, "probe-long-tokens", 512,
		"long generation length")
	profileCmd.Flags().BoolVar(&profileSuggest, "suggest-config", false,
		"emit a ready-to-paste speed_aware_timeouts block using the measured rate")
	rootCmd.AddCommand(profileCmd)
}

func runProfile(cmd *cobra.Command, _ []string) error {
	// A probe needs no history, so it runs on a deployment that has never
	// executed a task — exactly the cold-start case a fit cannot serve. Tried
	// FIRST, and the database is only consulted if the probe alone was not asked
	// for: requiring a reachable database to run a pure endpoint probe was the
	// same defect as requiring a DSN at all.
	if profileEndpoint != "" && profileDSN == "" &&
		os.Getenv("VORNIK_PROFILE_DSN") == "" && os.Getenv("VORNIK_BENCH_DSN") == "" {
		return probeOnly(cmd)
	}

	// The daemon already knows which database it writes, so asking the operator
	// for a DSN was a defect, not a safeguard: `vornikctl profile` failed on a
	// clean install with "no database given" and no hint that the answer was
	// sitting in the daemon's own config. Explicit --dsn and the env vars still
	// win, for pointing at a bench or a copy.
	db, closeDB, err := openProfileDB()
	if err != nil {
		return err
	}
	defer closeDB()

	ctx := cmd.Context()
	models, err := speedprofile.Models(ctx, db, profileWindow)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no model usage recorded in the last %s — run some work first, "+
			"or widen --window", profileWindow)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "MODEL\tSTEPS\tDECODE tok/s\t±\tPER TOOL CALL\tFIXED\n")
	var fitted int
	fittedByModel := map[string]speedprofile.Profile{}
	// Refusals print too. A model whose profile cannot be trusted is the most
	// important line in the table: it is the one whose timeouts are still being
	// sized by a configured guess.
	var refusals []string
	for _, m := range models {
		samples, err := speedprofile.LoadSamples(ctx, db, m, profileWindow)
		if err != nil {
			return err
		}
		p, err := speedprofile.Fit(m, samples)
		if err != nil {
			refusals = append(refusals, err.Error())
			_, _ = fmt.Fprintf(w, "%s\t%d\t—\t—\t—\n", m, len(samples))
			continue
		}
		fitted++
		fittedByModel[m] = p
		_, _ = fmt.Fprintf(w, "%s\t%d\t%.0f\t%.0f%%\t%.2fs\t%.1fs\n",
			m, p.Samples, p.DecodeTokensPerSec(), p.DecodeUncertaintyRatio()*100,
			p.MSPerToolCall/1000, p.FixedMS/1000)
	}
	_ = w.Flush()

	for _, r := range refusals {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\nnot profiled: %s\n", r)
	}

	if err := reportProbeAgainstFit(cmd, fittedByModel); err != nil {
		return err
	}
	if profileSuggest {
		suggestConfig(cmd, fittedByModel)
	}

	if fitted == 0 {
		return fmt.Errorf("no model could be profiled; see the refusals above")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"\nDecode is the model's own rate — the figure a timeout should scale on.\n"+
			"Per-tool-call and fixed costs are diagnostics: a rising per-tool cost is a\n"+
			"tool problem, and must not buy the model more time.\n")
	return nil
}

// probeOnly runs the controlled probe with no ledger to compare against.
func probeOnly(cmd *cobra.Command) error {
	res, err := runProbe(cmd)
	if err != nil {
		return err
	}
	if profileSuggest {
		suggestFromProbe(cmd, res)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"%s: %.0f tok/s median marginal decode (%.0f-%.0f over %d pairs), "+
			"%.0fms per-request overhead\n\n"+
			"Marginal decode with no workload around it: measured as the SLOPE between a\n"+
			"short and a long generation, so per-request cost cancels rather than dragging\n"+
			"the rate. This is what the HARDWARE can do;\n"+
			"it is not what a step will see once tools, container start and queueing are\n"+
			"in the way. Run this again with a database to compare the two.\n",
		res.Model, res.MedianTokensPerSec, res.MinTokensPerSec, res.MaxTokensPerSec,
		res.Samples, res.FixedMS)
	return nil
}

func runProbe(cmd *cobra.Command) (speedprofile.ProbeResult, error) {
	if profileModel == "" {
		return speedprofile.ProbeResult{}, fmt.Errorf("--probe-model is required with --probe-endpoint")
	}
	return speedprofile.Probe(cmd.Context(), &http.Client{Timeout: 3 * time.Minute},
		speedprofile.ProbeOptions{
			Endpoint:    strings.TrimSuffix(profileEndpoint, "/"),
			APIKey:      os.Getenv(profileAPIKeyEnv),
			Model:       profileModel,
			Samples:     profileSamples,
			ShortTokens: profileShort,
			LongTokens:  profileLong,
		})
}

// absPct renders how far a ratio sits from parity, in percent.
func absPct(ratio float64) float64 {
	d := (ratio - 1) * 100
	if d < 0 {
		return -d
	}
	return d
}

// reportProbeAgainstFit prints the controlled probe beside the fitted rate.
//
// Separate from runProfile because the two measure different things and the
// comparison is the interesting part: printing them together is what makes
// contention visible.
func reportProbeAgainstFit(cmd *cobra.Command, fittedByModel map[string]speedprofile.Profile) error {
	if profileEndpoint == "" {
		return nil
	}
	// Probe and fit measure DIFFERENT things and are never averaged. Printing
	// them together makes the gap visible, and the gap is the point: it is the
	// deployment's contention, not the model's speed.
	res, perr := runProbe(cmd)
	if perr != nil {
		return perr
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"\nPROBE %s: %.0f tok/s median marginal decode (%.0f-%.0f, %d pairs), "+
			"%.0fms per-request overhead.\n",
		res.Model, res.MedianTokensPerSec, res.MinTokensPerSec, res.MaxTokensPerSec,
		res.Samples, res.FixedMS)
	if p, ok := fittedByModel[res.Model]; ok {
		// Both are now slopes, so the comparison is meaningful. Say something
		// TRUE in each direction rather than asserting contention that may not
		// be there — an earlier version printed "1x slower", which is not a
		// finding.
		ratio := speedprofile.ContentionRatio(res, p)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "      under load it fits %.0f tok/s",
			p.DecodeTokensPerSec())
		switch {
		case ratio >= 1.2:
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				" — %.1fx slower than idle.\n"+
					"      That gap is contention and scheduling, NOT the model. A timeout sized\n"+
					"      from the probe alone would under-budget every real step.\n", ratio)
		case ratio <= 0.8:
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				" — FASTER than the idle probe (%.2fx).\n"+
					"      Not an error: concurrent requests batch on most serving stacks, so\n"+
					"      loaded throughput per request can exceed a single idle stream. Size\n"+
					"      from the fit, which reflects the workload that actually runs.\n", ratio)
		default:
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				" — within %.0f%% of idle.\n"+
					"      No meaningful contention on this deployment: the hardware and the\n"+
					"      workload agree, so either figure would size a timeout similarly.\n",
				absPct(ratio))
		}
	}
	return nil
}

// suggestConfig emits the config block, with the reference filled in from what
// was just measured.
//
// This exists because the reference is otherwise a chicken-and-egg problem: it
// is defined as "the decode rate the shipped base timeouts were calibrated
// against", and an operator who never measured that hardware has no way to
// name it — so the feature would stay off forever, which the design review
// flagged as a real risk rather than a theoretical one.
//
// The resolution is a question the operator CAN answer: do the current timeouts
// work on this host? If they do, this host IS the reference, and the measured
// rate is the honest number to declare. Every slower host then scales relative
// to a baseline that demonstrably worked, which is exactly what the factor is
// supposed to mean.
func suggestConfig(cmd *cobra.Command, fitted map[string]speedprofile.Profile) {
	var slowest speedprofile.Profile
	for _, p := range fitted {
		if slowest.MSPerCompletionToken == 0 || p.DecodeTokensPerSec() < slowest.DecodeTokensPerSec() {
			slowest = p
		}
	}
	if slowest.MSPerCompletionToken == 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"\nno usable profile, so no reference can be suggested — see the refusals above\n")
		return
	}

	// The SLOWEST profiled model, not the fastest or an average: the reference
	// must be a rate at which the current timeouts are known to hold, and they
	// hold today only because the slowest model still fits inside them.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), `
SUGGESTED CONFIG — paste into the daemon config the daemon actually reads
(check with: vornikctl config show), then reload.

scheduler:
  speed_aware_timeouts:
    enabled: true
    reference_tokens_per_sec: %.0f   # measured here, on %s
    min_factor: 0.5
    max_factor: 8.0
    min_samples: %d
    window: 24h

Read that reference as a CLAIM, and only make it if it is true: "the timeouts
in this deployment work as they are, on hardware that decodes at %.0f tok/s".
It is taken from the SLOWEST model profiled, because that is the one the
current budgets have to accommodate today.

A host at half this rate then gets 2x the time; the measured 12 tok/s box
would ask for ~%.0fx and hit the ceiling, which is reported rather than
silently satisfied.
`, slowest.DecodeTokensPerSec(), slowest.Model, speedprofile.MinSamples,
		slowest.DecodeTokensPerSec(), slowest.DecodeTokensPerSec()/12)
}

// openProfileDB resolves the database to profile, preferring an explicit
// override and falling back to the daemon's own configuration.
func openProfileDB() (*sql.DB, func(), error) {
	dsn := profileDSN
	for _, env := range []string{"VORNIK_PROFILE_DSN", "VORNIK_BENCH_DSN"} {
		if dsn == "" {
			dsn = os.Getenv(env)
		}
	}
	if dsn != "" {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open database from --dsn: %w", err)
		}
		return db, func() { _ = db.Close() }, nil
	}

	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("no --dsn given and the daemon config could not be read (%w).\n"+
			"Either point VORNIK_CONFIG at the daemon's config file, or pass --dsn, or\n"+
			"profile an endpoint directly with --probe-endpoint and --probe-model, which\n"+
			"needs no database at all", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open the daemon's database: %w", err)
	}
	db, err := requirePostgresDB(backend, "vornikctl profile")
	if err != nil {
		_ = backend.Close()
		return nil, nil, err
	}
	return db, func() { _ = backend.Close() }, nil
}

// suggestFromProbe emits the config block from a probe alone.
//
// A fresh install has no history to fit, which is precisely when an operator is
// deciding whether the hardware is usable. Requiring a database here would mean
// the feature could only be configured after running the workload it is meant to
// protect.
func suggestFromProbe(cmd *cobra.Command, res speedprofile.ProbeResult) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), `
SUGGESTED CONFIG — from a PROBE (no workload history on this install yet).

scheduler:
  speed_aware_timeouts:
    enabled: true
    reference_tokens_per_sec: <the rate your timeouts were tuned for>
    observed_tokens_per_sec: %.0f   # measured here, on %s
    min_factor: 0.5
    max_factor: 8.0

Fill in the reference yourself: it is the rate at which the CURRENT timeouts are
known to work, which this box cannot tell you. If this is the machine those
timeouts were chosen on, the two are the same number and the feature is a no-op
until you run it somewhere slower.

Re-run with a database once real work has flowed, and the fitted rate will
replace this one — the fit reflects the workload, the probe only the hardware.
`, res.MedianTokensPerSec, res.Model)
}
