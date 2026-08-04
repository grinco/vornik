package version

import "strings"

// userAgentContact is the informational URL carried in the User-Agent. It
// gives a vendor whose WAF or rate limiter sees Vornik traffic somewhere to
// look, which is the whole point of identifying ourselves honestly instead
// of hiding behind a library default or spoofing a browser.
const userAgentContact = "https://vornik.io"

// UserAgent renders the canonical outbound User-Agent for Vornik's own HTTP
// traffic: "Vornik/<version> (+https://vornik.io)".
//
// Lives here rather than in a client package because more than one subsystem
// needs it (the MCP client today, MCP OAuth discovery next) and none of them
// should invent their own spelling — a vendor allowlisting one form and
// seeing another is exactly the failure this replaces.
//
// An empty version falls back to Default so an unstamped archive build still
// sends a well-formed value. CR/LF are stripped: the version arrives from a
// build-time ldflag, and a header value must stay a single line regardless of
// what was injected.
func UserAgent(ver string) string {
	if ver == "" {
		ver = Default
	}
	ver = sanitizeUserAgentToken(ver)
	if ver == "" {
		ver = Default
	}
	return "Vornik/" + ver + " (+" + userAgentContact + ")"
}

// maxUserAgentVersionLen bounds the version token. Real versions are ~20
// chars ("2026.7.7-95-g7dc76b5c5"); anything longer is a mistake, and a
// bounded header is one less thing an upstream can choke on.
const maxUserAgentVersionLen = 64

// sanitizeUserAgentToken truncates at the first character that is not legal
// in a product-version token. Truncating rather than filtering matters: a
// filter that merely drops CR/LF would turn "1.0\r\nX-Evil: 1" into
// "1.0X-Evil1" and keep the smuggled text, whereas everything after the
// first illegal byte is by definition not part of the version.
func sanitizeUserAgentToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '.', r == '-', r == '_', r == '+':
			if b.Len() >= maxUserAgentVersionLen {
				return b.String()
			}
			b.WriteRune(r)
		default:
			return b.String()
		}
	}
	return b.String()
}
