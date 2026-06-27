// Package auth defines the pluggable authentication backend
// interface AuthMiddleware will delegate to once the middleware
// refactor in slice 2 lands. The interface is the gating artifact
// for the larger 2026.6.0 milestone — once it's stable, OIDC,
// SAML, and any future identity protocol can land as separate
// implementations without disturbing the request-routing layer.
//
// Today's authentication surfaces (static API keys, DB-backed
// per-project keys, HMAC webhook signatures) each correspond to
// one Backend implementation. The middleware walks a chain of
// backends in order; the first one that returns a non-nil
// Identity wins. Failure to find a matching backend → the
// caller's UNAUTHORIZED response path stays unchanged.
//
// 2026.6.0 — companion design doc:
// https://docs.vornik.io
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Role vocabulary. These role-name constants are the CE-visible identity
// vocabulary: CE UI/admin-gate code branches on them (RoleUser sees a
// narrowed surface; RoleAdmin is instance-wide), and the EE RBAC resolver
// (internal/enterprise/identity/authz) re-uses them when computing a
// principal's effective role. Re-homed to CE in Phase 2c so the UI no longer
// imports the EE authz package — the constants are pure data, no IP, and the
// role split is a Community concept (admin vs scoped user) even when the EE
// OIDC/RBAC machinery that *populates* a session's role is absent.
//
// Admin is instance-wide; user is scoped by the Identity's Projects.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Session Extra keys. These are the CE-visible contract between whatever
// backend authenticates a browser-login cookie (the EE SessionBackend in
// internal/enterprise/identity stamps them) and the CE request middleware
// that reads them off Identity.Extra to derive the session role/id for the
// admin gate and logout. The keys stay CE in Phase 2c because internal/api
// reads them while consuming the session backend itself only as an opaque
// auth.Backend — the EE backend references these constants (EE→CE legal).
const (
	ExtraSessionRole   = "session_role"    // "admin" | "user"
	ExtraSessionID     = "session_id"      // ui_sessions.id (logout)
	ExtraSessionUserID = "session_user_id" // users.id
)

// Backend resolves an inbound credential into an Identity. Each
// implementation owns ONE credential family (static keys,
// DB-backed bearer tokens, OIDC ID tokens, SAML assertions,
// webhook HMAC signatures, …). The middleware doesn't know the
// difference — it just walks the chain.
type Backend interface {
	// Name returns a short stable identifier for this backend
	// (e.g. "static-keys", "db-keys", "oidc", "hmac-webhook").
	// Surfaces in logs and audit rows so operators can tell
	// "which path authenticated this request".
	Name() string

	// Authenticate inspects the inbound credential and either:
	//   - returns (Identity, nil)        on success
	//   - returns (nil, ErrNoCredential) when this backend has
	//     no opinion (the credential isn't its shape — e.g.
	//     the OIDC backend sees a sk-vornik-* bearer; the
	//     middleware then tries the next backend in the chain)
	//   - returns (nil, ErrUnauthorized) when this backend
	//     RECOGNISED the credential shape but rejected it
	//     (expired token, revoked key, bad signature). The
	//     middleware terminates the walk on this error so a
	//     rejected OIDC token doesn't accidentally fall through
	//     to the static-keys path and succeed via collision.
	//   - returns (nil, ErrBackendUnavailable) — wrapping the
	//     underlying cause — for transport / dependency errors
	//     the backend KNOWS are transient and wants to degrade
	//     past (a DB blip on the DB-keys backend shouldn't lock
	//     out static-keys callers). The chain falls through to
	//     the next backend.
	//   - returns (nil, err) for any OTHER error. The chain FAILS
	//     CLOSED on it: an unclassified error from a backend (a
	//     misconfigured OIDC provider, a coding bug) must NOT
	//     silently downgrade the request to a later backend and
	//     admit it. Backends that want graceful degrade must say
	//     so explicitly via ErrBackendUnavailable.
	Authenticate(ctx context.Context, cred Credential) (*Identity, error)
}

// Credential is the inbound auth material the middleware
// presents to every Backend in the chain. The full HTTP request
// isn't passed because backends shouldn't need it — that would
// invite leaks of request body / cookies into auth decisions.
// The middleware extracts the relevant headers and constructs
// this struct.
type Credential struct {
	// BearerToken is the value after "Authorization: Bearer ",
	// also accepting the legacy X-API-Key header form.
	// Empty when no bearer was presented.
	BearerToken string

	// HMACPresent records that the request bears a webhook HMAC
	// signature header (X-Vornik-Signature or X-Hub-Signature-256)
	// that its path's handler will verify. The signature VALUE is
	// deliberately not carried here — the handler re-reads the
	// header and verifies it against the source's secret; backends
	// only need the presence signal. (Until 2026-06-07 this was a
	// string field carrying the literal "present"; the slice-2
	// review flagged the marker-in-a-value-field shape and the
	// migration landed before the OIDC providers add backends to
	// the chain, as that note prescribed.)
	//
	// CONTRACT: set this ONLY when the request bears the
	// signature header its path's handler will actually verify
	// (hasWebhookSignatureForPath in internal/api) — never for
	// arbitrary requests that happen to carry a signature header.
	// HMACWebhookBackend treats true as "admit without a key" and
	// does NOT re-check the path itself.
	HMACPresent bool

	// Path is the URL path of the request. Backends that scope
	// to specific paths (HMAC webhook backend gates on
	// /api/v1/webhooks/) read this field; everyone else
	// ignores it.
	Path string

	// SessionToken is the raw value of the vornik_session cookie.
	// Empty when no cookie was presented. The middleware extracts
	// it; SessionBackend validates. NOTE: an invalid/expired token
	// returns ErrNoCredential, NOT ErrUnauthorized — a stale
	// browser cookie must not block a valid Bearer on the same
	// request, and a random session token cannot collide into the
	// key backends. The middleware's browser path handles the
	// redirect-to-login.
	SessionToken string
}

// Identity is the resolved authenticated principal. The
// middleware stashes this on the request context; downstream
// handlers (cost attribution, IDOR guards, audit) read from it.
type Identity struct {
	// Subject is the platform-stable user identifier. For
	// static-keys this is the key string itself; for DB-keys
	// it's the api_keys.id; for OIDC it's the `sub` claim;
	// for HMAC webhooks it's "webhook:<request path>". Used as the
	// audit-trail principal.
	Subject string

	// Backend is the Name() of the Backend that authenticated
	// this request. Surfaces in audit rows so operators can
	// tell "this row was authenticated via the OIDC path".
	Backend string

	// Projects is the set of project IDs this Identity may
	// access. Empty slice means "all projects" (the legacy
	// static-keys "no list" semantics); non-empty acts as an
	// exact-match whitelist. ProjectAuthMiddleware enforces
	// the IDOR guard against this list.
	Projects []string

	// BoundProjectID, when non-empty, is the SINGLE project
	// this Identity is bound to and CANNOT override via the
	// X-Vornik-Project-ID header. DB-backed keys with
	// project_id != "" set this; legacy static-keys leave it
	// empty.
	BoundProjectID string

	// DisplayName is operator-friendly (OIDC `name` claim,
	// static-keys "static:<prefix>", DB-keys's api_keys.name).
	DisplayName string

	// IssuedAt is the credential's mint time, when available.
	// Empty for credentials that don't carry a timestamp
	// (static keys).
	IssuedAt time.Time

	// ExpiresAt is the credential's expiry, when set. Backends
	// that don't honour expiry (e.g. static keys) leave it
	// zero. The middleware doesn't re-check expiry — that's
	// the backend's job during Authenticate.
	ExpiresAt time.Time

	// Extra carries backend-specific metadata that downstream
	// code may consume (e.g. OIDC's full claim set, the
	// per-key rate-limit columns from DB-keys). The middleware
	// is opaque to its contents. Future code paths that need
	// OIDC-claim-specific behaviour read from this map.
	Extra map[string]any
}

// Sentinel errors. Backends return these so the middleware can
// decide whether to fall through to the next backend (no
// opinion) or short-circuit (recognised but rejected).
var (
	// ErrNoCredential means this backend has no opinion on the
	// presented credential — the credential isn't its shape.
	// The middleware tries the next backend in the chain.
	ErrNoCredential = errors.New("auth: backend has no opinion on this credential")

	// ErrUnauthorized means this backend RECOGNISED the
	// credential shape but rejected it (expired, revoked, bad
	// signature, etc.). The middleware terminates the walk and
	// returns 401 — does NOT fall through, otherwise a rejected
	// OIDC token could collide-match a static key.
	ErrUnauthorized = errors.New("auth: credential rejected")

	// ErrBackendUnavailable is the EXPLICIT graceful-degrade
	// signal: this backend hit a transient dependency error (DB
	// blip, resolver timeout) it has classified as safe to fall
	// through. The chain tries the next backend, exactly as it
	// does for ErrNoCredential. Backends wrap their underlying
	// cause (errors.Join / %w) so the real error stays loggable.
	//
	// This sentinel exists so the chain can FAIL CLOSED on every
	// OTHER (unclassified) backend error without losing the
	// legitimate "a flaky DB-keys backend shouldn't lock out
	// static-keys callers" property — the backend opts into the
	// degrade rather than the chain guessing from a bare error.
	ErrBackendUnavailable = errors.New("auth: backend unavailable (transient)")
)

// Chain is the ordered list of Backends the middleware walks.
// Stops at the first Authenticate that returns a non-nil
// Identity. Returns the matching backend's Identity + name on
// success, or (nil, ErrUnauthorized) when every backend
// returned ErrNoCredential (or ErrBackendUnavailable) or any
// single one returned ErrUnauthorized.
//
// FAIL-CLOSED contract (hardening 2026-06-15, AUDIT batch-2
// "auth chain should fail closed"): only ErrNoCredential and the
// explicit ErrBackendUnavailable degrade sentinel fall through to
// the next backend. ANY other error short-circuits the walk and
// is returned to the caller (which renders 401 / login redirect),
// so a misconfigured backend — e.g. a broken OIDC provider that
// returns a bare error — can never silently downgrade the request
// to a later backend (static keys) and admit it via collision.
//
// Implementations of Backend MUST be safe for concurrent use —
// the middleware doesn't serialise calls across backends in a
// chain.
func Chain(ctx context.Context, backends []Backend, cred Credential) (*Identity, error) {
	for _, b := range backends {
		id, err := b.Authenticate(ctx, cred)
		switch {
		case err == nil && id != nil:
			id.Backend = b.Name()
			return id, nil
		case errors.Is(err, ErrUnauthorized):
			// Hard reject — don't fall through.
			return nil, ErrUnauthorized
		case errors.Is(err, ErrNoCredential):
			// No opinion — try the next backend.
			continue
		case errors.Is(err, ErrBackendUnavailable):
			// Explicit, backend-classified transient error.
			// Graceful degrade — try the next backend.
			continue
		case err != nil:
			// Unclassified error. FAIL CLOSED: stop the walk so a
			// misconfigured / broken backend cannot silently
			// downgrade the request to a later backend and admit
			// it. The caller treats any non-nil error as denied.
			return nil, fmt.Errorf("auth: backend %q failed closed: %w", b.Name(), err)
		default:
			// err == nil but id == nil — a backend contract
			// violation. Treat as no opinion rather than panic.
			continue
		}
	}
	return nil, ErrUnauthorized
}
