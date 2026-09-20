package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/authsession"
)

func csrfRequest(method, cookie, header string) *http.Request {
	r := httptest.NewRequest(method, "https://vornik.example/api/v1/tasks", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: authsession.CSRFCookieName, Value: cookie})
	}
	if header != "" {
		r.Header.Set(authsession.CSRFHeaderName, header)
	}
	return r
}

func TestCSRFDoubleSubmit(t *testing.T) {
	tests := []struct {
		name   string
		method string
		cookie string
		header string
		want   bool
	}{
		{
			name: "safe methods are never blocked", method: http.MethodGet,
			cookie: "tok", header: "", want: true,
		},
		{name: "HEAD is safe", method: http.MethodHead, cookie: "tok", want: true},
		{name: "OPTIONS is safe", method: http.MethodOptions, cookie: "tok", want: true},
		{
			name: "matching token passes", method: http.MethodPost,
			cookie: "tok", header: "tok", want: true,
		},
		{
			// The attack: a sibling subdomain can make the browser send
			// the cookie, but cannot READ it to produce the header.
			name: "no header fails", method: http.MethodPost,
			cookie: "tok", header: "", want: false,
		},
		{
			name: "wrong header fails", method: http.MethodPost,
			cookie: "tok", header: "guess", want: false,
		},
		{
			name: "a prefix of the token fails", method: http.MethodPost,
			cookie: "token-value", header: "token", want: false,
		},
		{
			// A session minted before this mechanism existed. Not a hole
			// an attacker can open: they do not choose which cookies the
			// victim's browser sends, and such sessions age out.
			name: "absent cookie skips", method: http.MethodPost,
			cookie: "", header: "", want: true,
		},
		{
			name: "empty cookie skips", method: http.MethodPost,
			cookie: "", header: "anything", want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := csrfDoubleSubmitOK(csrfRequest(tt.method, tt.cookie, tt.header)); got != tt.want {
				t.Fatalf("csrfDoubleSubmitOK() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The gap this closes, stated as a test so nobody removes it as redundant:
// the ladder ACCEPTS Sec-Fetch-Site: same-site, which is true for a sibling
// subdomain. A daemon on vornik.example and an XSS on blog.example are
// same-site and different origins.
func TestCSRFDoubleSubmitClosesTheSameSiteHoleTheLadderAccepts(t *testing.T) {
	r := csrfRequest(http.MethodPost, "secret-token", "")
	r.Header.Set("Sec-Fetch-Site", "same-site")

	if !isCSRFSafe(r) {
		t.Fatal("precondition: the ladder is expected to ACCEPT same-site; if it no longer does, " +
			"re-read whether the double-submit token still has a gap to close")
	}
	if csrfDoubleSubmitOK(r) {
		t.Fatal("a same-site cross-origin mutation with no matching token was allowed; " +
			"that is the exact case this mechanism exists for")
	}
}

// And the converse, so the two are not confused: against a CROSS-site
// attacker the ladder already refuses, and the token adds nothing.
func TestCSRFLadderAlreadyRefusesCrossSite(t *testing.T) {
	r := csrfRequest(http.MethodPost, "", "")
	r.Header.Set("Sec-Fetch-Site", "cross-site")

	if isCSRFSafe(r) {
		t.Fatal("the ladder accepted a cross-site mutating request")
	}
}
