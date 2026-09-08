package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// captureKnowledgeRollup runs the command against an io.Writer rather than
// stdout — the function takes one precisely so its output is assertable
// without redirecting a process-global.
func captureKnowledgeRollup(t *testing.T, skillID string, windowHours int) (string, error) {
	t.Helper()
	var buf strings.Builder
	err := runKnowledgeRollup(&buf, skillID, windowHours)
	return buf.String(), err
}

// The CLI must print the counterfactual beside the number, or it becomes the
// surface that reintroduces the vanity metric the design refuses.
func TestKnowledgeRollup_PrintsBothArmsAndCoverage(t *testing.T) {
	ratingHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills/skill-x/rating-rollup" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"skill_id":"skill-x","window_hours":168,"summary":"low_lift",
			"contexts":[{"project_id":"p1","workflow_id":"digest","verdict":"low_lift","lift":-0.7,
			"treatment":{"coverage":0.2,"rated_n":20,"up_n":4,"contested_n":1},
			"baseline":{"coverage":0.2,"rated_n":20,"up_n":18,"contested_n":0}}]}`)
	})

	out, err := captureKnowledgeRollup(t, "skill-x", 0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	for _, want := range []string{
		"low_lift",  // the verdict
		"digest",    // the context, since a verdict is per-context
		"4/20",      // the treatment arm
		"18/20",     // the baseline it must be read against
		"coverage",  // the evidence that the arms are comparable
		"contested", // the exclusions, so they are not invisible
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// not_measurable must read as "not used in this window", not as an error or an
// empty table an operator reads as "nothing wrong".
func TestKnowledgeRollup_NotMeasurableSaysWhy(t *testing.T) {
	ratingHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"skill_id":"s","window_hours":168,"summary":"not_measurable","contexts":[]}`)
	})
	out, err := captureKnowledgeRollup(t, "s", 0)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if !strings.Contains(out, "not injected") && !strings.Contains(out, "no executions") {
		t.Errorf("not_measurable did not explain itself:\n%s", out)
	}
}

func TestKnowledgeRollup_PassesTheWindow(t *testing.T) {
	var query string
	ratingHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"skill_id":"s","window_hours":48,"summary":"not_measurable","contexts":[]}`)
	})
	if _, err := captureKnowledgeRollup(t, "s", 48); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "window_hours=48") {
		t.Errorf("query = %q, want the window passed through", query)
	}
}

// A verdict that refuses the comparison must not print the difference anyway.
// The number exists and does not mean what it looks like; the design's own
// review made the point — operators ignore the badge and quote the number.
func TestKnowledgeRollup_SuppressesTheNumberWhenTheVerdictRefusesIt(t *testing.T) {
	for _, verdict := range []string{"not_comparable", "unknown"} {
		t.Run(verdict, func(t *testing.T) {
			ratingHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"skill_id":"s","window_hours":168,"summary":"`+verdict+`",
					"contexts":[{"project_id":"p","workflow_id":"w","verdict":"`+verdict+`","lift":-0.65,
					"treatment":{"coverage":0.4,"rated_n":40,"up_n":10,"contested_n":0},
					"baseline":{"coverage":0.1,"rated_n":10,"up_n":9,"contested_n":0}}]}`)
			})
			out, err := captureKnowledgeRollup(t, "s", 0)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "difference") {
				t.Errorf("%s printed a difference it had just refused to stand behind:\n%s", verdict, out)
			}
			// The arms still print — the operator should see WHY it was refused.
			if !strings.Contains(out, "coverage") {
				t.Errorf("%s hid the arms as well as the number:\n%s", verdict, out)
			}
		})
	}
}

// The verdicts that DO stand behind a number still print it.
func TestKnowledgeRollup_PrintsTheNumberWhenTheVerdictStandsBehindIt(t *testing.T) {
	ratingHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"skill_id":"s","window_hours":168,"summary":"low_lift",
			"contexts":[{"project_id":"p","workflow_id":"w","verdict":"low_lift","lift":-0.7,
			"treatment":{"coverage":0.2,"rated_n":20,"up_n":4,"contested_n":0},
			"baseline":{"coverage":0.2,"rated_n":20,"up_n":18,"contested_n":0}}]}`)
	})
	out, err := captureKnowledgeRollup(t, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "difference") {
		t.Errorf("low_lift did not print the difference it is asserting:\n%s", out)
	}
}
