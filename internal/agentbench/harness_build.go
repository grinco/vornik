package agentbench

import (
	"strings"

	"vornik.io/vornik/internal/version"
)

// UnknownHarnessBuild is recorded when the scoring binary carries no VCS data
// (an archive build, or -buildvcs=false). Written rather than left empty: a
// journal that says "unknown" is telling the reader something, an absent field
// is indistinguishable from an older journal that never recorded one.
const UnknownHarnessBuild = "unknown"

// HarnessBuild identifies the binary that scores a run.
//
// See RunManifest.HarnessBuild for why this is recorded at all. Short version:
// the arm key pins the daemon under test and the scoring CONTRACT version, but
// nothing pinned the binary that actually computes the metrics — so a stale
// vornikctl silently journaled zeros for a metric it was too old to read, and
// every keyed axis still matched.
func HarnessBuild() string {
	rev, dirty, ok := version.BuildRevision()
	if !ok || rev == "" {
		return UnknownHarnessBuild
	}
	if dirty {
		// A dirty tree cannot be recovered from any commit, so the revision
		// alone would overstate what the reader can reproduce.
		return rev + "+dirty"
	}
	return rev
}

// BinaryBuild identifies another binary on disk — the daemon under test — in
// the same shape HarnessBuild uses for the running process, so the two can be
// printed and compared without the reader learning two formats.
func BinaryBuild(path string) string {
	if path == "" {
		return ""
	}
	rev, dirty, ok := version.RevisionOfBinary(path)
	if !ok || rev == "" {
		return UnknownHarnessBuild
	}
	if dirty {
		return rev + "+dirty"
	}
	return rev
}

// HarnessBuildTrustworthy reports whether a recorded build stamp identifies a
// reproducible binary.
//
// A run scored by an unknown or dirty build is not refused — the data is real
// and discarding it would also discard the evidence — but it cannot be
// reproduced, and a reader comparing it against another arm deserves to know
// that before quoting a delta.
func HarnessBuildTrustworthy(build string) bool {
	return build != "" && build != UnknownHarnessBuild &&
		!strings.HasSuffix(build, "+dirty")
}
