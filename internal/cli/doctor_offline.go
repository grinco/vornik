package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// runDoctorOffline is the daemon-down escape hatch (LLD 2026-07-07-control-
// plane-design §8a, review Critical #1). It runs static checks WITHOUT the
// daemon RPC — the whole point is to diagnose why the daemon won't start:
//   - config file present + parses
//   - database reachable (direct connection, not the daemon's pool)
//   - migration state (applied head vs the binary's expected head)
//   - recent daemon journal errors ("why won't it boot")
//
// It never mutates anything. Exit is non-zero if any check FAILs.
// buildOfflineDoctorReport runs the static (daemon-down) checks and returns the
// report + the resolved config path. Extracted so `vornikctl report` can reuse
// the offline diagnostics at install time (design 2026-07-25-vornik-report).
func buildOfflineDoctorReport(ctx context.Context) (doctorReport, string) {
	report := doctorReport{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary:   "offline",
	}
	cfg, cfgPath := offlineCheckConfig(&report)
	if cfg != nil {
		offlineCheckDatabase(ctx, cfg, &report)
	}
	offlineCheckJournal(&report)

	failed := 0
	for _, c := range report.Checks {
		if c.Status == "fail" {
			failed++
		}
	}
	if failed == 0 {
		report.Summary = "offline: all static checks passed"
	} else {
		report.Summary = fmt.Sprintf("offline: %d check(s) FAILED", failed)
	}
	return report, cfgPath
}

func runDoctorOffline(_ *cobra.Command) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, cfgPath := buildOfflineDoctorReport(ctx)
	failed := 0
	for _, c := range report.Checks {
		if c.Status == "fail" {
			failed++
		}
	}

	if doctorJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Printf("vornik doctor (offline)%s\n\n", cfgHint(cfgPath))
		for _, c := range report.Checks {
			mark := "✓"
			switch c.Status {
			case "fail":
				mark = "✗"
			case "warn":
				mark = "!"
			}
			fmt.Printf("  %s %-14s %s\n", mark, c.Name, c.Message)
			for _, it := range c.Items {
				fmt.Printf("       %s\n", it)
			}
		}
		fmt.Printf("\n%s\n", report.Summary)
	}
	if failed > 0 {
		return fmt.Errorf("%d offline check(s) failed", failed)
	}
	return nil
}

func cfgHint(path string) string {
	if path == "" {
		return ""
	}
	return " — " + path
}

func offlineCheckConfig(r *doctorReport) (*config.Config, string) {
	cfg, path, err := config.Load()
	if err != nil {
		r.Checks = append(r.Checks, doctorCheck{
			Name: "config", Status: "fail",
			Message: "config did NOT parse: " + err.Error(),
		})
		return nil, path
	}
	r.Checks = append(r.Checks, doctorCheck{
		Name: "config", Status: "ok", Message: "parsed OK",
	})
	return cfg, path
}

func offlineCheckDatabase(ctx context.Context, cfg *config.Config, r *doctorReport) {
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		r.Checks = append(r.Checks, doctorCheck{
			Name: "database", Status: "fail",
			Message: "cannot open database: " + err.Error(),
		})
		return
	}
	defer func() { _ = backend.Close() }()
	r.Checks = append(r.Checks, doctorCheck{
		Name: "database", Status: "ok", Message: "reachable",
	})

	db, err := requirePostgresDB(backend, "doctor offline")
	if err != nil {
		// SQLite / non-postgres backend — migration-head check via the
		// live schema isn't wired here; skip rather than fail.
		r.Checks = append(r.Checks, doctorCheck{
			Name: "migrations", Status: "warn",
			Message: "skipped (non-postgres backend)",
		})
		return
	}
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&applied); err != nil {
		r.Checks = append(r.Checks, doctorCheck{
			Name: "migrations", Status: "fail",
			Message: "cannot read applied migrations: " + err.Error(),
		})
		return
	}
	head := migrationHead()
	switch {
	case applied == head:
		r.Checks = append(r.Checks, doctorCheck{
			Name: "migrations", Status: "ok",
			Message: fmt.Sprintf("up to date (applied %d, head %d)", applied, head),
		})
	case applied < head:
		r.Checks = append(r.Checks, doctorCheck{
			Name: "migrations", Status: "warn",
			Message: fmt.Sprintf("behind: applied %d < binary head %d (daemon will migrate on next boot)", applied, head),
		})
	default:
		r.Checks = append(r.Checks, doctorCheck{
			Name: "migrations", Status: "warn",
			Message: fmt.Sprintf("applied %d is AHEAD of this binary's head %d (older binary?)", applied, head),
		})
	}
}

// migrationHead returns the highest migration version the binary knows about.
func migrationHead() int {
	head := 0
	for _, m := range persistence.DefaultMigrations {
		if m.Version > head {
			head = m.Version
		}
	}
	return head
}

// offlineCheckJournal tails the daemon's user-journal for recent fatal/panic/
// error lines. Best-effort: if journalctl isn't available (non-systemd host),
// it's a warn, not a fail.
func offlineCheckJournal(r *doctorReport) {
	out, err := exec.Command("journalctl", "--user", "-u", "vornik", "-n", "80", "--no-pager").CombinedOutput()
	if err != nil {
		r.Checks = append(r.Checks, doctorCheck{
			Name: "journal", Status: "warn",
			Message: "journalctl unavailable (skipped): " + strings.TrimSpace(err.Error()),
		})
		return
	}
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "panic") || strings.Contains(l, "fatal") ||
			strings.Contains(l, "\"level\":\"error\"") {
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			hits = append(hits, strings.TrimSpace(line))
		}
	}
	if len(hits) == 0 {
		r.Checks = append(r.Checks, doctorCheck{
			Name: "journal", Status: "ok", Message: "no recent fatal/panic/error lines",
		})
		return
	}
	if len(hits) > 8 {
		hits = hits[len(hits)-8:]
	}
	r.Checks = append(r.Checks, doctorCheck{
		Name: "journal", Status: "warn",
		Message: fmt.Sprintf("%d recent error/fatal line(s) in the daemon journal", len(hits)),
		Items:   hits,
	})
}
