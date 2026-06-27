package featuredoctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthPrereq_AdminKeyPresence(t *testing.T) {
	dir := t.TempDir()
	f := authFeature()
	var keyCheck *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "admin key present" {
			keyCheck = &f.Prereqs[i]
		}
	}
	if keyCheck == nil {
		t.Fatal("missing 'admin key present' prereq")
	}
	// Absent -> unmet, not doctor-fixable.
	if res := keyCheck.Check(context.Background(), Deps{SecretsDir: dir}); res.OK || res.Fixable {
		t.Fatalf("absent admin key must be unmet+unfixable, got %+v", res)
	}
	// Present -> met.
	if err := os.WriteFile(filepath.Join(dir, "admin-key.txt"), []byte("VORNIK_ADMIN_KEY=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := keyCheck.Check(context.Background(), Deps{SecretsDir: dir}); !res.OK {
		t.Fatalf("present admin key must be met, got %+v", res)
	}
}
