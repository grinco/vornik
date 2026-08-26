package mcp

import (
	"errors"
	"fmt"
	"net/http"
)

// FailureClass names WHY a tool call failed, in terms an operator can act on
// and a workflow can gate against.
//
// It exists because tool failures were untyped. `tool_audit_log` stored the
// tool's output verbatim with no status, so a 401 was just text in a blob and
// nothing could alert, count, or gate on a class of failure it could not name
// — gap 1 of the 2026-08-25 P0 (a connector that lost auth degraded into a
// success-shaped task). agentbench.callFailure inferred failure by sniffing
// that text and said so in its own comment: "That is lossy."
//
// The class is derived from the TRANSPORT — an HTTP status, or a typed
// sentinel from the credential layer — never from message text. Relocating the
// sniffing would not have fixed anything.
//
// Design: https://docs.vornik.io §3.2.
type FailureClass string

const (
	// FailureAuth — the vendor rejected the credential (401/403), or the
	// daemon could not produce one. A human must reconnect; retrying does
	// not help. This is the class that fails a step outright (§3.3a).
	FailureAuth FailureClass = "auth"
	// FailureRateLimit — 429. Transient by construction; the caller backs off.
	FailureRateLimit FailureClass = "rate_limit"
	// FailureTransport — no HTTP response at all: dial failure, timeout,
	// TLS error, a dead stdio subprocess.
	FailureTransport FailureClass = "transport"
	// FailureServer — 5xx. The vendor is broken, not us.
	FailureServer FailureClass = "server"
	// FailureNotFound — 404. Usually a misconfigured URL or a withdrawn tool.
	FailureNotFound FailureClass = "not_found"
	// FailureInvalidRequest — any other 4xx: a malformed call, a bad
	// argument, a rejected payload. The one class that is plausibly the
	// agent's own fault.
	FailureInvalidRequest FailureClass = "invalid_request"
)

// ClassifyHTTPStatus maps an HTTP status to a FailureClass.
//
// 403 is auth, not authorization-as-policy: an OAuth grant whose scopes were
// reduced at the vendor answers 403, and the fix is the same reconnect a 401
// needs. A tool the ROLE is not allowed to call never reaches the vendor — the
// daemon refuses it at roleAllowsMCPTool — so a 403 here is always the
// credential.
func ClassifyHTTPStatus(status int) FailureClass {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return FailureAuth
	case status == http.StatusTooManyRequests:
		return FailureRateLimit
	case status == http.StatusNotFound:
		return FailureNotFound
	case status >= 500:
		return FailureServer
	case status >= 400:
		return FailureInvalidRequest
	default:
		return FailureTransport
	}
}

// CallError is a typed MCP tool-call failure.
//
// The untrusted upstream body is deliberately NOT carried: mcpHTTPStatusError
// has always logged it truncated and returned a fixed-shape status-only error,
// and that boundary holds — a vendor error body reaches an agent's context
// otherwise.
type CallError struct {
	Server string
	Tool   string
	// Status is the HTTP status, or 0 when the failure had no HTTP response
	// (dial error, timeout, stdio subprocess death, unresolvable credential).
	Status int
	Class  FailureClass
	// Err is the underlying cause, when there is one worth unwrapping —
	// notably mcpconnect.ErrNeedsReconnect, which callers match with
	// errors.Is to tell "reconnect me" apart from "the vendor said no".
	Err error
}

func (e *CallError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("mcp: server %q tool %q returned %d (%s)",
			e.Server, e.Tool, e.Status, e.Class)
	}
	if e.Err != nil {
		return fmt.Sprintf("mcp: server %q tool %q failed (%s): %v", e.Server, e.Tool, e.Class, e.Err)
	}
	return fmt.Sprintf("mcp: server %q tool %q failed (%s)", e.Server, e.Tool, e.Class)
}

func (e *CallError) Unwrap() error { return e.Err }

// IsAuthFailure reports whether err is (or wraps) a credential rejection.
//
// Deliberately narrow: it matches the TYPE, never the message. An error whose
// text happens to contain "401" is not an auth failure — believing otherwise is
// the lossiness this design removes.
func IsAuthFailure(err error) bool {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.Class == FailureAuth
	}
	return false
}

// ClassOf returns the FailureClass of err, and whether err carried one at all.
// A nil error is (", false)" — "no class" and "class unknown" are the same
// answer here, and the caller distinguishes them by checking err first.
func ClassOf(err error) (FailureClass, bool) {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.Class, true
	}
	return "", false
}
