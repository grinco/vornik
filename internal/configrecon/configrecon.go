// Package configrecon is the config-tree round-trip guard: a small registry of
// canonical-field normalizers applied at the control-plane MIRROR SEAM, before a
// deployed file is written back over the operator's source checkout and
// git-committed.
//
// Motivation (https://docs.vornik.io
// design.md, Slice A / v1). The apply engine drafts proposals against the
// DEPLOYED tree and mirrors the whole file byte-for-byte back into the repo. A
// stale CANONICAL FIELD in deployed — most notably a swarm role `image:` that
// regressed to a bare `vornik-agent:latest` short-name — therefore round-trips
// into source on every apply touching that file. That exact defect re-broke the
// repo 3× (see 2026-07-23-agent-image-qualification-audit.md). This package
// normalises such declared-defect fields at the seam so they can never reach
// source, WITHOUT perturbing legitimate operator tuning.
//
// Normalizer contract (LLD §3.1, review A1/A2), honoured by every MirrorSafe
// normalizer registered here:
//   - Pure + deterministic, NO I/O — a byte→byte transform reading no external
//     state (else the "MirrorSafe" claim is false).
//   - Idempotent — Normalize(Normalize(x)) == Normalize(x); a second apply over
//     already-normalized content is a no-op (empty git diff, no churn commit).
//   - NO error return by design — a normalizer that cannot parse its input
//     PASSES THROUGH unchanged (changed=false); it never errors or panics, so a
//     malformed file is mirrored verbatim (the pre-feature behaviour) rather
//     than risking a bad rewrite.
//   - AppliesTo is evaluated on the CONFIG-ROOT-RELATIVE path (review A5) so
//     host-local files (anything under projects/) can never match.
package configrecon

import (
	"path"
	"strings"

	"vornik.io/vornik/internal/runtime"
)

// RiskClass ranks where a normalizer is safe to run.
type RiskClass int

const (
	// MirrorSafe normalizers are safe to run at the git-committed mirror seam:
	// they only rewrite declared canonical-defect fields that are, by their
	// required justification, never legitimate operator tuning.
	MirrorSafe RiskClass = iota
	// ReconcilerOnly normalizers are higher-risk and must only run via the
	// (v2, §10) backed-up canonical-file refresh reconciler — never at the
	// mirror seam. ApplyMirrorNormalizers skips them.
	ReconcilerOnly
)

// Normalizer is one canonical-field defect class and its deterministic fix.
type Normalizer struct {
	// Name identifies the normalizer in logs, metrics, and commit trailers.
	Name string
	// Justification is REQUIRED (review F5/A7): why this field is ALWAYS a
	// defect and NEVER operator tuning. The registry lint enforces presence;
	// correctness is the human code-review gate.
	Justification string
	// Risk gates where the normalizer may run (see RiskClass).
	Risk RiskClass
	// AppliesTo reports whether this normalizer applies to a given
	// config-root-relative path (review A5).
	AppliesTo func(relPath string) bool
	// Normalize is the pure byte→byte transform (see the package contract). It
	// returns the (possibly rewritten) content, whether it changed anything,
	// and a human-readable note describing the change (empty when unchanged).
	Normalize func(content []byte) (out []byte, changed bool, note string)
}

// NormalizationNote records a normalizer that actually fired, keeping the
// normalizer name bound to its message (LLD §3.1 minor).
type NormalizationNote struct {
	Name    string
	Message string
	Changed bool
}

// registered is the ordered package registry of normalizers.
// ApplyMirrorNormalizers runs them in this order. It is a package var (not a
// const) so tests can swap in probe normalizers; production code never mutates
// it after init.
var registered = []Normalizer{
	agentImageQualify(),
}

// ApplyMirrorNormalizers runs every registered Risk==MirrorSafe normalizer whose
// AppliesTo matches relPath, in registry order, feeding each normalizer the
// output of the previous one. It returns the final content and a note for ONLY
// those normalizers that actually changed the content (change-only emission,
// review A4) — an AppliesTo-match that makes no change is silent. ReconcilerOnly
// normalizers are never run here.
func ApplyMirrorNormalizers(relPath string, content []byte) ([]byte, []NormalizationNote) {
	var notes []NormalizationNote
	out := content
	for _, n := range registered {
		if n.Risk != MirrorSafe {
			continue
		}
		if n.AppliesTo == nil || n.Normalize == nil {
			continue
		}
		if !n.AppliesTo(relPath) {
			continue
		}
		next, changed, note := n.Normalize(out)
		if changed {
			out = next
			notes = append(notes, NormalizationNote{Name: n.Name, Message: note, Changed: true})
		}
	}
	return out, notes
}

// agentImageQualify builds the one v1 normalizer: it qualifies a bare
// `image: vornik-agent[:tag|@digest]` value in a swarm file to
// `localhost/vornik-agent…`, reusing runtime.QualifyAgentImage so the rule is
// not duplicated. Only the image line is rewritten; every other byte is left
// identical.
func agentImageQualify() Normalizer {
	return Normalizer{
		Name: "agent-image-qualify",
		Justification: "a bare short-name agent image cannot be pulled non-interactively " +
			"(podman short-name resolution); localhost/ qualification is never operator " +
			"intent — incident 2026-07-23, re-broke repo 3× " +
			"(https://docs.vornik.io).",
		Risk:      MirrorSafe,
		AppliesTo: isSwarmFile,
		Normalize: normalizeSwarmImage,
	}
}

// isSwarmFile matches swarms/*.md on the config-root-relative path. A leading
// "configs/" is tolerated (the mirror may pass either "configs/swarms/x.md" or
// "swarms/x.md"). Files nested deeper than one level under swarms/, and any
// host-local path (e.g. projects/…/swarms/x.md), do NOT match — that is the
// review-A5 guarantee that host-local files stay untouched.
func isSwarmFile(relPath string) bool {
	rel := path.Clean(strings.ReplaceAll(relPath, "\\", "/"))
	rel = strings.TrimPrefix(rel, "configs/")
	dir, file := path.Split(rel)
	return dir == "swarms/" && strings.HasSuffix(file, ".md") && file != ".md"
}

// normalizeSwarmImage rewrites bare agent `image:` lines that live in the swarm
// file's YAML FRONTMATTER. It is line-based (LLD §3.1): it never parses YAML, so
// an unparseable file simply has none of its lines matched and passes through
// unchanged. Only an `image:` key found INSIDE the leading `---` … `---` fence is
// considered — the markdown BODY (the agent system-prompt prose after the second
// `---`) is never matched, because this is a config-tree guard whose contract is
// to touch config keys, never operator content, at the irreversible git-committed
// mirror seam (review I1). A file with no frontmatter fence therefore has nothing
// to normalise. Matched lines are qualified via runtime.QualifyAgentImage and
// re-emitted preserving the original indentation, key spacing, quote style, and
// any trailing whitespace/line ending.
func normalizeSwarmImage(content []byte) (out []byte, changed bool, note string) {
	// SplitAfter keeps each line's terminator so the join is byte-exact.
	pieces := strings.SplitAfter(string(content), "\n")
	anyChanged := false
	// Frontmatter tracking: the block is delimited by the FIRST two lines whose
	// trimmed content is exactly "---". We only consider lines strictly between
	// them. A file whose first non-empty line is not "---" has no frontmatter,
	// so inFrontmatter never opens and no line is matched.
	sawOpenFence := false
	inFrontmatter := false
	for i, piece := range pieces {
		if isFrontmatterFence(piece) {
			if !sawOpenFence {
				sawOpenFence = true
				inFrontmatter = true
				continue
			}
			if inFrontmatter {
				// Closing fence: everything after this is the markdown body.
				inFrontmatter = false
			}
			continue
		}
		// The opening fence must be the file's first non-blank line; if body
		// content appears before any fence, treat the file as fence-less.
		if !sawOpenFence {
			if strings.TrimSpace(stripEOL(piece)) != "" {
				break // no frontmatter — nothing to normalise
			}
			continue
		}
		if !inFrontmatter {
			continue // in the markdown body — never rewrite
		}
		newPiece, lineChanged := normalizeImageLine(piece)
		if lineChanged {
			pieces[i] = newPiece
			anyChanged = true
		}
	}
	if !anyChanged {
		return content, false, ""
	}
	return []byte(strings.Join(pieces, "")), true, "qualified bare vornik-agent image to localhost/…"
}

// stripEOL returns the line without a trailing "\n"/"\r\n".
func stripEOL(piece string) string {
	piece = strings.TrimSuffix(piece, "\n")
	return strings.TrimSuffix(piece, "\r")
}

// isFrontmatterFence reports whether a line is a YAML frontmatter delimiter — its
// content (sans line ending) is exactly "---".
func isFrontmatterFence(piece string) bool {
	return stripEOL(piece) == "---"
}

// normalizeImageLine rewrites a single `image:` line if it carries a bare
// vornik-agent short-name; otherwise it returns the line untouched. The line
// may include a trailing "\n"/"\r\n" which is preserved verbatim.
func normalizeImageLine(piece string) (string, bool) {
	// Peel off the line ending so it survives byte-for-byte.
	body := piece
	eol := ""
	if strings.HasSuffix(body, "\n") {
		body = body[:len(body)-1]
		eol = "\n"
	}
	if strings.HasSuffix(body, "\r") {
		body = body[:len(body)-1]
		eol = "\r" + eol
	}

	trimmed := strings.TrimLeft(body, " \t")
	indent := body[:len(body)-len(trimmed)]
	const key = "image:"
	if !strings.HasPrefix(trimmed, key) {
		return piece, false
	}
	afterKey := trimmed[len(key):]

	// Separate the spacing after the key, the value core, and trailing space.
	valWithTrail := strings.TrimLeft(afterKey, " \t")
	keySpaces := afterKey[:len(afterKey)-len(valWithTrail)]
	core := strings.TrimRight(valWithTrail, " \t")
	trail := valWithTrail[len(core):]
	if core == "" {
		return piece, false
	}

	newCore, coreChanged := qualifyValueCore(core)
	if !coreChanged {
		return piece, false
	}
	return indent + key + keySpaces + newCore + trail + eol, true
}

// qualifyValueCore qualifies a YAML scalar value that may be double-quoted,
// single-quoted, or bare. It only rewrites a clean single token (or a cleanly
// quoted value) — anything with embedded whitespace that is not fully quoted
// (e.g. a trailing inline comment) is treated as complex and passed through, so
// the normalizer never mangles a line it doesn't fully understand.
func qualifyValueCore(core string) (string, bool) {
	if len(core) >= 2 {
		q := core[0]
		if (q == '"' || q == '\'') && core[len(core)-1] == q {
			inner := core[1 : len(core)-1]
			qual := runtime.QualifyAgentImage(inner)
			if qual == inner {
				return core, false
			}
			return string(q) + qual + string(q), true
		}
	}
	// Bare value: only a single whitespace-free token is eligible. A bare value
	// carrying a trailing inline comment (e.g. `vornik-agent:latest # note`)
	// contains whitespace and so is a DELIBERATE pass-through here (review M3) —
	// if such a form ever reaches a spawn, the runtime QualifyAgentImage
	// fail-safe still qualifies it, so nothing breaks; we simply decline to
	// rewrite a line the line-based parser can't split cleanly.
	if strings.ContainsAny(core, " \t") {
		return core, false
	}
	qual := runtime.QualifyAgentImage(core)
	if qual == core {
		return core, false
	}
	return qual, true
}
