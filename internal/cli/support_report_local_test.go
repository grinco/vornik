package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/version"
)

// LOCAL COLLECTION — the Community-Edition path
// (https://docs.vornik.io).
//
// The failure these tests exist against is concrete: on 2026-08-05 a Community
// operator filing a bug was told by the product to run `support-report`, which
// answers only with 501 EDITION_UNSUPPORTED.

// sqliteConfigWithData migrates a scratch database, seeds one task, and returns
// a config pointing at it. Open (not OpenReadOnly) is deliberate here — the
// FIXTURE is allowed to create the schema; the collector is not.
func sqliteConfigWithData(t *testing.T, secret string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "vornik.db")

	b, err := storage.Open(context.Background(), config.DatabaseConfig{Driver: "sqlite", Path: dbPath})
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	wf := "wf1"
	task := &persistence.Task{
		ID:         "task_local_1",
		ProjectID:  "p1",
		WorkflowID: &wf,
		Status:     persistence.TaskStatusCompleted,
		// The secret rides in the payload, which the bundle carries verbatim —
		// so this fixture proves the local path redacts a DATABASE row, not
		// only the config file.
		Payload: []byte(`{"brief":"a task whose logs mention ` + secret + `"}`),
	}
	if err := b.Repos.Tasks.Create(context.Background(), task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	cfg := &config.Config{}
	cfg.Database = config.DatabaseConfig{Driver: "sqlite", Path: dbPath}
	// A secret in a field whose NAME says "token": the bundle's config section
	// must not carry it out of the trust boundary.
	cfg.Telegram = config.TelegramConfig{Enabled: true, BotToken: secret}
	return cfg, filepath.Join(dir, "config.yaml")
}

// §4.1 — whose version is in the bundle. The field a support engineer trusts
// first must not quietly become the wrong build's, and the two genuinely
// differ: on the reference host the client was one commit ahead of the daemon
// for part of 2026-09-04.
func TestResolveProvenance_VersionFollowsTheDaemonWhenItAnswers(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          daemonIdentity
		wantSource  string
		wantVersion string
		wantEdition string
		wantNote    string
	}{
		{
			name:        "daemon reachable: its version and edition win",
			id:          daemonIdentity{Version: "2026.9.1-71", Edition: version.EditionEnterprise, Reachable: true},
			wantSource:  "daemon",
			wantVersion: "2026.9.1-71",
			wantEdition: version.EditionEnterprise,
			wantNote:    "DAEMON",
		},
		{
			name:        "daemon unreachable: the CLI's, labelled",
			id:          daemonIdentity{Reachable: false, Err: "connection refused"},
			wantSource:  "cli",
			wantVersion: Version,
			wantEdition: version.NormalizeEdition(edition),
			wantNote:    "the CLI's",
		},
		{
			// A daemon too old to report an edition. Borrowing the CLI's would
			// be a mixed answer, and the two CAN be different editions.
			name:        "daemon without an edition field: unknown, not the CLI's",
			id:          daemonIdentity{Version: "2026.8.0", Reachable: true},
			wantSource:  "daemon",
			wantVersion: "2026.8.0",
			wantEdition: "unknown",
			wantNote:    "not an edition",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := resolveProvenance(tc.id)
			if p.VersionSource != tc.wantSource {
				t.Errorf("version_source = %q, want %q", p.VersionSource, tc.wantSource)
			}
			if p.Version != tc.wantVersion {
				t.Errorf("version = %q, want %q", p.Version, tc.wantVersion)
			}
			if p.Edition != tc.wantEdition {
				t.Errorf("edition = %q, want %q", p.Edition, tc.wantEdition)
			}
			if !strings.Contains(p.Note, tc.wantNote) {
				t.Errorf("note %q does not say %q — the label is the point", p.Note, tc.wantNote)
			}
			if p.DaemonReachable != tc.id.Reachable {
				t.Errorf("daemon_reachable = %t, want %t", p.DaemonReachable, tc.id.Reachable)
			}
		})
	}
}

// The local path cannot produce health or metrics: they are the daemon's live
// state. They must be SECTION ERRORS rather than quietly absent — an absent
// section means "not built into this edition" on Community and "broken" on
// Enterprise, and a reader who cannot tell them apart is guessing.
func TestCollectLocalBundle_RecordsTheUnavailableSections(t *testing.T) {
	cfg, cfgPath := sqliteConfigWithData(t, "sk-localsectiontestAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	prov := resolveProvenance(daemonIdentity{Reachable: false, Err: "connection refused"})

	res, err := collectLocalBundleFrom(context.Background(), cfg, cfgPath, testDetector(t),
		supportReportOptions{Task: "task_local_1", MaxSize: supportDefaultMaxSize}, prov)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	for _, section := range []string{"health.json", "metrics.txt"} {
		msg, ok := res.SectionErrs[section]
		if !ok {
			t.Errorf("%s is not in section_errors; it must say WHY it is missing, not just be missing", section)
			continue
		}
		if !strings.Contains(msg, "local collection path") {
			t.Errorf("%s section error does not explain the local path: %q", section, msg)
		}
	}
	// The blackbox trace is Enterprise and reached through the daemon, so on
	// this path it is absent by construction. collection.json is what tells a
	// reader which, so the section itself must simply not be there.
	if _, ok := res.Files["task/blackbox_trace.json"]; ok {
		t.Error("the local path produced a blackbox trace; it has no way to")
	}
	if _, ok := res.Files["version.txt"]; !ok {
		t.Error("version.txt missing — the operator asked for the version to be stated explicitly")
	}
}

// One collector means one redaction path. A secret in the config must not
// survive into the bundle on the local path either.
func TestCollectLocalBundle_RedactsTheConfigSecret(t *testing.T) {
	const secret = "sk-locALrEdactIonTESTbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cfg, cfgPath := sqliteConfigWithData(t, secret)
	prov := resolveProvenance(daemonIdentity{Reachable: false})

	res, err := collectLocalBundleFrom(context.Background(), cfg, cfgPath, testDetector(t),
		supportReportOptions{Task: "task_local_1", MaxSize: supportDefaultMaxSize}, prov)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for name, content := range res.Files {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("the seeded secret survived into %s on the local path", name)
		}
	}
	if snap, ok := res.Files["config.redacted.yaml"]; !ok || len(snap) == 0 {
		t.Error("config.redacted.yaml missing — the config section is one the local path CAN produce")
	}
}

// The fallback: a Community daemon answers 501, and the operator gets a bundle
// rather than the dead end that started this.
func TestExecuteSupportReport_FallsBackToLocalOn501(t *testing.T) {
	const secret = "sk-fallbackTESTccccccccccccccccccccccccccccccccc"
	cfg, cfgPath := sqliteConfigWithData(t, secret)
	restore := stubSupportConfig(t, cfg, cfgPath)
	defer restore()

	outPath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	cmd, buf := supportTestCommand()
	daemon := &fakeDaemon{status: http.StatusNotImplemented,
		bundle: []byte(`{"error":{"code":"EDITION_UNSUPPORTED","message":"support reports require Enterprise Edition"}}`)}

	err := executeSupportReport(cmd, daemon, testDetector(t), fakeHost{},
		supportReportOptions{Task: "task_local_1", Output: outPath, MaxSize: supportDefaultMaxSize, Lines: 10})
	if err != nil {
		t.Fatalf("the 501 was not survivable: %v", err)
	}

	if !strings.Contains(buf.String(), "falling back to LOCAL collection") {
		t.Errorf("the fallback was silent:\n%s", buf.String())
	}
	files := readArchive(t, outPath)
	rec, ok := files["collection.json"]
	if !ok {
		t.Fatalf("collection.json missing; have %v", keysOfStr(files))
	}
	var prov collectionProvenance
	if err := json.Unmarshal([]byte(rec), &prov); err != nil {
		t.Fatalf("collection.json does not decode: %v", err)
	}
	if prov.Path != "local" {
		t.Errorf("collection path = %q, want local", prov.Path)
	}
	if prov.VersionSource != "cli" {
		t.Errorf("version_source = %q; the fake daemon serves no capabilities, so it must be the CLI's", prov.VersionSource)
	}
	for name, content := range files {
		if strings.Contains(content, secret) {
			t.Fatalf("the seeded secret survived into %s", name)
		}
	}
}

// --require-daemon is for a script that would rather fail fast than quietly
// receive a weaker bundle.
func TestExecuteSupportReport_RequireDaemonRefusesTheFallback(t *testing.T) {
	cfg, cfgPath := sqliteConfigWithData(t, "sk-requiredaemonTESTddddddddddddddddddddddddddd")
	restore := stubSupportConfig(t, cfg, cfgPath)
	defer restore()

	cmd, _ := supportTestCommand()
	daemon := &fakeDaemon{status: http.StatusNotImplemented,
		bundle: []byte(`{"error":{"code":"EDITION_UNSUPPORTED","message":"nope"}}`)}

	err := executeSupportReport(cmd, daemon, testDetector(t), fakeHost{},
		supportReportOptions{Task: "task_local_1", Output: filepath.Join(t.TempDir(), "b.tar.gz"),
			MaxSize: supportDefaultMaxSize, Lines: 10, RequireDaemon: true})
	if err == nil {
		t.Fatal("--require-daemon fell back anyway")
	}
	if !strings.Contains(err.Error(), "--require-daemon") {
		t.Errorf("the error does not say why it refused: %v", err)
	}
}

// --local forces the local path even where the daemon would answer — what an
// operator wants when the daemon is misbehaving, which is when a support
// bundle is most often needed.
func TestExecuteSupportReport_LocalSkipsTheDaemonEntirely(t *testing.T) {
	cfg, cfgPath := sqliteConfigWithData(t, "sk-forcelocalTESTeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	restore := stubSupportConfig(t, cfg, cfgPath)
	defer restore()

	outPath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	cmd, _ := supportTestCommand()
	daemon := &fakeDaemon{status: http.StatusOK, bundle: baseBundle(t),
		caps: `{"version":"2026.9.1-71","edition":"enterprise"}`}

	if err := executeSupportReport(cmd, daemon, testDetector(t), fakeHost{},
		supportReportOptions{Task: "task_local_1", Output: outPath, MaxSize: supportDefaultMaxSize,
			Lines: 10, Local: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if daemon.lastBody != nil {
		t.Error("--local still POSTed to the daemon")
	}

	files := readArchive(t, outPath)
	var prov collectionProvenance
	if err := json.Unmarshal([]byte(files["collection.json"]), &prov); err != nil {
		t.Fatalf("collection.json: %v", err)
	}
	// The daemon answered the capabilities probe, so §4.1 says the bundle
	// reports the DAEMON's build even though the collection was local.
	if prov.VersionSource != "daemon" || prov.Version != "2026.9.1-71" {
		t.Errorf("version provenance = %s/%s, want daemon/2026.9.1-71", prov.VersionSource, prov.Version)
	}
	if prov.Edition != version.EditionEnterprise {
		t.Errorf("edition = %q, want the daemon's", prov.Edition)
	}
}

// Which daemon failures the local path may answer. Falling back on ANY error
// would hide a real answer to the operator's request — a rejected window or a
// 500 is information, not an excuse to collect something different.
func TestShouldFallBackLocal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"501 edition gate", &APIError{StatusCode: http.StatusNotImplemented, Code: "EDITION_UNSUPPORTED"}, true},
		{"daemon not running", errWrap("call daemon: dial tcp 127.0.0.1:8080: connection refused"), true},
		{"403 scoped key", &APIError{StatusCode: http.StatusForbidden, Code: "GLOBAL_ADMIN_REQUIRED"}, false},
		{"400 bad window", &APIError{StatusCode: http.StatusBadRequest, Code: "VALIDATION_ERROR"}, false},
		{"500 daemon bug", &APIError{StatusCode: http.StatusInternalServerError}, false},
		{"404 task not found", &APIError{StatusCode: http.StatusNotFound, Code: "NOT_FOUND"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFallBackLocal(tc.err); got != tc.want {
				t.Errorf("shouldFallBackLocal(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// The daemon path records its provenance too, so the two archives are read the
// same way.
func TestCompleteProvenance_DaemonPathTakesTheManifestsVersion(t *testing.T) {
	got := completeProvenance(
		collectionProvenance{Path: "daemon", VersionSource: "daemon", DaemonReachable: true},
		map[string]any{"vornik_version": "2026.9.1-71", "vornik_edition": "enterprise"})
	if got.Version != "2026.9.1-71" || got.Edition != "enterprise" {
		t.Errorf("provenance = %+v; the daemon stated both in its manifest", got)
	}
}

func TestProbeDaemonIdentity_UnreachableIsNotFatal(t *testing.T) {
	id := probeDaemonIdentity(&fakeDaemon{}) // no caps → transport failure
	if id.Reachable {
		t.Error("a refused connection reported the daemon reachable")
	}
	if id.Err == "" {
		t.Error("the reason is dropped; 'connection refused' and '401' are different diagnoses")
	}
}

// stageBundleFiles is a staging writer, and a staging writer that trusts its
// input is how a traversal gets in later.
func TestStageBundleFiles_RefusesEscape(t *testing.T) {
	dir := t.TempDir()
	err := stageBundleFiles(dir, map[string][]byte{"../escaped.txt": []byte("x")})
	if err == nil {
		t.Fatal("a path escaping the staging dir was staged")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); statErr == nil {
		t.Fatal("the file landed outside the staging directory")
	}
}

// ---- helpers ----

// stubSupportConfig points local collection at an explicit config. config.Load
// applies VORNIK_* env overrides, so without this a test would collect from
// whatever database the developer's shell names.
func stubSupportConfig(t *testing.T, cfg *config.Config, path string) func() {
	t.Helper()
	prev := loadSupportConfig
	loadSupportConfig = func() (*config.Config, string, error) { return cfg, path, nil }
	return func() { loadSupportConfig = prev }
}

func supportTestCommand() (*cobra.Command, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetIn(strings.NewReader(""))
	return cmd, buf
}

type wrappedErr struct{ msg string }

func (e wrappedErr) Error() string { return e.msg }

func errWrap(msg string) error { return wrappedErr{msg: msg} }

var _ io.Reader = (*bytes.Buffer)(nil)
