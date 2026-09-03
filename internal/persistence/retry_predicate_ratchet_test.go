package persistence

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoSeventhRetrySpelling is the ratchet that stops the extraction being
// undone one call site at a time.
//
// Six places had independently spelled "attempt < maxAttempts", and one of them
// (executor.taskWillRetry) documented itself as a hand-copied mirror kept in
// sync by comment. That is how the drift happened, and a seventh spelling would
// restart it — silently, because a budget-only check still compiles, still
// passes its own tests, and only misbehaves on the narrow class of failures
// that must not retry.
//
// Design https://docs.vornik.io §5.
func TestNoSeventhRetrySpelling(t *testing.T) {
	root := repoRootFromPersistence(t)

	// The shape being banned: a comparison of an attempt counter against a
	// max-attempts counter, in the packages that own retry decisions.
	pattern := regexp.MustCompile(`(?i)\battempt\s*<\s*\w*max\w*attempts?\b`)

	// Known, deliberate exceptions — each is NOT a retry decision.
	allowed := map[string]string{
		// The delegated-child retry-storm guard fakes budget exhaustion. It
		// predates the predicate, reaches the same end state, and replacing it
		// is a behaviour change with its own measurement (see the note there).
		"internal/executor/workflow.go": "delegated-child retry-storm guard: drains the budget deliberately",
		// The predicate itself.
		"internal/persistence/retry_predicate.go": "the predicate",
	}

	for _, dir := range []string{"internal/scheduler", "internal/executor"} {
		walkErr := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if _, ok := allowed[rel]; ok {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for i, line := range strings.Split(string(body), "\n") {
				// Comments describing the old shape are fine; code is not.
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				if pattern.MatchString(line) {
					t.Errorf("%s:%d spells the retry-budget check by hand:\n    %s\n\n"+
						"Call persistence.TaskShouldRetry(attempt, maxAttempts, class) instead. Six\n"+
						"hand-copied spellings is how the executor's notification gating drifted out\n"+
						"of sync with the scheduler's actual decision; a seventh restarts that.\n"+
						"If this genuinely is not a retry decision, add it to `allowed` with a reason.",
						rel, i+1, trimmed)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
}

// repoRootFromPersistence walks up to the module root.
func repoRootFromPersistence(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}
