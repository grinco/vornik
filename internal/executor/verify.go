package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// verifyClaimedFiles reads three classes of file claims out of an agent's
// result.json and confirms each named path exists on disk and was written
// during the step window:
//
//   - modified_files[]:        files the agent claims it edited in place.
//     motivating incident: writer claimed
//     PROJECT_CONTEXT.md was updated but only
//     wrote artifacts/out/. mtime check rules
//     that out.
//   - outputArtifacts[].path:  the wrapped <step>-response.md gets
//     auto-injected by the entrypoint; roles can
//     add their own artifacts. agents that
//     fabricate a path get caught here.
//   - produced_files[]:        opt-in convention for roles that produce
//     non-artifact files (vision OCR dumps,
//     scout PROJECT_CONTEXT.md, etc.). roles
//     that don't emit the array are unaffected.
//
// All three short-circuit to success when absent. Returning a single
// error with the joined problem list keeps attribution simple — the
// caller treats it as one schema_violation regardless of how many
// individual files failed.
//
// stepStart is the wall-clock time captured at the top of executeAgentStep,
// used as the mtime floor. Any claimed file whose on-disk mtime is earlier
// than stepStart did not get written during this step. A 1-second slack
// accommodates filesystems with sub-second mtime resolution that may round
// down.
func (e *Executor) verifyClaimedFiles(resultBytes []byte, workspaceDir, projectDir string, stepStart time.Time) error {
	if len(resultBytes) == 0 {
		return nil
	}
	var parsed struct {
		ModifiedFiles   []string `json:"modified_files"`
		ProducedFiles   []string `json:"produced_files"`
		OutputArtifacts []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"outputArtifacts"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		return nil
	}

	type claim struct {
		source string // "modified_files" / "produced_files" / "outputArtifacts"
		path   string
	}
	var claims []claim
	for _, p := range parsed.ModifiedFiles {
		claims = append(claims, claim{"modified_files", p})
	}
	for _, p := range parsed.ProducedFiles {
		claims = append(claims, claim{"produced_files", p})
	}
	for _, a := range parsed.OutputArtifacts {
		if a.Path != "" {
			claims = append(claims, claim{"outputArtifacts", a.Path})
		}
	}
	if len(claims) == 0 {
		return nil
	}

	cutoff := stepStart.Add(-1 * time.Second)
	var problems []string
	for _, c := range claims {
		hostPath := resolveClaimedPath(c.path, workspaceDir, projectDir)
		if hostPath == "" {
			problems = append(problems, fmt.Sprintf("%s %q: unresolvable path (outside workspace/project)", c.source, c.path))
			continue
		}
		info, err := os.Stat(hostPath)
		if err != nil && os.IsNotExist(err) && projectDir != "" {
			// Fallback: the agent may have written to the project workspace
			// (project/artifacts/out/X inside the container) but claimed the
			// file with a bare relative path (artifacts/out/X, no "project/"
			// prefix). resolveClaimedPath maps that to workspaceDir which
			// doesn't have the file. persistArtifacts source #3 walks
			// effectiveProjectDir/artifacts/out/ and WOULD have found it.
			// Before declaring "does not exist", try the same relative portion
			// under projectDir (incident be7e: janka jobspin-cz 2026-06-20
			// scan falsely failed after producing its output files).
			if strings.HasPrefix(hostPath, workspaceDir+string(os.PathSeparator)) {
				rel := strings.TrimPrefix(hostPath, workspaceDir+string(os.PathSeparator))
				if alt := safeJoinUnder(projectDir, rel); alt != "" {
					if altInfo, altErr := os.Stat(alt); altErr == nil {
						info = altInfo
						err = nil
						hostPath = alt
					}
				}
			}
		}
		if err != nil {
			if os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s %q: file does not exist at %s", c.source, c.path, hostPath))
			} else {
				problems = append(problems, fmt.Sprintf("%s %q: stat failed: %v", c.source, c.path, err))
			}
			continue
		}
		if info.IsDir() {
			problems = append(problems, fmt.Sprintf("%s %q: is a directory, not a file", c.source, c.path))
			continue
		}
		if info.ModTime().Before(cutoff) {
			problems = append(problems, fmt.Sprintf(
				"%s %q: mtime %s predates step start %s (file was not written during this step)",
				c.source, c.path,
				info.ModTime().UTC().Format(time.RFC3339),
				stepStart.UTC().Format(time.RFC3339),
			))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"agent claimed %d file(s) but verification failed: %s",
		len(claims),
		strings.Join(problems, "; "),
	)
}

// verifyClaimedModifications kept as a thin alias so external callers
// and existing tests don't break across the rename. New callers should
// use verifyClaimedFiles directly.
func (e *Executor) verifyClaimedModifications(resultBytes []byte, workspaceDir, projectDir string, stepStart time.Time) error {
	return e.verifyClaimedFiles(resultBytes, workspaceDir, projectDir, stepStart)
}

// resolveClaimedPath maps a claim entry to a host-side filesystem path.
// The agent container sees the project dir at /app/workspace/project and
// the per-task workspace at /app/workspace. Entries may be:
//
//   - Container-absolute under /app/workspace/project/... → host projectDir
//   - Container-absolute under /app/workspace/...         → host workspaceDir
//   - Relative "project/..."                              → host projectDir
//   - Any other relative path                             → host workspaceDir
//
// Paths that would escape projectDir or workspaceDir via `..` are rejected
// (returned as empty string, which the caller flags as "unresolvable").
func resolveClaimedPath(claim, workspaceDir, projectDir string) string {
	claim = strings.TrimSpace(claim)
	if claim == "" {
		return ""
	}
	const (
		projectAbs   = "/app/workspace/project/"
		workspaceAbs = "/app/workspace/"
	)
	var base, rel string
	switch {
	case claim == "/app/workspace/project" || claim == "/app/workspace":
		// Referring to the mount point itself is not a file.
		return ""
	case strings.HasPrefix(claim, projectAbs):
		base, rel = projectDir, strings.TrimPrefix(claim, projectAbs)
	case strings.HasPrefix(claim, workspaceAbs):
		base, rel = workspaceDir, strings.TrimPrefix(claim, workspaceAbs)
	case strings.HasPrefix(claim, "project/"):
		base, rel = projectDir, strings.TrimPrefix(claim, "project/")
	default:
		base, rel = workspaceDir, claim
	}
	if base == "" || rel == "" {
		return ""
	}
	return safeJoinUnder(base, rel)
}

// safeJoinUnder joins base+rel and returns "" if the result escapes base
// (e.g. via `..`). Symlinks are not followed — we want to catch the path
// claim at the level the agent made it, not wherever a link points.
func safeJoinUnder(base, rel string) string {
	cleanBase := filepath.Clean(base)
	joined := filepath.Clean(filepath.Join(cleanBase, rel))
	if joined == cleanBase {
		return ""
	}
	if !strings.HasPrefix(joined, cleanBase+string(os.PathSeparator)) {
		return ""
	}
	return joined
}
