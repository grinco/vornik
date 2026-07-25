package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

const maxGitCGIHeaderBytes = 64 << 10

// gitWorkspaceRoot resolves the on-disk workspace directory for projectID.
// It sanitizes the project ID first (404-safe rejection of traversal), then
// confines it under Config.Runtime.ProjectWorkspacePath via safepath.JoinUnder.
// The returned path is only os.Stat'd (existence check) — the git subprocess
// uses the raw configured ProjectWorkspacePath + a PATH_INFO from the URL — so
// JoinUnder's symlink resolution of the return value is harmless here.
func (s *Server) gitWorkspaceRoot(projectID string) (string, error) {
	id, err := sanitizeGitProjectID(projectID)
	if err != nil {
		return "", err
	}
	if s.config == nil || s.config.Runtime.ProjectWorkspacePath == "" {
		return "", fmt.Errorf("gitWorkspaceRoot: ProjectWorkspacePath not configured")
	}
	return safepath.JoinUnder(s.config.Runtime.ProjectWorkspacePath, id)
}

// countingResponseWriter wraps an http.ResponseWriter and counts the bytes
// written to the body. It is used to record response size in the audit row.
//
// wroteHeader tracks whether the status line has been COMMITTED to the wire.
// The streaming CGI path (streamCGIResponse) sends headers before the body, so
// "bytesWritten == 0" no longer implies "nothing sent yet" — a mid-body failure
// with zero body bytes copied would otherwise let respondError graft a JSON
// envelope onto an already-200 git response.
type countingResponseWriter struct {
	http.ResponseWriter
	bytesWritten int64
	statusCode   int
	wroteHeader  bool
}

func (c *countingResponseWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	c.statusCode = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingResponseWriter) Write(b []byte) (int, error) {
	// net/http commits the 200 status line on the first Write; mirror that so
	// wroteHeader is accurate even when WriteHeader was never called explicitly.
	c.wroteHeader = true
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

	// Wrap the response writer to count bytes for the audit row.
	cw := &countingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		respondError(cw, http.StatusInternalServerError, "GIT_BACKEND_ERROR",
			"git http-backend pipe error: "+err.Error())
		s.writeGitAudit(r, rawID, "error", cw.bytesWritten)
		return
	}
	if err := cmd.Start(); err != nil {
		respondError(cw, http.StatusInternalServerError, "GIT_BACKEND_ERROR",
			"git http-backend start error: "+err.Error())
		s.writeGitAudit(r, rawID, "error", cw.bytesWritten)
		return
	}

	status, streamErr := streamCGIResponse(cw, stdout)
	if streamErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		// A client disconnect surfaces here as a copy error; it is not a
		// backend fault, so don't write a response or an "error" audit row.
		if r.Context().Err() != nil {
			return
		}
		if cw.wroteHeader {
			// Headers are already on the wire — a JSON envelope now would be
			// appended to the git response body under the git status code.
			// Log instead; the truncated body is all the client can get.
			s.logger.Warn().Err(streamErr).Str("project_id", rawID).
				Msg("git http-backend: response truncated after headers were sent")
		} else {
			respondError(cw, http.StatusInternalServerError, "GIT_BACKEND_ERROR",
				"git http-backend error: malformed CGI output: "+streamErr.Error())
		}
		s.writeGitAudit(r, rawID, "error", cw.bytesWritten)
		return
	}
	execErr := cmd.Wait()
	if execErr != nil {
		if r.Context().Err() == context.Canceled {
			return
		}
		// The response is already fully streamed, so this can only be logged.
		// Without it a non-zero git exit is invisible outside the audit row.
		s.logger.Warn().Err(execErr).Str("project_id", rawID).
			Msg("git http-backend exited non-zero after streaming its response")
		s.writeGitAudit(r, rawID, "error", cw.bytesWritten)
		return
	}

	result := "ok"
	if status >= 400 {
		result = "error"
	}
	s.writeGitAudit(r, rawID, result, cw.bytesWritten)
}

func streamCGIResponse(w http.ResponseWriter, src io.Reader) (int, error) {
	// Size the buffer AT the cap and read with ReadSlice, not ReadString:
	// ReadString accumulates an entire line before returning, so a newline-free
	// stream is fully buffered in memory before any length check can run —
	// defeating maxGitCGIHeaderBytes. ReadSlice reports ErrBufferFull instead.
	br := bufio.NewReaderSize(src, maxGitCGIHeaderBytes)
	var header bytes.Buffer
	for {
		// line aliases br's internal buffer and is invalidated by the next
		// read, so it is copied into header before looping.
		line, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return 0, fmt.Errorf("CGI header line exceeds %d bytes", maxGitCGIHeaderBytes)
			}
			return 0, fmt.Errorf("read CGI headers: %w", err)
		}
		if header.Len()+len(line) > maxGitCGIHeaderBytes {
			return 0, fmt.Errorf("CGI headers exceed %d bytes", maxGitCGIHeaderBytes)
		}
		header.Write(line)
		if string(line) == "\n" || string(line) == "\r\n" {
			break
		}
	}

	hdrs, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(header.Bytes()))).ReadMIMEHeader()
	if err != nil {
		return 0, fmt.Errorf("parse CGI headers: %w", err)
	}
	status := http.StatusOK
	if raw := hdrs.Get("Status"); raw != "" {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			return 0, fmt.Errorf("invalid CGI Status %q", raw)
		}
		code, err := strconv.Atoi(fields[0])
		if err != nil || code < 100 || code > 999 {
			return 0, fmt.Errorf("invalid CGI Status %q", raw)
		}
		status = code
		hdrs.Del("Status")
	}
	for key, vals := range hdrs {
		for _, value := range vals {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, br); err != nil {
		return status, fmt.Errorf("stream CGI body: %w", err)
	}
	return status, nil
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
