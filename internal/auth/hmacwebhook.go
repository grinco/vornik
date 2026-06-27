package auth

import "context"

// HMACWebhookBackend authenticates webhook deliveries that carry a
// per-source HMAC signature instead of an API key. It is a PASS-
// THROUGH marker: the signature itself is verified by the webhook
// handler against the source's configured secret (IngestWebhook),
// exactly as on the legacy path — this backend only decides that
// the request may reach that handler.
//
// Construction contract: the middleware sets
// Credential.HMACPresent ONLY when the request bears the
// signature header its path's handler will actually verify
// (hasWebhookSignatureForPath in internal/api). The per-path header
// table therefore lives in one place; this backend trusts it.
//
// SHARP EDGE: this backend does NOT validate that Path is a
// webhook route — it trusts the middleware's construction
// completely. A bug that set HMACPresent on a non-webhook
// path would mint a pass-through identity for that route with
// no key and no downstream HMAC verifier. The trade-off is
// deliberate (the per-path header table must not be duplicated
// across packages); the regression test pins the delegation so
// it stays visible.
//
// A request carrying BOTH a bearer and a signature returns
// ErrNoCredential so the bearer is validated by the key backends —
// "API key present — fall through to standard validation".
type HMACWebhookBackend struct{}

// NewHMACWebhookBackend constructs the backend.
func NewHMACWebhookBackend() *HMACWebhookBackend { return &HMACWebhookBackend{} }

// Name returns the audit-trail identifier for this backend.
func (b *HMACWebhookBackend) Name() string { return "hmac-webhook" }

// Authenticate returns a pass-through Identity for signed,
// key-less webhook deliveries; ErrNoCredential otherwise.
func (b *HMACWebhookBackend) Authenticate(_ context.Context, cred Credential) (*Identity, error) {
	if cred.BearerToken != "" || !cred.HMACPresent {
		return nil, ErrNoCredential
	}
	subject := "webhook:" + cred.Path
	return &Identity{
		Subject:     subject,
		DisplayName: subject,
	}, nil
}
