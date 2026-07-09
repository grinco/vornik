package forge

import (
	"strings"
	"testing"

	forgeapi "vornik.io/vornik/internal/forge"
)

// T8b: backlog-origin (numberless) ForgeJob support. forgeJobFromTask must
// accept a repo+kind=backlog+slug job, still accept an issue-driven
// repo+number job, and reject a backlog job that omits the slug. The
// branch/title/body helpers take their own deterministic backlog path.

func TestForgeJobFromTask_BacklogKind(t *testing.T) {
	cases := []struct {
		name    string
		job     forgeapi.ForgeJob
		wantErr bool
	}{
		{
			name: "backlog job with repo+slug passes",
			job:  forgeapi.ForgeJob{Repo: "o/r", Kind: "backlog", Slug: "fix-the-parser"},
		},
		{
			name: "issue-driven number-only still passes",
			job:  forgeapi.ForgeJob{Repo: "o/r", Number: 5},
		},
		{
			name:    "backlog job missing slug is rejected",
			job:     forgeapi.ForgeJob{Repo: "o/r", Kind: "backlog"},
			wantErr: true,
		},
		{
			name:    "backlog job missing repo is rejected",
			job:     forgeapi.ForgeJob{Kind: "backlog", Slug: "x"},
			wantErr: true,
		},
		{
			name:    "no number and no backlog kind is rejected",
			job:     forgeapi.ForgeJob{Repo: "o/r"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := forgeJobFromTask(taskWithJob(tc.job), "h")
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %+v", tc.job)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %+v: %v", tc.job, err)
			}
		})
	}
}

func TestBranchForJob_Backlog(t *testing.T) {
	got := branchForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "fix-the-parser"})
	if got != "backlog/fix-the-parser" {
		t.Errorf("backlog branch = %q, want backlog/fix-the-parser", got)
	}
	// A backlog job ignores labels (never a fix/feat verb).
	got = branchForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "s", Labels: []string{"enhancement"}})
	if got != "backlog/s" {
		t.Errorf("backlog branch ignoring labels = %q, want backlog/s", got)
	}
}

func TestTitleForJob_Backlog(t *testing.T) {
	if got := titleForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "s", Title: "Fix the parser"}); got != "Backlog: Fix the parser" {
		t.Errorf("title = %q, want 'Backlog: Fix the parser'", got)
	}
	// Empty title falls back to the slug.
	if got := titleForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "fix-the-parser"}); got != "Backlog: fix-the-parser" {
		t.Errorf("title fallback = %q, want 'Backlog: fix-the-parser'", got)
	}
	// Whitespace-only title also falls back to slug.
	if got := titleForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "s", Title: "   "}); got != "Backlog: s" {
		t.Errorf("title ws fallback = %q, want 'Backlog: s'", got)
	}
}

func TestBodyForJob_Backlog(t *testing.T) {
	body := bodyForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "s", Body: "Please fix the flaky test."})
	if strings.Contains(body, "Closes #") {
		t.Errorf("backlog body must NOT contain a Closes #N line: %q", body)
	}
	if !strings.Contains(body, "**Requested:** Please fix the flaky test.") {
		t.Errorf("backlog body missing request framing: %q", body)
	}
	if !strings.Contains(body, "_Opened automatically by vornik.") {
		t.Errorf("backlog body missing trailer: %q", body)
	}

	// Empty body: no request line, just the trailer.
	empty := bodyForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "s"})
	if strings.Contains(empty, "**Requested:**") {
		t.Errorf("empty backlog body must not carry a Requested line: %q", empty)
	}
	if !strings.Contains(empty, "_Opened automatically by vornik.") {
		t.Errorf("empty backlog body missing trailer: %q", empty)
	}

	// 600-char cap + ellipsis, matching the issue path.
	long := bodyForJob(forgeapi.ForgeJob{Kind: "backlog", Slug: "s", Body: strings.Repeat("x", 700)})
	if !strings.Contains(long, strings.Repeat("x", 600)+"…") {
		t.Errorf("backlog body should cap request text at 600 chars + ellipsis")
	}
	if strings.Contains(long, strings.Repeat("x", 601)) {
		t.Errorf("backlog body should not carry more than 600 request chars")
	}

	// Determinism: same job → identical body across calls (no timestamps).
	j := forgeapi.ForgeJob{Kind: "backlog", Slug: "s", Body: "det"}
	first := bodyForJob(j)
	second := bodyForJob(j)
	if first != second {
		t.Errorf("backlog body must be deterministic: %q vs %q", first, second)
	}
}
