package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/archiveutil"
	"vornik.io/vornik/internal/secrets"
)

// vornikctl support-report
// =======================
//
// Produces a single, self-contained, REDACTED bundle for support: it
// POSTs to the daemon's /api/v1/support-report (which returns the
// already-redacted server-collectable core), unpacks that into a
// staging dir, APPENDS host-only sections (journald daemon logs,
// podman/systemctl/vornikctl versions) — redacted client-side with the
// SAME internal/secrets package the daemon uses — and re-tars to the
// final archive (atomic temp→rename).
//
// See https://docs.vornik.io

var (
	supportTask       string
	supportSince      string
	supportUntil      string
	supportOutput     string
	supportMaxSize    int64
	supportDryRun     bool
	supportIncludeRaw bool
	supportYes        bool
	supportLines      int
)

const supportDefaultMaxSize = 200 << 20 // 200 MiB

var supportReportCmd = &cobra.Command{
	Use:   "support-report",
	Short: "Collect a redacted support bundle for a task or time window",
	Long: `Build a single, self-contained, redacted tar.gz bundle for the support
team: task/execution lifecycle, tool + admin audit, LLM usage, conversation,
judge/post-mortem, the task's text artifacts, container logs, redacted config,
a doctor diagnosis, version + health — plus host-only sections (journald daemon
logs, podman/systemctl/vornikctl versions) collected on this machine.

Everything is redacted by default through vornik's secret detector BEFORE it is
written, on both the daemon and this client. The archive is meant to leave the
operator's trust boundary; no secret enters it unless you pass --include-raw.

Exactly one of --task or --since is required.

Examples:
  vornikctl support-report --task task_2026...
  vornikctl support-report --since 2h
  vornikctl support-report --since 2026-06-20T00:00:00Z --until 2026-06-20T06:00:00Z
  vornikctl support-report --task task_... --dry-run
  vornikctl support-report --task task_... --include-raw   # gated; writes -RAW.tar.gz
`,
	RunE: runSupportReport,
}

func init() {
	f := supportReportCmd.Flags()
	f.StringVar(&supportTask, "task", "", "task ID to collect (XOR --since)")
	f.StringVar(&supportSince, "since", "", "window start: RFC3339 timestamp or Go duration like 2h/90m (XOR --task)")
	f.StringVar(&supportUntil, "until", "", "window end: RFC3339 or duration (default now)")
	f.StringVarP(&supportOutput, "output", "o", "", "output archive path (default ./vornik-support-<task|window>-<RFC3339>.tar.gz)")
	f.Int64Var(&supportMaxSize, "max-size", supportDefaultMaxSize, "total archive size cap in bytes")
	f.BoolVar(&supportDryRun, "dry-run", false, "print the would-be manifest + redaction counts, write nothing")
	f.BoolVar(&supportIncludeRaw, "include-raw", false, "DANGER: skip redaction; writes <name>-RAW.tar.gz with secrets intact")
	f.BoolVar(&supportYes, "yes", false, "skip the interactive confirmation for --include-raw")
	f.IntVar(&supportLines, "lines", 5000, "max journald lines to collect for the host section")
	rootCmd.AddCommand(supportReportCmd)
}

func runSupportReport(cmd *cobra.Command, _ []string) error {
	hasTask := strings.TrimSpace(supportTask) != ""
	hasWindow := strings.TrimSpace(supportSince) != ""
	if hasTask == hasWindow {
		return fmt.Errorf("exactly one of --task or --since is required")
	}

	if supportIncludeRaw && !supportDryRun {
		if err := confirmRaw(cmd, supportYes); err != nil {
			return err
		}
	}

	detector, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		return fmt.Errorf("build secret detector: %w", err)
	}

	opts := supportReportOptions{
		Task:       supportTask,
		Since:      supportSince,
		Until:      supportUntil,
		Output:     supportOutput,
		MaxSize:    supportMaxSize,
		DryRun:     supportDryRun,
		IncludeRaw: supportIncludeRaw,
		Lines:      supportLines,
	}
	runner := &execHostRunner{}
	return executeSupportReport(cmd, ClientFromEnv(), detector, runner, opts)
}

// supportReportOptions is the resolved flag set, separated from cobra
// so executeSupportReport is unit-testable.
type supportReportOptions struct {
	Task       string
	Since      string
	Until      string
	Output     string
	MaxSize    int64
	DryRun     bool
	IncludeRaw bool
	Lines      int
}

// supportHTTPClient is the subset of *Client executeSupportReport
// needs (so tests can inject a fake daemon).
type supportHTTPClient interface {
	Post(path string, body interface{}) (*http.Response, error)
}

// hostCommandRunner abstracts the host-only command invocations so
// tests can supply canned output instead of shelling out.
type hostCommandRunner interface {
	Journald(since, until string, lines int) ([]byte, error)
	PodmanVersion() ([]byte, error)
	SystemctlStatus() ([]byte, error)
	SwarmctlVersion() ([]byte, error)
}

// executeSupportReport is the testable core: call the daemon, unpack,
// append redacted host sections, re-tar (or print a dry-run manifest).
func executeSupportReport(cmd *cobra.Command, client supportHTTPClient, detector secrets.Detector, host hostCommandRunner, opts supportReportOptions) error {
	out := cmd.OutOrStdout()

	// 1. Call the daemon.
	reqBody := map[string]any{"max_size": opts.MaxSize, "include_raw": opts.IncludeRaw}
	mode := "window"
	if strings.TrimSpace(opts.Task) != "" {
		reqBody["task_id"] = opts.Task
		mode = "task"
	} else {
		reqBody["since"] = opts.Since
		if strings.TrimSpace(opts.Until) != "" {
			reqBody["until"] = opts.Until
		}
	}
	resp, err := client.Post("/api/v1/support-report", reqBody)
	if err != nil {
		return fmt.Errorf("call daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	// 2. Stream the daemon bundle into a staging dir.
	staging, err := os.MkdirTemp("", "vornik-support-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	daemonArchive := filepath.Join(staging, "daemon.tar.gz")
	if err := streamToFile(daemonArchive, resp.Body); err != nil {
		return fmt.Errorf("stream daemon bundle: %w", err)
	}
	bundleDir := filepath.Join(staging, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}
	if err := archiveutil.UntarGz(daemonArchive, bundleDir); err != nil {
		return fmt.Errorf("unpack daemon bundle: %w", err)
	}
	_ = os.Remove(daemonArchive)

	// 3. Append host-only sections, redacted client-side (unless raw).
	hostTally, err := appendHostSections(bundleDir, detector, host, opts)
	if err != nil {
		return fmt.Errorf("collect host sections: %w", err)
	}

	// 4. Read the daemon MANIFEST so we can extend it (host files +
	//    raw stamp + archive sha) and surface a summary.
	mf, _ := readManifest(filepath.Join(bundleDir, "MANIFEST.json"))

	// 5. Dry-run: print the would-be manifest + redaction counts, write nothing.
	if opts.DryRun {
		printDryRun(out, bundleDir, mf, hostTally, opts)
		return nil
	}

	// 6. Re-tar the staging bundle to the final path (atomic temp→rename).
	finalPath := resolveOutputPath(opts, mode)
	if err := rewriteManifestAndTar(bundleDir, finalPath, mf, hostTally, opts); err != nil {
		return err
	}

	// 7. Summary.
	printSummary(out, finalPath, bundleDir, hostTally, opts)
	return nil
}

// appendHostSections collects the four host-only sections, redacts each
// through internal/secrets (unless raw), and writes them under host/.
// Returns per-type redaction counts for the host sections.
func appendHostSections(bundleDir string, detector secrets.Detector, host hostCommandRunner, opts supportReportOptions) (map[string]int, error) {
	hostDir := filepath.Join(bundleDir, "host")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return nil, err
	}
	tally := map[string]int{}

	write := func(name string, data []byte, collectErr error) error {
		target := filepath.Join(hostDir, name)
		if collectErr != nil {
			// Best-effort: record the error in the section file + continue.
			data = []byte(fmt.Sprintf("error collecting section: %v\n", collectErr))
		}
		if !opts.IncludeRaw {
			findings := detector.Scan(data)
			if len(findings) > 0 {
				for _, f := range findings {
					tally[f.Type]++
				}
				data = secrets.Redact(data, findings)
			}
		}
		return os.WriteFile(target, data, 0o600)
	}

	jd, jerr := host.Journald(opts.Since, opts.Until, opts.Lines)
	if err := write("daemon_journald.json", jd, jerr); err != nil {
		return nil, err
	}
	pv, perr := host.PodmanVersion()
	if err := write("podman_version.txt", pv, perr); err != nil {
		return nil, err
	}
	st, serr := host.SystemctlStatus()
	if err := write("systemctl_status.txt", st, serr); err != nil {
		return nil, err
	}
	sv, verr := host.SwarmctlVersion()
	if err := write("vornikctl_version.txt", sv, verr); err != nil {
		return nil, err
	}
	return tally, nil
}

// rewriteManifestAndTar updates MANIFEST.json with host files, the raw
// stamp, and (for raw) the archive sha256, then tars atomically.
func rewriteManifestAndTar(bundleDir, finalPath string, mf map[string]any, hostTally map[string]int, opts supportReportOptions) error {
	if mf == nil {
		mf = map[string]any{}
	}
	mf["raw"] = opts.IncludeRaw
	mf["host_redaction_by_type"] = hostTally
	// Recompute the files list to include the appended host sections.
	if files, err := listBundleFiles(bundleDir); err == nil {
		mf["files"] = files
	}

	// For raw bundles we stamp the archive sha256 AFTER writing, so do
	// a first marshal without it, tar, hash, then re-stamp + re-tar.
	if err := writeManifest(filepath.Join(bundleDir, "MANIFEST.json"), mf); err != nil {
		return err
	}

	tmp := finalPath + ".tmp"
	if err := archiveutil.TarGzDir(bundleDir, tmp); err != nil {
		return fmt.Errorf("archive: %w", err)
	}

	if opts.IncludeRaw {
		sum, err := sha256File(tmp)
		if err == nil {
			mf["archive_sha256"] = sum
			if err := writeManifest(filepath.Join(bundleDir, "MANIFEST.json"), mf); err != nil {
				return err
			}
			if err := archiveutil.TarGzDir(bundleDir, tmp); err != nil {
				return fmt.Errorf("archive (raw re-stamp): %w", err)
			}
		}
	}

	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalize archive: %w", err)
	}
	return nil
}

// resolveOutputPath honours -o, otherwise auto-names; raw bundles get
// an unmistakable -RAW suffix.
func resolveOutputPath(opts supportReportOptions, mode string) string {
	if strings.TrimSpace(opts.Output) != "" {
		p := opts.Output
		if opts.IncludeRaw && !strings.Contains(p, "-RAW") {
			ext := ".tar.gz"
			base := strings.TrimSuffix(p, ext)
			p = base + "-RAW" + ext
		}
		return p
	}
	scope := mode
	if mode == "task" {
		scope = sanitizePathSegment(opts.Task)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("vornik-support-%s-%s", scope, stamp)
	if opts.IncludeRaw {
		name += "-RAW"
	}
	return "./" + name + ".tar.gz"
}

func confirmRaw(cmd *cobra.Command, yes bool) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "WARNING: --include-raw DISABLES redaction.")
	_, _ = fmt.Fprintln(out, "The archive WILL contain secrets (API keys, tokens, connection strings).")
	_, _ = fmt.Fprintln(out, "It must NOT leave the operator's trust boundary — that INCLUDES the support team.")
	_, _ = fmt.Fprintln(out, "Use this only for LOCAL debugging. The file will be named <name>-RAW.tar.gz.")
	if yes {
		_, _ = fmt.Fprintln(out, "(--yes supplied; proceeding)")
		return nil
	}
	_, _ = fmt.Fprint(out, "Type 'yes' to proceed: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	line, _ := reader.ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return fmt.Errorf("aborted: --include-raw not confirmed")
	}
	return nil
}

func printDryRun(out io.Writer, bundleDir string, mf map[string]any, hostTally map[string]int, opts supportReportOptions) {
	_, _ = fmt.Fprintln(out, "DRY RUN — no archive written.")
	_, _ = fmt.Fprintf(out, "raw mode: %t\n", opts.IncludeRaw)
	files, _ := listBundleFiles(bundleDir)
	_, _ = fmt.Fprintf(out, "\nwould include %d files:\n", len(files))
	for _, f := range files {
		_, _ = fmt.Fprintf(out, "  %s\n", f.Name)
	}
	if mf != nil {
		if rb, ok := mf["redaction_by_type"]; ok {
			_, _ = fmt.Fprintf(out, "\ndaemon redactions by type: %v\n", rb)
		}
	}
	_, _ = fmt.Fprintf(out, "host redactions by type: %v\n", hostTally)
}

func printSummary(out io.Writer, finalPath, bundleDir string, hostTally map[string]int, opts supportReportOptions) {
	_, _ = fmt.Fprintf(out, "\nsupport report written: %s (%d bytes)\n", finalPath, archiveutil.FileSize(finalPath))
	files, _ := listBundleFiles(bundleDir)
	_, _ = fmt.Fprintf(out, "sections: %d\n", len(files))
	if opts.IncludeRaw {
		_, _ = fmt.Fprintln(out, "WARNING: this is a RAW bundle — it contains UNREDACTED secrets. Keep it local.")
	} else {
		total := 0
		for _, v := range hostTally {
			total += v
		}
		_, _ = fmt.Fprintf(out, "host-section redactions: %d\n", total)
	}
}

// ---- small helpers ----

type manifestFileEntry struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

func listBundleFiles(dir string) ([]manifestFileEntry, error) {
	var out []manifestFileEntry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, manifestFileEntry{Name: filepath.ToSlash(rel), Bytes: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

func readManifest(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is inside our staging dir
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeManifest(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func streamToFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is inside our staging dir
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sanitizePathSegment(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, " ", "_")
	if s == "" {
		return "task"
	}
	return s
}

// execHostRunner is the production hostCommandRunner — it shells out to
// journalctl / podman / systemctl and reports vornikctl's own version.
type execHostRunner struct{}

func (execHostRunner) Journald(since, until string, lines int) ([]byte, error) {
	args := []string{"--user", "-u", "vornik.service", "-o", "json", "--no-pager"}
	if s := normalizeJournalTime(since); s != "" {
		args = append(args, "--since", s)
	}
	if u := normalizeJournalTime(until); u != "" {
		args = append(args, "--until", u)
	}
	if lines > 0 {
		args = append(args, "-n", fmt.Sprintf("%d", lines))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "journalctl", args...).Output()
}

func (execHostRunner) PodmanVersion() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "podman", "version").CombinedOutput()
}

func (execHostRunner) SystemctlStatus() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// systemctl status returns non-zero exit when the unit is
	// inactive; CombinedOutput still carries the useful text.
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "status", "vornik.service").CombinedOutput()
	if len(out) > 0 {
		return out, nil
	}
	return out, err
}

func (execHostRunner) SwarmctlVersion() ([]byte, error) {
	return []byte(Version + "\n"), nil
}

// normalizeJournalTime passes RFC3339 timestamps through; for Go
// durations it converts to an absolute "YYYY-MM-DD HH:MM:SS" journalctl
// understands (journalctl doesn't accept "2h" the way our flag does).
func normalizeJournalTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04:05")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d).Format("2006-01-02 15:04:05")
	}
	return s
}
