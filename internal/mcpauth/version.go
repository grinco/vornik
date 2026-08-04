package mcpauth

import "sync/atomic"

// daemonVersion is the build version stamped into the User-Agent on every discovery, dynamic
// registration and token request.
//
// A second holder rather than reading internal/mcp's: this package must stay a leaf (see the
// package doc), and internal/mcp is the one place it may not import. service.Run stamps both
// explicitly — two visible lines beat one hidden cross-package call.
var daemonVersion atomic.Value // string

// SetVersion records the daemon's build version for outbound OAuth requests. Called once during
// startup; safe to call concurrently and safe never to call at all (the version package's
// default applies).
func SetVersion(v string) { daemonVersion.Store(v) }

func buildVersion() string {
	v, _ := daemonVersion.Load().(string)
	return v
}
