package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"vornik.io/vornik/internal/imagemanifest"
)

// `vornik-images -obtain` — Stage 2's obtain step.
//
// Design: 2026-08-28-packaged-image-provenance-design.md, "Amendment
// 2026-09-06 — Stage 2: the obtain half".
//
// It decides, pulls, and records; then prints to STDOUT the manifest rows that
// still need a LOCAL BUILD, in the same five-column format `vornik-images`
// already emits. The shell consumes those and builds them exactly as before.
//
// WHY THE SPLIT IS HERE. The decision has to be Go-owned so the daemon's doctor
// check and the update path reach the same verdict (§4.2) — and before this,
// vornik-update.sh carried its own copy of the skip rule, which is precisely
// the "safety check with two implementations" the tenets warn about. The BUILD
// stays in the shell because that is where the build args, uid/gid and
// Containerfile paths already live. So: Go decides and obtains, the shell
// builds what Go could not.

// obtainOpts is the injectable surface, so the command is testable without a
// registry or a container engine.
type obtainOpts struct {
	arch       string
	head       string
	recordPath string
	obtainedAt string

	inspect func(tag string) (imagemanifest.LocalImage, error)
	resolve imagemanifest.DigestLookup
	pull    func(ref string) error
	remove  func(ref string) error
	log     func(format string, args ...any)
}

func runObtain(images []imagemanifest.Image, o obtainOpts) (build []imagemanifest.Image, err error) {
	rec, recErr := imagemanifest.LoadReleaseRecord(o.recordPath)
	switch {
	case recErr == nil:
	case errors.Is(recErr, imagemanifest.ErrRecordAbsent):
		// Legitimate: a source install, a dev box, a CE quickstart. The commit
		// tag answers instead.
		rec = nil
	case imagemanifest.IsCorrupt(recErr):
		// NOT the same as absent. A corrupt record means the host cannot know
		// what the release declared, and quietly degrading to the commit tag
		// would let it obtain something the release never named.
		return nil, fmt.Errorf("release record at %s: %w", o.recordPath, recErr)
	default:
		return nil, recErr
	}

	obtained, err := imagemanifest.LoadObtained(imagemanifest.DefaultObtainedPath())
	if err != nil {
		return nil, err
	}

	for _, img := range images {
		local, err := o.inspect(img.Tag)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", img.Tag, err)
		}
		target, err := imagemanifest.ResolveTarget(img.Tag, rec, o.arch, o.head, o.resolve)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", img.Tag, err)
		}

		action, reason := imagemanifest.Decide(img.Tag, local, target)
		switch action {
		case imagemanifest.ActionSkip:
			o.log("  %s — already current", img.Tag)

		case imagemanifest.ActionLeave:
			// Change nothing, and say so. A host that changes nothing and says
			// nothing is indistinguishable from one that checked and was happy.
			o.log("  %s — LEFT AS IS: %s", img.Tag, reason)

		case imagemanifest.ActionPull:
			ref := refWithDigest(img.Tag, target.Digest)
			o.log("  %s — pulling %s", img.Tag, target.Digest)
			if perr := o.pull(ref); perr != nil {
				// A partial pull can leave a manifest with incomplete layers.
				// Building over that, or leaving it for the runtime to serve,
				// is how a broken image reaches a task (§S2.2).
				if rerr := o.remove(ref); rerr != nil {
					o.log("  %s — could not clean the partial pull: %v", img.Tag, rerr)
				}
				o.log("  %s — pull failed (%v); building locally instead", img.Tag, perr)
				build = append(build, img)
				obtained.Note(imagemanifest.ObtainedImage{
					Tag: img.Tag, Method: imagemanifest.MethodBuilt, Reference: o.head, At: o.obtainedAt,
				})
				continue
			}
			obtained.Note(imagemanifest.ObtainedImage{
				Tag: img.Tag, Method: imagemanifest.MethodPulled, Reference: ref,
				// What we resolved THROUGH, kept beside what we got, so a
				// moved tag is detectable afterwards rather than absorbed.
				ResolvedFrom: imagemanifest.CommitTag(img.Tag, o.head), At: o.obtainedAt,
			})

		case imagemanifest.ActionBuild:
			if reason != "" {
				o.log("  %s — %s", img.Tag, reason)
			}
			build = append(build, img)
			obtained.Note(imagemanifest.ObtainedImage{
				Tag: img.Tag, Method: imagemanifest.MethodBuilt, Reference: o.head, At: o.obtainedAt,
			})
		}
	}

	if err := obtained.Save(imagemanifest.DefaultObtainedPath()); err != nil {
		// Fatal on purpose. C5 says a host records what it obtained; a run that
		// obtained images and could not say so leaves the host in exactly the
		// state this contract exists to make impossible.
		return nil, fmt.Errorf("cannot record what this host obtained: %w", err)
	}
	return build, nil
}

// refWithDigest turns a tag into its digest reference, dropping any :tag.
func refWithDigest(tag, digest string) string {
	repo, _, _ := strings.Cut(tag, ":")
	return repo + "@" + digest
}

// --- the real host bindings -------------------------------------------------

func podmanInspect(tag string) (imagemanifest.LocalImage, error) {
	out, err := exec.Command("podman", "image", "inspect", tag,
		"--format", `{{json .RepoDigests}}{{"\t"}}{{index .Labels "org.opencontainers.image.revision"}}`).Output()
	if err != nil {
		// Absent is the common case and is not an error.
		return imagemanifest.LocalImage{}, nil
	}
	digestsJSON, revision, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	local := imagemanifest.LocalImage{Present: true, Revision: strings.TrimSpace(revision)}
	if local.Revision == "<no value>" {
		local.Revision = ""
	}
	var digests []string
	if err := json.Unmarshal([]byte(digestsJSON), &digests); err == nil {
		// podman reports "repo@sha256:…"; the selector compares bare digests.
		for _, d := range digests {
			if _, dig, ok := strings.Cut(d, "@"); ok {
				local.Digests = append(local.Digests, dig)
			}
		}
	}
	return local, nil
}

func skopeoResolve(ref string) (string, error) {
	cmd := exec.Command("skopeo", "inspect", "--format", "{{.Digest}}",
		"--override-os", "linux", "--override-arch", runtime.GOARCH, "docker://"+ref)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", imagemanifest.ClassifyLookupError(err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func podmanPull(ref string) error {
	cmd := exec.Command("podman", "pull", ref)
	cmd.Stdout = os.Stderr // progress belongs on stderr; stdout carries rows
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func podmanRemove(ref string) error {
	// Ignore "no such image": the point is that nothing partial is left.
	_ = exec.Command("podman", "rmi", "-f", ref).Run()
	return nil
}
