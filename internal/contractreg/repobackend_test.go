package contractreg

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeBackendFixture lays out a miniature module: the repository interfaces
// in internal/persistence, the shared suites in its repotest subdirectory, and
// a backend contract test that CALLS every suite the fixture declares — since
// an uninvoked suite is not coverage. writeBackendFixtureUncalled omits that
// last part, which is what the uninvoked case asserts.
func writeBackendFixture(t *testing.T, suites string) string {
	t.Helper()
	root := writeBackendFixtureUncalled(t, suites)
	writeSuiteCallers(t, root, suites)
	return root
}

// writeSuiteCallers writes a backend contract test invoking each Run*Suite the
// fixture declares.
func writeSuiteCallers(t *testing.T, root, suites string) {
	t.Helper()
	dir := filepath.Join(root, "internal", "persistence", "postgres")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("package postgres\n\nimport (\n\t\"testing\"\n\n\t\"vornik.io/vornik/internal/persistence/repotest\"\n)\n\nfunc TestContracts(t *testing.T) {\n")
	for _, m := range suiteNamePattern.FindAllStringSubmatch(suites, -1) {
		body.WriteString("\trepotest." + m[1] + "(t)\n")
	}
	body.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(dir, "contract_test.go"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

var suiteNamePattern = regexp.MustCompile(`(?m)^func (Run[A-Za-z0-9_]*Suite)\(`)

func writeBackendFixtureUncalled(t *testing.T, suites string) string {
	t.Helper()
	root := t.TempDir()
	pdir := filepath.Join(root, "internal", "persistence")
	rdir := filepath.Join(pdir, "repotest")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "interfaces.go"), []byte(backendIfaces), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rdir, "suites.go"), []byte(suites), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const backendIfaces = `package persistence

import "context"

type WidgetRepository interface{ Get(ctx context.Context, id string) error }
type GadgetRepository interface{ Get(ctx context.Context, id string) error }
type SprocketRepository interface{ Get(ctx context.Context, id string) error }
// Not a repository: the suffix is what the codebase's own naming keys on.
type WidgetPublisher interface{ Publish(ctx context.Context) error }
`

// A repository named by a suite PARAMETER is covered — regardless of what the
// suite is called. Name matching would report this one uncovered.
func TestRepoBackendCoverage_ParameterNamesTheRepository(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Named for what it tests, not for its argument — the shape the real
// RunOpenCheckpointRepairSuite has.
func RunRoundTripSuite(t *testing.T, repo persistence.WidgetRepository) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !audit.Covered["WidgetRepository"] {
		t.Error("a repository taken as a suite parameter must count as covered, whatever the suite is named")
	}
	if audit.Covered["WidgetPublisher"] {
		t.Error("a non-Repository interface must not be tracked")
	}
	for _, iface := range audit.Interfaces {
		if iface.Name == "WidgetPublisher" {
			t.Error("WidgetPublisher was collected as a repository interface")
		}
	}
}

// One suite covering several repositories covers all of them — the
// RunStepPromptSuite(prompts, outcomes) shape.
func TestRepoBackendCoverage_MultiParameterSuite(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func RunPairSuite(t *testing.T, a persistence.WidgetRepository, b persistence.GadgetRepository) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, want := range []string{"WidgetRepository", "GadgetRepository"} {
		if !audit.Covered[want] {
			t.Errorf("%s must be covered: the suite takes it", want)
		}
	}
}

// The gate itself: an uncovered, unlisted repository fails, and the message
// tells the reader what to do about it.
func TestRepoBackendCoverage_UncoveredAndUnlistedFails(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func RunWidgetSuite(t *testing.T, repo persistence.WidgetRepository) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	allow := map[string]RepoBackendAllowEntry{
		"GadgetRepository": {Name: "GadgetRepository", Reason: "postgres-only"},
	}
	findings := CheckRepoBackendCoverage(audit, allow)
	if len(findings) != 1 {
		t.Fatalf("want exactly one finding (SprocketRepository), got %d: %v", len(findings), findings)
	}
	if findings[0].Name != "SprocketRepository" {
		t.Errorf("finding names %q, want SprocketRepository", findings[0].Name)
	}
	if !strings.Contains(findings[0].Detail, "repo_backend_allowlist.txt") {
		t.Errorf("the finding must say how to resolve it: %q", findings[0].Detail)
	}
	if len(findings[0].Sources) == 0 {
		t.Error("the finding must locate the interface declaration")
	}
}

// The property that keeps the list moving: an entry whose repository NOW has a
// suite is itself a failure, so closing a gap must delete its line.
func TestRepoBackendCoverage_StaleEntryFails(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func RunAllSuite(t *testing.T, a persistence.WidgetRepository, b persistence.GadgetRepository, c persistence.SprocketRepository) {
}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	findings := CheckRepoBackendCoverage(audit, map[string]RepoBackendAllowEntry{
		"GadgetRepository": {Name: "GadgetRepository", Reason: "dual-backend debt"},
	})
	if len(findings) != 1 || findings[0].Name != "GadgetRepository" {
		t.Fatalf("a covered repository still on the allowlist must fail: %v", findings)
	}
	if !strings.Contains(findings[0].Detail, "delete the line") {
		t.Errorf("the finding must say to delete the entry: %q", findings[0].Detail)
	}
}

// A slot left behind is one the next repository falls into silently.
func TestRepoBackendCoverage_EntryForAVanishedRepositoryFails(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func RunAllSuite(t *testing.T, a persistence.WidgetRepository, b persistence.GadgetRepository, c persistence.SprocketRepository) {
}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	findings := CheckRepoBackendCoverage(audit, map[string]RepoBackendAllowEntry{
		"DeletedRepository": {Name: "DeletedRepository", Reason: "postgres-only"},
	})
	if len(findings) != 1 || findings[0].Name != "DeletedRepository" {
		t.Fatalf("an entry for a repository that no longer exists must fail: %v", findings)
	}
}

// A suite that exists and is never CALLED is not coverage. Writing one,
// deleting the allowlist line and never wiring it up would turn the gate green
// while nothing runs — the one way to satisfy this check cosmetically.
func TestRepoBackendCoverage_UninvokedSuiteIsNotCoverage(t *testing.T) {
	suites := `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func RunWidgetSuite(t *testing.T, repo persistence.WidgetRepository) {}
`
	root := writeBackendFixtureUncalled(t, suites)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if audit.Covered["WidgetRepository"] {
		t.Error("a suite nobody calls counted as coverage")
	}
	for _, s := range audit.Suites {
		if s.Name == "RunWidgetSuite" && s.Invoked {
			t.Error("RunWidgetSuite reported as invoked with no caller in the fixture")
		}
	}

	// The same fixture WITH a caller covers it — so the difference is the
	// call, not the fixture.
	called := writeBackendFixture(t, suites)
	audit2, err := AuditRepoBackendContracts(called)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !audit2.Covered["WidgetRepository"] {
		t.Error("a suite that IS called must count as coverage")
	}
}

// The package's local name is RESOLVED, not assumed. Assuming the identifier
// is literally "persistence" made an aliased suite read as covering nothing —
// which fails loudly for an unlisted repository, but for an ALLOWLISTED one
// leaves the entry looking stale-free while a suite exists, so the cleanup
// prompt never fires (review-20260904-0af0, finding 2).
func TestRepoBackendCoverage_ResolvesTheImportAlias(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	p "vornik.io/vornik/internal/persistence"
)

func RunAliasedSuite(t *testing.T, repo p.WidgetRepository) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !audit.Covered["WidgetRepository"] {
		t.Error("an aliased import must still count: the alias is a local name, not a different package")
	}
}

// A dot-import puts the type in scope unqualified.
func TestRepoBackendCoverage_ResolvesTheDotImport(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	. "vornik.io/vornik/internal/persistence"
)

func RunDotSuite(t *testing.T, repo WidgetRepository) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !audit.Covered["WidgetRepository"] {
		t.Error("a dot-imported repository parameter must count")
	}
}

// A variadic parameter is still a parameter.
func TestRepoBackendCoverage_VariadicParameterCounts(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func RunManySuite(t *testing.T, repos ...persistence.WidgetRepository) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !audit.Covered["WidgetRepository"] {
		t.Error("a variadic repository parameter must count")
	}
}

// A repository reached through a struct is NOT counted — the safe direction,
// and the documented blind spot. Asserted so the behaviour is deliberate
// rather than incidental.
func TestRepoBackendCoverage_RepositoryInsideAStructIsNotCounted(t *testing.T) {
	root := writeBackendFixture(t, `package repotest

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type Bundle struct{ Widgets persistence.WidgetRepository }

func RunBundleSuite(t *testing.T, b Bundle) {}
`)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if audit.Covered["WidgetRepository"] {
		t.Error("a repository inside a struct must read as uncovered — the fix is to widen the signature")
	}
}

// An exemption nobody explained is what the file exists to prevent.
func TestParseRepoBackendAllowlist_RequiresAReason(t *testing.T) {
	if _, err := ParseRepoBackendAllowlist("WidgetRepository\n"); err == nil {
		t.Fatal("a line with no reason must be rejected")
	}
	if _, err := ParseRepoBackendAllowlist("WidgetRepository  #   \n"); err == nil {
		t.Fatal("a blank reason must be rejected")
	}
	got, err := ParseRepoBackendAllowlist("# a comment\n\nWidgetRepository  # postgres-only: no SQLite impl\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got["WidgetRepository"].Reason != "postgres-only: no SQLite impl" {
		t.Errorf("parsed = %+v", got)
	}
	if _, err := ParseRepoBackendAllowlist("A  # x\nA  # y\n"); err == nil {
		t.Fatal("a duplicated entry must be rejected")
	}
}

// The gate must be GREEN on this repository as shipped: the allowlist is
// seeded from the audit's own output, so any drift here is real drift and not
// a fixture that was never true.
func TestRepoBackendCoverage_ThisRepositoryIsGreen(t *testing.T) {
	root := repoRootForTest(t)
	audit, err := AuditRepoBackendContracts(root)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit.Interfaces) < 50 {
		t.Fatalf("audit found only %d repository interfaces — it is not reading the real tree", len(audit.Interfaces))
	}
	data, err := os.ReadFile(filepath.Join(root, "cmd", "lint-lld-contracts", "repo_backend_allowlist.txt"))
	if os.IsNotExist(err) {
		// The Community export prunes cmd/lint-lld-contracts (Enterprise
		// tooling); the coverage gate runs in the Enterprise tree, where the
		// allowlist lives. Found 2026-09-05 when the export's CI first ran
		// this test.
		t.Skip("repo backend allowlist not in this tree (Community export)")
	}
	if err != nil {
		t.Fatalf("read allowlist: %v", err)
	}
	allow, err := ParseRepoBackendAllowlist(string(data))
	if err != nil {
		t.Fatalf("parse allowlist: %v", err)
	}
	if findings := CheckRepoBackendCoverage(audit, allow); len(findings) != 0 {
		for _, f := range findings {
			t.Errorf("%s: %s", f.Name, f.Detail)
		}
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
