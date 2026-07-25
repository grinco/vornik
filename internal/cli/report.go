package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/report"
)

var (
	reportSummary string
	reportTask    string
	reportSince   string
	reportOffline bool
	reportURLOnly bool
	reportDryRun  bool
)

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "File an anonymized problem report to grinco/vornik (guided, zero-auth)",
	Long: `Collect anonymized diagnostics and produce a ready-to-open grinco/vornik
GitHub issue URL you submit with your OWN account. Works with the daemon up
(rich diagnostics) or down (offline static checks — e.g. an install failure).

Nothing is posted automatically. The issue body is anonymized (secrets, emails,
home paths, LAN IPs, and this machine's hostname are stripped); review it before
you submit.`,
	RunE: runReport,
}

func init() {
	reportCmd.Flags().StringVar(&reportSummary, "summary", "", "one-line description of the problem (anonymized before posting)")
	reportCmd.Flags().StringVar(&reportTask, "task", "", "a task id whose support-report bundle you may attach")
	reportCmd.Flags().StringVar(&reportSince, "since", "", "a time window whose support-report bundle you may attach")
	reportCmd.Flags().BoolVar(&reportOffline, "offline", false, "force offline diagnostics (skip the daemon)")
	reportCmd.Flags().BoolVar(&reportURLOnly, "url-only", false, "print only the prefilled issue URL")
	reportCmd.Flags().BoolVar(&reportDryRun, "dry-run", false, "print the anonymized body only; submit/emit nothing else")
	rootCmd.AddCommand(reportCmd)
}

func runReport(_ *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	daemonUp, checks := collectDoctorForReport(ctx)

	host, _ := os.Hostname()
	body, err := report.AnonymizeBody(report.BodyInput{
		Version:  Version,
		Edition:  edition,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: host,
		DaemonUp: daemonUp,
		Checks:   checks,
		Symptom:  reportSummary,
	})
	if err != nil {
		return err // static fail-closed message (never wraps the offending value)
	}

	issueURL := report.IssueURL(reportTitle(checks, daemonUp), body)

	switch {
	case reportDryRun:
		fmt.Println("--- anonymized issue body (preview — nothing is submitted) ---")
		fmt.Println(body)
		return nil
	case reportURLOnly:
		fmt.Println(issueURL)
		return nil
	}

	fmt.Println("The issue body below is ANONYMIZED and will be PUBLIC on github.com/grinco/vornik.")
	fmt.Println("Review it before you submit.")
	fmt.Println()
	fmt.Println(body)
	fmt.Println("Open this prefilled issue, review it, then submit with your GitHub account:")
	fmt.Println("  " + issueURL)
	if reportTask != "" || reportSince != "" {
		sel := "--task " + reportTask
		if reportSince != "" {
			sel = "--since " + reportSince
		}
		fmt.Println()
		fmt.Println("For a detailed bundle to attach, run:")
		fmt.Println("  vornikctl support-report " + sel)
		fmt.Println("  (redacted for secrets, but may carry project names — OPEN + inspect it")
		fmt.Println("   (e.g. grep MANIFEST.json for your project name) BEFORE attaching.)")
	}
	return nil
}

// collectDoctorForReport returns (daemonUp, checks). It tries the daemon's
// /doctor; on any connection error (or --offline) it falls back to the offline
// static checks — it NEVER hard-fails on a down daemon (install-time reporting
// is the whole point).
func collectDoctorForReport(ctx context.Context) (bool, []report.Check) {
	if !reportOffline {
		if checks, ok := onlineDoctorChecks(); ok {
			return true, checks
		}
	}
	rep, _ := buildOfflineDoctorReport(ctx)
	return false, toReportChecks(rep.Checks)
}

func onlineDoctorChecks() ([]report.Check, bool) {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/doctor", nil)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	var rep doctorReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, false
	}
	return toReportChecks(rep.Checks), true
}

func toReportChecks(cs []doctorCheck) []report.Check {
	out := make([]report.Check, 0, len(cs))
	for _, c := range cs {
		out = append(out, report.Check{Name: c.Name, Status: c.Status, Message: c.Message})
	}
	return out
}

// reportTitle builds a short issue title from fixed strings only. Check names
// normally come from daemon enums, but the wire shape is an unrestricted
// string; copying it into a public URL would bypass the body anonymizer.
func reportTitle(checks []report.Check, daemonUp bool) string {
	for _, c := range checks {
		if strings.EqualFold(c.Status, "fail") {
			return "vornik: doctor check failing"
		}
	}
	if !daemonUp {
		return "vornik problem (offline / install)"
	}
	return "vornik problem report"
}
