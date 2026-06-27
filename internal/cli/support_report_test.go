package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/secrets"
)

// fakeDaemon returns a canned daemon bundle tar.gz with a seeded secret
// in one file (so we can also assert the client doesn't mangle the
// already-redacted daemon side — here we ship a clean daemon bundle).
type fakeDaemon struct {
	status   int
	bundle   []byte
	lastBody map[string]any
}

func (f *fakeDaemon) Post(_ string, body interface{}) (*http.Response, error) {
	if b, ok := body.(map[string]any); ok {
		f.lastBody = b
	}
	rc := io.NopCloser(bytes.NewReader(f.bundle))
	return &http.Response{
		StatusCode: f.status,
		Body:       rc,
		Header:     http.Header{},
	}, nil
}

func makeDaemonBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// fakeHost returns canned host output; journald carries a seeded secret.
type fakeHost struct {
	secret string
}

func (h fakeHost) Journald(_, _ string, _ int) ([]byte, error) {
	return []byte(`{"MESSAGE":"started, token=` + h.secret + `"}` + "\n"), nil
}
func (fakeHost) PodmanVersion() ([]byte, error)   { return []byte("podman 5.0.0\n"), nil }
func (fakeHost) SystemctlStatus() ([]byte, error) { return []byte("active (running)\n"), nil }
func (fakeHost) SwarmctlVersion() ([]byte, error) { return []byte("test-version\n"), nil }

func newTestCmd() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetIn(strings.NewReader("yes\n"))
	return c, &out
}

func testDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// hostSecret is an openai-shaped key (matches the detector).
const hostSecret = "sk-HOSTSECRET00000000000000000000000000000000"

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = string(data)
	}
	return out
}

func baseBundle(t *testing.T) []byte {
	return makeDaemonBundle(t, map[string]string{
		"MANIFEST.json":  `{"schema_version":1,"mode":"task","task_id":"t1","redaction_by_type":{"openai_key":2}}`,
		"REDACTION.txt":  "total redactions: 2\n",
		"version.txt":    "2026.6.0\n",
		"task/task.json": `{"id":"t1"}`,
	})
}

func TestSupportReport_HostSectionsRedacted(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "bundle.tar.gz")
	cmd, _ := newTestCmd()
	daemon := &fakeDaemon{status: http.StatusOK, bundle: baseBundle(t)}
	host := fakeHost{secret: hostSecret}
	opts := supportReportOptions{Task: "t1", Output: outPath, MaxSize: supportDefaultMaxSize, Lines: 100}

	if err := executeSupportReport(cmd, daemon, testDetector(t), host, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}

	files := readArchive(t, outPath)
	jd, ok := files["host/daemon_journald.json"]
	if !ok {
		t.Fatalf("missing host/daemon_journald.json; have %v", keysOfStr(files))
	}
	if strings.Contains(jd, hostSecret) {
		t.Errorf("host journald leaked the raw secret:\n%s", jd)
	}
	if !strings.Contains(jd, "[REDACTED:") {
		t.Errorf("host journald not redacted:\n%s", jd)
	}
	// Host version sections present.
	for _, n := range []string{"host/podman_version.txt", "host/systemctl_status.txt", "host/vornikctl_version.txt"} {
		if _, ok := files[n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
	// Daemon sections preserved.
	if _, ok := files["task/task.json"]; !ok {
		t.Error("daemon section task/task.json lost")
	}
	// Request body carried task_id, max_size.
	if daemon.lastBody["task_id"] != "t1" {
		t.Errorf("request body task_id = %v", daemon.lastBody["task_id"])
	}
}

func TestSupportReport_IncludeRaw(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "bundle.tar.gz")
	cmd, _ := newTestCmd()
	daemon := &fakeDaemon{status: http.StatusOK, bundle: baseBundle(t)}
	host := fakeHost{secret: hostSecret}
	opts := supportReportOptions{Task: "t1", Output: outPath, MaxSize: supportDefaultMaxSize, IncludeRaw: true, Lines: 100}

	if err := executeSupportReport(cmd, daemon, testDetector(t), host, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Output renamed with -RAW.
	rawPath := filepath.Join(dir, "bundle-RAW.tar.gz")
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("expected %s; got err %v", rawPath, err)
	}
	files := readArchive(t, rawPath)
	jd := files["host/daemon_journald.json"]
	if !strings.Contains(jd, hostSecret) {
		t.Errorf("raw mode should keep the host secret verbatim:\n%s", jd)
	}
	// MANIFEST stamped raw:true + archive_sha256.
	var mf map[string]any
	if err := json.Unmarshal([]byte(files["MANIFEST.json"]), &mf); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if mf["raw"] != true {
		t.Errorf("manifest raw = %v, want true", mf["raw"])
	}
	if s, _ := mf["archive_sha256"].(string); len(s) != 64 {
		t.Errorf("archive_sha256 = %q, want 64 hex chars", s)
	}
}

func TestSupportReport_DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "bundle.tar.gz")
	cmd, out := newTestCmd()
	daemon := &fakeDaemon{status: http.StatusOK, bundle: baseBundle(t)}
	host := fakeHost{secret: hostSecret}
	opts := supportReportOptions{Task: "t1", Output: outPath, MaxSize: supportDefaultMaxSize, DryRun: true, Lines: 100}

	if err := executeSupportReport(cmd, daemon, testDetector(t), host, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write the archive; stat err = %v", err)
	}
	// Lists files + redaction counts.
	s := out.String()
	if !strings.Contains(s, "DRY RUN") || !strings.Contains(s, "host/daemon_journald.json") {
		t.Errorf("dry-run output missing expected content:\n%s", s)
	}
	if !strings.Contains(s, "host redactions by type") {
		t.Errorf("dry-run missing redaction counts:\n%s", s)
	}
}

func TestSupportReport_DaemonError(t *testing.T) {
	cmd, _ := newTestCmd()
	daemon := &fakeDaemon{status: http.StatusForbidden, bundle: []byte(`{"error":{"code":"ADMIN_SCOPE_REQUIRED","message":"admin scope required"}}`)}
	opts := supportReportOptions{Task: "t1", MaxSize: supportDefaultMaxSize}
	err := executeSupportReport(cmd, daemon, testDetector(t), fakeHost{}, opts)
	if err == nil {
		t.Fatal("expected error from daemon 403")
	}
}

func TestResolveOutputPath(t *testing.T) {
	// Auto-name, task mode.
	p := resolveOutputPath(supportReportOptions{Task: "task_abc"}, "task")
	if !strings.Contains(p, "vornik-support-task_abc-") || !strings.HasSuffix(p, ".tar.gz") {
		t.Errorf("auto task name = %q", p)
	}
	// Raw adds -RAW.
	p = resolveOutputPath(supportReportOptions{Task: "x", IncludeRaw: true}, "task")
	if !strings.Contains(p, "-RAW.tar.gz") {
		t.Errorf("raw name = %q", p)
	}
	// Explicit -o + raw gets -RAW injected.
	p = resolveOutputPath(supportReportOptions{Output: "out.tar.gz", IncludeRaw: true}, "task")
	if p != "out-RAW.tar.gz" {
		t.Errorf("explicit raw name = %q", p)
	}
	// Window auto-name.
	p = resolveOutputPath(supportReportOptions{Since: "2h"}, "window")
	if !strings.Contains(p, "vornik-support-window-") {
		t.Errorf("window name = %q", p)
	}
}

func TestConfirmRaw(t *testing.T) {
	// --yes skips the prompt.
	cmd, _ := newTestCmd()
	if err := confirmRaw(cmd, true); err != nil {
		t.Fatalf("--yes should pass: %v", err)
	}
	// Wrong input aborts.
	c2 := &cobra.Command{}
	var out bytes.Buffer
	c2.SetOut(&out)
	c2.SetIn(strings.NewReader("no\n"))
	if err := confirmRaw(c2, false); err == nil {
		t.Fatal("non-yes input should abort")
	}
	// "yes" proceeds.
	c3 := &cobra.Command{}
	c3.SetOut(&out)
	c3.SetIn(strings.NewReader("yes\n"))
	if err := confirmRaw(c3, false); err != nil {
		t.Fatalf("'yes' should pass: %v", err)
	}
}

func TestNormalizeJournalTime(t *testing.T) {
	if got := normalizeJournalTime("2026-06-20T10:00:00Z"); got != "2026-06-20 10:00:00" {
		t.Errorf("rfc3339 -> %q", got)
	}
	if got := normalizeJournalTime(""); got != "" {
		t.Errorf("empty -> %q", got)
	}
	if got := normalizeJournalTime("2h"); got == "" || strings.Contains(got, "h") {
		t.Errorf("duration -> %q (should be absolute)", got)
	}
}

func keysOfStr(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExecHostRunner_SwarmctlVersion(t *testing.T) {
	out, err := execHostRunner{}.SwarmctlVersion()
	if err != nil || len(out) == 0 {
		t.Fatalf("SwarmctlVersion: %q %v", out, err)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "MANIFEST.json")
	in := map[string]any{"mode": "task", "raw": false}
	if err := writeManifest(p, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readManifest(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got["mode"] != "task" {
		t.Errorf("round-trip lost mode: %v", got)
	}
	if _, err := readManifest(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("reading a missing manifest should error")
	}
}

func TestSha256File(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := sha256File(p)
	if err != nil || len(sum) != 64 {
		t.Fatalf("sha256File: %q %v", sum, err)
	}
	if _, err := sha256File(filepath.Join(dir, "missing")); err == nil {
		t.Error("sha256 of missing file should error")
	}
}

func TestRunSupportReport_XOR(t *testing.T) {
	// Neither flag set → error (no daemon call).
	supportTask, supportSince = "", ""
	if err := runSupportReport(supportReportCmd, nil); err == nil {
		t.Error("expected XOR validation error with no flags")
	}
	// Both set → error.
	supportTask, supportSince = "t1", "2h"
	if err := runSupportReport(supportReportCmd, nil); err == nil {
		t.Error("expected XOR validation error with both flags")
	}
	supportTask, supportSince = "", ""
}

// errHost makes every host command fail, hitting the best-effort error
// branch in appendHostSections (the section file records the error).
type errHost struct{}

func (errHost) Journald(string, string, int) ([]byte, error) { return nil, errStubCli("jd") }
func (errHost) PodmanVersion() ([]byte, error)               { return nil, errStubCli("pv") }
func (errHost) SystemctlStatus() ([]byte, error)             { return nil, errStubCli("st") }
func (errHost) SwarmctlVersion() ([]byte, error)             { return nil, errStubCli("sv") }

type errStubCli string

func (e errStubCli) Error() string { return string(e) }

func TestSupportReport_HostErrorsRecorded(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "b.tar.gz")
	cmd, _ := newTestCmd()
	daemon := &fakeDaemon{status: http.StatusOK, bundle: baseBundle(t)}
	opts := supportReportOptions{Task: "t1", Output: outPath, MaxSize: supportDefaultMaxSize, Lines: 10}
	if err := executeSupportReport(cmd, daemon, testDetector(t), errHost{}, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	files := readArchive(t, outPath)
	jd := files["host/daemon_journald.json"]
	if !strings.Contains(jd, "error collecting section") {
		t.Errorf("host section should record the collection error:\n%s", jd)
	}
}

func TestSupportReport_AutoOutputName(t *testing.T) {
	// No -o: writes an auto-named file in cwd; clean it up.
	cmd, _ := newTestCmd()
	daemon := &fakeDaemon{status: http.StatusOK, bundle: baseBundle(t)}
	opts := supportReportOptions{Task: "autotask", MaxSize: supportDefaultMaxSize, Lines: 1}
	if err := executeSupportReport(cmd, daemon, testDetector(t), fakeHost{secret: hostSecret}, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Find + remove the auto-named file.
	matches, _ := filepath.Glob("vornik-support-autotask-*.tar.gz")
	if len(matches) == 0 {
		t.Fatal("expected an auto-named archive in cwd")
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func TestSupportReport_WindowMode(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "win.tar.gz")
	cmd, _ := newTestCmd()
	bundle := makeDaemonBundle(t, map[string]string{
		"MANIFEST.json":     `{"schema_version":1,"mode":"window"}`,
		"window/tasks.json": `[]`,
	})
	daemon := &fakeDaemon{status: http.StatusOK, bundle: bundle}
	opts := supportReportOptions{Since: "2h", Until: "1h", Output: outPath, MaxSize: supportDefaultMaxSize, Lines: 5}
	if err := executeSupportReport(cmd, daemon, testDetector(t), fakeHost{secret: hostSecret}, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if daemon.lastBody["since"] != "2h" || daemon.lastBody["until"] != "1h" {
		t.Errorf("window request body wrong: %+v", daemon.lastBody)
	}
	files := readArchive(t, outPath)
	if _, ok := files["window/tasks.json"]; !ok {
		t.Errorf("window bundle missing window/tasks.json; have %v", keysOfStr(files))
	}
}

func TestRewriteManifestAndTar_NilManifest(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "b")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "x.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "out.tar.gz")
	// nil manifest + non-raw: still writes a MANIFEST.json + archive.
	if err := rewriteManifestAndTar(bundleDir, final, nil, map[string]int{}, supportReportOptions{}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	files := readArchive(t, final)
	if _, ok := files["MANIFEST.json"]; !ok {
		t.Error("MANIFEST.json should be created from nil manifest")
	}
}
