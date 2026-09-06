package api

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
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
// realImageDigest reads an image's manifest digest.
//
// podman computes one for a LOCALLY BUILT, never-pushed image and records it in
// RepoDigests — Docker does not, leaving RepoDigests empty until push. That
// difference is why a digest is usable on the packaged path at all, and it was
// verified on the reference host before the design relied on it.
func realImageDigest(ctx context.Context, image string) (string, error) {
	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		return "", err
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(inspectCtx, podmanPath, "image", "inspect", image,
		"--format", "{{.Digest}}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// checkImageFreshness reports whether the images this deployment uses are the
// ones this release declares, AND whether the running daemon is that release.
//
// TWO COMPARISONS, NOT ONE (design §5.2.0). Draft v2 of the design compared only
// the image against the record and was wrong in a mundane way: an rpm upgrade
// replaces /usr/bin/vornik-enterprise on disk while the RUNNING PROCESS keeps
// executing the old binary until the unit is restarted. So minutes after
// `dnf upgrade` a host has record=N, image=N, running daemon=N-1 — a genuinely
// half-applied state that a single comparison reports as OK.
//
//  1. image  <-> record : is this image the build the release declares?
//  2. record <-> daemon : is the running daemon actually that release?
//
// The worse of the two is reported, and (2) yields a message naming its own fix
// rather than a generic drift notice.
//
// Severity stays WARNING, never ERROR: a deliberate pin is legitimate and an
// ERROR would fail a correct setup. The gain here is PRECISION — the routine,
// correct case stops warning, so a warning means something again.
func (h *DoctorHandlers) checkImageFreshness(ctx context.Context) DoctorCheck {
	const name = "image_freshness"

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

	loadRecord := h.imageRecordFunc
	if loadRecord == nil {
		loadRecord = func() (*imagemanifest.ReleaseRecord, error) {
			return imagemanifest.LoadReleaseRecord(imagemanifest.RecordPath)
		}
	}

	rec, err := loadRecord()
	switch {
	case errors.Is(err, imagemanifest.ErrRecordAbsent):
		// Correct and silent: a source install declares no record.
		return h.legacyFreshness(ctx, daemonRev)
	case err != nil:
		// CORRUPT IS NOT ABSENT. The provenance check is not running, on a
		// host that believes it is, so say exactly that.
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "image record is unreadable, so the provenance check is DISABLED — " +
				"falling back to comparing image labels against the daemon revision. " +
				"Reinstall the package to restore it.",
			Items: []string{err.Error()},
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
	readDigest := h.imageDigestFunc
	if readDigest == nil {
		readDigest = realImageDigest
	}

	var okCount int
	var warn []string
	var warned []string // tags, for the aggregate line

	// Comparison 2, first: it describes the whole deployment rather than one
	// image, and its remedy is the one an operator can act on immediately.
	if recCommit, has := rec.SourceCommit(); has && !revisionsMatch(recCommit, daemonRev) {
		warned = append(warned, "daemon")
		warn = append(warn, fmt.Sprintf(
			"running daemon is %s but this release declares %s — the package was upgraded "+
				"and the daemon has not been restarted: systemctl restart vornik-enterprise",
			shortRev(daemonRev), shortRev(recCommit)))
	}

	// Comparison 1, per image.
	for _, img := range imagemanifest.Deployable(prober) {
		declared, hasEntry := rec.Lookup(img.Tag)
		if !hasEntry {
			warned = append(warned, img.Tag)
			warn = append(warn, fmt.Sprintf(
				"%s: not declared by this release, so it cannot be verified", img.Tag))
			continue
		}

		// DIGEST FIRST. An image pulled from the registry was built in the
		// public CE tree, so its revision label is a CE commit while the
		// record declares an EE one — they can never match, because the export
		// maps one tree onto the other. A digest needs no such mapping, so an
		// exact digest is both the strongest statement available and the only
		// one that works across that boundary.
		// The digest compared is THIS HOST'S ARCHITECTURE. A release publishes
		// one manifest per platform, and a host observes its own — comparing
		// against another architecture's digest, or against the manifest-list
		// digest, fails on every host forever.
		//
		// A host-built image has no digest by design (it builds its own, so no
		// recorded value could match) and falls straight through to the commit
		// comparison, which is the only cross-machine statement available for it.
		declaredDigest, haveDigest := declared.DigestForArch(runtime.GOARCH)
		if readDigest != nil && haveDigest {
			if got, dErr := readDigest(ctx, img.Tag); dErr == nil && got == declaredDigest {
				okCount++
				continue
			}
			// A digest that differs is NOT a verdict on its own: container
			// builds are not bit-reproducible, so a legitimate local rebuild
			// of the declared commit differs here. Fall through to the commit.
		}

		rev, labelled, readErr := readRevision(ctx, img.Tag)
		switch {
		case errors.Is(readErr, errImageAbsent):
			// checkAgentImages owns this failure.
			continue
		case readErr != nil:
			warned = append(warned, img.Tag)
			warn = append(warn, fmt.Sprintf("%s: provenance unreadable (%v)", img.Tag, readErr))
		case !labelled:
			warned = append(warned, img.Tag)
			warn = append(warn, fmt.Sprintf(
				"%s: carries no build-revision label, so it cannot be placed", img.Tag))
		case !revisionsMatch(rev, declared.SourceCommit):
			warned = append(warned, img.Tag)
			warn = append(warn, fmt.Sprintf(
				"%s: built from %s but this release declares %s",
				img.Tag, shortRev(rev), shortRev(declared.SourceCommit)))
		default:
			// Same commit. A digest difference here is a LEGITIMATE local
			// rebuild of the declared source — container builds are not
			// bit-reproducible, and warning on it would punish an operator
			// for doing exactly the right thing (design §5.2.1).
			okCount++
		}
	}

	sort.Strings(warn)

	// Contract C5 (Stage 2, 2026-09-06): say how each image got here.
	//
	// This cannot be inspected after the fact. §5.2 established by running it
	// that podman writes a RepoDigests entry even for a never-pushed local
	// build, and once a pull has happened the revision label is the CE commit
	// the image was built from — indistinguishable from a host that built at
	// that commit. So the obtain step writes it down and this reads it back.
	//
	// A HOST WITH NO RECORD IS NORMAL, not a fault: any install predating
	// Stage 2, and any image obtained outside the update path. Absent is
	// reported as unknown rather than guessed, because a guess here is exactly
	// the "examined and clean" / "never examined" conflation the tenets forbid.
	provenance := obtainProvenanceSummary()

	if len(warn) == 0 {
		msg := fmt.Sprintf("%d checked, all match the build this release declares (%s)",
			okCount, shortRev(daemonRev))
		if provenance != "" {
			msg += ". " + provenance
		}
		return DoctorCheck{Name: name, Status: "OK", Message: msg}
	}

	// AGGREGATE FIRST, detail after. One image passing must not read as the
	// deployment passing: an operator who reads "agent OK" and stops has been
	// told the opposite of the truth when broker is stale. The aggregate can
	// never be greener than its worst row.
	sort.Strings(warned)
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf("%d OK, %d WARNING (%s). Agent-side code ships INSIDE these images, "+
			"so a release that changed both the daemon and an image is only half-applied here.%s",
			okCount, len(warn), strings.Join(warned, ", "), provenanceSuffix(provenance)),
		Items: warn,
	}
}

// obtainProvenanceSummary reports how this host obtained its images, or "" when
// it has no record to read.
func obtainProvenanceSummary() string {
	rec, err := imagemanifest.LoadObtained(imagemanifest.DefaultObtainedPath())
	if err != nil {
		// Corrupt is worth saying out loud: the host believes it is recording
		// provenance and is not.
		return fmt.Sprintf("Obtain provenance unreadable (%v)", err)
	}
	if len(rec.Images) == 0 {
		return ""
	}
	var pulled, built int
	for _, img := range rec.Images {
		switch img.Method {
		case imagemanifest.MethodPulled:
			pulled++
		case imagemanifest.MethodBuilt:
			built++
		}
	}
	return fmt.Sprintf("Obtained: %d pulled, %d built locally", pulled, built)
}

func provenanceSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

// legacyFreshness is the pre-record behaviour: compare each image's label
// against the running daemon's revision.
//
// It is REACHED ONLY when no release record exists — a source install or a dev
// box — and is preserved verbatim for that case. Every host installed before the
// record shipped lands here, so a change to it is a change to their experience.
func (h *DoctorHandlers) legacyFreshness(ctx context.Context, daemonRev string) DoctorCheck {
	name := "image_freshness"

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
		case !revisionsMatch(rev, daemonRev):
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

// revisionsMatch reports whether an image label and the daemon's revision name
// the same commit.
//
// They arrive in DIFFERENT SHAPES and a plain `!=` is wrong. The image label
// is a full 40-character SHA written by the build; the daemon's comes from
// version.BuildRevision, which truncates to git's 12-character short form and
// may append "-dirty". Comparing them directly made this check impossible to
// pass — it fired on a host where the daemon and every image had just been
// built from the same commit, and rendered the finding as
// "image e2c94d1a47bf, daemon e2c94d1a47bf" because the message shortens both
// for display. Two identical strings, reported as a difference.
//
// A control that always alarms is a control that gets ignored, which is worse
// than not having it: this check is what the packaged-EE backlog item relies
// on to make a half-applied release visible.
//
// Prefix comparison, not equality, because the shorter form is by construction
// a prefix of the longer. The dirty marker is stripped first — a dirty daemon
// build is a real thing to report, but it is not a DIFFERENT COMMIT, and the
// operator's fix differs (commit your tree vs rebuild the image).
func revisionsMatch(imageRev, daemonRev string) bool {
	image := strings.TrimSuffix(imageRev, dirtySuffix)
	daemon := strings.TrimSuffix(daemonRev, dirtySuffix)
	if image == "" || daemon == "" {
		return false
	}
	// Whichever is shorter must be a prefix of the other. Requiring a minimum
	// length keeps a truncated-to-nothing value from matching everything.
	shorter, longer := image, daemon
	if len(longer) < len(shorter) {
		shorter, longer = longer, shorter
	}
	if len(shorter) < minRevisionMatchLen {
		return false
	}
	return strings.HasPrefix(longer, shorter)
}

const (
	// dirtySuffix marks a build from a modified worktree. See
	// resolveDaemonRevision, which appends it.
	dirtySuffix = "-dirty"
	// minRevisionMatchLen is the shortest prefix we will accept as identifying
	// a commit. Git's short form is 12; anything shorter is not evidence.
	minRevisionMatchLen = 12
)
