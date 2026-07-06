package projectdoctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvSecrets_SetMakesLiveAndPersists(t *testing.T) {
	dir := t.TempDir()
	es := NewEnvSecrets(dir)
	const name = "PROJECTDOCTOR_TEST_SECRET"
	t.Cleanup(func() { _ = os.Unsetenv(name) })

	if es.Has(name) {
		t.Fatal("secret should be absent initially")
	}
	if err := es.Set(name, "s3cr3t"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Live in the process now (no restart).
	if !es.Has(name) || os.Getenv(name) != "s3cr3t" {
		t.Fatal("Set must make the secret live via os.Setenv")
	}
	// Persisted to the env file for next boot.
	data, err := os.ReadFile(filepath.Join(dir, "project-secrets.env"))
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	if !contains(string(data), name+"=s3cr3t") {
		t.Fatalf("env file missing secret line: %s", data)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
