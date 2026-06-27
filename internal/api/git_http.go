package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// gitSafeJoinUnder joins base+rel and returns ("", error) if the result
// escapes base (e.g. via `..`).  Symlinks are not followed.
// Mirrors internal/executor.safeJoinUnder but is unexported there.
func gitSafeJoinUnder(base, rel string) (string, error) {
	cleanBase := filepath.Clean(base)
	joined := filepath.Clean(filepath.Join(cleanBase, rel))
	if joined == cleanBase {
		return "", fmt.Errorf("gitSafeJoinUnder: %q resolves to base", rel)
	}
	if !strings.HasPrefix(joined, cleanBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("gitSafeJoinUnder: %q escapes base %q", rel, base)
	}
	return joined, nil
}

// gitWorkspaceRoot resolves the on-disk workspace directory for projectID.
// It sanitizes the project ID first (404-safe rejection of traversal), then
// joins it under Config.Runtime.ProjectWorkspacePath.
func (s *Server) gitWorkspaceRoot(projectID string) (string, error) {
	id, err := sanitizeGitProjectID(projectID)
	if err != nil {
		return "", err
	}
	if s.config == nil || s.config.Runtime.ProjectWorkspacePath == "" {
		return "", fmt.Errorf("gitWorkspaceRoot: ProjectWorkspacePath not configured")
	}
	return gitSafeJoinUnder(s.config.Runtime.ProjectWorkspacePath, id)
}

// countingResponseWriter wraps an http.ResponseWriter and counts the bytes
// written to the body. It is used to record response size in the audit row.
type countingResponseWriter struct {
	http.ResponseWriter
	bytesWritten int64
	statusCode   int
}

func (c *countingResponseWriter) WriteHeader(code int) {
	c.statusCode = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingResponseWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.bytesWritten += int64(n)
	return n, err
}

// GitHTTPBackend is the git smart-HTTP handler for BOTH the read (upload-pack)
// and write (receive-pack) paths. It execs git-http-backend as a CGI child,
// forwarding the request body to its stdin and streaming the CGI response
// (status + headers + body) back to the client.
//
// GIT_PROJECT_ROOT/PATH_INFO mapping:
//
//	The client URL is /api/v1/git/{projectID}.git/{suffix}.  On disk the repo
//	lives at <ProjectWorkspacePath>/<projectID> (no ".git" suffix).  We set:
//	  GIT_PROJECT_ROOT = <ProjectWorkspacePath>
//	  PATH_INFO        = /{projectID}/{suffix}   (no ".git")
//
//	git-http-backend resolves GIT_PROJECT_ROOT + PATH_INFO[1:] =
//	  <ProjectWorkspacePath>/<projectID>  ← the repo directory.
//
// Workspace-lock safety model (Task 2.4, design §4.3/§4.4):
//
//   - READ (upload-pack): take the shared RLock for the whole invocation so a
//     fetch never reads the tree mid-`reset --hard` from a concurrent task.
//   - PUSH (receive-pack): take the EXCLUSIVE Lock for the WHOLE invocation
//     (held across the buffered git subprocess). Holding it, fast-fail with
//     503 when the project has an active (RUNNING/LEASED) task, then re-assert
//     the push guards, then exec. The lock — not the active-task check — is the
//     safety mechanism: every executor mutation takes the same per-project
//     lock, so a task leased mid-push blocks at its first mutation and the
//     tree stays clean for updateInstead.
func (s *Server) GitHTTPBackend(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("projectID")
	service, _ := r.Context().Value(gitServiceCtxKey{}).(gitService)

	wsRoot, err := s.gitWorkspaceRoot(rawID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Verify the repo directory exists; 404 if not.
	if _, statErr := os.Stat(wsRoot); statErr != nil {
		http.NotFound(w, r)
		return
	}

	// Per-project opt-in gate (design §4.5): expose git routes ONLY when the
	// project's Git.Enabled flag is set. This runs AFTER auth (the key holder
	// already owns the project, so a 404 here leaks nothing) and BEFORE any
	// exec. We respond with a plain 404 (no audit row) to match the
	// cross-project mismatch 404 in gitHTTPAuth.
	//
	// nil-registry skip: unit tests that don't wire a registry treat the
	// project as enabled so the existing git handler tests keep passing. The
	// production container ALWAYS wires WithProjectRegistry (container_http.go),
	// so the gate is always active in prod.
	if s.projectRegistry != nil {
		if p := s.projectRegistry.GetProject(rawID); p == nil || !p.Git.Enabled {
			http.NotFound(w, r)
			return
		}
	}

	// Acquire the per-project workspace lock for the ENTIRE invocation and
	// release on return (covers the buffered cmd.Output() exec). Push takes
	// the exclusive Lock + runs the gate; read takes the shared RLock.
	if service == gitServiceReceive {
		unlock := s.workspaceLock.Lock(rawID)
		defer unlock()
		// Holding the exclusive lock, run the active-task fast-fail (503) +
		// idempotent guard re-assert. done==true means the response is
		// already written; abort before exec.
		if done := s.gateReceivePack(w, r, rawID); done {
			return
		}
	} else {
		unlock := s.workspaceLock.RLock(rawID)
		defer unlock()
	}

	// GIT_PROJECT_ROOT is the workspace root (parent of the per-project dir).
	// Build PATH_INFO by stripping the URL prefix and the ".git" suffix so
	// git-http-backend maps to the on-disk directory name (no ".git").
	gitProjectRoot := s.config.Runtime.ProjectWorkspacePath

	// urlPath  = /api/v1/git/proj_clone.git/info/refs
	// pathInfo = /proj_clone/info/refs
	const apiPrefix = "/api/v1/git/"
	afterPrefix := strings.TrimPrefix(r.URL.Path, apiPrefix)
	pathInfo := "/" + strings.Replace(afterPrefix, rawID+".git", rawID, 1)

	env := buildGitCGIEnv(r, gitProjectRoot, pathInfo)

	cmd := exec.CommandContext(r.Context(), "git", "http-backend") //nolint:gosec
	cmd.Env = env
	cmd.Stdin = r.Body

	out, execErr := cmd.Output()

	// Wrap the response writer to count bytes for the audit row.
	cw := &countingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

	if execErr != nil {
		if r.Context().Err() == context.Canceled {
			return
		}
		// FIX 4: JSON envelope for parity with the auth-path 4xx/503 responses.
		// Safe here: nothing has been written to the response yet (the CGI
		// output is buffered via cmd.Output()), so headers are not yet sent.
		respondError(cw, http.StatusInternalServerError, "GIT_BACKEND_ERROR",
			"git http-backend error: "+execErr.Error())
		s.writeGitAudit(r, rawID, "error", cw.bytesWritten)
		return
	}

	// Parse CGI output: split at the blank line, extract headers + body.
	status, hdrs, body, parseErr := parseCGIOutput(out)
	if parseErr != nil {
		// FIX 4: JSON envelope (same safety rationale as above).
		respondError(cw, http.StatusInternalServerError, "GIT_BACKEND_ERROR",
			"git http-backend: malformed CGI output: "+parseErr.Error())
		s.writeGitAudit(r, rawID, "error", cw.bytesWritten)
		return
	}

	for key, vals := range hdrs {
		for _, v := range vals {
			cw.Header().Add(key, v)
		}
	}
	cw.WriteHeader(status)
	_, _ = io.Copy(cw, bytes.NewReader(body))

	result := "ok"
	if status >= 400 {
		result = "error"
	}
	s.writeGitAudit(r, rawID, result, cw.bytesWritten)
}

// gateReceivePack runs the push gate while the caller holds the exclusive
// workspace lock: (1) a 503 fast-fail when the project has an active
// (RUNNING/LEASED) task, and (2) the idempotent push-guard re-assert. It
// returns done==true when it has written the response and the caller must
// abort before exec.
//
// The active-task check is an optimisation, NOT the safety boundary (the lock
// is): every executor mutation takes the same per-project lock, so a task
// leased mid-push blocks at its first mutation and the tree stays clean for
// updateInstead. The 503 just gives the pusher an immediate, actionable answer
// instead of blocking — possibly for minutes — until a long task releases.
func (s *Server) gateReceivePack(w http.ResponseWriter, r *http.Request, projectID string) (done bool) {
	if s.taskRepo != nil {
		counts, ctErr := s.taskRepo.CountByStatus(r.Context(), projectID)
		if ctErr == nil &&
			counts[persistence.TaskStatusRunning]+counts[persistence.TaskStatusLeased] > 0 {
			w.Header().Set("Retry-After", "5")
			respondError(w, http.StatusServiceUnavailable, "ACTIVE_TASK",
				"push rejected: project has an active task (RUNNING/LEASED); retry shortly")
			s.writeGitAudit(r, projectID, "rejected", 0)
			return true
		}
	}

	// Re-assert the push guards (idempotent) before exec so repos that predate
	// the guard install — or were hand-tampered — are protected.
	if s.gitReceiveGuards != nil {
		if guardErr := s.gitReceiveGuards(r.Context(), projectID); guardErr != nil {
			http.Error(w, "git push setup error: "+guardErr.Error(), http.StatusInternalServerError)
			s.writeGitAudit(r, projectID, "error", 0)
			return true
		}
	}
	return false
}

// writeGitAudit writes one admin_audit row for a git smart-HTTP read request.
// It is a best-effort write: errors are silently dropped (the caller has
// already written the response). When adminAuditRepo is nil (test or minimal
// deployment) the call is a no-op.
func (s *Server) writeGitAudit(r *http.Request, projectID, result string, bytesWritten int64) {
	if s.adminAuditRepo == nil {
		return
	}
	// Resolve the principal: use the key ID stamped by gitHTTPAuth, or
	// "anonymous" when auth is disabled (nil key on context).
	principal := "anonymous"
	if key, _ := r.Context().Value(gitKeyCtxKey{}).(*persistence.APIKey); key != nil {
		principal = key.ID
	}
	// Resolve the service from context so push rows are distinguishable
	// from read rows (Action git.receive-pack vs git.upload-pack).
	service, _ := r.Context().Value(gitServiceCtxKey{}).(gitService)
	serviceName := "upload-pack"
	action := "git.upload-pack"
	if service == gitServiceReceive {
		serviceName = "receive-pack"
		action = "git.receive-pack"
	}
	afterJSON, _ := json.Marshal(map[string]any{
		"service": serviceName,
		"bytes":   bytesWritten,
		"result":  result,
	})
	entry := &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Principal: principal,
		Source:    "git",
		Action:    action,
		Target:    projectID,
		After:     string(afterJSON),
		IP:        clientIPFromRequest(r),
		UserAgent: r.UserAgent(),
	}
	_ = s.adminAuditRepo.Insert(r.Context(), entry)
}

// buildGitCGIEnv constructs the minimal CGI environment for git-http-backend.
// Only variables required by the CGI spec + git are included; the process
// environment is not inherited to keep the child isolated.
func buildGitCGIEnv(r *http.Request, gitProjectRoot, pathInfo string) []string {
	remoteUser, _ := r.Context().Value(gitRemoteUserCtxKey{}).(string)
	if remoteUser == "" {
		remoteUser = "anonymous"
	}

	env := []string{
		"GIT_PROJECT_ROOT=" + gitProjectRoot,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=" + pathInfo,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"REMOTE_USER=" + remoteUser,
	}

	if ct := r.Header.Get("Content-Type"); ct != "" {
		env = append(env, "CONTENT_TYPE="+ct)
	}
	if cl := r.Header.Get("Content-Length"); cl != "" {
		env = append(env, "CONTENT_LENGTH="+cl)
	} else if r.ContentLength >= 0 {
		env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	}
	if gp := r.Header.Get("Git-Protocol"); gp != "" {
		env = append(env, "GIT_PROTOCOL="+gp)
	}

	// Provide PATH so git can locate sub-helpers.
	if p := os.Getenv("PATH"); p != "" {
		env = append(env, "PATH="+p)
	}

	return env
}

// parseCGIOutput splits raw CGI bytes from git-http-backend into an HTTP
// status code, response headers, and the body.
//
// CGI output (RFC 3875):
//
//	Header: value\r\n
//	Header: value\r\n
//	\r\n              ← blank line (may be \r\n or \n) separates headers from body
//	<binary body>
//
// git-http-backend uses \r\n line endings throughout.
func parseCGIOutput(raw []byte) (status int, hdrs http.Header, body []byte, err error) {
	status = http.StatusOK
	hdrs = make(http.Header)

	// Locate the blank-line separator.  Prefer \r\n\r\n (what git sends).
	var headerEnd, bodyStart int
	if idx := bytes.Index(raw, []byte("\r\n\r\n")); idx >= 0 {
		headerEnd = idx
		bodyStart = idx + 4
	} else if idx := bytes.Index(raw, []byte("\n\n")); idx >= 0 {
		headerEnd = idx
		bodyStart = idx + 2
	} else {
		// No blank line — all headers, no body.
		headerEnd = len(raw)
		bodyStart = len(raw)
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw[:headerEnd]))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			return 0, nil, nil, fmt.Errorf("malformed CGI header line: %q", line)
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if strings.EqualFold(key, "Status") {
			// "Status: 200 OK" or "Status: 404 Not Found"
			parts := strings.SplitN(val, " ", 2)
			code, e := strconv.Atoi(parts[0])
			if e != nil {
				return 0, nil, nil, fmt.Errorf("invalid CGI Status value: %q", val)
			}
			status = code
			continue
		}
		hdrs.Add(key, val)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return 0, nil, nil, scanErr
	}

	return status, hdrs, raw[bodyStart:], nil
}
