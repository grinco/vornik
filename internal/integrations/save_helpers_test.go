package integrations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertFieldValue(t *testing.T) {
	t.Run("plain string passthrough", func(t *testing.T) {
		got, err := convertFieldValue(CredentialField{Key: "x"}, "hello")
		if err != nil || got != "hello" {
			t.Errorf("got (%v, %v), want (\"hello\", nil)", got, err)
		}
	})

	t.Run("list splits and trims", func(t *testing.T) {
		got, err := convertFieldValue(CredentialField{Key: "repos", List: true}, "org/a, org/b ,  org/c")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := []string{"org/a", "org/b", "org/c"}
		list, ok := got.([]string)
		if !ok || len(list) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if list[i] != want[i] {
				t.Errorf("list[%d] = %q, want %q", i, list[i], want[i])
			}
		}
	})

	t.Run("int parses valid", func(t *testing.T) {
		got, err := convertFieldValue(CredentialField{Key: "app_id", Int: true}, "12345")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got != 12345 {
			t.Errorf("got %v, want 12345", got)
		}
	})

	t.Run("int defaults empty to zero", func(t *testing.T) {
		got, err := convertFieldValue(CredentialField{Key: "imap_port", Int: true}, "")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})

	t.Run("int rejects non-numeric input", func(t *testing.T) {
		_, err := convertFieldValue(CredentialField{Key: "app_id", Int: true}, "not-a-number")
		if err == nil {
			t.Fatal("want an error for a non-numeric Int field")
		}
	})
}

func TestSplitCommaList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a, b ,, c", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := splitCommaList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitCommaList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("splitCommaList(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestPlaceSecretFile(t *testing.T) {
	t.Run("writes the file at 0600, creating parent dirs", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nested", "secret.pem")
		if err := placeSecretFile(path, "pem-content"); err != nil {
			t.Fatalf("placeSecretFile: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "pem-content" {
			t.Fatalf("got (%q, %v), want (\"pem-content\", nil)", got, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %o, want 0600", perm)
		}
	})

	t.Run("re-write overwrites in place", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "secret.pem")
		if err := placeSecretFile(path, "first"); err != nil {
			t.Fatal(err)
		}
		if err := placeSecretFile(path, "second"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "second" {
			t.Errorf("got %q, want %q (overwritten, not appended)", got, "second")
		}
	})

	t.Run("errors when the parent path is not a directory", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(blocker, "secret.pem")
		if err := placeSecretFile(path, "content"); err == nil {
			t.Fatal("want an error when the parent path exists as a regular file")
		}
	})
}

func TestSanitizeEnvSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"proj-a", "PROJ_A"},
		{"proj_a", "PROJ_A"},
		{"Proj.A", "PROJ_A"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sanitizeEnvSuffix(tc.in); got != tc.want {
			t.Errorf("sanitizeEnvSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProjectScopedEnvName(t *testing.T) {
	got := projectScopedEnvName("EMAIL_IMAP_PASSWORD", "proj-a")
	want := "EMAIL_IMAP_PASSWORD_PROJ_A"
	if got != want {
		t.Errorf("projectScopedEnvName(...) = %q, want %q", got, want)
	}
}

// --- splitAndPlaceFields: direct white-box coverage of its error branches
// (task 5.2b's SecretFile mode and Int/List conversion), exercised below
// Save()'s own gates so each failure point is isolated.

func TestSplitAndPlaceFields_ConvertFieldValueErrorPropagates(t *testing.T) {
	kind := IntegrationKind{ID: "x", Scope: ScopeProject, Fields: []CredentialField{{Key: "n", Int: true}}}
	target := SaveTarget{Scope: ScopeProject}
	cand := CandidateConfig{ProjectID: "proj-a", Values: map[string]string{"n": "not-a-number"}}
	deps := SaveDeps{ConfigDir: t.TempDir()}

	_, _, err := splitAndPlaceFields(kind, target, cand, deps)
	if err == nil || !strings.Contains(err.Error(), "whole number") {
		t.Fatalf("err = %v, want a whole-number conversion error", err)
	}
}

func TestSplitAndPlaceFields_SecretFileMissingTargetEntry(t *testing.T) {
	kind := IntegrationKind{ID: "x", Scope: ScopeProject, Fields: []CredentialField{{Key: "k", Secret: true, SecretFile: true}}}
	target := SaveTarget{Scope: ScopeProject} // no SecretFilePaths at all
	cand := CandidateConfig{ProjectID: "proj-a", Values: map[string]string{"k": "pem-content"}}
	deps := SaveDeps{ConfigDir: t.TempDir()}

	_, _, err := splitAndPlaceFields(kind, target, cand, deps)
	if err == nil || !strings.Contains(err.Error(), "no SecretFilePaths entry") {
		t.Fatalf("err = %v, want a missing-SecretFilePaths error", err)
	}
}

func TestSplitAndPlaceFields_SecretFilePathFnErrors(t *testing.T) {
	kind := IntegrationKind{ID: "x", Scope: ScopeProject, Fields: []CredentialField{{Key: "k", Secret: true, SecretFile: true}}}
	target := SaveTarget{
		Scope: ScopeProject,
		SecretFilePaths: map[string]func(string, string) (string, error){
			"k": func(_, _ string) (string, error) { return "", fmt.Errorf("boom") },
		},
	}
	cand := CandidateConfig{ProjectID: "proj-a", Values: map[string]string{"k": "pem-content"}}
	deps := SaveDeps{ConfigDir: t.TempDir()}

	_, _, err := splitAndPlaceFields(kind, target, cand, deps)
	if err == nil || !strings.Contains(err.Error(), "resolve secret file path") {
		t.Fatalf("err = %v, want a path-resolution error", err)
	}
}

func TestSplitAndPlaceFields_PlaceSecretFileErrors(t *testing.T) {
	configDir := t.TempDir()
	// Block the secrets dir's "blocked" child from ever becoming a
	// directory by pre-creating it as a regular file.
	if err := os.MkdirAll(filepath.Join(configDir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(configDir, "secrets", "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	kind := IntegrationKind{ID: "x", Scope: ScopeProject, Fields: []CredentialField{{Key: "k", Secret: true, SecretFile: true}}}
	target := SaveTarget{
		Scope: ScopeProject,
		SecretFilePaths: map[string]func(string, string) (string, error){
			//nolint:unparam // signature is fixed by SecretFilePaths' func type; this fixture never needs to return an error itself (placeSecretFile is what fails).
			"k": func(secretsDir, _ string) (string, error) {
				return filepath.Join(secretsDir, "blocked", "secret.pem"), nil
			},
		},
	}
	cand := CandidateConfig{ProjectID: "proj-a", Values: map[string]string{"k": "pem-content"}}
	deps := SaveDeps{ConfigDir: configDir}

	_, _, err := splitAndPlaceFields(kind, target, cand, deps)
	if err == nil || !strings.Contains(err.Error(), "place secret file") {
		t.Fatalf("err = %v, want a place-secret-file error", err)
	}
}

func TestSplitAndPlaceFields_SecretFileBoundaryBugSimulation(t *testing.T) {
	// The simulated bug returns a "path" with no directory component at
	// all (required so hasSecretLiteral's own "looks like a path"
	// exemption doesn't apply — see its doc) — placeSecretFile runs
	// BEFORE the boundary check catches this, so it would otherwise write
	// a real file relative to the test binary's working directory.
	// t.Chdir confines that write to a throwaway temp dir.
	t.Chdir(t.TempDir())
	kind := IntegrationKind{ID: "x", Scope: ScopeProject, Fields: []CredentialField{{Key: "k", Secret: true, SecretFile: true}}}
	target := SaveTarget{
		Scope: ScopeProject,
		SecretFilePaths: map[string]func(string, string) (string, error){
			// Bug simulation: a broken SecretFilePaths implementation
			// returns something that looks like a bare secret literal
			// (no "/", long) instead of a real file path.
			"k": func(_, _ string) (string, error) { return "aVeryLongBareSecretLookingPathNoSlash123", nil },
		},
	}
	cand := CandidateConfig{ProjectID: "proj-a", Values: map[string]string{"k": "pem-content"}}
	deps := SaveDeps{ConfigDir: t.TempDir()}

	_, _, err := splitAndPlaceFields(kind, target, cand, deps)
	if err == nil || !strings.Contains(err.Error(), "bug in SecretFilePaths") {
		t.Fatalf("err = %v, want a SecretFilePaths-bug boundary error", err)
	}
}

func TestSplitAndPlaceFields_PlaceSecretErrors(t *testing.T) {
	// ConfigDir itself is a regular file, so deps.secretsDir()
	// (ConfigDir+"/secrets") can never be created — placeSecret's
	// os.MkdirAll fails for both daemon (onboarding.WriteEnvSecret) and
	// project (projectdoctor.EnvSecrets.Set) scope.
	dir := t.TempDir()
	blockingConfigDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blockingConfigDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	kind := IntegrationKind{ID: "x", Scope: ScopeProject, Fields: []CredentialField{{Key: "k", Secret: true, EnvName: "TEST_ENV_NAME"}}}
	target := SaveTarget{Scope: ScopeProject, SecretValue: func(name string) string { return name }}
	cand := CandidateConfig{ProjectID: "proj-a", Values: map[string]string{"k": "literal-secret-value"}}
	deps := SaveDeps{ConfigDir: blockingConfigDir}

	_, _, err := splitAndPlaceFields(kind, target, cand, deps)
	if err == nil || !strings.Contains(err.Error(), "place secret") {
		t.Fatalf("err = %v, want a place-secret error", err)
	}
}
