package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
)

// Self-service account routes — oidc-identity-permissions-design §5.5.
//
// These differ from the operator routes in one way that decides everything
// else: WHOSE account they act on is taken from the SESSION, never from the
// request. A body-supplied user id is ignored rather than honoured, because a
// link code binds whichever channel redeems it — issuing one for somebody else
// would be a way to capture their channel, and a claim aimed at another
// account would move that person's recorded spend.
//
// They need no operator capability for the same reason: acting on your own
// account is not a privileged operation. They DO need a session, because
// without one there is no "own account" to act on.

// AccountSelf routes /api/v1/account/... for the signed-in user.
func (s *Server) AccountSelf(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		respondError(w, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", "identity core not wired on this daemon")
		return
	}
	userID := sessionUserID(r)
	if userID == "" {
		respondError(w, http.StatusUnauthorized, "SESSION_REQUIRED",
			"these routes act on your own account and need a signed-in session")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/account/")
	switch {
	case rest == "identities" && r.Method == http.MethodGet:
		s.selfListIdentities(ctx, w, userID)
	case rest == "link-codes" && r.Method == http.MethodPost:
		s.selfIssueLinkCode(ctx, w, r, userID)
	case rest == "keys/claim" && r.Method == http.MethodPost:
		s.selfClaimKey(ctx, w, r, userID)
	case strings.HasPrefix(rest, "identities/") && r.Method == http.MethodDelete:
		s.selfUnlink(ctx, w, r, userID, strings.TrimPrefix(rest, "identities/"))
	default:
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown account route")
	}
}

// sessionUserID reads the session's user id. An API key carries no user, so a
// key-authenticated caller has no self-service account and is refused — the
// operator routes are the door for that caller.
func sessionUserID(r *http.Request) string {
	id := IdentityFromContext(r.Context())
	if id == nil || id.Extra == nil {
		return ""
	}
	uid, _ := id.Extra[auth.ExtraSessionUserID].(string)
	return uid
}

func (s *Server) selfListIdentities(ctx context.Context, w http.ResponseWriter, userID string) {
	v, err := s.accounts.Get(ctx, userID)
	if err != nil {
		s.respondAccountError(w, err)
		return
	}
	// The §5.5 panel polls this. It returns the caller's own bindings only,
	// which is not a filter applied here but a property of Get: the view is
	// keyed by user id.
	//
	// It also returns the caller's outstanding link codes, by opaque id and
	// expiry. That is what makes the observation contract exact rather than
	// heuristic: the panel watches for ITS OWN code's id to leave this list,
	// instead of watching the identity list grow — which the design forbids,
	// because a second outstanding code or a concurrent admin key-assignment
	// both add an identity the poller did not cause. A code leaves the list
	// on redemption or on expiry, and the panel already knows its expiry, so
	// the two are distinguishable without a second endpoint.
	body := map[string]any{"identities": toAccountJSON(*v).Identities}
	codes, cerr := s.accounts.OutstandingLinkCodes(ctx, userID)
	if cerr != nil {
		// Omitted from the body, but NOT silent. Everything else curated out
		// of a response in this feature goes to the log; without this a
		// hard-down link-code store is invisible — the identity list still
		// 200s, and the only symptom is people being told a redeemed code
		// expired (review-20260915-80fa F5).
		s.logger.Warn().Err(cerr).Str("user_id", userID).
			Msg("accounts: outstanding link codes unavailable; the panel cannot observe redemption")
	}
	if cerr == nil {
		out := make([]map[string]any, 0, len(codes))
		for _, lc := range codes {
			out = append(out, map[string]any{
				"id":        authz.LinkCodeIDOf(lc),
				"expiresAt": lc.ExpiresAt.UTC().Format(time.RFC3339),
			})
		}
		body["outstandingCodes"] = out
	}
	// A link-code store that is absent or failing must not break the
	// identity list: the list is the answer to the question asked, and the
	// codes are an aid to a page that can still be reloaded by hand.
	respondJSON(w, http.StatusOK, body)
}

func (s *Server) selfIssueLinkCode(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) {
	code, err := s.accounts.IssueLinkCode(ctx, userID, selfActor(r, userID))
	if err != nil {
		s.respondAccountError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{
		"code":      code,
		"codeId":    authz.LinkCodeID(code),
		"expiresIn": linkCodeTTLSeconds,
	})
}

func (s *Server) selfClaimKey(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) {
	var req struct {
		KeyID     string `json:"keyId"`
		KeySecret string `json:"keySecret"`
	}
	if !decodeSelfBody(w, r, &req) {
		return
	}
	// The attempt bound is enforced inside ClaimKey, so it holds for this
	// door, the operator route and the browser form alike — the claim that
	// used to be made in this comment while the browser form bypassed it
	// entirely (audit 2026-09-15 CA-09). ErrKeyClaimRateLimited maps to 429.
	if err := s.accounts.ClaimKey(ctx, userID, req.KeyID, req.KeySecret, selfActor(r, userID)); err != nil {
		s.respondAccountError(w, err)
		return
	}
	s.selfListIdentities(ctx, w, userID)
}

func (s *Server) selfUnlink(ctx context.Context, w http.ResponseWriter, r *http.Request, userID, rest string) {
	channel, externalID, ok := strings.Cut(rest, "/")
	if !ok || channel == "" || externalID == "" {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "expected /api/v1/account/identities/{channel}/{externalId}")
		return
	}
	if err := s.accounts.Unlink(ctx, userID, channel, externalID, selfActor(r, userID)); err != nil {
		s.respondAccountError(w, err)
		return
	}
	s.selfListIdentities(ctx, w, userID)
}

// decodeSelfBody reads a JSON body, treating an empty one as an empty struct.
func decodeSelfBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	limitJSONBody(w, r)
	if r.Body == nil {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// selfActor names the caller for the audit row as the session's own user.
func selfActor(r *http.Request, userID string) authz.Actor {
	return authz.Actor{
		Principal: "session:" + userID,
		Source:    "api",
		IP:        clientIPFromRequest(r),
		UserAgent: r.UserAgent(),
	}
}
