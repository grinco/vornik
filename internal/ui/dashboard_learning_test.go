package ui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// Template anchors bounding the "Health & knowledge" row in dashboard.html.
// Declared here so the coupling between this test and the template's section
// comments is explicit — if either comment is reworded, the failure names the
// anchor rather than looking like a layout regression.
const (
	healthRowAnchor = "Health & knowledge"
	nextRowAnchor   = "Model Leaderboard (top"
)

// learningTileHTML slices the rendered dashboard down to just the Learning
// tile: from its data-tile attribute to the next tile's. The boundary is an
// attribute rather than a marker comment because html/template strips
// comments, so they never reach the response body.
//
// Count assertions have to be fragment-scoped: a bare ">7<" search over the
// whole page would happily match an unrelated tile's number and the test
// would pass for the wrong reason.
func learningTileHTML(t *testing.T, body string) string {
	t.Helper()
	const open = `data-tile="learning"`
	start := strings.Index(body, open)
	if start < 0 {
		t.Fatalf("Learning tile not found in dashboard body:\n%s", body)
	}
	rest := body[start+len(open):]
	end := strings.Index(rest, `data-tile=`)
	if end < 0 {
		t.Fatalf("Learning tile has no following data-tile boundary — the slice would run to the page end")
	}
	return rest[:end]
}

// TestDashboardHealthRowFillsItsGrid is the regression guard for the defect
// this tile was built to fix: after the Trading safety tile was removed
// (2026-08-07, commit 99237734) the "Health & knowledge" row still declared
// lg:grid-cols-3 while holding only two tiles, so a third of the row rendered
// as dead space at ≥1024px — reported by the operator as "the UI looks
// incomplete".
//
// The invariant is that the row's declared column count equals the number of
// tiles in it. Asserted against the template source, because the failure is a
// layout mismatch that a rendered-body content check cannot see: a page with a
// hole in it returns 200 and contains every string you would think to assert.
func TestDashboardHealthRowFillsItsGrid(t *testing.T) {
	src, err := templatesFS.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	body := renderDashboardBody(t)

	// The row's tiles, in render order. Each must carry its data-tile marker
	// so the count below stays honest as tiles come and go.
	tiles := []string{"memory", "learning", "judge"}
	for _, tile := range tiles {
		if !strings.Contains(body, `data-tile="`+tile+`"`) {
			t.Errorf("Health & knowledge row is missing the %q tile", tile)
		}
	}

	// Everything below is anchored to THIS row's span of the template, from
	// its section comment to the next row's. Both a page-global data-tile
	// count and an unanchored grid-class search would be wrong here: the
	// "Right now" row above carries an identical grid class, and a data-tile
	// added to any other row would either mask a missing tile here or fail
	// this test for something that isn't its business.
	rowStart := strings.Index(string(src), healthRowAnchor)
	if rowStart < 0 {
		t.Fatalf("dashboard.html no longer has the %q row comment — this test's anchor is gone", healthRowAnchor)
	}
	rowOpen := string(src)[rowStart:]
	if next := strings.Index(rowOpen, nextRowAnchor); next > 0 {
		rowOpen = rowOpen[:next]
	} else {
		t.Fatalf("no %q anchor follows the Health & knowledge row — cannot bound the row", nextRowAnchor)
	}

	if got := strings.Count(rowOpen, "data-tile="); got != len(tiles) {
		t.Errorf("the Health & knowledge row holds %d data-tile markers but documents %d tiles — "+
			"a tile was added or removed without updating this test (and possibly without "+
			"updating the row's grid-cols)", got, len(tiles))
	}
	// With three tiles in the row, the grid must be a three-column grid.
	div := strings.Index(rowOpen, `<div class="grid`)
	if div < 0 {
		t.Fatal("no grid div follows the Health & knowledge comment")
	}
	rowTag := rowOpen[div : div+strings.Index(rowOpen[div:], ">")+1]
	if !strings.Contains(rowTag, "lg:grid-cols-3") {
		t.Errorf("Health & knowledge row holds %d tiles but declares %q — "+
			"tile count and column count must agree", len(tiles), rowTag)
	}
}

// seedLearningSkills writes a store with a deliberately distinct count per
// maturity so a digit found in the tile fragment identifies exactly one
// bucket: 4 trusted, 7 active, 2 draft, 9 retired.
func seedLearningSkills(t *testing.T) persistence.SkillRepository {
	t.Helper()
	repo := newSkillRepoUI(t)
	n := 0
	mk := func(maturity string, count int) {
		for i := 0; i < count; i++ {
			n++
			id := maturity + "-" + string(rune('a'+n))
			if err := repo.Create(context.Background(), &persistence.Skill{
				ID: id, ProjectID: "p1", Name: id, Description: "d",
				Body: "# body", BodySHA256: "h-" + id, Maturity: maturity,
			}); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}
	}
	mk(persistence.SkillMaturityTrusted, 4)
	mk(persistence.SkillMaturityActive, 7)
	mk(persistence.SkillMaturityDraft, 2)
	mk(persistence.SkillMaturityRetired, 9)
	return repo
}

// TestDashboardLearningTile_RendersMaturityCountsAndDraftQueue is the core
// contract: the tile that fills the third slot of the "Health & knowledge"
// row (empty since the trading tile was removed 2026-08-07) reports the skill
// store's maturity distribution and the depth of the operator review queue.
func TestDashboardLearningTile_RendersMaturityCountsAndDraftQueue(t *testing.T) {
	body := renderDashboardBody(t, WithSkillRepository(seedLearningSkills(t)))
	tile := learningTileHTML(t, body)

	for _, want := range []string{
		">4<", // trusted
		">7<", // active
		">2<", // draft
		"trusted",
		"active",
		"draft",
		"2 awaiting approval", // the review queue, non-empty
		"/ui/admin/skills",    // where the operator acts on it
		"13 skills in the store",
	} {
		if !strings.Contains(tile, want) {
			t.Errorf("Learning tile missing %q:\n%s", want, tile)
		}
	}
	// Retired skills are excluded from both the triplet and the total: a
	// retired skill injects nowhere, so counting it would overstate what the
	// store actually contributes.
	if strings.Contains(tile, ">9<") {
		t.Errorf("Learning tile renders the retired count (9) — retired must be excluded:\n%s", tile)
	}
}

// TestDashboardLearningTile_DraftQueueClear: zero drafts must read as a
// deliberate "nothing waiting on you", not as a bare 0. Mirrors how the
// sibling Memory tile renders "drained" for an empty ingest backlog.
func TestDashboardLearningTile_DraftQueueClear(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "only-trusted", ProjectID: "p1", Name: "only-trusted", Description: "d",
		Body: "# b", BodySHA256: "h", Maturity: persistence.SkillMaturityTrusted,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tile := learningTileHTML(t, renderDashboardBody(t, WithSkillRepository(repo)))
	if !strings.Contains(tile, "review queue clear") {
		t.Errorf("empty draft queue must render an explicit clear state:\n%s", tile)
	}
	if strings.Contains(tile, "awaiting approval") {
		t.Errorf("no drafts, yet the tile claims something awaits approval:\n%s", tile)
	}
}

// TestDashboardLearningTile_UnwiredRepoDegrades: the tile must behave like its
// two siblings when its repo isn't wired — render a one-line explanation, not
// a panic and not a bank of zeros that reads as "you have no skills".
func TestDashboardLearningTile_UnwiredRepoDegrades(t *testing.T) {
	tile := learningTileHTML(t, renderDashboardBody(t))
	if !strings.Contains(tile, "Skill store not wired") {
		t.Errorf("unwired skill repo must render the not-wired state:\n%s", tile)
	}
}

// TestDashboardLearningTile_InstinctsAreEnterpriseOnly guards the edition
// boundary. Instincts are an EE feature — every existing entry point sits
// behind .EnterpriseAdmin (admin_landing.html) — while the skill store ships
// in both editions. A Community dashboard must therefore render the tile
// WITHOUT the instinct row: no empty slot, no "Enterprise only" teaser, the
// same absent-feature treatment the nav gives trading.
func TestDashboardLearningTile_InstinctsAreEnterpriseOnly(t *testing.T) {
	counts := []persistence.InstinctDomainStatusCount{
		{Domain: "software", Status: persistence.InstinctStatusActive, Count: 5},
		{Domain: "software", Status: persistence.InstinctStatusPromoted, Count: 3},
		{Domain: "net", Status: persistence.InstinctStatusCandidate, Count: 4},
		{Domain: "net", Status: persistence.InstinctStatusRetired, Count: 2},
	}
	repo := seedLearningSkills(t)

	t.Run("community_hides_the_instinct_row", func(t *testing.T) {
		instincts := &stubInstinctRepo{domainStatusCounts: counts}
		tile := learningTileHTML(t, renderDashboardBody(t,
			WithSkillRepository(repo),
			WithInstinctPlaybooks(instincts, true),
			// no WithAllEEAdmin — this is a Community build
		))
		if strings.Contains(tile, "Instincts live") {
			t.Errorf("Community build must not render the instinct row:\n%s", tile)
		}
		// The skills half still has to work — that's the point of gating the
		// row rather than the tile.
		if !strings.Contains(tile, "2 awaiting approval") {
			t.Errorf("Community build lost the skills half of the tile:\n%s", tile)
		}
	})

	t.Run("enterprise_counts_active_and_promoted_as_live", func(t *testing.T) {
		instincts := &stubInstinctRepo{domainStatusCounts: counts}
		tile := learningTileHTML(t, renderDashboardBody(t,
			WithSkillRepository(repo),
			WithInstinctPlaybooks(instincts, true),
			WithAllEEAdmin(),
		))
		if !strings.Contains(tile, "Instincts live") {
			t.Errorf("Enterprise build must render the instinct row:\n%s", tile)
		}
		// 5 active + 3 promoted = 8. A promoted instinct is still firing, so
		// excluding it would under-report; candidate (4) and retired (2) fire
		// nowhere and must not count.
		if !strings.Contains(tile, ">8<") {
			t.Errorf("live instincts must be active+promoted = 8:\n%s", tile)
		}
	})
}

// TestDashboardLearningTile_ScopedSessionSeesNoCounts: the counts span every
// project, so they follow the same rule as the spend and KG rollups on this
// page — a project-scoped session must not see instance-wide totals. Same
// leak class that was fixed on /ui/memory and /ui/insights/trends.
func TestDashboardLearningTile_ScopedSessionSeesNoCounts(t *testing.T) {
	srv := NewServer(
		WithSkillRepository(seedLearningSkills(t)),
		WithOnboardingDetector(alreadyOnboardedDetector()),
	)
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "janka"))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard returned %d", rr.Code)
	}

	tile := learningTileHTML(t, rr.Body.String())
	// Distinct from the unwired state on purpose: the store IS wired here, the
	// caller just may not see instance-wide totals. Telling them "not wired"
	// would be a lie about the deployment.
	if !strings.Contains(tile, "admin only") {
		t.Errorf("project-scoped session must get the restricted state, got:\n%s", tile)
	}
	if strings.Contains(tile, "not wired") {
		t.Errorf("scoped session must not be told the store is unwired:\n%s", tile)
	}
	for _, leaked := range []string{"13 skills in the store", "2 awaiting approval"} {
		if strings.Contains(tile, leaked) {
			t.Errorf("cross-project leak to a scoped session: %q\n%s", leaked, tile)
		}
	}
}
