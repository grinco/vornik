package executor

import "testing"

// TestNamedSecretEnv_ProjectAllowlist is the per-secret allowlist regression:
// a named secret is injected into an agent container ONLY for a project on its
// allowlist; an empty allowlist means all projects (the AllowedTools
// convention); a secret missing a name or value is skipped.
func TestNamedSecretEnv_ProjectAllowlist(t *testing.T) {
	e := &Executor{config: &Config{}}
	e.config.NamedSecrets = []NamedSecret{
		{Name: "BROKER_KEY", Value: "sek-broker", AllowedProjects: []string{"ibkr-trader"}},
		{Name: "SHARED_KEY", Value: "sek-shared"},                                   // empty allowlist → all
		{Name: "OTHER_KEY", Value: "sek-other", AllowedProjects: []string{"janka"}}, // not for ibkr
		{Name: "", Value: "x"},     // skipped (no name)
		{Name: "NOVAL", Value: ""}, // skipped (no value)
	}

	// ibkr-trader gets its own secret + the shared one, NOT janka's.
	got := e.namedSecretEnv("ibkr-trader")
	if got["BROKER_KEY"] != "sek-broker" {
		t.Errorf("allowed project must get BROKER_KEY, got %q", got["BROKER_KEY"])
	}
	if got["SHARED_KEY"] != "sek-shared" {
		t.Errorf("empty-allowlist secret must reach every project, got %q", got["SHARED_KEY"])
	}
	if _, ok := got["OTHER_KEY"]; ok {
		t.Errorf("non-allowed secret OTHER_KEY must NOT be injected for ibkr-trader")
	}
	if _, ok := got[""]; ok {
		t.Errorf("nameless secret must be skipped")
	}
	if _, ok := got["NOVAL"]; ok {
		t.Errorf("valueless secret must be skipped")
	}

	// A different project gets the shared + its own, not ibkr's.
	got2 := e.namedSecretEnv("janka")
	if got2["OTHER_KEY"] != "sek-other" || got2["SHARED_KEY"] != "sek-shared" {
		t.Errorf("janka should get OTHER_KEY + SHARED_KEY, got %+v", got2)
	}
	if _, ok := got2["BROKER_KEY"]; ok {
		t.Errorf("janka must NOT get ibkr-trader's BROKER_KEY")
	}

	// No configured secrets → nil (no injection).
	if env := (&Executor{}).namedSecretEnv("p"); env != nil {
		t.Errorf("no configured secrets must yield nil, got %+v", env)
	}
}
