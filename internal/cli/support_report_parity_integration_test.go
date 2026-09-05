//go:build integration

package cli

// STRUCTURAL PARITY between the bundle's two drivers.
//
// The support-bundle-in-CE design's one rule is that there is ONE collector:
// "a safety check with two implementations has one that is wrong", and the
// thing that would be duplicated here is the REDACTION path. Phase 1 made the
// collector shared; this test is what keeps it shared, because a shared
// collector can still be driven into producing different bundles by two
// different wirings — a repo one driver forgets, a config snapshot rendered
// twice, a window parsed differently.
//
// So it does not spot-check a section. It seeds ONE fixture, runs BOTH drivers
// against the SAME database, and asserts:
//
//  1. every section present in both is byte-identical;
//  2. section_errors differ only by the expected unavailable set;
//  3. the redaction tallies match exactly, per type and per file;
//  4. version provenance follows §4.1.
//
// It imports internal/api, which production CLI code deliberately does not.
// That is a test-only edge: it is the only place both drivers exist, and a
// test import puts no HTTP server in the vornikctl binary.
//
// Run: see db_integration_harness_cov_test.go for the POSTGRES_* env.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/supportbundle"
	"vornik.io/vornik/internal/version"
)

// paritySecret is threaded through EVERY carrier the bundle has — the config,
// a database row, and a text artifact — by ONE fixture, so the redaction-parity
// assertion and the structural one cannot drift into testing different things.
const paritySecret = "sk-PARITYfixtureSECRET00000000000000000000000000000"

const parityTaskID = "task_parity_fixture"

// The sections the local path CANNOT produce. Everything else must match.
var parityExpectedLocalOnlyErrors = []string{"health.json", "metrics.txt"}

// Sections whose bytes legitimately differ between the drivers, with the
// reason. Anything not listed here must be identical — the list is the
// specification of what "parity" excludes, so it is short on purpose.
var parityDivergentSections = map[string]string{
	"MANIFEST.json":   "carries generated_at and the section_errors that differ by construction",
	"REDACTION.txt":   "renders the per-file tally, which includes the sections that differ",
	"health.json":     "daemon-only live state",
	"metrics.txt":     "daemon-only live state",
	"collection.json": "written by the CLI on both paths, describing the path itself",
}

func TestSupportBundle_StructuralParityBetweenDrivers(t *testing.T) {
	db := dbcovSetup(t)
	dbcovResetFlags()
	ctx := context.Background()

	repos := storage.Build(db)
	seedParityFixture(t, ctx, repos)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	cfg := &config.Config{}
	cfg.Database = dbcovDBConfig(t)
	cfg.Telegram = config.TelegramConfig{Enabled: true, BotToken: paritySecret}
	registryDir := parityRegistryDir(t)

	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatalf("detector: %v", err)
	}

	const buildVersion = "2026.9.1-parity"
	opts := supportReportOptions{Task: parityTaskID, MaxSize: supportDefaultMaxSize}

	// --- driver A: the CLI, locally ---
	prov := resolveProvenance(daemonIdentity{
		Version: buildVersion, Edition: version.EditionEnterprise, Reachable: true,
	})
	// Both drivers get the SAME doctor. In production they differ by design —
	// the local one is the offline doctor and says so (§4) — but a doctor
	// report carrying a timestamp and this host's journal would drown the
	// comparison in differences that are not about the collector. The local
	// doctor's own content is asserted in
	// TestSupportBundle_LocalDoctorIsTheOfflineOne below.
	fixedDoctor := parityFixedDoctor{}
	localRes, err := collectLocalBundleFromWithDoctor(ctx, cfg, cfgPath, det, opts, prov, fixedDoctor)
	if err != nil {
		t.Fatalf("local collection: %v", err)
	}

	// --- driver B: the daemon, over its endpoint ---
	daemonFiles := collectViaDaemon(t, cfg, repos, registryDir, det, buildVersion, fixedDoctor)

	// (1) every section present in both is byte-identical.
	for name, want := range daemonFiles {
		if why, skip := parityDivergentSections[name]; skip {
			t.Logf("skipping %s: %s", name, why)
			continue
		}
		got, ok := localRes.Files[name]
		if !ok {
			t.Errorf("section %s is in the daemon bundle but not the local one — a repo the local driver does not wire", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("section %s differs between drivers\nlocal:  %q\ndaemon: %q", name, truncateForDiff(got), truncateForDiff(want))
		}
	}
	for name := range localRes.Files {
		if _, ok := daemonFiles[name]; ok {
			continue
		}
		if _, expected := parityDivergentSections[name]; expected {
			continue
		}
		t.Errorf("section %s is in the local bundle but not the daemon one", name)
	}

	// (2) section_errors differ only by the expected unavailable set.
	daemonManifest := parseParityManifest(t, daemonFiles["MANIFEST.json"])
	for _, section := range parityExpectedLocalOnlyErrors {
		if _, ok := localRes.SectionErrs[section]; !ok {
			t.Errorf("%s must be a SECTION ERROR on the local path, not silently absent", section)
		}
		if _, ok := daemonManifest.SectionErrors[section]; ok {
			t.Errorf("%s errored on the DAEMON path too; the fixture is not exercising what it claims", section)
		}
	}
	for section := range localRes.SectionErrs {
		if parityListed(parityExpectedLocalOnlyErrors, section) {
			continue
		}
		if _, ok := daemonManifest.SectionErrors[section]; !ok {
			t.Errorf("local-only section error %q: %s", section, localRes.SectionErrs[section])
		}
	}

	// (3) the redaction tallies match exactly, per type and per file.
	assertTallyParity(t, localRes, daemonManifest)

	// (4) version provenance — §4.1 with the daemon reachable.
	if prov.VersionSource != "daemon" || prov.Version != buildVersion {
		t.Errorf("provenance = %s/%s, want daemon/%s", prov.VersionSource, prov.Version, buildVersion)
	}
	if got := string(localRes.Files["version.txt"]); !strings.Contains(got, buildVersion) {
		t.Errorf("local version.txt does not carry the daemon's version: %q", got)
	}

	// And the secret is gone from BOTH, which is the assertion that makes "one
	// collector" mean something.
	for name, content := range localRes.Files {
		if bytes.Contains(content, []byte(paritySecret)) {
			t.Fatalf("the secret survived the LOCAL path in %s", name)
		}
	}
	for name, content := range daemonFiles {
		if bytes.Contains(content, []byte(paritySecret)) {
			t.Fatalf("the secret survived the DAEMON path in %s", name)
		}
	}
}

// §4.1's other half: with the daemon unreachable the bundle reports the CLI's
// build and says so, rather than silently presenting it as the deployment's.
func TestSupportBundle_LocalProvenanceWhenDaemonIsDown(t *testing.T) {
	db := dbcovSetup(t)
	dbcovResetFlags()
	ctx := context.Background()
	seedParityFixture(t, ctx, storage.Build(db))

	cfgDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Database = dbcovDBConfig(t)
	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatalf("detector: %v", err)
	}

	prov := resolveProvenance(daemonIdentity{Reachable: false, Err: "connection refused"})
	res, err := collectLocalBundleFrom(ctx, cfg, filepath.Join(cfgDir, "config.yaml"), det,
		supportReportOptions{Task: parityTaskID, MaxSize: supportDefaultMaxSize}, prov)
	if err != nil {
		t.Fatalf("local collection: %v", err)
	}
	if prov.VersionSource != "cli" {
		t.Errorf("version_source = %q, want cli", prov.VersionSource)
	}
	if got := string(res.Files["version.txt"]); !strings.Contains(got, Version) {
		t.Errorf("version.txt = %q, want the CLI's version %q", got, Version)
	}
	// The task's rows are still there: an unreachable daemon costs the live
	// sections, not the evidence.
	if _, ok := res.Files["task/task.json"]; !ok {
		t.Errorf("task.json missing with the daemon down; have %v", sortedNames(res.Files))
	}
}

// ---- fixture ----

func seedParityFixture(t *testing.T, ctx context.Context, repos *storage.Repositories) {
	t.Helper()
	now := time.Now().UTC()
	wf := "parity-workflow"

	// A task carrying the secret in its payload — the database-row carrier.
	task := &persistence.Task{
		ID:         parityTaskID,
		ProjectID:  "parity-project",
		WorkflowID: &wf,
		Status:     persistence.TaskStatusCompleted,
		Payload:    []byte(fmt.Sprintf(`{"brief":"deploy with %s"}`, paritySecret)),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := repos.Tasks.Create(ctx, task); err != nil {
		// A rerun against the same database finds the row already there, which
		// is fine: the assertions are about shape, not about being first.
		t.Logf("seed task (may already exist): %v", err)
	}

	exec := &persistence.Execution{
		ID:         "exec_parity_1",
		TaskID:     parityTaskID,
		ProjectID:  "parity-project",
		WorkflowID: wf,
		Status:     persistence.ExecutionStatusCompleted,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := repos.Executions.Create(ctx, exec); err != nil {
		t.Logf("seed execution (may already exist): %v", err)
	}

	if repos.Messages != nil {
		msg := &persistence.TaskMessage{
			ID:          "msg_parity_1",
			TaskID:      parityTaskID,
			AuthorKind:  persistence.TaskMessageAuthorOperator,
			MessageKind: persistence.TaskMessageKindMessage,
			Content:     "the key is " + paritySecret,
			CreatedAt:   now,
		}
		if err := repos.Messages.Insert(ctx, msg); err != nil {
			t.Logf("seed message (may already exist): %v", err)
		}
	}
}

// parityRegistryDir gives BOTH drivers the same deployed registry — the REPO's
// configs/, which carries real workflow prompts, so the registry sections being
// compared are not two empty lists agreeing with each other.
//
// It pins VORNIK_CONFIGS_DIR because that is what resolveConfigsDir reads
// first, and on a developer's host it points at the operator's live
// deployment: without this the local driver would collect the real registry
// while the daemon driver collected the fixture, and the test would report a
// difference that is the test's own fault.
func parityRegistryDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "configs"))
	if err != nil {
		t.Fatalf("resolve configs dir: %v", err)
	}
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Skipf("repo configs/ has no %s dir, skipping the registry half of parity: %v", sub, err)
		}
	}
	t.Setenv("VORNIK_CONFIGS_DIR", dir)
	return dir
}

// collectViaDaemon drives the endpoint — the daemon driver's real entry point,
// including its own wiring of the same repositories.
func collectViaDaemon(t *testing.T, cfg *config.Config, repos *storage.Repositories,
	registryDir string, det secrets.Detector, buildVersion string, doctor supportbundle.DoctorRunner) map[string][]byte {
	t.Helper()

	reg := registry.New()
	_ = reg.Load(registryDir)

	s := api.NewServer(
		api.WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-parity-admin"}}),
		api.WithConfig(cfg),
		api.WithSecrets(det, secrets.DefaultCheckpoints()),
		api.WithBuildVersionFunc(func() string { return buildVersion }),
		api.WithProjectRegistry(reg),
		api.WithTaskRepository(repos.Tasks),
		api.WithExecutionRepository(repos.Executions),
		api.WithExecutionStepOutcomeRepository(repos.StepOutcomes),
		api.WithToolAuditRepository(repos.ToolAudit),
		api.WithLLMUsageRepository(repos.LLMUsage),
		api.WithTaskMessageRepository(repos.Messages),
		api.WithAdminAuditRepository(repos.AdminAudit),
		api.WithArtifactRepository(repos.Artifacts),
		api.WithWebhookEventRepository(repos.Webhooks),
	)
	s.SetSupportReportCollectors(doctor, nil, nil, repos.JudgeVerdicts, repos.PostMortems)

	body := fmt.Sprintf(`{"task_id":%q}`, parityTaskID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(body))
	rec := httptest.NewRecorder()
	// Through the REAL auth middleware rather than a hand-built context: what
	// is being compared is the daemon driver as it actually runs, gate
	// included. Auth is off here, which is what requireAdminGate reads to admit
	// the request — the gate itself is asserted by internal/api's own tests.
	handler := api.AuthMiddleware(api.AuthConfig{Enabled: false})(http.HandlerFunc(s.SupportReport))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("daemon driver returned %d: %s", rec.Code, rec.Body.String())
	}
	return untarBundle(t, rec.Body.Bytes())
}

// parityFixedDoctor is a deterministic doctor: the same bytes on both drivers,
// so doctor.json can join the byte comparison instead of being excluded.
type parityFixedDoctor struct{}

func (parityFixedDoctor) Run(context.Context) (any, error) {
	return map[string]string{"summary": "fixed for parity"}, nil
}

// The real local doctor IS the offline one, and the bundle must say so — a
// reader comparing a local doctor.json with a daemon one otherwise cannot tell
// a weaker report from a failing deployment.
func TestSupportBundle_LocalDoctorIsTheOfflineOne(t *testing.T) {
	db := dbcovSetup(t)
	dbcovResetFlags()
	seedParityFixture(t, context.Background(), storage.Build(db))

	cfg := &config.Config{}
	cfg.Database = dbcovDBConfig(t)
	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatalf("detector: %v", err)
	}
	res, err := collectLocalBundleFrom(context.Background(), cfg, filepath.Join(t.TempDir(), "config.yaml"), det,
		supportReportOptions{Task: parityTaskID, MaxSize: supportDefaultMaxSize},
		resolveProvenance(daemonIdentity{Reachable: false}))
	if err != nil {
		t.Fatalf("local collection: %v", err)
	}
	doctor := string(res.Files["doctor.json"])
	if !strings.Contains(doctor, "offline") {
		t.Errorf("doctor.json does not identify itself as the offline report: %s", truncateForDiff([]byte(doctor)))
	}
	if !strings.Contains(doctor, "STATIC checks only") {
		t.Errorf("doctor.json does not state that it is weaker than the daemon's: %s", truncateForDiff([]byte(doctor)))
	}
}

// ---- assertions + helpers ----

func assertTallyParity(t *testing.T, local *supportbundle.Result, daemonManifest parityManifest) {
	t.Helper()

	// The per-type comparison is only meaningful if the sections that
	// legitimately differ carried no redactions — otherwise a difference in
	// THEM would read as a difference in the redaction path. Assert that
	// precondition rather than compensating for it silently.
	for file := range parityDivergentSections {
		if n := local.Tally.PerFile[file]; n > 0 {
			t.Errorf("%s carried %d redaction(s) on the local path; it is excluded from the byte comparison, "+
				"so its redactions make the per-type tally incomparable. Narrow the exclusion or move the secret.", file, n)
		}
	}

	for typ, want := range daemonManifest.RedactionByType {
		if got := local.Tally.ByType[typ]; got != want {
			t.Errorf("redaction tally for %q: local %d, daemon %d — the two drivers redact differently, "+
				"which is the failure this design exists to prevent", typ, got, want)
		}
	}
	for typ, got := range local.Tally.ByType {
		if _, ok := daemonManifest.RedactionByType[typ]; !ok && got > 0 {
			t.Errorf("local driver reported redaction type %q (%d) the daemon did not", typ, got)
		}
	}
	if local.Tally.Total != daemonManifest.RedactionTotal {
		t.Errorf("total redactions: local %d, daemon %d", local.Tally.Total, daemonManifest.RedactionTotal)
	}
}

type parityManifest struct {
	SectionErrors   map[string]string `json:"section_errors"`
	RedactionByType map[string]int    `json:"redaction_by_type"`
	RedactionTotal  int               `json:"redaction_total"`
	VornikVersion   string            `json:"vornik_version"`
	VornikEdition   string            `json:"vornik_edition"`
}

func parseParityManifest(t *testing.T, raw []byte) parityManifest {
	t.Helper()
	var m parityManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("MANIFEST.json: %v", err)
	}
	if m.SectionErrors == nil {
		m.SectionErrors = map[string]string{}
	}
	if m.RedactionByType == nil {
		m.RedactionByType = map[string]int{}
	}
	return m
}

func untarBundle(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")] = data
	}
	return out
}

func truncateForDiff(b []byte) string {
	const max = 400
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

func parityListed(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedNames(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for k := range files {
		out = append(out, k)
	}
	return out
}
