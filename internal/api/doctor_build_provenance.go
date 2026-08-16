package api

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/version"
)

// checkBuildProvenance reports whether the running daemon was built with a
// stamped release version, or from an ad-hoc `go build`.
//
// WHY THIS IS A CHECK AND NOT A COMMENT. Three times a binary shipped without
// -X main.Version and reported a stale constant instead (2026-07-30,
// 2026-08-03, and 2026-08-15, when the deployed daemon showed "2026.4.5" four
// months after that release). Each time the fix was "build through the
// Makefile", and each time the next hand-built binary regressed silently,
// because nothing in the product ever said the build was unstamped — the UI
// footer showed a plausible number and everyone believed it.
//
// version.Resolve now makes such a build self-identifying (dev+g<commit>), and
// this check makes it *findable*: an operator who runs doctor gets told, rather
// than having to notice a wrong number in a footer and think to question it.
func (h *DoctorHandlers) checkBuildProvenance() DoctorCheck {
	const name = "build_provenance"

	var ver string
	if h != nil && h.server != nil {
		ver = strings.TrimSpace(h.server.BuildVersion())
	}
	switch {
	case ver == "" || ver == version.Default:
		// No stamp AND no VCS metadata — an archive build, or -buildvcs=false.
		// Nothing can identify this binary, which is the worst of the three.
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "this daemon reports no build version: it was built without " +
				"-X main.Version and without usable VCS metadata, so the running " +
				"code cannot be traced to a commit. Build with `make build`.",
		}
	case !version.IsStamped(ver):
		// VCS fallback did its job: we know exactly what this is.
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: fmt.Sprintf("running an unstamped build (%s) — built from a "+
				"source tree with `go build` rather than `make build`, so it "+
				"carries no release version. Traceable to the commit shown, but "+
				"not a release artifact.", ver),
		}
	default:
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("release build %s", ver),
		}
	}
}
