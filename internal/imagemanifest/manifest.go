// Package imagemanifest is the single source of truth for the container
// images this repo builds, and for when each one is part of a deployment.
//
// It exists because that question had no owner. Until 2026-08-25 the answer
// lived in whichever consumer you happened to read: `make build-images` built
// one set, `vornik-update.sh` rebuilt exactly one image and only when asked,
// the quickstart rebuilt a different one unconditionally, and nothing built
// the two tags `cluster.compose.yaml` requires. A CE customer ran the
// documented update path for six weeks and never received an agent image; the
// daemon advanced every release while the image stayed frozen at install date.
// That split half-applied commit 356e74cd, a security fix spanning
// internal/agenttools AND images/vornik-agent/entrypoint.sh.
//
// The package is Go-owned rather than a shell manifest so the daemon's doctor
// check and the shell build paths read the same rows. It is deliberately NOT
// derived from swarm configs: registry.LoadSwarms answers "which images do
// configured roles launch" (per-host runtime truth), and every role image in
// the tree is localhost/vornik-agent:latest. The broker, scraper and cluster
// images are MCP sidecars and compose services that no role references, so
// swarm configs are structurally incapable of naming them. See
// https://docs.vornik.io §4.2.
package imagemanifest

import "strings"

// Condition values. A row's condition states when its image is part of a
// deployment, and is resolved against operator INTENT rather than running
// state — see Needed.
const (
	// ConditionAlways marks an image every install needs.
	ConditionAlways = "always"
	// ConditionTest marks a CI-only image. Never built by an update.
	ConditionTest = "test"
	// ConditionExcluded marks an image this repo does not own. Carried in
	// the manifest so the parity test can see it has a home, never built.
	ConditionExcluded = "excluded"

	// ConditionUnitPrefix gates an image on a systemd user unit being
	// ENABLED (intent), not active.
	ConditionUnitPrefix = "unit:"
	// ConditionComposePrefix gates an image on a compose stack having any
	// containers, INCLUDING stopped ones (intent).
	ConditionComposePrefix = "compose:"
)

// AgentImageTag is the image every install runs agents from. Named because
// several consumers special-case it and a string literal in each is how the
// swarmd->vornik rename left configs pointing at an unbuilt short name.
//
// ONE NAME, ONLINE OR NOT (2026-08-28). This is a registry reference, but an
// air-gapped host does NOT need the registry: it builds the image locally and
// tags it with this same name. The reference is then identical everywhere and
// only the provenance differs — built locally versus pulled — which is what the
// release record (record.go) distinguishes. Two names for one image was the
// alternative, and it would have meant every config, template and doc carrying
// a conditional nobody could test both halves of.
//
// This works offline because podman's default pull policy is "missing" (podman
// 5.8.4, verified on the reference host) and the daemon sets no --pull flag, so
// a locally present image is used without touching the network. An explicit
// --pull=always anywhere in the run path would silently break air-gapped
// installs; see 2026-08-28-packaged-image-provenance-design.md §3.1.1.
const AgentImageTag = "ghcr.io/grinco/vornik-agent:latest"

// Image is one buildable container image.
type Image struct {
	// Tag is the fully-qualified local reference. Qualified deliberately:
	// under podman's enforced short-name mode a bare name cannot resolve
	// without a TTY, so an unqualified ref fails every job at container
	// start.
	Tag string
	// Containerfile is repo-relative.
	Containerfile string
	// Target is the build stage to select, or "" to build the last stage.
	// Only the cluster images use it: thin and full share one Dockerfile
	// and are distinguishable ONLY by --target.
	Target string
	// Context is the repo-relative build context.
	Context string
	// Condition states when this image is part of a deployment.
	Condition string
}

// Prober answers questions about host state. Injected so condition
// evaluation is unit-testable without systemctl or podman.
type Prober interface {
	// UnitEnabled reports whether a systemd user unit is enabled.
	UnitEnabled(name string) bool
	// StackHasContainers reports whether a compose stack has any
	// containers, running or stopped.
	StackHasContainers(stack string) bool
}

// manifest is the authoritative list. Adding an image here is what makes it
// covered by every build and update path (contract C4).
// baseImages is the Community manifest: every image whose Containerfile ships
// in the public CE tree.
//
// Enterprise-only images live in manifest_enterprise.go, which the CE export
// prunes, and are appended to `manifest` by that file's init(). Keeping them
// out of THIS slice is what makes the split real: scripts/export-public-ce.sh
// does `rm -rf services images/vornik-broker images/vornik-scraper-login`, so
// an EE entry left here would name a Containerfile absent from the tree it
// shipped in — which is exactly what CE shipped until 2026-08-26, when the
// export's EE-feature marker sweep caught one of these rows by name and the
// other five dead references came out with it.
var baseImages = []Image{

	{
		Tag:           AgentImageTag,
		Containerfile: "images/vornik-agent/Containerfile",
		Context:       ".",
		Condition:     ConditionAlways,
	},
	// The cluster pair. Both come from deployments/docker/Dockerfile and
	// differ only by build stage. Before 2026-08-25 neither tag had a
	// builder at all: `docker-build` passed no --target and tagged the
	// last stage $(DOCKER_IMAGE):$(IMAGE_TAG), which no compose file
	// references, while cluster.compose.yaml and both clustering guides
	// name thin and full.
	{
		Tag:           "localhost/vornik:thin",
		Containerfile: "deployments/docker/Dockerfile",
		Target:        "thin",
		Context:       ".",
		Condition:     ConditionComposePrefix + "cluster",
	},
	{
		Tag:           "localhost/vornik:full",
		Containerfile: "deployments/docker/Dockerfile",
		Target:        "full",
		Context:       ".",
		Condition:     ConditionComposePrefix + "cluster",
	},
	{
		Tag:           "fake-agent:latest",
		Containerfile: "images/fake-agent/Containerfile",
		Context:       "images/fake-agent",
		Condition:     ConditionTest,
	},
}

// manifest is the authoritative list for THIS build. Adding an image here (or
// to enterpriseImages) is what makes it covered by every build and update path
// (contract C4).
//
// Assembled from baseImages so the enterprise file can append without either
// side needing a build tag — the edition boundary is which FILES ship, which is
// the boundary the export already enforces.
//
// WHY NOT BUILD TAGS. A `//go:build enterprise` pair would give the same file
// boundary AND catch a mis-filed image at COMPILE time rather than at export
// time, which is earlier. It was not chosen because it needs a second build
// configuration and the discipline to keep the tags mutually exclusive, while
// the export is already the enforcement point and already prunes by file. That
// is a real trade, not an obvious win: if EE ever builds CE artifacts directly
// (without the export), revisit it, because then nothing would catch a mis-file
// until export time.
//
// EXACTLY ONE init() MAY EXIST IN THIS PACKAGE, and it is the append in
// manifest_enterprise.go. Go does not specify init() ordering within a package
// beyond the order files are presented to the compiler, so a SECOND init() that
// READS `manifest` could observe it before or after the enterprise rows are
// appended — a silent, build-dependent correctness bug. If this package ever
// needs another init(), give `manifest` an explicit accessor that assembles on
// first use instead.
var manifest = append([]Image(nil), baseImages...)

// excluded lists Containerfiles IN THIS TREE that are deliberately not built
// as part of a deployment, each with the reason. It exists so the parity test
// can distinguish "deliberately not ours" from "nobody wired it up" — the
// distinction the cluster-tag gap collapsed.
//
// Currently empty, and that is the accurate state: every Containerfile in the
// tree is one we build. The nearest candidate, PageDrop, has no Containerfile
// here at all — deployments/podman/pagedrop/sync.sh clones upstream and builds
// from the upstream Dockerfile, so the file never lands in this repo and the
// walk never sees it. Adding a row for it would have been dead config masking
// a real future file.
var excluded = map[string]string{}

// isExcluded reports whether a repo-relative Containerfile path is
// deliberately outside the manifest.
func isExcluded(rel string) bool {
	_, ok := excluded[rel]
	return ok
}

// All returns every manifest row.
func All() []Image {
	out := make([]Image, len(manifest))
	copy(out, manifest)
	return out
}

// Needed reports whether this image is part of the deployment on this host.
//
// Conditions resolve against operator INTENT, not running state. An enabled
// unit and a stopped-but-present compose stack both express intent, so their
// images are rebuilt; a disabled unit and a torn-down stack do not.
//
// The distinction is load-bearing. Resolving `compose:` against RUNNING
// containers — the first draft of the design — reopens the defect the design
// closes, merely delayed: a stack stopped for maintenance is skipped by the
// update and then restarted onto code older than the daemon, invisibly.
func (i Image) Needed(p Prober) bool {
	switch {
	case i.Condition == ConditionAlways:
		return true
	case i.Condition == ConditionTest, i.Condition == ConditionExcluded:
		return false
	case strings.HasPrefix(i.Condition, ConditionUnitPrefix):
		return p.UnitEnabled(strings.TrimPrefix(i.Condition, ConditionUnitPrefix))
	case strings.HasPrefix(i.Condition, ConditionComposePrefix):
		return p.StackHasContainers(strings.TrimPrefix(i.Condition, ConditionComposePrefix))
	default:
		// An unrecognised condition is a typo, and a typo must drop the
		// row rather than resolve to "build everything".
		return false
	}
}

// Deployable returns the manifest rows whose condition holds on this host.
func Deployable(p Prober) []Image {
	var out []Image
	for _, img := range manifest {
		if img.Needed(p) {
			out = append(out, img)
		}
	}
	return out
}

// EmitRows renders images as tab-separated rows for shell consumers:
//
//	tag<TAB>containerfile<TAB>target<TAB>context<TAB>condition
//
// No header line, one row per image. Shell reads these with
// `while IFS=$'\t' read -r tag file target ctx cond`, which mis-assigns every
// column if a field ever contains a space or the output grows a header — both
// asserted against in the tests.
//
// Target is empty for single-stage images; every other field is always set.
func EmitRows(images []Image) string {
	var b strings.Builder
	for _, img := range images {
		b.WriteString(img.Tag)
		b.WriteByte('\t')
		b.WriteString(img.Containerfile)
		b.WriteByte('\t')
		b.WriteString(img.Target)
		b.WriteByte('\t')
		b.WriteString(img.Context)
		b.WriteByte('\t')
		b.WriteString(img.Condition)
		b.WriteByte('\n')
	}
	return b.String()
}
