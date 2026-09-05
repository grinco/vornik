package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"vornik.io/vornik/internal/version"
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
	// supportLocal / supportRequireDaemon select the collection path. Default
	// is neither: try the daemon, fall back locally if it cannot answer.
	supportLocal         bool
	supportRequireDaemon bool
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

By default this asks the daemon. If the daemon cannot answer — Community
Edition, which ships no admin surface, or a daemon that is down — it falls back
to collecting LOCALLY: the same collector, reading the database and config on
this host, and it says so. --local forces that path; --require-daemon forbids
it. Health and metrics are the daemon's live state and are recorded as section
errors on the local path rather than silently missing.

Examples:
  vornikctl support-report --task task_2026...
  vornikctl support-report --since 2h
  vornikctl support-report --since 2026-06-20T00:00:00Z --until 2026-06-20T06:00:00Z
  vornikctl support-report --task task_... --dry-run
  vornikctl support-report --task task_... --local          # collect on this host
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
	f.BoolVar(&supportLocal, "local", false, "collect locally from the database and config instead of asking the daemon (works on Community, and with the daemon down)")
	f.BoolVar(&supportRequireDaemon, "require-daemon", false, "fail instead of falling back to local collection when the daemon cannot answer")
	rootCmd.AddCommand(supportReportCmd)
}

func runSupportReport(cmd *cobra.Command, _ []string) error {
	hasTask := strings.TrimSpace(supportTask) != ""
	hasWindow := strings.TrimSpace(supportSince) != ""
	if hasTask == hasWindow {
		return fmt.Errorf("exactly one of --task or --since is required")
	}
	if supportLocal && supportRequireDaemon {
		return fmt.Errorf("--local and --require-daemon contradict each other: one forces local collection, the other forbids it")
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
		Task:          supportTask,
		Since:         supportSince,
		Until:         supportUntil,
		Output:        supportOutput,
		MaxSize:       supportMaxSize,
		DryRun:        supportDryRun,
		IncludeRaw:    supportIncludeRaw,
		Lines:         supportLines,
		Local:         supportLocal,
		RequireDaemon: supportRequireDaemon,
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
	// Local forces in-process collection; RequireDaemon refuses to fall back
	// to it. They are mutually exclusive and the command rejects the pair.
	Local         bool
	RequireDaemon bool
}

// supportHTTPClient is the subset of *Client executeSupportReport
// needs (so tests can inject a fake daemon).
//
// Get is here for the capabilities probe that answers "whose version is in
// this bundle" (support-bundle-in-CE design §4.1) — a local collection beside
// a REACHABLE daemon must report the daemon's build, not the client's.
type supportHTTPClient interface {
	Post(path string, body interface{}) (*http.Response, error)
	Get(path string) (*http.Response, error)
}

// hostCommandRunner abstracts the host-only command invocations so
// tests can supply canned output instead of shelling out.
type hostCommandRunner interface {
	Journald(since, until string, lines int) ([]byte, error)
	PodmanVersion() ([]byte, error)
	SystemctlStatus() ([]byte, error)
	SwarmctlVersion() ([]byte, error)
}

// executeSupportReport is the testable core: obtain the bundle (from the
// daemon or, on Community and when the daemon is down, locally), append
// redacted host sections, re-tar (or print a dry-run manifest).
func executeSupportReport(cmd *cobra.Command, client supportHTTPClient, detector secrets.Detector, host hostCommandRunner, opts supportReportOptions) error {
	out := cmd.OutOrStdout()

	mode := "window"
	if strings.TrimSpace(opts.Task) != "" {
		mode = "task"
	}

	staging, err := os.MkdirTemp("", "vornik-support-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	bundleDir := filepath.Join(staging, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}

	// 1. Stage the collector's output — daemon or local.
	prov, err := stageBundle(cmd, client, detector, staging, bundleDir, opts)
	if err != nil {
		return err
	}

	// 2. Append host-only sections, redacted client-side (unless raw).
	hostTally, err := appendHostSections(bundleDir, detector, host, opts)
	if err != nil {
		return fmt.Errorf("collect host sections: %w", err)
	}

	// 3. Read the daemon MANIFEST so we can extend it (host files +
	//    raw stamp + archive sha) and surface a summary.
	mf, _ := readManifest(filepath.Join(bundleDir, "MANIFEST.json"))

	// 4. Record which path produced this archive, in the bundle and in the
	//    manifest. Without it a reader cannot tell "Community has no metrics
	//    endpoint" from "the daemon was down" — opposite diagnoses.
	prov = completeProvenance(prov, mf)
	if err := writeCollectionRecord(bundleDir, prov); err != nil {
		return err
	}

	// 5. Dry-run: print the would-be manifest + redaction counts, write nothing.
	if opts.DryRun {
		printDryRun(out, bundleDir, mf, hostTally, opts)
		printProvenance(out, prov)
		return nil
	}

	// 6. Re-tar the staging bundle to the final path (atomic temp→rename).
	finalPath := resolveOutputPath(opts, mode)
	if err := rewriteManifestAndTar(bundleDir, finalPath, mf, hostTally, opts, prov); err != nil {
		return err
	}

	// 7. Summary.
	printSummary(out, finalPath, bundleDir, hostTally, opts)
	printProvenance(out, prov)
	return nil
}

// stageBundle fills bundleDir with the collector's output and reports which
// path produced it.
//
// Default behaviour is the daemon, then fall back: a CE operator should not
// have to know the word "local" to get a bundle — the 2026-08-05 dead-end was
// precisely a CE operator being handed a command they could not run. The
// fallback is never silent, and --require-daemon refuses it for a script that
// would rather fail fast than quietly produce a weaker bundle.
func stageBundle(cmd *cobra.Command, client supportHTTPClient, detector secrets.Detector, staging, bundleDir string, opts supportReportOptions) (collectionProvenance, error) {
	out := cmd.OutOrStdout()

	if opts.Local {
		// --local forces the local path in Enterprise too, which is what an
		// operator wants when the daemon is DOWN — the case a support bundle is
		// most often needed for.
		return runLocalCollection(cmd, client, detector, bundleDir, opts)
	}

	err := fetchDaemonBundle(client, staging, bundleDir, opts)
	if err == nil {
		return collectionProvenance{Path: "daemon", VersionSource: "daemon", DaemonReachable: true,
			CLIVersion: Version, CLIEdition: version.NormalizeEdition(edition),
			Note: "collected by the daemon over /api/v1/support-report"}, nil
	}
	if opts.RequireDaemon {
		return collectionProvenance{}, fmt.Errorf("%w (--require-daemon: not falling back to local collection)", err)
	}
	if !shouldFallBackLocal(err) {
		return collectionProvenance{}, err
	}
	_, _ = fmt.Fprintf(out, "the daemon could not produce the bundle (%v)\n", err)
	_, _ = fmt.Fprintln(out, "falling back to LOCAL collection — reading the database and config on this host.")
	return runLocalCollection(cmd, client, detector, bundleDir, opts)
}

// runLocalCollection probes the daemon for version provenance, builds the
// bundle in-process, and stages it.
func runLocalCollection(cmd *cobra.Command, client supportHTTPClient, detector secrets.Detector, bundleDir string, opts supportReportOptions) (collectionProvenance, error) {
	// cobra hands a context to a command it EXECUTES; a command constructed
	// directly (a test, or a future programmatic caller) has none, and a nil
	// context panics several layers down in the database driver.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	prov := resolveProvenance(probeDaemonIdentity(client))
	res, err := collectLocalBundle(ctx, detector, opts, prov)
	if err != nil {
		return collectionProvenance{}, fmt.Errorf("collect locally: %w", err)
	}
	if err := stageBundleFiles(bundleDir, res.Files); err != nil {
		return collectionProvenance{}, err
	}
	return prov, nil
}

// fetchDaemonBundle calls the endpoint and unpacks its archive into bundleDir.
func fetchDaemonBundle(client supportHTTPClient, staging, bundleDir string, opts supportReportOptions) error {
	reqBody := map[string]any{"max_size": opts.MaxSize, "include_raw": opts.IncludeRaw}
	if strings.TrimSpace(opts.Task) != "" {
		reqBody["task_id"] = opts.Task
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

	daemonArchive := filepath.Join(staging, "daemon.tar.gz")
	if err := streamToFile(daemonArchive, resp.Body); err != nil {
		return fmt.Errorf("stream daemon bundle: %w", err)
	}
	defer func() { _ = os.Remove(daemonArchive) }()
	if err := archiveutil.UntarGz(daemonArchive, bundleDir); err != nil {
		return fmt.Errorf("unpack daemon bundle: %w", err)
	}
	return nil
}

// shouldFallBackLocal decides whether a daemon failure is one the local path
// can answer.
//
// The two cases are the ones the feature exists for: Community answering 501
// EDITION_UNSUPPORTED because it ships no admin surface, and a daemon that is
// not there at all. Anything else — a rejected window, an authorization
// failure, a 500 — is a real answer to the operator's request, and silently
// producing a DIFFERENT bundle instead of reporting it would hide the thing
// they need to see.
func shouldFallBackLocal(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotImplemented
	}
	// A transport failure: connection refused, no such host, timeout.
	return strings.Contains(err.Error(), "call daemon:")
}

// completeProvenance fills the daemon path's version/edition from the bundle's
// own manifest — the daemon stated them, and restating them here keeps
// collection.json readable without cross-referencing.
func completeProvenance(prov collectionProvenance, mf map[string]any) collectionProvenance {
	if prov.Path != "daemon" || mf == nil {
		return prov
	}
	if v, ok := mf["vornik_version"].(string); ok {
		prov.Version = v
		prov.DaemonVersion = v
	}
	if e, ok := mf["vornik_edition"].(string); ok {
		prov.Edition = e
		prov.DaemonEdition = e
	}
	return prov
}

// writeCollectionRecord writes collection.json into the staging tree. It is
// recomputed into the manifest's file list by rewriteManifestAndTar, so it is
// carried like any other section.
func writeCollectionRecord(bundleDir string, prov collectionProvenance) error {
	payload, err := json.MarshalIndent(prov, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "collection.json"), payload, 0o600)
}

// printProvenance states the path and whose version the bundle carries. The
// operator ASKED for this to be explicit (2026-09-04): a bundle that does not
// say which edition produced it makes an absent section unreadable, since the
// same absence means "not built into this edition" on Community and "broken"
// on Enterprise.
func printProvenance(out io.Writer, prov collectionProvenance) {
	_, _ = fmt.Fprintf(out, "collected: %s | version %s (%s) | edition %s\n",
		prov.Path, prov.Version, prov.VersionSource, prov.Edition)
	if prov.Path == "local" && !prov.DaemonReachable {
		_, _ = fmt.Fprintf(out, "  daemon unreachable (%s) — health and metrics are recorded as section errors.\n", prov.DaemonError)
	}
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
func rewriteManifestAndTar(bundleDir, finalPath string, mf map[string]any, hostTally map[string]int, opts supportReportOptions, prov collectionProvenance) error {
	if mf == nil {
		mf = map[string]any{}
	}
	mf["raw"] = opts.IncludeRaw
	mf["host_redaction_by_type"] = hostTally
	// The manifest is what a tool reads, so the collection path belongs here
	// as well as in collection.json.
	mf["collection_path"] = prov.Path
	mf["version_source"] = prov.VersionSource
	mf["daemon_reachable"] = prov.DaemonReachable
	if prov.Path == "local" {
		// A locally-collected bundle states its version and edition even when
		// the daemon path never wrote them (an empty manifest from a failed
		// read is still a manifest).
		mf["vornik_version"] = prov.Version
		mf["vornik_edition"] = prov.Edition
	}
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
		// Deliberately NO attach guidance here: a raw bundle must never be nudged
		// toward a public issue.
		return
	}
	total := 0
	for _, v := range hostTally {
		total += v
	}
	_, _ = fmt.Fprintf(out, "host-section redactions: %d\n", total)
	// Where it is + how to get it onto a report (operator instruction 2026-08-03:
	// naming the command was not enough — reporters never attached the bundle).
	_, _ = fmt.Fprintln(out, "\nBefore you share it, read it:")
	_, _ = fmt.Fprintf(out, "  tar -tzf %s\n", finalPath)
	_, _ = fmt.Fprintf(out, "  tar -xOzf %s MANIFEST.json    # sections, truncations, redaction counts\n", finalPath)
	_, _ = fmt.Fprintln(out, "It is redacted for secrets but MAY still carry project, swarm and workflow")
	_, _ = fmt.Fprintln(out, "names, task ids and prompt text.")
	_, _ = fmt.Fprintln(out, "To attach it to a problem report: run `vornikctl report` for the prefilled")
	_, _ = fmt.Fprintln(out, "issue, then drag this .tar.gz into the GitHub comment box (25 MB per attachment max —")
	_, _ = fmt.Fprintln(out, "narrow the scope or use --max-size if it is bigger).")
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
