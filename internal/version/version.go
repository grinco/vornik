// Package version provides the default build metadata for vornik.
package version

import (
	"debug/buildinfo"
	"fmt"
	"runtime/debug"
	"strings"
)

const (
	// Default is the last-resort version string, used only when the build was
	// neither stamped by the Makefile nor produced from a source tree Go could
	// read VCS metadata from (a bare archive build).
	//
	// It is deliberately NOT a plausible release number. Until 2026-08-15 this
	// const was "2026.4.5", and an unstamped binary reported that — a real
	// version, four months stale, indistinguishable in the UI from a correct
	// build. Two separate regressions (2026-07-30, 2026-08-03) were diagnosed
	// slowly because the symptom was a believable number rather than an
	// obviously absent one. A value that cannot be mistaken for a release is
	// the point.
	Default = "unstamped"

	// UnknownBuildDate is used when build metadata is not injected.
	UnknownBuildDate = "unknown"

	// devPrefix marks a version derived from VCS metadata rather than from a
	// release tag. "dev+g<revision>" mirrors git describe's convention so the
	// string reads as a commit reference to anyone who has seen one.
	devPrefix = "dev+g"

	// shortRevLen is how much of the commit hash to show. Twelve is git's
	// own disambiguating length for large repositories.
	shortRevLen = 12
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

// Resolve returns the version and build date a program should report, given
// whatever ldflags injected (which may be nothing).
//
// WHY THIS EXISTS. The Makefile stamps -X main.Version from `git describe`,
// and for a Makefile build this function simply passes that through. But a
// hand-run `go build` — including the documented edition-only incantation,
// which passes `-X main.Edition=<edition>` and is silent on Version — leaves
// Version at Default. (Spelled indirectly: the literal flag+value pair is an
// EE-feature marker the CE export sweep refuses to ship.) Three times now the fix has been
// "remember to build through the Makefile", and three times the binary shipped
// unstamped anyway (2026-07-30, 2026-08-03, 2026-08-15).
//
// Go already embeds vcs.revision, vcs.time and vcs.modified into every binary
// built from a source tree, with no cooperation from the build command. Reading
// them makes an unstamped build SELF-REPORTING rather than silently wrong: it
// names the commit it was built from, which is both honest and directly
// actionable. Nothing has to be remembered for that to work, which is the
// property the previous two fixes lacked.
//
// A stamped version always wins; VCS data only fills what the build did not
// supply, so a release build is unaffected.
func Resolve(injected, injectedDate string) (string, string) {
	ver := strings.TrimSpace(injected)
	date := strings.TrimSpace(injectedDate)
	stamped := ver != "" && ver != Default

	rev, vcsDate, dirty, haveVCS := vcsInfo()

	if !stamped {
		switch {
		case haveVCS:
			ver = devPrefix + rev
			if dirty {
				ver += ".dirty"
			}
		case ver == "":
			ver = Default
		}
	}
	if date == "" || date == UnknownBuildDate {
		if haveVCS && vcsDate != "" {
			date = vcsDate
		} else {
			date = UnknownBuildDate
		}
	}
	return ver, date
}

// IsStamped reports whether ver came from a release build rather than from
// the VCS fallback or the last-resort constant. Used by the build_provenance
// doctor check to tell an operator their daemon is running an ad-hoc build.
func IsStamped(ver string) bool {
	v := strings.TrimSpace(ver)
	return v != "" && v != Default && !strings.HasPrefix(v, devPrefix)
}

// BuildRevision reports the commit THIS binary was built from, and whether the
// tree was dirty. ok is false when the build carries no VCS data at all.
//
// Exported for provenance recording, where the question is not "what version do
// we call this" but "which build produced these numbers". The benchmark harness
// needs it because a stale scoring binary is otherwise invisible: on 2026-08-16
// a long-horizon arm journaled durationMs=0 for every record while the database
// held the durations, because the vornikctl doing the scoring was 27 commits
// behind and predated the fix. The arm manifest pinned the DAEMON's sha, and the
// daemon was current — nothing recorded the binary that computed the metrics.
func BuildRevision() (rev string, dirty, ok bool) {
	rev, _, dirty, ok = vcsInfo()
	return rev, dirty, ok
}

// RevisionOfBinary reports the commit ANOTHER binary on disk was built from.
//
// The sibling of BuildRevision, which answers the same question about the
// running process. Needed because a benchmark run is produced by more than one
// binary — a daemon under test and a harness that scores it — and on 2026-08-16
// those two came from different commits without anything noticing. Reading the
// other binary's stamp is the only way to record what actually ran, since the
// harness cannot ask a file to introspect itself.
//
// ok is false when the file is missing, is not a Go binary, or carries no VCS
// data. Callers record "unknown" rather than failing: an unidentifiable binary
// is a fact about the run worth journaling, not a reason to refuse it.
func RevisionOfBinary(path string) (rev string, dirty, ok bool) {
	info, err := buildinfo.ReadFile(path)
	if err != nil || info == nil {
		return "", false, false
	}
	rev, _, dirty, ok = vcsFromSettings(info.Settings)
	return rev, dirty, ok
}

// vcsInfo pulls the git metadata Go embeds at build time. ok is false for a
// build with no VCS data at all (archive builds, or -buildvcs=false).
func vcsInfo() (rev, date string, dirty, ok bool) {
	info, available := debug.ReadBuildInfo()
	if !available || info == nil {
		return "", "", false, false
	}
	return vcsFromSettings(info.Settings)
}

// vcsFromSettings is the pure core of vcsInfo, split out so it can be tested
// without producing a binary. A revision is required — a build that reports
// only vcs.time tells us nothing we can trace.
func vcsFromSettings(settings []debug.BuildSetting) (rev, date string, dirty, ok bool) {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			date = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "", "", false, false
	}
	if len(rev) > shortRevLen {
		rev = rev[:shortRevLen]
	}
	// vcs.time is RFC3339; the build line wants a plain date. Trimming beats
	// parsing here — a malformed stamp degrades to the raw value rather than
	// to "unknown", which is still more than the caller had.
	if len(date) >= len("2006-01-02") {
		if t, _, found := strings.Cut(date, "T"); found {
			date = t
		}
	}
	return rev, date, dirty, true
}

// BuildLine renders the canonical version line for a vornik program. It is
// the single source of the "(built <date>, <edition> edition)" format, used
// by both the daemon --version output and `vornikctl version`, and it
// normalizes the edition so callers never print an untrusted raw value.
func BuildLine(prog, ver, buildDate, edition string) string {
	return fmt.Sprintf("%s %s (built %s, %s edition)", prog, ver, buildDate, NormalizeEdition(edition))
}
