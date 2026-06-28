package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteEnvSecret_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteEnvSecret(dir, "chat.env", "VORNIK_CHAT_API_KEY", "sk-abc")
	if err != nil {
		t.Fatalf("WriteEnvSecret: %v", err)
	}
	if filepath.Base(path) != "chat.env" {
		t.Errorf("path base = %q, want chat.env", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
	got, _ := os.ReadFile(path)
	if strings.TrimSpace(string(got)) != "VORNIK_CHAT_API_KEY=sk-abc" {
		t.Errorf("content = %q, want VORNIK_CHAT_API_KEY=sk-abc", got)
	}
}

func TestWriteEnvSecret_ReplacesOnlyNamedLine(t *testing.T) {
	dir := t.TempDir()
	existing := "OTHER_VAR=keepme\nVORNIK_CHAT_API_KEY=old-value\nANOTHER=1\n"
	if err := os.WriteFile(filepath.Join(dir, "chat.env"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEnvSecret(dir, "chat.env", "VORNIK_CHAT_API_KEY", "new-value"); err != nil {
		t.Fatalf("WriteEnvSecret: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "chat.env"))
	body := string(got)
	if !strings.Contains(body, "OTHER_VAR=keepme") || !strings.Contains(body, "ANOTHER=1") {
		t.Errorf("other env vars must be preserved, got: %s", body)
	}
	if strings.Contains(body, "old-value") {
		t.Errorf("old key value must be replaced, got: %s", body)
	}
	if !strings.Contains(body, "VORNIK_CHAT_API_KEY=new-value") {
		t.Errorf("new value must be present, got: %s", body)
	}
}

func TestWriteEnvSecret_CreatesDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if _, err := WriteEnvSecret(dir, "chat.env", "VORNIK_CHAT_API_KEY", "k"); err != nil {
		t.Fatalf("WriteEnvSecret should create dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestWriteEnvSecret_RejectsUnsafeNameOrValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "", value: "sk-abc"},
		{name: "BAD=NAME", value: "sk-abc"},
		{name: "BAD\nNAME", value: "sk-abc"},
		{name: "VORNIK_CHAT_API_KEY", value: "sk-abc\nINJECTED=1"},
		{name: "VORNIK_CHAT_API_KEY", value: "sk-abc\rINJECTED=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.value, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "secrets")
			if _, err := WriteEnvSecret(dir, "chat.env", tt.name, tt.value); err == nil {
				t.Fatalf("WriteEnvSecret(%q, %q) error = nil, want validation error", tt.name, tt.value)
			}
			if _, err := os.Stat(filepath.Join(dir, "chat.env")); !os.IsNotExist(err) {
				t.Fatalf("unsafe input must not create secret file, stat err=%v", err)
			}
		})
	}
}
