package mcpconnect

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/mcpauth"
)

// CallbackHandler serves the OAuth redirect URI.
//
// It lives in the CORE UI server rather than inside the control-plane hub tab because an operator
// who hand-edits mcp.servers needs a browser callback whether or not they ever open the hub
// (design §7.2a). It is mounted inside the /ui subtree, so it sits behind the same session
// authentication as every other UI page — which is right: the browser arriving here is the
// operator's, and an unauthenticated callback endpoint would accept a forged code from anyone.
//
// The page is plain HTML rather than a UI template on purpose: it is a terminal redirect target
// with three possible messages, reached once per connect, and giving it a template would couple
// this package to the UI's render pipeline for no operator-visible gain.
func (c *Connector) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		// The vendor reports a refused consent through the redirect, not an
		// HTTP error. Distinguish it from our own failures: nothing is wrong
		// with the deployment, the human said no (or the vendor said no on
		// their behalf).
		if vendorErr := strings.TrimSpace(q.Get("error")); vendorErr != "" {
			desc := strings.TrimSpace(q.Get("error_description"))
			c.Logger.Warn().
				Str("error", vendorErr).
				Msg("mcp oauth: the authorization server refused the consent")
			// Both values come from an untrusted redirect and are rendered,
			// so they are escaped HERE rather than trusting the renderer.
			c.renderCallback(w, http.StatusBadRequest, "Authorization was not granted",
				fmt.Sprintf("The authorization server returned %q%s. Nothing was changed; you can start again from the control plane or with vornikctl mcp connect.",
					html.EscapeString(vendorErr), html.EscapeString(optionalDetail(desc))))
			return
		}

		state := strings.TrimSpace(q.Get("state"))
		code := strings.TrimSpace(q.Get("code"))
		if state == "" || code == "" {
			c.renderCallback(w, http.StatusBadRequest, "Incomplete callback",
				"This URL is the OAuth redirect target and is not meant to be opened directly.")
			return
		}

		tok, err := c.Complete(r.Context(), state, code)
		if err != nil {
			status := http.StatusBadGateway
			title := "Could not complete the connection"
			detail := "The authorization server rejected the token request. The daemon log names the reason."
			switch {
			case errors.Is(err, ErrUnknownState):
				status = http.StatusBadRequest
				title = "This authorization has expired"
				detail = "Authorization attempts stay open for a few minutes. Start again from the control plane or with vornikctl mcp connect."
			case errors.Is(err, mcpauth.ErrAuthorizationServer), errors.Is(err, mcpauth.ErrInvalidGrant):
				// Keep the vendor's own words out of the page: an error
				// body can echo request parameters, and on a token
				// request that means the client secret.
				detail = "The authorization server rejected the exchange. The daemon log carries the (redacted) reason."
			}
			c.Logger.Error().Err(err).Msg("mcp oauth: callback failed")
			c.renderCallback(w, status, title, detail)
			return
		}

		// Never render the token. What the operator needs to see is the ask
		// they consented to, so they can compare it with what they intended.
		c.renderCallback(w, http.StatusOK, "Connected",
			fmt.Sprintf("%s is connected for %s.<br>Resource: <code>%s</code><br>Granted scopes: <code>%s</code>",
				html.EscapeString(tok.ServerName),
				html.EscapeString(scopeLabel(tok.ProjectID)),
				html.EscapeString(tok.Resource),
				html.EscapeString(orNone(tok.Scopes))))
	})
}

func scopeLabel(projectID string) string {
	if projectID == "" {
		return "every project on this daemon (daemon-scope server)"
	}
	return "project " + projectID
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none reported by the server)"
	}
	return s
}

func optionalDetail(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return " (" + s + ")"
}

// renderCallback writes the terminal page. detail is trusted HTML assembled by the caller from
// escaped parts — every value that comes from a vendor or from config goes through
// html.EscapeString at the call site.
func (c *Connector) renderCallback(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// This page is a one-shot result: caching it would show a stale
	// "Connected" to the next attempt.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>%s — Vornik</title>
<style>
 body{font:15px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f6f7f9;color:#14171a}
 main{max-width:34rem;padding:2rem;background:#fff;border-radius:10px;box-shadow:0 1px 3px rgba(0,0,0,.12)}
 h1{margin:0 0 .5rem;font-size:1.25rem}
 code{background:#f0f1f3;padding:.1rem .3rem;border-radius:4px;word-break:break-all}
 p{margin:.5rem 0 0}
 @media (prefers-color-scheme:dark){body{background:#15181c;color:#e8eaed}main{background:#1e2126;box-shadow:none}code{background:#2a2e35}}
</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>
`, html.EscapeString(title), html.EscapeString(title), detail)
}
