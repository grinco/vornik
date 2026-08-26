package imagemanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the module root so the
// parity and label tests can enumerate the real tree rather than a fixture.
// A fixture would defeat the point: these two tests exist to catch a NEW
// Containerfile that nobody wired up, and a fixture only ever contains what
// someone remembered to add.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the package directory")
		}
		dir = parent
	}
}

// TestAllRowsAreWellFormed guards the manifest's own shape. A row missing a
// tag or a Containerfile silently drops an image from every consumer.
func TestAllRowsAreWellFormed(t *testing.T) {
	rows := All()
	if len(rows) == 0 {
		t.Fatal("manifest is empty")
	}
	seen := map[string]bool{}
	for _, img := range rows {
		if img.Tag == "" {
			t.Errorf("row with empty tag: %+v", img)
		}
		if img.Containerfile == "" {
			t.Errorf("%s: empty containerfile", img.Tag)
		}
		if img.Context == "" {
			t.Errorf("%s: empty build context", img.Tag)
		}
		if img.Condition == "" {
			t.Errorf("%s: empty condition", img.Tag)
		}
		if seen[img.Tag] {
			t.Errorf("%s: duplicate tag in manifest", img.Tag)
		}
		seen[img.Tag] = true
	}
}

// TestEveryManifestContainerfileExists catches a row pointing at a path that
// was moved or deleted.
func TestEveryManifestContainerfileExists(t *testing.T) {
	root := repoRoot(t)
	for _, img := range All() {
		if _, err := os.Stat(filepath.Join(root, img.Containerfile)); err != nil {
			t.Errorf("%s: containerfile %s: %v", img.Tag, img.Containerfile, err)
		}
	}
}

// TestManifestParity is contract C4: every Containerfile in the repo appears
// in the manifest or on the explicit exclusion list.
//
// This is the check that would have caught the cluster-tag gap found while
// writing the design (2026-08-25): cluster.compose.yaml consumes
// localhost/vornik:thin and :full, both clustering guides tell operators to
// use them, and NOTHING in the repo built either tag — `docker-build` passed
// no --target and emitted vornik/vornik:<version>, which no compose file
// references. A hand-maintained list is how a documented image ends up with
// no builder.
func TestManifestParity(t *testing.T) {
	root := repoRoot(t)

	inManifest := map[string]bool{}
	for _, img := range All() {
		inManifest[img.Containerfile] = true
	}

	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Build artifacts and vendored trees carry copies of the
			// repo's own Containerfiles; walking them would report every
			// image twice.
			switch d.Name() {
			case ".git", "node_modules", ".vornik-export", ".vornik-public-clone":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name == "Containerfile" || name == "Dockerfile" {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("walked the repo and found no Containerfiles — the walk is broken")
	}

	for _, rel := range found {
		if inManifest[rel] || isExcluded(rel) {
			continue
		}
		t.Errorf("%s is in the repo but not in the manifest and not excluded — "+
			"add a manifest row or an exclusion with a reason (contract C4)", rel)
	}
}

// TestEveryManifestContainerfileDeclaresProvenanceLabels is contract C2.
// Both labels, not one: the design specifies revision AND version, and an
// image carrying only one cannot answer both questions the freshness check
// and the release record ask of it.
func TestEveryManifestContainerfileDeclaresProvenanceLabels(t *testing.T) {
	root := repoRoot(t)
	for _, img := range All() {
		if img.Condition == ConditionExcluded {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, img.Containerfile))
		if err != nil {
			t.Errorf("%s: %v", img.Tag, err)
			continue
		}
		text := string(body)
		for _, label := range []string{
			"org.opencontainers.image.revision",
			"org.opencontainers.image.version",
		} {
			if !strings.Contains(text, label) {
				t.Errorf("%s (%s): missing LABEL %s — an unlabelled image cannot be "+
					"compared against the daemon revision (contract C2)",
					img.Tag, img.Containerfile, label)
			}
		}
	}
}

// stubProbe records what was asked and answers from fixed maps, so condition
// evaluation is testable without systemctl or podman on the box.
type stubProbe struct {
	enabledUnits   map[string]bool
	stacksWithCtrs map[string]bool
	unitCalls      []string
	stackCalls     []string
}

func (s *stubProbe) UnitEnabled(name string) bool {
	s.unitCalls = append(s.unitCalls, name)
	return s.enabledUnits[name]
}

func (s *stubProbe) StackHasContainers(stack string) bool {
	s.stackCalls = append(s.stackCalls, stack)
	return s.stacksWithCtrs[stack]
}

func TestAlwaysConditionIsAlwaysNeeded(t *testing.T) {
	probe := &stubProbe{}
	img := Image{Tag: "x", Condition: ConditionAlways}
	if !img.Needed(probe) {
		t.Error("an `always` row must be needed with no host state at all")
	}
	if len(probe.unitCalls)+len(probe.stackCalls) != 0 {
		t.Error("an `always` row must not probe the host")
	}
}

func TestTestAndExcludedRowsAreNeverNeeded(t *testing.T) {
	probe := &stubProbe{}
	for _, cond := range []string{ConditionTest, ConditionExcluded} {
		img := Image{Tag: "x", Condition: cond}
		if img.Needed(probe) {
			t.Errorf("%s row must never be deployable: CI-only and third-party "+
				"images are not part of an update", cond)
		}
	}
}

func TestUnitConditionFollowsEnabledNotActive(t *testing.T) {
	img := Image{Tag: "x", Condition: "unit:vornik-scraper"}

	enabled := &stubProbe{enabledUnits: map[string]bool{"vornik-scraper": true}}
	if !img.Needed(enabled) {
		t.Error("an enabled unit expresses intent, so its images are needed")
	}
	if len(enabled.unitCalls) != 1 || enabled.unitCalls[0] != "vornik-scraper" {
		t.Errorf("expected one probe for `vornik-scraper`, got %v", enabled.unitCalls)
	}

	disabled := &stubProbe{enabledUnits: map[string]bool{}}
	if img.Needed(disabled) {
		t.Error("a disabled unit expresses no intent, so its images are skipped")
	}
}

// TestComposeConditionCountsStoppedContainers is the regression test for
// review finding 2 (companion review-20260825-6947.md).
//
// The first draft resolved `compose:` against RUNNING containers. That reopens
// the very defect this design exists to close, merely delayed: stack stopped
// for maintenance -> update skips its images -> operator restarts -> the stack
// comes up on code older than the daemon, invisibly, possibly under a
// trading-hours timer. Intent, not running state, is the criterion.
func TestComposeConditionCountsStoppedContainers(t *testing.T) {
	img := Image{Tag: "x", Condition: "compose:trading"}

	stopped := &stubProbe{stacksWithCtrs: map[string]bool{"trading": true}}
	if !img.Needed(stopped) {
		t.Error("a stopped-but-not-torn-down stack is still intended, so its " +
			"images must be rebuilt (review finding 2)")
	}

	tornDown := &stubProbe{stacksWithCtrs: map[string]bool{}}
	if img.Needed(tornDown) {
		t.Error("a torn-down stack has no containers and no intent, so it is skipped")
	}
}

func TestUnknownConditionIsNotSilentlyDeployable(t *testing.T) {
	img := Image{Tag: "x", Condition: "wat:nonsense"}
	if img.Needed(&stubProbe{}) {
		t.Error("an unrecognised condition must not resolve to needed — a typo " +
			"in a condition should drop the row loudly, not build everything")
	}
}

func TestDeployableFiltersByCondition(t *testing.T) {
	probe := &stubProbe{
		enabledUnits:   map[string]bool{},
		stacksWithCtrs: map[string]bool{},
	}
	got := Deployable(probe)
	if len(got) == 0 {
		t.Fatal("with no optional stacks present, the always-rows must still deploy")
	}
	for _, img := range got {
		if img.Condition != ConditionAlways {
			t.Errorf("%s: condition %q resolved as deployable on a bare host",
				img.Tag, img.Condition)
		}
	}
}

// TestAgentImageIsAlways pins the one invariant every install depends on:
// the agent image is not optional, and a refactor that made it conditional
// would silently reintroduce the reported bug.
func TestAgentImageIsAlways(t *testing.T) {
	for _, img := range All() {
		if img.Tag == AgentImageTag {
			if img.Condition != ConditionAlways {
				t.Fatalf("%s must be `always`, got %q", AgentImageTag, img.Condition)
			}
			return
		}
	}
	t.Fatalf("%s is not in the manifest", AgentImageTag)
}

// TestClusterImagesCarryBuildTargets guards the §4.1 finding: thin and full
// come from one Dockerfile and are distinguishable ONLY by --target. A row
// that loses its target silently builds the wrong stage under the right tag.
func TestClusterImagesCarryBuildTargets(t *testing.T) {
	targets := map[string]string{}
	for _, img := range All() {
		if strings.HasPrefix(img.Condition, "compose:cluster") {
			targets[img.Tag] = img.Target
		}
	}
	if len(targets) == 0 {
		t.Fatal("no cluster images in the manifest, but cluster.compose.yaml consumes two")
	}
	for tag, target := range targets {
		if target == "" {
			t.Errorf("%s: cluster images share one Dockerfile and differ only by "+
				"build target; an empty target builds the last stage under the "+
				"wrong tag (§4.1)", tag)
		}
	}
}

// TestEmitRowsIsShellParseable pins the contract the shell consumers depend
// on: one row per image, tab-separated, no header. A stray header line or a
// space-separated field would make `while IFS=$'\t' read ...` silently
// mis-assign every column.
func TestEmitRowsIsShellParseable(t *testing.T) {
	probe := &stubProbe{
		enabledUnits:   map[string]bool{"vornik-scraper": true},
		stacksWithCtrs: map[string]bool{},
	}
	out := EmitRows(Deployable(probe))

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no rows emitted")
	}
	var sawAgent bool
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			t.Fatalf("row %q has %d tab-separated fields, want 5 "+
				"(tag, containerfile, target, context, condition)", line, len(fields))
		}
		for i, f := range fields {
			// Target is legitimately empty for single-stage images; the
			// other four never are, and an empty one would shift columns
			// for a naive reader.
			if i != 2 && f == "" {
				t.Errorf("row %q: field %d is empty", line, i)
			}
			if strings.ContainsAny(f, " \n") {
				t.Errorf("row %q: field %q contains whitespace that breaks field splitting", line, f)
			}
		}
		if fields[0] == AgentImageTag {
			sawAgent = true
		}
	}
	if !sawAgent {
		t.Error("the agent image must always be emitted")
	}
	// The unit-gated case ("an enabled scraper unit emits its images") is
	// asserted in manifest_enterprise_test.go, because every unit-gated image
	// is Enterprise-only — the CE export prunes the trees they build from, so
	// asserting one here would fail in the Community tree by construction.
}

func TestEmitRowsIsEmptyForNoImages(t *testing.T) {
	if got := EmitRows(nil); got != "" {
		t.Errorf("no images must emit nothing, got %q", got)
	}
}
