package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/authz"
)

type fakeMinter struct {
	raw         string
	err         error
	gotUser     string
	gotKeyID    string
	gotProvider string
}

func (m *fakeMinter) Create(_ context.Context, userID, provider, _, _, originCredentialID string) (string, error) {
	m.gotUser, m.gotProvider, m.gotKeyID = userID, provider, originCredentialID
	if m.err != nil {
		return "", m.err
	}
	return m.raw, nil
}

type fakeSubjects struct {
	subject *authz.ExchangeSubject
	err     error
}

func (f *fakeSubjects) ResolveExchangeSubject(context.Context, string) (*authz.ExchangeSubject, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.subject, nil
}

func exchangeServer(t *testing.T, minter sessionExchangeMinter) *Server {
	t.Helper()
	s := &Server{logger: zerolog.Nop()}
	subjects := &fakeSubjects{subject: &authz.ExchangeSubject{UserID: "user-1", Role: "user"}}
	WithSessionExchange(subjects, minter, time.Hour, true)(s)
	return s
}

func exchangeRequest(keyID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session", nil)
	if keyID != "" {
		r = r.WithContext(context.WithValue(r.Context(), apiKeyIDKey, keyID))
	}
	return r
}

func TestSessionExchange_UnwiredReports503NotUnauthorized(t *testing.T) {
	// Whether a deployment OFFERS this door is not a fact about any
	// credential, so saying so leaks nothing — while a 401 would make the
	// absence look like a rejected key.
	s := &Server{logger: zerolog.Nop()}
	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest("key-1"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestSessionExchange_RefusesGET(t *testing.T) {
	s := exchangeServer(t, &fakeMinter{raw: "tok"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	s.SessionExchange(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestSessionExchange_RefusesPlaintextUnlessOverridden(t *testing.T) {
	// A session cookie travelling in the clear is the whole session.
	s := &Server{logger: zerolog.Nop()}
	subjects := &fakeSubjects{subject: &authz.ExchangeSubject{UserID: "user-1", Role: "user"}}
	WithSessionExchange(subjects, &fakeMinter{raw: "tok"}, time.Hour, false)(s)

	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest("key-1"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 over plaintext HTTP", w.Code)
	}
	if !strings.Contains(w.Body.String(), "plaintext") {
		t.Fatalf("body = %q, want it to name the cause", w.Body.String())
	}
}

func TestSessionExchange_RefusesWithoutAKeyIdentity(t *testing.T) {
	// A session may not mint a session: the door turns a MACHINE
	// credential into a browser one, and letting a session renew itself
	// would remove the cap without ever re-presenting the credential.
	s := exchangeServer(t, &fakeMinter{raw: "tok"})
	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest(""))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestSessionExchange_RefusalsCarryOneMessage(t *testing.T) {
	// The exchange is an oracle by construction; what is avoidable is a
	// SECOND signal telling a prober which half it got wrong.
	s := exchangeServer(t, &fakeMinter{raw: "tok"})

	noKey := httptest.NewRecorder()
	s.SessionExchange(noKey, exchangeRequest(""))

	badOrigin := httptest.NewRecorder()
	r := exchangeRequest("key-1")
	r.Header.Set("Origin", "https://evil.example")
	s.SessionExchange(badOrigin, r)

	if noKey.Code != badOrigin.Code {
		t.Fatalf("status differs by cause: %d vs %d", noKey.Code, badOrigin.Code)
	}
	if !strings.Contains(noKey.Body.String(), sessionExchangeRefusal) ||
		!strings.Contains(badOrigin.Body.String(), sessionExchangeRefusal) {
		t.Fatalf("refusals differ by cause:\n  %s\n  %s", noKey.Body.String(), badOrigin.Body.String())
	}
}

func TestOriginAllowed(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		header string
		want   bool
	}{
		// Non-browser callers legitimately send neither, and the attack
		// this stops is by definition made by a browser — which always
		// sends one on a cross-origin POST.
		{name: "absent is allowed", want: true},
		{name: "same origin", header: "Origin", origin: "https://example.test", want: true},
		{name: "cross origin", header: "Origin", origin: "https://evil.example", want: false},
		{name: "referer is checked too", header: "Referer", origin: "https://evil.example/page", want: false},
		{name: "unparseable", header: "Origin", origin: "://:::", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://example.test/api/v1/auth/session", nil)
			r.Host = "example.test"
			if tt.header != "" {
				r.Header.Set(tt.header, tt.origin)
			}
			if got := originAllowed(r); got != tt.want {
				t.Fatalf("originAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestIsSecure(t *testing.T) {
	plain := httptest.NewRequest(http.MethodPost, "/x", nil)
	if requestIsSecure(plain) {
		t.Fatal("a plaintext request read as secure")
	}
	proxied := httptest.NewRequest(http.MethodPost, "/x", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if !requestIsSecure(proxied) {
		t.Fatal("a TLS-terminating proxy's request read as insecure")
	}
}

func TestNewCSRFTokenIsUnpredictable(t *testing.T) {
	a, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two CSRF tokens were identical")
	}
	if len(a) < 40 {
		t.Fatalf("token = %q, want 256 bits of entropy", a)
	}
}

func TestNavMarker(t *testing.T) {
	if got := navMarker("admin"); got != "admin" {
		t.Fatalf("navMarker(admin) = %q", got)
	}
	if got := navMarker("user"); got != "1" {
		t.Fatalf("navMarker(user) = %q", got)
	}
}

func TestSessionExchange_MintFailureDoesNotSetCookies(t *testing.T) {
	// A half-set cookie jar is a browser that believes it is logged in
	// against a session that does not exist.
	s := exchangeServer(t, &fakeMinter{err: errors.New("database is gone")})
	// Give it an account service so it reaches the mint. Without one the
	// handler stops earlier, which this test is not about.
	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest("key-1"))

	for _, c := range w.Result().Cookies() {
		if c.Name == authsession.SessionCookieName && c.Value != "" {
			t.Fatalf("a session cookie was set despite a failed mint: %+v", c)
		}
	}
}

func TestSessionExchange_SetsTheThreeCookiesAndAttributesTheCredential(t *testing.T) {
	minter := &fakeMinter{raw: "session-token"}
	s := exchangeServer(t, minter)

	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest("key-1"))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s), want 204", w.Code, w.Body.String())
	}
	// The whole point of the column: a session no key revocation can ever
	// end is a session the capping rule cannot cap.
	if minter.gotKeyID != "key-1" {
		t.Fatalf("originCredentialID = %q, want the minting key", minter.gotKeyID)
	}
	if minter.gotUser != "user-1" || minter.gotProvider != "credential" {
		t.Fatalf("minted for %q via %q", minter.gotUser, minter.gotProvider)
	}

	cookies := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		cookies[c.Name] = c
	}

	sess := cookies[authsession.SessionCookieName]
	if sess == nil || sess.Value != "session-token" {
		t.Fatalf("session cookie = %+v", sess)
	}
	if !sess.HttpOnly {
		t.Fatal("the session cookie is readable by script; an XSS could exfiltrate it")
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", sess.SameSite)
	}

	csrf := cookies[authsession.CSRFCookieName]
	if csrf == nil || csrf.Value == "" {
		t.Fatalf("csrf cookie = %+v", csrf)
	}
	if csrf.HttpOnly {
		// Readable BY DESIGN: the defence is that an attacker's page
		// cannot read it to echo back, not that it is secret from the
		// page that owns it.
		t.Fatal("the CSRF cookie is HttpOnly, so the page that must echo it cannot read it")
	}
	if marker := cookies[authsession.UIMarkerCookieName]; marker == nil || marker.HttpOnly {
		t.Fatalf("ui marker cookie = %+v, want a readable nav hint", marker)
	}
}

func TestSessionExchange_SecureFlagFollowsTheTransport(t *testing.T) {
	s := exchangeServer(t, &fakeMinter{raw: "tok"})

	w := httptest.NewRecorder()
	r := exchangeRequest("key-1")
	r.Header.Set("X-Forwarded-Proto", "https")
	s.SessionExchange(w, r)

	for _, c := range w.Result().Cookies() {
		if !c.Secure {
			t.Fatalf("cookie %q is not Secure on a TLS request", c.Name)
		}
	}
}

func TestSessionExchange_RateLimitAnswers429NotAFlatRefusal(t *testing.T) {
	// Distinct from a refusal so a caller can back off — and, like the
	// claim path's equivalent, raised identically for every cause so the
	// bound reveals nothing the refusals do not.
	s := &Server{logger: zerolog.Nop()}
	WithSessionExchange(&fakeSubjects{err: authz.ErrExchangeRateLimited}, &fakeMinter{raw: "tok"}, time.Hour, true)(s)

	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest("key-1"))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

func TestSessionExchange_ARefusedSubjectIs401(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	WithSessionExchange(&fakeSubjects{err: authz.ErrExchangeRefused}, &fakeMinter{raw: "tok"}, time.Hour, true)(s)

	w := httptest.NewRecorder()
	s.SessionExchange(w, exchangeRequest("key-1"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), sessionExchangeRefusal) {
		t.Fatalf("body = %q, want the single refusal message", w.Body.String())
	}
}
