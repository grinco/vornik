package onboarding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteEnvSecret writes or replaces one NAME=value line in the file at
// dir/filename, creating the file (0600) and dir (0700) as needed.
// Other lines in the file are preserved, so a shared secrets file
// (e.g. chat.env alongside aws.env) keeps unrelated env vars.
//
// Used by the onboarding chat commit to write VORNIK_CHAT_API_KEY into
// <configDir>/secrets/chat.env without disturbing other secrets.
func WriteEnvSecret(dir, filename, name, value string) (string, error) {
	if err := validateEnvSecretNameValue(name, value); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create secrets dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, filename)
	var lines []string
	if existing, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read %q: %w", path, err)
	}

	prefix := name + "="
	replaced := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, prefix) {
			lines[i] = prefix + value
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, prefix+value)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write %q: %w", path, err)
	}
	// Chmod after write in case the file already existed with looser perms.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod %q: %w", path, err)
	}
	return path, nil
}

func validateEnvSecretNameValue(name, value string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("env secret name is required")
	}
	if strings.ContainsAny(name, "=\r\n") {
		return fmt.Errorf("env secret name %q contains an invalid character", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("env secret value for %s must be a single line", name)
	}
	return nil
}
