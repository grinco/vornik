package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A control has three answers — pass, fail, and I COULD NOT EVALUATE THIS — and
// this codebase kept collapsing the third into the first. On 2026-08-26, 30 of
// the doctor checks returned Status "OK" on a path that meant "not evaluated":
// "no database; skipping", "API metrics not wired", "no config dir; skipping".
//
// So `vornikctl doctor` reported green on a half-configured deployment — which
// is exactly the deployment where a green report is most likely to be believed,
// and exactly where "not wired" is most likely to be true. A missing control is
// visibly missing; a control that reports OK over a surface it never examined
// actively buys down the operator's suspicion.
//
// SKIPPED already existed, was already excluded from the issue count
// (doctor_handlers.go), and 7 checks used it. It was an adoption gap, not a
// design gap. This test is what stops the thirty-first.
//
// See docs/audits/2026-08-26-silent-controls-audit.md Finding A.

// skipShaped matches a message that admits the check did not run.
var skipShaped = regexp.MustCompile(
	`(?i)skip|not wired|no config|not configured|unavailable|no database|not captured|not readable|no eval suites|not in use|no workspaces root|no .* to scan|address not captured`)

// vacuousMarker exempts a site where OK is genuinely correct because the check
// RAN and found an empty set (a true pass), not because it could not run. The
// marker must carry a reason — a bare directive would let the next author
// silence the test instead of thinking.
const vacuousMarker = "doctor-vacuous:"

func TestUnknownIsNotReportedAsOK(t *testing.T) {
	files, err := filepath.Glob("doctor_*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no doctor_*.go files found — the test is looking in the wrong place")
	}

	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if !strings.Contains(line, `Status: "OK"`) || !skipShaped.MatchString(line) {
				continue
			}
			// A justification may sit on the line or in the comment block
			// immediately above it.
			justified := strings.Contains(line, vacuousMarker)
			for j := i - 1; j >= 0 && j >= i-12 && !justified; j-- {
				trimmed := strings.TrimSpace(lines[j])
				if !strings.HasPrefix(trimmed, "//") {
					break
				}
				if strings.Contains(trimmed, vacuousMarker) {
					justified = true
				}
			}
			if !justified {
				offenders = append(offenders, fmt.Sprintf("%s:%d  %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("%d doctor check(s) report Status \"OK\" with a message admitting the check "+
			"did not run.\n\nAn operator reading OK cannot tell \"I checked and it is fine\" from "+
			"\"I could not check\" — which is how a half-configured deployment reports green.\n\n"+
			"Use Status \"SKIPPED\" (already excluded from the issue count, so it will not break "+
			"a build).\n\nIf OK is genuinely correct because the check RAN and found an empty set, "+
			"add a `%s <reason>` comment to the line, or to the comment block "+
			"immediately above the return, saying which case it is.\n\n  %s",
			len(offenders), vacuousMarker, strings.Join(offenders, "\n  "))
	}
}

// TestSkippedIsNotCountedAsAnIssue pins the property that makes the conversion
// safe: SKIPPED must never turn a clean report into a failing one, or the
// honest answer would carry a cost and authors would keep reaching for OK.
func TestSkippedIsNotCountedAsAnIssue(t *testing.T) {
	body, err := os.ReadFile("doctor_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	// Both summary sites must exclude SKIPPED alongside OK.
	const guard = `c.Status != "OK" && c.Status != "SKIPPED"`
	if n := strings.Count(string(body), guard); n < 2 {
		t.Errorf("the report summary must exclude SKIPPED from the issue count at every site; "+
			"found %d occurrence(s) of %q. Without this, converting a check to SKIPPED would "+
			"turn a passing report into a failing one and authors would rationally keep using OK.",
			n, guard)
	}
}
