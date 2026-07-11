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

// AssertUnder verifies that an already-absolute path stays under base, with
// the same symlink discipline as JoinUnder: symlinks are resolved in base and
// in the deepest existing prefix of path (so a broken symlink whose target
// does not yet exist can't slip past a lexical-only check — audit 2026-07-09
// O-3). Empty base disables the check (test scenarios). Used by callers that
// receive a stored absolute path (e.g. an artifact row) rather than joining
// components themselves.
func AssertUnder(base, path string) error {
	if base == "" {
		return nil
	}
	cleanBase := filepath.Clean(base)
	if resolved, err := filepath.EvalSymlinks(cleanBase); err == nil {
		cleanBase = resolved
	}
	cleanPath := filepath.Clean(path)
	// Resolve the deepest EXISTING prefix (not just the full path): a broken
	// symlink makes EvalSymlinks(cleanPath) fail, which previously left the
	// lexical path in place and passed the check — then the open followed the
	// symlink. Resolving the existing prefix closes that edge.
	if resolved, ok, err := evalExistingPrefix(cleanPath); err != nil {
		return err
	} else if ok {
		cleanPath = resolved
	}
	rel, err := filepath.Rel(cleanBase, cleanPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q outside root %q", cleanPath, cleanBase)
	}
	return nil
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
