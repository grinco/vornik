package cli

import (
	"bytes"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

func TestKeystore_StoreLoadDelete(t *testing.T) {
	keyring.MockInit() // in-memory provider; never touches the real keychain

	if _, err := LoadStoredAPIKey(); err != ErrNoStoredKey {
		t.Fatalf("empty keychain: err = %v, want ErrNoStoredKey", err)
	}
	if err := StoreAPIKey("sk-vornik-tag.secret"); err != nil {
		t.Fatalf("StoreAPIKey: %v", err)
	}
	got, err := LoadStoredAPIKey()
	if err != nil {
		t.Fatalf("LoadStoredAPIKey: %v", err)
	}
	if got != "sk-vornik-tag.secret" {
		t.Errorf("loaded key = %q, want sk-vornik-tag.secret", got)
	}
	if err := DeleteStoredAPIKey(); err != nil {
		t.Fatalf("DeleteStoredAPIKey: %v", err)
	}
	// Idempotent: deleting again is not an error.
	if err := DeleteStoredAPIKey(); err != nil {
		t.Errorf("second DeleteStoredAPIKey: %v", err)
	}
	if _, err := LoadStoredAPIKey(); err != ErrNoStoredKey {
		t.Errorf("after delete: err = %v, want ErrNoStoredKey", err)
	}
}

func TestAuthLogin_FromStdin_Stores(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { _ = DeleteStoredAPIKey() })

	cmd := authLoginCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader("  sk-vornik-tag.fromstdin\n"))

	if err := runAuthLogin(cmd, nil); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	got, err := LoadStoredAPIKey()
	if err != nil {
		t.Fatalf("LoadStoredAPIKey after login: %v", err)
	}
	if got != "sk-vornik-tag.fromstdin" {
		t.Errorf("stored key = %q, want trimmed sk-vornik-tag.fromstdin", got)
	}
}

func TestAuthLogin_EmptyStdin_Errors(t *testing.T) {
	keyring.MockInit()
	cmd := authLoginCmd
	cmd.SetIn(strings.NewReader("   \n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := runAuthLogin(cmd, nil); err == nil {
		t.Fatal("empty stdin should error, got nil")
	}
}

func TestClientFromEnv_PrefersEnvThenKeychain(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { _ = DeleteStoredAPIKey() })

	// Keychain has a key; env unset → keychain wins over the default.
	if err := StoreAPIKey("sk-vornik-tag.fromkeychain"); err != nil {
		t.Fatalf("StoreAPIKey: %v", err)
	}
	t.Setenv("VORNIK_API_KEY", "")
	if c := ClientFromEnv(); c.apiKey != "sk-vornik-tag.fromkeychain" {
		t.Errorf("apiKey = %q, want keychain value", c.apiKey)
	}

	// Env set → env wins over keychain.
	t.Setenv("VORNIK_API_KEY", "sk-vornik-tag.fromenv")
	if c := ClientFromEnv(); c.apiKey != "sk-vornik-tag.fromenv" {
		t.Errorf("apiKey = %q, want env value", c.apiKey)
	}
}
