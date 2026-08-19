package contractreg

import (
	"go/ast"
	"go/parser"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guard behind the 2026-08-19 P0: a hand-written repository double that
// disagrees with production about what an absent row looks like does not
// merely miss a bug, it certifies the broken path as covered. A build-time
// check is the only thing that scales to the 255 doubles in this module.

// writeModule lays out a throwaway tree so each case states exactly the
// source it depends on, rather than pinning real files that will drift.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const ifaceSrc = `package persistence

type ExtractedDocument struct{ ID string }

type ExtractedDocumentRepository interface {
	Get(ctx context.Context, id string) (*ExtractedDocument, error)
	GetByArtifact(ctx context.Context, artifactID string) (*ExtractedDocument, error)
}
`

func TestAuditRepoDoubles_findsATestTypeImplementingARegisteredLookup(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/handler_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatalf("AuditRepoDoubles: %v", err)
	}
	if len(audit.Doubles) != 1 {
		t.Fatalf("want 1 double, got %d: %+v", len(audit.Doubles), audit.Doubles)
	}
	d := audit.Doubles[0]
	if d.Type != "fakeDocRepo" || d.Method != "Get" {
		t.Errorf("misidentified double: %+v", d)
	}
	if d.Key != "ExtractedDocumentRepository.Get" {
		t.Errorf("Key = %q, want ExtractedDocumentRepository.Get", d.Key)
	}
}

// A double is only a finding if its package never asserts the contract. The
// assertion is what proves the double was checked against production.
func TestCheckRepoDoubleConformance_flagsADoubleWithNoAssertion(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/handler_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`,
	})
	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}

	findings := CheckRepoDoubleConformance(audit, nil)

	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0].String(), "fakeDocRepo") {
		t.Errorf("finding does not name the double: %s", findings[0])
	}
}

func TestCheckRepoDoubleConformance_acceptsADoubleWhosePackageAsserts(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/handler_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, persistence.ErrNotFound
}

func TestConforms(t *testing.T) {
	repotest.AssertMiss(t, "ExtractedDocumentRepository.Get", func() (*persistence.ExtractedDocument, error) {
		return (&fakeDocRepo{}).Get(nil, "absent")
	})
}
`,
	})
	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}

	if findings := CheckRepoDoubleConformance(audit, nil); len(findings) != 0 {
		t.Fatalf("an asserted double was flagged: %v", findings)
	}
}

func TestCheckRepoDoubleConformance_allowlistSuppressesAKnownOffender(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/handler_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`,
	})
	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{"internal/app:fakeDocRepo.Get": true}

	if findings := CheckRepoDoubleConformance(audit, allow); len(findings) != 0 {
		t.Fatalf("an allowlisted double was flagged: %v", findings)
	}
}

// The allowlist is shrink-only. An entry that no longer names an offender is
// itself a failure, so a cleanup cannot leave the list to rot and a deleted
// double cannot silently reserve a slot for a new one.
func TestCheckRepoDoubleConformance_flagsAStaleAllowlistEntry(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/handler_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, persistence.ErrNotFound
}

func TestConforms(t *testing.T) {
	repotest.AssertMiss(t, "ExtractedDocumentRepository.Get", func() (*persistence.ExtractedDocument, error) {
		return (&fakeDocRepo{}).Get(nil, "absent")
	})
}
`,
	})
	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{"internal/app:fakeDocRepo.Get": true}

	findings := CheckRepoDoubleConformance(audit, allow)

	if len(findings) != 1 {
		t.Fatalf("want 1 stale-entry finding, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(strings.ToLower(findings[0].String()), "stale") {
		t.Errorf("finding does not say the entry is stale: %s", findings[0])
	}
}

// Non-test code is not a double. Only _test.go declarations are candidates —
// production implementations are covered by the shared repotest suites.
func TestAuditRepoDoubles_ignoresProductionImplementations(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/persistence/sqlite/repo.go": `package sqlite

type ExtractedDocumentRepository struct{}

func (r *ExtractedDocumentRepository) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Doubles) != 0 {
		t.Fatalf("a production implementation was treated as a double: %+v", audit.Doubles)
	}
}

// CheckRepoLookupRegistration keeps misscontract.Contract honest as the
// interfaces grow: a new single-entity lookup must be registered or
// explicitly excluded, never merely absent.
func TestCheckRepoLookupRegistration_flagsAnUnregisteredLookup(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": `package persistence

type Widget struct{ ID string }

type WidgetRepository interface {
	Get(ctx context.Context, id string) (*Widget, error)
}
`,
	})

	findings, err := CheckRepoLookupRegistration(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding for the unregistered lookup, got %d: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0].String(), "WidgetRepository.Get") {
		t.Errorf("finding does not name the lookup: %s", findings[0])
	}
}

// Run against the real module, every lookup must already be accounted for.
func TestCheckRepoLookupRegistration_realModuleIsFullyRegistered(t *testing.T) {
	findings, err := CheckRepoLookupRegistration(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("unregistered single-entity lookup: %s", f)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

// A generic double — fakeRepo[T] — is still a double. The receiver's type
// name has to survive the type-parameter wrapper or the guard would have a
// silent blind spot that any offender could adopt.
func TestAuditRepoDoubles_seesThroughAGenericReceiver(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/generic_test.go": `package app

type fakeRepo[T any] struct{}

func (f *fakeRepo[T]) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Doubles) != 1 || audit.Doubles[0].Type != "fakeRepo" {
		t.Fatalf("generic receiver not resolved: %+v", audit.Doubles)
	}
}

// An assertion reached through a bare identifier (dot-import, or a call from
// inside repotest itself) counts the same as a qualified one.
func TestAuditRepoDoubles_countsAnUnqualifiedAssertion(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/bare_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, persistence.ErrNotFound
}

func TestBare(t *testing.T) {
	AssertMissRepo(t, "ExtractedDocumentRepository.Get", (&fakeDocRepo{}).Get)
}
`,
	})
	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}

	if findings := CheckRepoDoubleConformance(audit, nil); len(findings) != 0 {
		t.Fatalf("an unqualified assertion was not counted: %v", findings)
	}
}

// This repo keeps whole exported copies of itself under .vornik-export and
// .vornik-public-clone. Scanning those would triple every count and put paths
// in the allowlist that no commit can change.
func TestAuditRepoDoubles_skipsDottedDirectories(t *testing.T) {
	double := `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go":                ifaceSrc,
		"internal/app/handler_test.go":                 double,
		".vornik-export/internal/app/handler_test.go":  double,
		"vendor/other/internal/app/handler_test.go":    double,
		"internal/app/testdata/internal/app/x_test.go": double,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Doubles) != 1 {
		t.Fatalf("want 1 double from the real tree, got %d: %+v", len(audit.Doubles), audit.Doubles)
	}
}

// A method that merely shares a name with a lookup is not a double. Matching
// on the returned entity type is what keeps the guard from crying wolf over
// every Get in the module.
func TestAuditRepoDoubles_ignoresAMethodReturningSomethingElse(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/other_test.go": `package app

type cache struct{}

func (c *cache) Get(ctx context.Context, id string) (*Session, error) { return nil, nil }

func (c *cache) Lookup(id string) *persistence.ExtractedDocument { return nil }
`,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Doubles) != 0 {
		t.Fatalf("unrelated methods were flagged: %+v", audit.Doubles)
	}
}

// A tree with no persistence package yields an empty index rather than an
// error, so the guard cannot break a module layout it does not recognise.
func TestAuditRepoDoubles_toleratesAMissingPersistencePackage(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/app/handler_test.go": `package app

type fakeDocRepo struct{}

func (f *fakeDocRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
`,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatalf("a module with no persistence package errored: %v", err)
	}
	if len(audit.Doubles) != 0 {
		t.Fatalf("doubles matched with no registry to match against: %+v", audit.Doubles)
	}
}

// An unparseable file is skipped rather than failing the lint: a file the
// compiler rejects is not a place a double can hide either.
func TestAuditRepoDoubles_skipsAnUnparseableFile(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/broken_test.go":   "package app\n\nthis is not go\n",
	})

	if _, err := AuditRepoDoubles(root); err != nil {
		t.Fatalf("an unparseable file failed the scan: %v", err)
	}
}

// The AST helpers carry the guard's blind spots: every shape they fail to
// recognise is a place an offending double could sit unnoticed, so each
// branch is pinned directly rather than only through a whole-module scan.

func parseExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return e
}

func TestBaseTypeName_unwrapsEveryReceiverShape(t *testing.T) {
	cases := map[string]string{
		"fakeRepo":            "fakeRepo",
		"*fakeRepo":           "fakeRepo",
		"persistence.Doc":     "Doc",
		"*persistence.Doc":    "Doc",
		"fakeRepo[T]":         "fakeRepo",
		"*fakeRepo[K, V]":     "fakeRepo",
		"map[string]struct{}": "", // not a named type: no double to name
	}
	for src, want := range cases {
		if got := baseTypeName(parseExpr(t, src)); got != want {
			t.Errorf("baseTypeName(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestIsAssertMissCall_recognisesEveryCallShape(t *testing.T) {
	yes := []string{
		"repotest.AssertMiss",
		"repotest.AssertMissRepo",
		"AssertMiss",
		"AssertMissRepo",
		"AssertMiss[persistence.Doc]",
		"repotest.AssertMissRepo[persistence.Doc]",
	}
	for _, src := range yes {
		if !isAssertMissCall(parseExpr(t, src)) {
			t.Errorf("isAssertMissCall(%q) = false, want true", src)
		}
	}
	no := []string{"assert.Nil", "require.NoError", "t.Fatalf", "foo()"}
	for _, src := range no {
		if isAssertMissCall(parseExpr(t, src)) {
			t.Errorf("isAssertMissCall(%q) = true, want false", src)
		}
	}
}

func TestReceiverTypeName_handlesAFunctionWithNoReceiver(t *testing.T) {
	if got := receiverTypeName(&ast.FuncDecl{Name: ast.NewIdent("Helper")}); got != "" {
		t.Errorf("receiverTypeName(plain func) = %q, want empty", got)
	}
	if got := receiverTypeName(&ast.FuncDecl{Recv: &ast.FieldList{}}); got != "" {
		t.Errorf("receiverTypeName(empty receiver list) = %q, want empty", got)
	}
}

func TestSingleEntityReturn_rejectsShapesThatAreNotLookups(t *testing.T) {
	cases := []string{
		"func()",                     // no results
		"func() error",               // one result
		"func() (Doc, error)",        // value, not pointer
		"func() (*Doc, bool)",        // second result is not an error
		"func() (*Doc, error, bool)", // three results
		"func() ([]*Doc, error)",     // a list: an empty result is not a miss
	}
	for _, src := range cases {
		ft, ok := parseExpr(t, src).(*ast.FuncType)
		if !ok {
			t.Fatalf("%q did not parse as a func type", src)
		}
		if _, isLookup := singleEntityReturn(ft); isLookup {
			t.Errorf("singleEntityReturn(%q) = true, want false", src)
		}
	}
	ft := parseExpr(t, "func(ctx context.Context, id string) (*persistence.Doc, error)").(*ast.FuncType)
	got, ok := singleEntityReturn(ft)
	if !ok || got != "Doc" {
		t.Errorf("singleEntityReturn(lookup) = (%q, %v), want (Doc, true)", got, ok)
	}
}

func TestCheckRepoLookupRegistration_toleratesAMissingPersistencePackage(t *testing.T) {
	findings, err := CheckRepoLookupRegistration(writeModule(t, map[string]string{
		"internal/app/app.go": "package app\n",
	}))
	if err != nil {
		t.Fatalf("a module with no persistence package errored: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings from an empty registry: %v", findings)
	}
}

// An excluded lookup is registered-by-decision and must not be reported.
func TestCheckRepoLookupRegistration_acceptsAnExcludedLookup(t *testing.T) {
	findings, err := CheckRepoLookupRegistration(writeModule(t, map[string]string{
		"internal/persistence/iface.go": `package persistence

type Task struct{ ID string }

type TaskRepository interface {
	LeaseTask(ctx context.Context, worker string) (*Task, error)
}
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("an excluded lookup was reported: %v", findings)
	}
}

// The mirror of the shipped defect: a double LOOSER than production. Where a
// stricter double hides the caller's real branch, a looser one can swallow a
// genuine ErrNotFound and let a broken caller pass. Flagging it is what makes
// the allowlist shrinkable by risk rather than alphabetically.
func TestAuditRepoDoubles_marksAPermissiveDouble(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/loose_test.go": `package app

type looseRepo struct{}

func (l *looseRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}

type strictRepo struct{}

func (s *strictRepo) GetByArtifact(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	return nil, persistence.ErrNotFound
}
`,
	})

	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]RepoDouble{}
	for _, d := range audit.Doubles {
		byType[d.Type] = d
	}
	if !byType["looseRepo"].Permissive {
		t.Error("a double returning (nil, nil) was not marked permissive")
	}
	if byType["strictRepo"].Permissive {
		t.Error("a conforming double was marked permissive")
	}
}

// A (nil, nil) inside a closure belongs to the closure, not to the method.
func TestReturnsNilNil_ignoresAClosuresOwnReturn(t *testing.T) {
	root := writeModule(t, map[string]string{
		"internal/persistence/iface.go": ifaceSrc,
		"internal/app/closure_test.go": `package app

type closureRepo struct{}

func (c *closureRepo) Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error) {
	load := func() (*persistence.ExtractedDocument, error) { return nil, nil }
	_ = load
	return nil, persistence.ErrNotFound
}
`,
	})
	audit, err := AuditRepoDoubles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Doubles) != 1 {
		t.Fatalf("want 1 double, got %+v", audit.Doubles)
	}
	if audit.Doubles[0].Permissive {
		t.Error("a closure's own (nil, nil) was attributed to the method")
	}
}
