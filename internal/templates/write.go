package templates

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/safepath"
)

// ExistingTargetError reports the rendered target that already exists.
type ExistingTargetError struct {
	Target string
}

func (e *ExistingTargetError) Error() string {
	if e == nil {
		return "target already exists"
	}
	return "target already exists: " + e.Target
}

// WriteRenderedFilesExclusive writes rendered template output below root
// without ever overwriting an existing target.
func WriteRenderedFilesExclusive(root string, rendered map[string]string) ([]string, error) {
	targets := SortedTargets(rendered)
	for _, target := range targets {
		fullPath, err := resolveRenderedTarget(root, target)
		if err != nil {
			return nil, fmt.Errorf("target %s refused: %w", target, err)
		}
		if _, err := os.Stat(fullPath); err == nil {
			return nil, &ExistingTargetError{Target: target}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", target, err)
		}
	}

	written := make([]string, 0, len(rendered))
	for _, target := range targets {
		body := rendered[target]
		fullPath, err := resolveRenderedTarget(root, target)
		if err != nil {
			return written, fmt.Errorf("target %s refused: %w", target, err)
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			return written, fmt.Errorf("mkdir %s: %w", target, err)
		}
		// 0o600 — template-materialised configs (project YAML,
		// SWARM.md, WORKFLOW.md) can carry credentials interpolated
		// from template parameters.
		f, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return written, &ExistingTargetError{Target: target}
			}
			return written, fmt.Errorf("create %s: %w", target, err)
		}
		if _, err := f.WriteString(body); err != nil {
			_ = f.Close()
			return written, fmt.Errorf("write %s: %w", target, err)
		}
		if err := f.Close(); err != nil {
			return written, fmt.Errorf("close %s: %w", target, err)
		}
		written = append(written, target)
	}
	return written, nil
}

func resolveRenderedTarget(root, target string) (string, error) {
	if err := validateRelativeTarget(target); err != nil {
		return "", err
	}
	return safepath.JoinUnder(root, filepath.FromSlash(target))
}
