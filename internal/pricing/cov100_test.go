package pricing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Branches(t *testing.T) {
	dir := t.TempDir()

	t.Run("absent file → empty table, no error", func(t *testing.T) {
		tbl, err := Load(filepath.Join(dir, "does-not-exist.yaml"))
		if err != nil {
			t.Fatalf("absent file must not error, got %v", err)
		}
		if tbl == nil || len(tbl.IDs()) != 0 {
			t.Fatalf("absent file should yield an empty table, got %+v", tbl)
		}
	})

	t.Run("unreadable (directory) → error", func(t *testing.T) {
		// os.ReadFile on a directory returns a non-IsNotExist error,
		// exercising the read-error branch.
		if _, err := Load(dir); err == nil {
			t.Fatal("reading a directory as a file must error")
		}
	})

	t.Run("malformed yaml → parse error", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(bad, []byte("models: [this is not a map"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(bad); err == nil {
			t.Fatal("malformed yaml must error")
		}
	})

	t.Run("valid yaml with models → IDs sorted", func(t *testing.T) {
		good := filepath.Join(dir, "good.yaml")
		yaml := "models:\n  zeta-1:\n    input: 1.0\n    output: 2.0\n  alpha-1:\n    input: 0.5\n    output: 1.0\ndefault:\n  input: 0.1\n  output: 0.2\n"
		if err := os.WriteFile(good, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		tbl, err := Load(good)
		if err != nil {
			t.Fatalf("valid yaml: %v", err)
		}
		ids := tbl.IDs()
		if len(ids) != 2 || ids[0] != "alpha-1" || ids[1] != "zeta-1" {
			t.Fatalf("IDs should be sorted [alpha-1 zeta-1], got %v", ids)
		}
	})

	t.Run("valid yaml with no models key → empty model set", func(t *testing.T) {
		nomodels := filepath.Join(dir, "nomodels.yaml")
		if err := os.WriteFile(nomodels, []byte("default:\n  input: 0.1\n  output: 0.2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		tbl, err := Load(nomodels)
		if err != nil {
			t.Fatalf("no-models yaml: %v", err)
		}
		if len(tbl.IDs()) != 0 {
			t.Fatalf("no models → empty IDs, got %v", tbl.IDs())
		}
	})
}

func TestIDs_NilReceiver(t *testing.T) {
	var tbl *Table
	if tbl.IDs() != nil {
		t.Error("nil-receiver IDs() should return nil")
	}
}

func TestSetWarnHook_NilReceiverAndSet(t *testing.T) {
	var nilT *Table
	nilT.SetWarnHook(func(string) {}) // must not panic

	tbl := Empty()
	called := false
	tbl.SetWarnHook(func(string) { called = true })
	tbl.Lookup("unknown-model") // first unknown lookup fires the hook
	if !called {
		t.Error("warn hook should fire on first unknown-model lookup")
	}
}
