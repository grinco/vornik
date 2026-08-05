package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One public origin, and exactly one way to read it.
//
// The 2026-08-04 work made server.public_base_url canonical with
// auth.external_base_url as a fallback, and the loader validates the pair
// through Config.PublicOrigin(). The backlog then recorded that the duplicate
// key could simply be deleted from the deployed config. It could not: five
// consumers read Config.Auth.ExternalBaseURL DIRECTLY rather than calling
// PublicOrigin(), so a config setting only server.public_base_url passed
// validation, booted cleanly, and handed those consumers an empty string.
//
// The worst of them was the login flow (loginflow.New), where an empty base
// URL yields a malformed OAuth callback and locks the operator out of the UI —
// a failure that appears only at the next login attempt, long after the edit.
// The others silently dropped the origin from steering alerts, chat-completion
// links and narrator milestones.
//
// A one-time fix does not hold: the field is public, the name is inviting, and
// nothing about `deps.Auth.ExternalBaseURL` looks wrong at a review. So the
// rule is enforced rather than documented — every read outside this package
// goes through Config.PublicOrigin().
func TestNoDirectExternalBaseURLReads(t *testing.T) {
	root := guardRepoRoot(t)

	// Allowed: the accessor itself and the loader's validation, both of which
	// must see the raw field to compare the two keys and reject a disagreement.
	allowed := map[string]bool{
		filepath.Join("internal", "config", "config.go"): true,
		filepath.Join("internal", "config", "loader.go"): true,
	}

	var offenders []string
	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "testdata" || info.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			if allowed[rel] {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // unparseable files are not this test's business
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ExternalBaseURL" {
					return true
				}
				offenders = append(offenders, rel+":"+
					strings.TrimPrefix(fset.Position(sel.Pos()).String(), path+":"))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("ExternalBaseURL must be read through Config.PublicOrigin(), not directly.\n"+
			"A config that sets only server.public_base_url leaves this field EMPTY, which\n"+
			"breaks OAuth login silently. Offending reads:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func guardRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod above the test directory")
		}
		dir = parent
	}
}
