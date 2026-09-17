package agentloop

import (
	"os"
	"path/filepath"
	"strings"
)

// errEscape is the workspace-confinement refusal, worded exactly as the
// entrypoint's resolve_path printed it.
type errEscape struct{ raw string }

func (e errEscape) Error() string { return "ERROR: path escapes workspace: " + e.raw }

// realpath is CPython's posixpath.realpath(strict=False): symlinks in every
// EXISTING component are resolved and the non-existent remainder is appended
// lexically. filepath.EvalSymlinks is not this — it fails on a path that does
// not exist — so the port walks back from the full path to the longest
// existing prefix, resolves that, and re-joins the tail
// (agent-tool dispatch design §3.1, resolve_path row).
func realpath(p string) string {
	p = filepath.Clean(p)
	prefix := p
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			if len(tail) == 0 {
				return resolved
			}
			parts := append([]string{resolved}, reverse(tail)...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent, last := filepath.Split(prefix)
		parent = filepath.Clean(parent)
		if parent == prefix || last == "" {
			return p
		}
		tail = append(tail, last)
		prefix = parent
	}
}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// resolvePath is the entrypoint's resolve_path: an absolute path inside the
// workspace is kept, an absolute path outside it is re-rooted under the
// workspace, a relative path is joined; the result is realpath'd and must
// stay inside the workspace.
func resolvePath(workspace, raw string) (string, error) {
	ws := realpath(workspace)
	var candidate string
	if filepath.IsAbs(raw) {
		if raw == ws || strings.HasPrefix(raw, ws+string(os.PathSeparator)) {
			candidate = raw
		} else {
			candidate = filepath.Join(ws, strings.TrimLeft(raw, string(os.PathSeparator)))
		}
	} else {
		candidate = filepath.Join(ws, deSlashedWorkspacePath(ws, raw))
	}
	resolved := realpath(filepath.Clean(candidate))
	if resolved != ws && !strings.HasPrefix(resolved, ws+string(os.PathSeparator)) {
		return "", errEscape{raw: raw}
	}
	return resolved, nil
}

// deSlashedWorkspacePath recovers an absolute container path whose leading
// slash the model dropped. "app/workspace/artifacts/in/x.md" with a workspace
// of "/app/workspace" is not a relative path INTO the workspace — it is the
// workspace's own absolute path, respelled. Joining it produces
// /app/workspace/app/workspace/artifacts/in/x.md, which never exists, and the
// entrypoint's repeat-miss guard then kills the step blaming an upstream role
// (incident 2026-09-16, exec_20260916164618_4c53e9deafd4e8e6: the same model
// spelled the same file three ways in one step and only this one missed).
//
// Returns raw unchanged unless it restates the workspace root on a COMPONENT
// boundary, so "app/workspace-other/x" is still an ordinary relative path.
// Confinement is not affected either way: the caller joins the result under
// the workspace and re-checks containment.
func deSlashedWorkspacePath(ws, raw string) string {
	deslashed := strings.TrimPrefix(filepath.Clean(ws), string(os.PathSeparator))
	if deslashed == "" || deslashed == "." {
		return raw
	}
	if raw == deslashed {
		return ""
	}
	if strings.HasPrefix(raw, deslashed+string(os.PathSeparator)) {
		return strings.TrimPrefix(raw, deslashed)
	}
	return raw
}

// isRegularFile is `[ -f path ]` / os.path.isfile: follows symlinks.
func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// toolResultsDir is the spill directory only tool_result_read may open.
func toolResultsDir(workspace string) string {
	return filepath.Join(realpath(workspace), ".tool_results") + string(os.PathSeparator)
}
