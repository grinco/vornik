// Package version provides the default build metadata for vornik.
package version

import "fmt"

const (
	// Default is the fallback version for archive builds without git metadata.
	Default = "2026.4.5"

	// UnknownBuildDate is used when build metadata is not injected.
	UnknownBuildDate = "unknown"
)

// Edition identifies which build of vornik this is. The Community Edition
// (AGPL) is the default; the Enterprise Edition is a separate proprietary
// build. See https://docs.vornik.io
const (
	// EditionCommunity is the free, AGPL-licensed build.
	EditionCommunity = "community"
	// EditionEnterprise is the proprietary closed build.
	EditionEnterprise = "enterprise"
	// DefaultEdition is the edition assumed when none is stamped at build time.
	DefaultEdition = EditionCommunity
)

// NormalizeEdition maps a raw (possibly empty or ldflag-injected) edition
// string to a known value. Anything that is not an exact match for
// EditionEnterprise normalizes to EditionCommunity — an untrusted or
// unstamped build is treated as the less-privileged edition (fail-safe).
func NormalizeEdition(s string) string {
	if s == EditionEnterprise {
		return EditionEnterprise
	}
	return EditionCommunity
}

// BuildLine renders the canonical version line for a vornik program. It is
// the single source of the "(built <date>, <edition> edition)" format, used
// by both the daemon --version output and `vornikctl version`, and it
// normalizes the edition so callers never print an untrusted raw value.
func BuildLine(prog, ver, buildDate, edition string) string {
	return fmt.Sprintf("%s %s (built %s, %s edition)", prog, ver, buildDate, NormalizeEdition(edition))
}
