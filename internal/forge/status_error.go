package forge

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// PermanentError is returned by a forge provider when the remote's response
// says the request can never succeed as issued — the PR does not exist, the
// installation was revoked, the payload is malformed. It exists so the retry
// ladder can stop spending attempts on a call that cannot come good.
//
// WHY A TYPE AND NOT A CLASSIFIER PATTERN. Both classifiers in the tree refuse
// to guess at upstream error strings, and both are right to:
// internal/executor/failure_classifier.go carries no HTTP patterns, and
// internal/scheduler/failure_class.go says outright that guessing "would
// produce confidently wrong classes, which is worse for an operator than
// UNKNOWN". So permanence is decided HERE, where the status code is in hand,
// and travels as a type. Same shape and same reason as PushRejectedError above,
// and as the DELEGATION_GUARD class the playbook describes as "authoritative,
// not text-matched".
//
// See https://docs.vornik.io
type PermanentError struct {
	// Status is the HTTP status that decided permanence.
	Status int
	// Op names the forge operation, for the operator-facing message.
	Op string
	// Detail is the remote's response excerpt. Never a credential: callers
	// pass the same excerpt they already log.
	Detail string
	// Err is the underlying error, if any.
	Err error
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("%s: permanent failure HTTP %d: %s", e.Op, e.Status, e.Detail)
}

func (e *PermanentError) Unwrap() error { return e.Err }

// AsPermanent reports whether err is (or wraps) a *PermanentError.
//
// Deliberately typed-only. A flattened "HTTP 404" in a message string must NOT
// report permanent — matching that text is precisely the shortcut this replaces,
// and it would make an ordinary error carrying those characters terminal.
func AsPermanent(err error) (*PermanentError, bool) {
	var pe *PermanentError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// StatusIsPermanent decides whether an HTTP status from a forge can ever
// succeed on a retry. Headers are consulted only for the 403 split; pass nil
// when there are none.
//
// The default for an unreasoned-about status is TRANSIENT, and that direction
// is chosen deliberately. The failure modes are asymmetric: wrongly permanent
// fails recoverable work on the first attempt and loses it, while wrongly
// transient is today's behaviour and merely wastes calls. Defaulting to
// transient means this can only ever REMOVE futile retries, never add a new way
// to lose work.
func StatusIsPermanent(status int, header http.Header) bool {
	switch status {
	case http.StatusNotFound, http.StatusGone:
		// The target does not exist. A second look cannot change that.
		return true
	case http.StatusUnauthorized:
		// Bad app credentials or a revoked installation. NOT an expired token:
		// Provider.token() refreshes against a TTL buffer BEFORE each request,
		// so a token within the buffer of expiry is re-minted rather than used.
		return true
	case http.StatusForbidden:
		// The one judgement call — GitHub uses 403 for both "you may not" and
		// (historically) secondary rate limits.
		return !looksRateLimited(header)
	case http.StatusUnprocessableEntity:
		// Malformed request. Deterministic: the same body fails the same way.
		return true
	default:
		// 429 (told to come back), 5xx (their problem), and everything
		// unreasoned-about.
		return false
	}
}

// looksRateLimited reports whether a 403's headers mark it as a secondary
// rate limit rather than an access denial.
func looksRateLimited(header http.Header) bool {
	if header == nil {
		return false
	}
	if strings.TrimSpace(header.Get("Retry-After")) != "" {
		return true
	}
	// Exhausted budget — and ONLY exhausted. A remaining budget of 4999 is a
	// healthy rate limit and says nothing about why the call was refused.
	return strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0"
}

// NewStatusError builds the right error for a forge HTTP failure: a
// *PermanentError when the status can never succeed, otherwise a plain error
// with the same message shape callers logged before.
func NewStatusError(op string, status int, header http.Header, detail string) error {
	if StatusIsPermanent(status, header) {
		return &PermanentError{Status: status, Op: op, Detail: detail}
	}
	return fmt.Errorf("%s HTTP %d: %s", op, status, detail)
}
