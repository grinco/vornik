package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSortStrings_StablyAscending(t *testing.T) {
	cases := [][]string{
		{},
		{"a"},
		{"b", "a"},
		{"c", "a", "b"},
		{"x", "y", "z"},      // already sorted
		{"b", "a", "a", "b"}, // dupes
	}
	for _, in := range cases {
		got := append([]string(nil), in...)
		sortStrings(got)
		for i := 1; i < len(got); i++ {
			if got[i-1] > got[i] {
				t.Errorf("input %v → %v not sorted at %d", in, got, i)
			}
		}
	}
}

func TestFilepathDir_AllPathShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b/c", "/a/b"},
		{"a/b", "a"},
		{"name", "."},
		{"", "."},
		{"/", ""},
		{"/single", ""},
	}
	for _, tc := range cases {
		if got := filepathDir(tc.in); got != tc.want {
			t.Errorf("filepathDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeEvalName_StripsNonAlnumAndCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"swarm-1", "swarm-1"},
		{"  Foo Bar  ", "foo-bar"},
		{"ID/with*slashes!", "id-with-slashes-"},
		{"", "case"}, // empty input maps to the fallback "case" so a save path never gets ""
		{"-leading", "-leading"},
	}
	for _, tc := range cases {
		if got := sanitizeEvalName(tc.in); got != tc.want {
			t.Errorf("sanitizeEvalName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEvalEquals_StructuralEquality(t *testing.T) {
	ok, err := evalEquals(json.RawMessage(`{"a":1,"b":2}`), json.RawMessage(`{"b":2,"a":1}`))
	if err != nil || !ok {
		t.Errorf("equal JSON should match: ok=%v err=%v", ok, err)
	}
	ok, err = evalEquals(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`))
	if err != nil || ok {
		t.Errorf("differing values should NOT match: ok=%v err=%v", ok, err)
	}
}

func TestEvalEquals_EmptyActualIsFalse(t *testing.T) {
	ok, err := evalEquals(json.RawMessage(`{}`), nil)
	if err != nil || ok {
		t.Errorf("nil actual: ok=%v err=%v", ok, err)
	}
}

func TestEvalEquals_BadExpectedIsError(t *testing.T) {
	_, err := evalEquals(json.RawMessage(`{not json}`), json.RawMessage(`{}`))
	if err == nil {
		t.Error("malformed expected should error")
	}
}

func TestEvalEquals_BadActualIsCleanFalse(t *testing.T) {
	ok, err := evalEquals(json.RawMessage(`{}`), json.RawMessage(`not json`))
	if err != nil || ok {
		t.Errorf("malformed actual: ok=%v err=%v (want false, nil)", ok, err)
	}
}

func TestEvalContains_SubsetMatches(t *testing.T) {
	reason, err := evalContains(
		json.RawMessage(`{"a":1}`),
		json.RawMessage(`{"a":1,"b":2}`),
	)
	if err != nil || reason != "" {
		t.Errorf("subset should match: reason=%q err=%v", reason, err)
	}
}

func TestEvalContains_KeyMissing(t *testing.T) {
	reason, _ := evalContains(
		json.RawMessage(`{"a":1}`),
		json.RawMessage(`{"b":2}`),
	)
	if !strings.Contains(reason, "key \"a\" is missing") {
		t.Errorf("got %q", reason)
	}
}

func TestEvalContains_ValueMismatch(t *testing.T) {
	reason, _ := evalContains(
		json.RawMessage(`{"a":1}`),
		json.RawMessage(`{"a":2}`),
	)
	if !strings.Contains(reason, "value mismatch") {
		t.Errorf("got %q", reason)
	}
}

func TestEvalContains_EmptyActual(t *testing.T) {
	reason, err := evalContains(json.RawMessage(`{"a":1}`), nil)
	if err != nil {
		t.Errorf("nil actual should not error: %v", err)
	}
	if !strings.Contains(reason, "empty") {
		t.Errorf("got %q", reason)
	}
}

func TestEvalContains_BadExpectedIsError(t *testing.T) {
	_, err := evalContains(json.RawMessage(`not json`), json.RawMessage(`{}`))
	if err == nil {
		t.Error("malformed expected should error")
	}
}

func TestEvalContains_BadActualIsCleanReason(t *testing.T) {
	reason, err := evalContains(json.RawMessage(`{}`), json.RawMessage(`not json`))
	if err != nil {
		t.Errorf("malformed actual should not error: %v", err)
	}
	if !strings.Contains(reason, "not a JSON object") {
		t.Errorf("got %q", reason)
	}
}

func TestTruncateForLog_BoundaryAndShort(t *testing.T) {
	if got := truncateForLog("abc", 10); got != "abc" {
		t.Errorf("short: %q", got)
	}
	if got := truncateForLog("abcdefghij", 10); got != "abcdefghij" {
		t.Errorf("boundary: %q", got)
	}
	if got := truncateForLog("abcdefghijk", 5); got != "abcde...[truncated]" {
		t.Errorf("trunc: %q", got)
	}
}

func TestAdminTruncate_Boundaries(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 3, "ab…"},
		{"x", 1, "x"},
		{"longer", 1, "l"},
	}
	for _, tc := range cases {
		if got := truncate(tc.s, tc.max); got != tc.want {
			t.Errorf("truncate(%q,%d)=%q want %q", tc.s, tc.max, got, tc.want)
		}
	}
}

func TestEvalLastRunPath_RespectsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	got := evalLastRunPath("my-swarm")
	if got != "/custom/state/vornik/evals/my-swarm.json" {
		t.Errorf("got %q", got)
	}
}

func TestEvalLastRunPath_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	got := evalLastRunPath("S")
	if got == "" {
		t.Skip("UserHomeDir returned empty in this test env")
	}
	if !strings.Contains(got, "/.local/state/vornik/evals/s.json") {
		t.Errorf("got %q (no fallback path?)", got)
	}
}

func TestSaveAndLoadEvalLastRun_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	current := evalRunSummary{Cases: map[string]evalCaseLast{
		"caseA": {Passed: true},
		"caseB": {Passed: false},
	}}
	if err := saveEvalLastRun("round-trip", current); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File should exist where evalLastRunPath says.
	path := evalLastRunPath("round-trip")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat saved: %v", err)
	}
	if dir := filepath.Dir(path); dir == "" {
		t.Fatal("path has empty dir")
	}
	got := loadEvalLastRun("round-trip")
	if got == nil {
		t.Fatal("load returned nil")
	}
	if len(got.Cases) != 2 || !got.Cases["caseA"].Passed || got.Cases["caseB"].Passed {
		t.Errorf("round-trip mismatch: %+v", got.Cases)
	}
}

func TestLoadEvalLastRun_MissingFileIsNil(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := loadEvalLastRun("does-not-exist"); got != nil {
		t.Errorf("missing file should yield nil, got %+v", got)
	}
}

func TestLoadEvalLastRun_MalformedFileIsNil(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	path := evalLastRunPath("malformed")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadEvalLastRun("malformed"); got != nil {
		t.Errorf("malformed file should yield nil, got %+v", got)
	}
}

func TestDiffEvalRuns_DetectsRegressionsAndRecoveries(t *testing.T) {
	prev := &evalRunSummary{Cases: map[string]evalCaseLast{
		"a": {Passed: true},
		"b": {Passed: false},
		"c": {Passed: true},
	}}
	current := evalRunSummary{Cases: map[string]evalCaseLast{
		"a": {Passed: false}, // regression
		"b": {Passed: true},  // recovery
		"c": {Passed: true},  // unchanged
		"d": {Passed: true},  // new case, ignored
	}}
	regressed, recovered := diffEvalRuns(prev, current)
	if len(regressed) != 1 || regressed[0] != "a" {
		t.Errorf("regressed = %v, want [a]", regressed)
	}
	if len(recovered) != 1 || recovered[0] != "b" {
		t.Errorf("recovered = %v, want [b]", recovered)
	}
}

func TestDiffEvalRuns_NilPrevReturnsEmpty(t *testing.T) {
	r, c := diffEvalRuns(nil, evalRunSummary{})
	if r != nil || c != nil {
		t.Errorf("nil prev should return nil/nil, got %v/%v", r, c)
	}
}
