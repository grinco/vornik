package configassist

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const projectWithSecret = "projectId: assistant\nnamed_secrets:\n  - name: OLLAMA\n    value: sk-live-1234567890abcdef1234567890\n  - name: OTHER\n    value: ${OTHER_ENV}\nchat:\n  api_key: abcdefghijklmnopqrstuvwxyz123456\nautonomy:\n  goal: be useful\n  feeds:\n    - slug: news\n      cadence: 1h\n"

// Test 31 (design §9): the snapshot never contains the secrets directory,
// and named_secrets[].value is replaced UNCONDITIONALLY — not only when it
// looks like a raw secret. Symlinks and special files are refused/skipped.
func TestBuild_SanitizesAndExcludesSecrets(t *testing.T) {
	root := writeTree(t, map[string]string{
		"projects/assistant.yaml": projectWithSecret,
		"secrets/admin-key.txt":   "VORNIK_ADMIN_KEY=supersecretsupersecret12345",
		"workflows/digest.md":     "---\nworkflowId: digest\n---\n# body\n",
	})
	// A symlink pointing outside the tree must not be copied.
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "projects", "escape.yaml")); err != nil {
		t.Skip("symlinks unsupported")
	}
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Files["secrets/admin-key.txt"]; ok {
		t.Fatal("secrets directory must never be in the snapshot")
	}
	if _, ok := s.Files["projects/escape.yaml"]; ok {
		t.Fatal("symlink must not be copied")
	}
	got := string(s.Files["projects/assistant.yaml"])
	if strings.Contains(got, "sk-live-") || strings.Contains(got, "abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("secret values leaked into the snapshot:\n%s", got)
	}
	if !strings.Contains(got, "value: ${OTHER_ENV}") {
		t.Fatal("an env reference is a NAME and must stay editable")
	}
	if s.Placeholders() != 2 {
		t.Fatalf("placeholders = %d, want 2 (named_secrets value + chat.api_key)", s.Placeholders())
	}
	if !strings.Contains(got, "goal: be useful") {
		t.Fatal("non-secret content must be intact")
	}
	// The file on disk under Root is the sanitized copy.
	disk, _ := os.ReadFile(filepath.Join(s.Root, "projects", "assistant.yaml"))
	if string(disk) != got {
		t.Fatal("disk copy differs from Files")
	}
}

func TestBuild_AuthorizerHidesSharedFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"projects/a.yaml":  "projectId: a\n",
		"projects/b.yaml":  "projectId: b\n",
		"swarms/shared.md": "---\nswarmId: s\n---\n",
	})
	s, err := Build(root, func(rel string) bool { return rel != "projects/b.yaml" }, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Files["projects/b.yaml"]; ok {
		t.Fatal("unauthorized file must be absent from the snapshot")
	}
	if _, ok := s.Files["projects/a.yaml"]; !ok {
		t.Fatal("authorized file missing")
	}
}

func TestBuild_Limits(t *testing.T) {
	root := writeTree(t, map[string]string{"projects/a.yaml": strings.Repeat("x", 100), "projects/b.yaml": "y"})
	if _, err := Build(root, nil, Limits{MaxFiles: 1, MaxFileBytes: 1000, MaxTotalBytes: 1000}); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("file count limit: %v", err)
	}
	if _, err := Build(root, nil, Limits{MaxFiles: 10, MaxFileBytes: 10, MaxTotalBytes: 1000}); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("file size limit: %v", err)
	}
}

// Test 15 (design §9): the walk emits create for a new file, replace for
// a changed one, nothing for an unchanged one, and NO op for a file the
// loop deleted — the deletion is reported, never applied.
func TestWalk_CreateReplaceNoDelete(t *testing.T) {
	root := writeTree(t, map[string]string{
		"projects/a.yaml":     "projectId: a\nautonomy:\n  goal: x\n",
		"projects/b.yaml":     "projectId: b\n",
		"workflows/digest.md": "---\nworkflowId: digest\n---\n",
	})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// edit a, create c, delete b, leave digest alone
	if err := os.WriteFile(filepath.Join(s.Root, "projects", "a.yaml"), []byte("projectId: a\nautonomy:\n  goal: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "projects", "c.yaml"), []byte("projectId: c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Root, "projects", "b.yaml")); err != nil {
		t.Fatal(err)
	}
	ops, deleted, err := s.Walk()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].Op != "replace" || ops[0].Path != "projects/a.yaml" || ops[1].Op != "create" || ops[1].Path != "projects/c.yaml" {
		t.Fatalf("ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Content, "goal: y") {
		t.Fatal("replace must carry the full new content")
	}
	if len(deleted) != 1 || deleted[0] != "projects/b.yaml" {
		t.Fatalf("deleted = %v, want the ignored deletion reported", deleted)
	}
	for _, op := range ops {
		if op.Op == "delete" {
			t.Fatal("there is no delete op")
		}
	}
	// Base hashes come from the REAL tree; an absent file hashes to "".
	hashes, err := s.BaseHashes([]string{"projects/a.yaml", "projects/c.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if hashes["projects/a.yaml"] == "" || hashes["projects/c.yaml"] != "" {
		t.Fatalf("hashes = %v", hashes)
	}
	if _, err := s.BaseHashes([]string{"../outside"}); !errors.Is(err, ErrPathOutsideSnapshot) {
		t.Fatalf("escape must be refused: %v", err)
	}
}

// Test 35 (design §9 / R7): a bundle that leaves every placeholder intact
// re-materialises byte-identically; altering, duplicating, moving or
// dropping a placeholder is a refusal, never a partial substitution.
func TestWalk_PlaceholderIntegrity(t *testing.T) {
	build := func(t *testing.T) *Snapshot {
		root := writeTree(t, map[string]string{"projects/assistant.yaml": projectWithSecret})
		s, err := Build(root, nil, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	edit := func(t *testing.T, s *Snapshot, f func(string) string) {
		p := filepath.Join(s.Root, "projects", "assistant.yaml")
		b, _ := os.ReadFile(p)
		if err := os.WriteFile(p, []byte(f(string(b))), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("intact placeholders re-materialise byte-identically", func(t *testing.T) {
		s := build(t)
		edit(t, s, func(in string) string { return strings.Replace(in, "cadence: 1h", "cadence: 2h", 1) })
		ops, _, err := s.Walk()
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(projectWithSecret, "cadence: 1h", "cadence: 2h", 1)
		if ops[0].Content != want {
			t.Fatalf("re-materialised:\n%s\nwant:\n%s", ops[0].Content, want)
		}
		if strings.Contains(ops[0].Content, "PLACEHOLDER") {
			t.Fatal("placeholders left in the bundle")
		}
	})
	t.Run("dropped placeholder refuses", func(t *testing.T) {
		s := build(t)
		edit(t, s, func(in string) string {
			return placeholderRe.ReplaceAllString(in, "changed") // drop both tokens
		})
		if _, _, err := s.Walk(); !errors.Is(err, ErrPlaceholderTampered) {
			t.Fatalf("want ErrPlaceholderTampered, got %v", err)
		}
	})
	t.Run("altered placeholder refuses", func(t *testing.T) {
		s := build(t)
		edit(t, s, func(in string) string {
			tok := placeholderRe.FindString(in)
			return strings.Replace(in, tok, "VORNIK_SECRET_PLACEHOLDER_000000000000000000000000", 1)
		})
		if _, _, err := s.Walk(); !errors.Is(err, ErrPlaceholderTampered) {
			t.Fatalf("want ErrPlaceholderTampered, got %v", err)
		}
	})
	t.Run("duplicated placeholder refuses", func(t *testing.T) {
		s := build(t)
		edit(t, s, func(in string) string {
			tok := placeholderRe.FindString(in)
			return in + "extra_key: " + tok + "\n"
		})
		if _, _, err := s.Walk(); !errors.Is(err, ErrPlaceholderTampered) {
			t.Fatalf("want ErrPlaceholderTampered, got %v", err)
		}
	})
	t.Run("moved placeholder refuses", func(t *testing.T) {
		s := build(t)
		edit(t, s, func(in string) string {
			toks := placeholderRe.FindAllString(in, -1)
			// swap the two tokens: each now sits on the other's key
			out := strings.Replace(in, toks[0], "TMP", 1)
			out = strings.Replace(out, toks[1], toks[0], 1)
			return strings.Replace(out, "TMP", toks[1], 1)
		})
		if _, _, err := s.Walk(); !errors.Is(err, ErrPlaceholderTampered) {
			t.Fatalf("want ErrPlaceholderTampered, got %v", err)
		}
	})
	t.Run("placeholder moved into another file refuses", func(t *testing.T) {
		s := build(t)
		b, _ := os.ReadFile(filepath.Join(s.Root, "projects", "assistant.yaml"))
		tok := placeholderRe.FindString(string(b))
		if err := os.WriteFile(filepath.Join(s.Root, "projects", "other.yaml"), []byte("projectId: other\napi_key: "+tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Walk(); !errors.Is(err, ErrPlaceholderTampered) {
			t.Fatalf("want ErrPlaceholderTampered, got %v", err)
		}
	})
}
