package mcpauth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// secureOAuthURL accepts HTTPS endpoints and HTTP only for local loopback
// development. OAuth codes, refresh tokens, and client secrets must never be
// sent over plaintext to a remote host.
func secureOAuthURL(raw, field string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		return nil, fmt.Errorf("mcpauth: invalid %s URL %q", field, raw)
	}
	if u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("mcpauth: invalid %s URL %q", field, raw)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("mcpauth: %s must use https (http is allowed only for loopback)", field)
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// doSensitivePOST never follows redirects. Go may replay a POST body on
// 307/308, which would forward an authorization code, refresh token, or client
// secret to a redirect target chosen by the endpoint.
func doSensitivePOST(client *http.Client, req *http.Request) (*http.Response, error) {
	return clientWithRedirectPolicy(client, func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}).Do(req)
}

// doSameOrigin follows metadata redirects only within the original origin.
// This preserves ordinary canonical-path redirects without allowing a
// discovered URL to turn into a cross-origin fetch.
func doSameOrigin(client *http.Client, req *http.Request) (*http.Response, error) {
	origin := req.URL
	return clientWithRedirectPolicy(client, func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !sameURLOrigin(origin, next.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}).Do(req)
}

func clientWithRedirectPolicy(client *http.Client, check func(*http.Request, []*http.Request) error) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = check
	return &clone
}

func sameURLOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return ""
}
