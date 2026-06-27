package featuredoctor

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// backupConfig writes a timestamped copy of the file at path and returns the
// backup path.  The backup filename is "<base>.<timestamp>" where timestamp is
// formatted as RFC3339 with colons replaced by dashes for filesystem safety.
//
// For a heavier, archive-level backup that wraps the full vornik config tree,
// use `vornikctl backup` instead.
func backupConfig(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("backup: read %s: %w", path, err)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	backupPath := filepath.Join(dir, base+"."+ts+".bak")

	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		return "", fmt.Errorf("backup: write %s: %w", backupPath, err)
	}
	return backupPath, nil
}

// restoreConfig copies the backup file byte-for-byte to target, replacing it.
// For a heavier, archive-level backup that wraps the full vornik config tree,
// use `vornikctl backup` instead.
func restoreConfig(backupPath, target string) error {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("restore: read backup %s: %w", backupPath, err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return fmt.Errorf("restore: write %s: %w", target, err)
	}
	return nil
}
