package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CleanPathComponent validates a single path component.
func CleanPathComponent(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("path component is empty")
	}
	if trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf("path component %q is not allowed", value)
	}
	if strings.Contains(trimmed, "/") || strings.Contains(trimmed, `\`) {
		return "", fmt.Errorf("path component %q must not contain path separators", value)
	}
	return trimmed, nil
}

// CleanFileName validates a filename and strips any surrounding path components.
func CleanFileName(name string) (string, error) {
	cleaned, err := CleanPathComponent(filepath.Base(strings.TrimSpace(name)))
	if err != nil {
		return "", err
	}
	if cleaned != strings.TrimSpace(name) {
		return "", fmt.Errorf("filename %q must not contain path components", name)
	}
	return cleaned, nil
}

// JoinUnder joins path components and verifies the result stays under root.
// If the candidate path already exists on disk, symlinks are fully resolved
// before the containment check — preventing symlink-based escape attacks.
func JoinUnder(root string, elems ...string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("root path is empty")
	}
	cleanRoot := filepath.Clean(root)
	// Resolve symlinks in root itself so the reference point is canonical.
	if resolved, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = resolved
	}
	all := append([]string{cleanRoot}, elems...)
	candidate := filepath.Clean(filepath.Join(all...))

	// Syntactic containment check (works even when path does not exist yet).
	rel, err := filepath.Rel(cleanRoot, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root %q", candidate, cleanRoot)
	}

	// Resolve symlinks in the deepest existing prefix and re-check.
	// This matters for writes to a new leaf under an existing symlinked
	// directory: EvalSymlinks(candidate) fails when the leaf does not
	// exist yet, but opening the returned candidate would still follow
	// the symlinked parent.
	if resolved, ok, err := evalExistingPrefix(candidate); err != nil {
		return "", err
	} else if ok {
		rel, err := filepath.Rel(cleanRoot, resolved)
		if err != nil {
			return "", fmt.Errorf("resolve symlink path: %w", err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q resolves to %q which escapes root %q", candidate, resolved, cleanRoot)
		}
		return resolved, nil
	}

	return candidate, nil
}

func evalExistingPrefix(path string) (string, bool, error) {
	cleaned := filepath.Clean(path)
	missing := []string{}
	cur := cleaned
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("resolve symlink path: %w", err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false, nil
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}
