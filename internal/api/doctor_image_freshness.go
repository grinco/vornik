package api

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/imagemanifest"
	"vornik.io/vornik/internal/version"
)

// errImageAbsent reports that an image is not present in local storage.
// Distinguished from a real inspect failure because the two get opposite
// verdicts: an absent image is checkAgentImages' to report, while a broken
// inspect means this check cannot answer and must say so.
var errImageAbsent = errors.New("image not present in local storage")

// revisionLabel is the OCI label every vornik image is built with. The value
// is the commit the image was built from — see the design's §3 and the
// provenance block in each Containerfile.
const revisionLabel = "org.opencontainers.image.revision"

// realImageRevision reads an image's build revision from its labels.
//
// A label read, not a container run: checkAgentImageUID has to `podman run`
// because the agent's baked uid comes from `USER vornik:vornik`, a name, and
// is therefore not in the metadata. A label is metadata, so this costs one
// inspect — which matters for a check that runs on every doctor invocation.
func realImageRevision(ctx context.Context, image string) (string, bool, error) {
	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		return "", false, err
	}

	existsCtx, cancelExists := context.WithTimeout(ctx, 5*time.Second)
	defer cancelExists()
	if err := exec.CommandContext(existsCtx, podmanPath, "image", "exists", image).Run(); err != nil {
		return "", false, errImageAbsent
	}

	inspectCtx, cancelInspect := context.WithTimeout(ctx, 5*time.Second)
	defer cancelInspect()
	out, err := exec.CommandContext(inspectCtx, podmanPath, "image", "inspect", image,
		"--format", fmt.Sprintf("{{index .Labels %q}}", revisionLabel)).Output()
	if err != nil {
		return "", false, err
	}

	rev := strings.TrimSpace(string(out))
	// podman renders a missing label as the empty string or "<no value>"
	// depending on version; both mean the image predates labelling.
	if rev == "" || rev == "<no value>" {
		return "", false, nil
	}
	return rev, true, nil
}

// checkImageFreshness reports whether the images this deployment uses were
// built from the same commit as the running daemon.
//
// It exists because nothing asked that question. checkAgentImages runs
// `podman image exists` and stops there, so a six-week-old image reported OK
// — an affirmative all-clear, which is worse than silence. That is how a CE
// customer ran the documented update path for weeks while
// localhost/vornik-agent:latest stayed frozen at install date, leaving commit
// 356e74cd (a security fix spanning internal/agenttools AND the agent
// entrypoint) half-applied.
//
// The two checks are deliberately separate and stay that way: one asks *can a
// container start*, the other asks *is it the right code*. Collapsing them
// would let a fix for one silently change the other's verdict.
//
// Severity is WARNING, never ERROR. A deliberate pin is legitimate — the
// benchmark host runs its own :bench tag so a bench rebuild cannot reach
// production's agents — and an ERROR would fail a correct setup. There is no
// per-image suppression: a flag that silences drift would recreate exactly the
// affirmative all-clear this check was written to remove.
func (h *DoctorHandlers) checkImageFreshness(ctx context.Context) DoctorCheck {
	name := "image_freshness"

	daemonRev, ok := h.resolveDaemonRevision()
	if !ok {
		// An unstamped build has nothing to compare against. That is the
		// build's shortcoming, not the image's, and warning about it
		// would train operators to ignore this check.
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "daemon build carries no VCS revision, so images cannot be compared against it",
		}
	}

	prober := h.imageProber
	if prober == nil {
		prober = imagemanifest.HostProber{}
	}
	readRevision := h.imageRevisionFunc
	if readRevision == nil {
		readRevision = realImageRevision
	}

	var (
		stale      []string
		unlabelled []string
		unreadable []string
	)
	for _, img := range imagemanifest.Deployable(prober) {
		rev, labelled, err := readRevision(ctx, img.Tag)
		switch {
		case errors.Is(err, errImageAbsent):
			// checkAgentImages owns this failure. Reporting it here too
			// would say the same thing twice in different words.
			continue
		case err != nil:
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", img.Tag, err))
		case !labelled:
			unlabelled = append(unlabelled, img.Tag)
		case rev != daemonRev:
			stale = append(stale, fmt.Sprintf("%s: image %s, daemon %s",
				img.Tag, shortRev(rev), shortRev(daemonRev)))
		}
	}

	sort.Strings(stale)
	sort.Strings(unlabelled)
	sort.Strings(unreadable)

	switch {
	case len(stale) > 0:
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: fmt.Sprintf("%d image(s) were built from a different commit than this daemon. "+
				"Agent-side code ships INSIDE these images, so a release that changed both "+
				"the daemon and an image is only half-applied here. Fix: "+
				"`deployments/podman/vornik-update.sh --force` (rebuilds what drifted).", len(stale)),
			Items: stale,
		}
	case len(unreadable) > 0:
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: "could not read image provenance; freshness is unverified",
			Items:   unreadable,
		}
	case len(unlabelled) > 0:
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: fmt.Sprintf("%d image(s) carry no build-revision label, so their freshness "+
				"cannot be verified. They predate provenance labelling; rebuild them to make "+
				"drift detectable.", len(unlabelled)),
			Items: unlabelled,
		}
	default:
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("deployment images match the running daemon (%s)", shortRev(daemonRev)),
		}
	}
}

// resolveDaemonRevision returns the commit this daemon was built from.
func (h *DoctorHandlers) resolveDaemonRevision() (string, bool) {
	if h.daemonRevisionFunc != nil {
		return h.daemonRevisionFunc()
	}
	rev, dirty, ok := version.BuildRevision()
	if !ok || rev == "" {
		return "", false
	}
	if dirty {
		rev += "-dirty"
	}
	return rev, true
}

// shortRev trims a commit sha for display without losing identifiability.
func shortRev(rev string) string {
	const short = 12
	if len(rev) > short {
		return rev[:short]
	}
	return rev
}
