// Package projectdeps provisions a project's declared dependencies into a
// content-addressed cache OUTSIDE the agent container, which the container
// then mounts read-only.
//
// The design is https://docs.vornik.io
// provisioning-design.md. Its load-bearing decision is §3: the agent never
// installs anything, on any role, in any deployment — which is what makes the
// air-gapped case the same code path as the connected one rather than a second
// mode only one kind of deployment ever exercises.
//
// Slice 1 (§7) is pip, connected deployments, the reviewer case.
package projectdeps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Ecosystem names a package ecosystem a manifest entry can declare.
type Ecosystem string

const (
	// EcosystemPip is the only ecosystem slice 1 materialises.
	EcosystemPip Ecosystem = "pip"
	// EcosystemNPM is recognised by the schema but not yet materialised, so
	// a project declaring it gets "not yet supported" rather than "unknown
	// ecosystem" — the difference between a gap and a typo.
	EcosystemNPM Ecosystem = "npm"
	// EcosystemGo is recognised on the same terms as EcosystemNPM.
	EcosystemGo Ecosystem = "go"
)

// Entry is one ecosystem's declaration inside a project's manifest.
type Entry struct {
	Ecosystem Ecosystem `yaml:"ecosystem"`
	// Lockfile is a path WITHIN the project tree. A lockfile rather than
	// a requirements list because the cache key must identify the exact
	// bytes that will be installed (design §5.1).
	Lockfile string `yaml:"lockfile"`
	// Install is declared only so it can be REFUSED. It is not a field
	// this schema supports; an implementer or operator who adds one gets
	// a validation error naming the 2026-08-03 ruling instead of a
	// silently ignored key that looks like it works.
	Install string `yaml:"install"`
}

// Manifest is a project's full dependency declaration.
type Manifest struct {
	Entries []Entry `yaml:"dependencies"`
}

// ValidationError is a manifest defect, addressed to whoever can fix it.
type ValidationError struct {
	Index   int
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("dependencies[%d]: %s", e.Index, e.Message)
	}
	return fmt.Sprintf("dependencies[%d].%s: %s", e.Index, e.Field, e.Message)
}

// Validate checks the manifest's shape. It does NOT read the lockfiles —
// that is RequireHashPinned's job, which needs the bytes and therefore the
// project tree.
func (m Manifest) Validate() error {
	seen := make(map[Ecosystem]int, len(m.Entries))
	for i, e := range m.Entries {
		if e.Install != "" {
			return ValidationError{Index: i, Field: "install", Message: "not a supported field: an install command is a remote-triggered exec by another name, which the 2026-08-03 ruling forbids for every path reachable from chat, API, A2A or MCP — and a project config is editable through the control plane, which is reachable from all four. Declare a lockfile instead"}
		}
		switch e.Ecosystem {
		case "":
			return ValidationError{Index: i, Field: "ecosystem", Message: "is required"}
		case EcosystemPip:
		case EcosystemNPM, EcosystemGo:
			return ValidationError{Index: i, Field: "ecosystem", Message: fmt.Sprintf("%q is recognised but not yet materialised — slice 1 provisions pip only", e.Ecosystem)}
		default:
			return ValidationError{Index: i, Field: "ecosystem", Message: fmt.Sprintf("unknown ecosystem %q (known: pip, npm, go)", e.Ecosystem)}
		}
		if prev, dup := seen[e.Ecosystem]; dup {
			return ValidationError{Index: i, Field: "ecosystem", Message: fmt.Sprintf("%q is already declared at dependencies[%d]; one entry per ecosystem", e.Ecosystem, prev)}
		}
		seen[e.Ecosystem] = i

		if err := validateLockfilePath(i, e.Lockfile); err != nil {
			return err
		}
	}
	return nil
}

// validateLockfilePath keeps the lockfile inside the project tree. An
// absolute path or a ".." escape would let a project config name a file the
// operator never put in the repo, and the cache key would then describe bytes
// nobody reviewed.
func validateLockfilePath(idx int, p string) error {
	if p == "" {
		return ValidationError{Index: idx, Field: "lockfile", Message: "is required — a floating requirement cannot identify the bytes that will be installed"}
	}
	if filepath.IsAbs(p) {
		return ValidationError{Index: idx, Field: "lockfile", Message: "must be relative to the project root, not an absolute path"}
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ValidationError{Index: idx, Field: "lockfile", Message: "must stay within the project tree"}
	}
	return nil
}

// Platform is the {os, arch} half of the cache key.
//
// It is in the key for EVERY ecosystem, including ones whose resolution is
// arch-independent (design §3): a key whose SHAPE depends on the ecosystem is
// a key that will be got wrong once.
func Platform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// CacheKey is {ecosystem, lockfile hash, platform} — the directory name under
// the deps root. A manifest change is a NEW key rather than a mutation of a
// live one, so a task that started against the old key finishes against it.
func CacheKey(eco Ecosystem, lockfile []byte, platform string) string {
	sum := sha256.Sum256(lockfile)
	return fmt.Sprintf("%s-%s-%s", eco, sanitisePlatform(platform), hex.EncodeToString(sum[:]))
}

// sanitisePlatform keeps the key a single safe path segment whatever a caller
// passes (GOOS/GOARCH are safe; an operator-supplied override may not be).
func sanitisePlatform(p string) string {
	if p == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range p {
		switch {
		// '.' is deliberately NOT in the safe set: GOOS/GOARCH never
		// carry one, and permitting it lets an operator-supplied
		// override put ".." in a directory name.
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
