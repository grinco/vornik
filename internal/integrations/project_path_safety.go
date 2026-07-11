package integrations

import (
	"errors"
	"fmt"
	"regexp"

	"vornik.io/vornik/internal/safepath"
)

// ErrInvalidProjectID is wrapped by every error validateProjectIDForPath
// returns (companion review review-20260709-9160 finding 3): callers that
// need to prove a rejection came from THIS gate specifically — rather than
// from some other, incidentally similarly-worded error elsewhere in the call
// chain — assert errors.Is(err, ErrInvalidProjectID) instead of matching on
// error-string substrings.
var ErrInvalidProjectID = errors.New("integrations: invalid project ID for path use")

// projectIDPathCharset is the allowed project-ID charset when a project ID
// is used to build a filesystem path: ASCII alphanumerics, underscore,
// dash. Mirrors internal/api/git_http_auth.go's gitProjectIDRe — the
// existing precedent for "a project ID is about to become a path
// component" — duplicated rather than imported: internal/api already
// depends on internal/integrations (the eventual hub HTTP handlers), so the
// reverse import would cycle. internal/api/git_http.go's gitSafeJoinUnder
// documents the same duplication trade-off for the analogous safe-join
// helper, so this isn't a new pattern.
var projectIDPathCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateProjectIDForPath rejects a project ID before it is ever used to
// build a filesystem path: empty, or containing any character outside
// projectIDPathCharset (which by construction excludes "/", "\", and "."
// — so "..", "../etc/passwd", and absolute-path shapes are all rejected by
// the charset check alone). Security-audit finding F-1 (2026-07-09,
// path-confinement): every project-scope path this package builds from a
// caller-influenced ProjectID (projects/<id>.yaml, the GitHub App PEM file)
// must reject a malformed ID here AND go through safepath.JoinUnder for
// the actual join — belt-and-suspenders, since the charset check alone
// already can't produce a traversal shape, but JoinUnder also defends
// against a future caller that forgets to validate first.
func validateProjectIDForPath(projectID string) error {
	if projectID == "" {
		return fmt.Errorf("%w: project ID is empty", ErrInvalidProjectID)
	}
	if !projectIDPathCharset.MatchString(projectID) {
		return fmt.Errorf("%w: project ID %q contains characters outside the allowed charset [A-Za-z0-9_-]", ErrInvalidProjectID, projectID)
	}
	return nil
}

// safeProjectPath validates projectID then joins elems onto root via
// safepath.JoinUnder, so a project-scope path can never resolve outside
// root even if the charset check above were somehow bypassed (defense in
// depth, not the only gate).
func safeProjectPath(root, projectID string, elems ...string) (string, error) {
	if err := validateProjectIDForPath(projectID); err != nil {
		return "", err
	}
	return safepath.JoinUnder(root, elems...)
}
