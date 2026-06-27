package cli

import (
	"errors"

	keyring "github.com/zalando/go-keyring"
)

// OS-keychain storage for the vornikctl API key (security LLD review
// batch 3 — "CLI key → OS keychain vs plaintext"). Backed by
// go-keyring: libsecret (Linux/D-Bus), Keychain (macOS), Credential
// Manager (Windows). Keeps the key out of shell history, .env files, and
// process-environment dumps. VORNIK_API_KEY env still works and takes
// precedence (CI / scripted use); the keychain is the recommended path
// for interactive operators.
const (
	keyringService = "vornik"
	keyringUser    = "api-key"
)

// ErrNoStoredKey is returned by LoadStoredAPIKey when no key is present
// in the OS keychain.
var ErrNoStoredKey = errors.New("no API key in the OS keychain")

// StoreAPIKey saves the API key in the OS keychain.
func StoreAPIKey(key string) error {
	return keyring.Set(keyringService, keyringUser, key)
}

// LoadStoredAPIKey reads the API key from the OS keychain. Returns
// ErrNoStoredKey when the key is absent. A keychain-unavailable error
// (e.g. a headless host with no secret service) is returned as-is so
// callers can decide whether to fall back silently (ClientFromEnv) or
// surface it (auth status).
func LoadStoredAPIKey() (string, error) {
	v, err := keyring.Get(keyringService, keyringUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNoStoredKey
		}
		return "", err
	}
	return v, nil
}

// DeleteStoredAPIKey removes the API key from the OS keychain. Deleting a
// key that isn't there is not an error (idempotent logout).
func DeleteStoredAPIKey() error {
	err := keyring.Delete(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
