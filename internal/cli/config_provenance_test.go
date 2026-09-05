package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The table prints one line per key, sorted, with origin and source — the
// shape an operator diffs between two daemons.
func TestRenderConfigProvenance(t *testing.T) {
	view := &configProvenanceView{ConfigPath: "/etc/vornik/config.yaml", LoadedAt: "2026-09-03T20:00:00Z"}
	view.Values = map[string]struct {
		Value  any    `json:"value"`
		Origin string `json:"origin"`
		Source string `json:"source"`
	}{
		"logging.level":     {Value: "debug", Origin: "env", Source: "VORNIK_LOG_LEVEL"},
		"database.password": {Value: "<redacted>", Origin: "env", Source: "VORNIK_DATABASE_PASSWORD"},
		"gateway.address":   {Value: nil, Origin: "unset"},
		"database.port":     {Value: 5432, Origin: "env_invalid", Source: `VORNIK_DATABASE_PORT ("abc" is not an integer)`},
	}
	var out bytes.Buffer
	if err := renderConfigProvenance(&out, view); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"config: /etc/vornik/config.yaml",
		"KEY", "ORIGIN", "SOURCE",
		"logging.level", "env", "VORNIK_LOG_LEVEL",
		"<redacted>",
		"gateway.address", "unset",
		"env_invalid", `"abc" is not an integer`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Trees render as a second table plus the refused files, named as NOT in effect.
	raw := `{"trees":{"layers":["/org","/user"],"sources":[{"kind":"project","id":"shared","path":"projects/shared.yaml","layer":"/org","shadowed_by":"/user"},{"kind":"project","id":"shared","path":"projects/shared.yaml","layer":"/user"}],"rejected":[{"kind":"project","path":"projects/typo.yaml","layer":"/user","error":"field mention_handel not found"}]}}`
	var withTrees configProvenanceView
	if err := json.Unmarshal([]byte(raw), &withTrees); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := renderConfigProvenance(&out, &withTrees); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"trees: 2 layer(s): /org < /user", "SHADOWED BY", "projects/shared.yaml", "/user", "NOT in effect", "mention_handel"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("trees rendering missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "KEY") {
		t.Error("no values were asked for, no values table should print")
	}
	out.Reset()
	if err := renderConfigProvenance(&out, view); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	// Sorted: database.* before gateway.* before logging.*.
	if strings.Index(got, "database.password") > strings.Index(got, "gateway.address") ||
		strings.Index(got, "gateway.address") > strings.Index(got, "logging.level") {
		t.Errorf("keys are not sorted:\n%s", got)
	}
}
