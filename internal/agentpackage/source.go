package agentpackage

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the manifest's name at the root of a package.
const ManifestFile = "vornik-package.yaml"

// maxPayloadBytes bounds a single contributed file. A package is a tarball an
// operator was handed, and an installer that reads an arbitrary-size member
// into memory is a denial of service with a friendly filename.
const maxPayloadBytes = 4 << 20

// maxPayloadFiles bounds how many members an archive may carry, for the same
// reason: a tarball with a million empty files would otherwise exhaust inodes
// before anything validated it.
const maxPayloadFiles = 512

// Source is an opened package: its manifest plus a reader over its payload.
type Source struct {
	Manifest Manifest
	root     string
	cleanup  func()
}

// ReadPayload reads one file from the package, refusing anything outside it.
func (s *Source) ReadPayload(rel string) ([]byte, error) {
	target, err := containedPath(s.root, rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxPayloadBytes {
		return nil, fmt.Errorf("%s is %d bytes; the per-file ceiling is %d", rel, info.Size(), maxPayloadBytes)
	}
	return os.ReadFile(target)
}

// Close releases anything the source extracted.
func (s *Source) Close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

// Open reads a package from a directory or a .tar.gz, and parses its manifest.
//
// Both forms are accepted deliberately. The design calls a package "a tarball
// someone hands you", and that is the artifact the lifecycle is for; a
// directory is what an author has while building one, and refusing it would
// make every iteration a repack.
func Open(path string) (*Source, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	src := &Source{}
	if info.IsDir() {
		src.root = path
	} else {
		dir, cleanup, err := extractArchive(path)
		if err != nil {
			return nil, err
		}
		src.root, src.cleanup = dir, cleanup
	}

	manifestPath := filepath.Join(src.root, ManifestFile)
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("read %s: %w", ManifestFile, err)
	}
	// Strict decoding: a key this schema does not model is a package built
	// for a newer Vornik, and installing the half of it this binary
	// understands is the upgrade-safety failure the project loader already
	// learned to refuse.
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&src.Manifest); err != nil {
		src.Close()
		return nil, fmt.Errorf("parse %s: %w", ManifestFile, err)
	}
	if err := src.Manifest.Validate(); err != nil {
		src.Close()
		return nil, err
	}
	return src, nil
}

// containedPath resolves rel under root and refuses anything that leaves it.
func containedPath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q must be relative to the package root", rel)
	}
	joined := filepath.Join(root, rel)
	within, err := filepath.Rel(root, joined)
	if err != nil {
		return "", err
	}
	if within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes the package tree", rel)
	}
	return joined, nil
}

// extractArchive unpacks a .tar.gz into a temporary directory.
//
// Every member is checked for containment before it is written. That is the
// zip-slip guard, and it is not optional: a package is an artifact from
// OUTSIDE this deployment, and "../../../.config/vornik/configs/workflows/x.md"
// is a member name a tar file is free to carry.
func extractArchive(path string) (string, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", nil, fmt.Errorf("%s is not a gzip archive (a package is a directory or a .tar.gz): %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	dir, err := os.MkdirTemp("", "vornik-package-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	tr := tar.NewReader(gz)
	files := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("read %s: %w", path, err)
		}
		if files++; files > maxPayloadFiles {
			cleanup()
			return "", nil, fmt.Errorf("%s carries more than %d members", path, maxPayloadFiles)
		}

		target, err := containedPath(dir, hdr.Name)
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("%s contains a member that escapes the package tree (%q)", path, hdr.Name)
		}

		if err := extractMember(tr, hdr, target); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return dir, cleanup, nil
}

// extractMember writes one already-contained archive member to target.
//
// Split out of extractArchive so the containment and counting logic stays
// readable beside the loop; the per-member rules are all here.
func extractMember(tr *tar.Reader, hdr *tar.Header, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o700)
	case tar.TypeReg:
		return extractRegular(tr, hdr, target)
	default:
		// Symlinks, devices, hardlinks: refused rather than skipped. A
		// skipped symlink is a package that installs differently than it
		// reads, and a followed one is the containment check undone by a
		// file the check already passed.
		return fmt.Errorf("member %q is not a regular file or directory; a package carries neither symlinks nor devices", hdr.Name)
	}
}

// extractRegular writes one regular file, bounded by the per-file ceiling.
func extractRegular(tr *tar.Reader, hdr *tar.Header, target string) error {
	if hdr.Size > maxPayloadBytes {
		return fmt.Errorf("member %q is %d bytes; the per-file ceiling is %d", hdr.Name, hdr.Size, maxPayloadBytes)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// io.CopyN, not io.Copy: the header's Size is the archive's claim about
	// ITSELF, and a truthful ceiling has to be enforced against the bytes
	// rather than against the claim.
	written, copyErr := io.CopyN(out, tr, maxPayloadBytes+1)
	if closeErr := out.Close(); closeErr != nil && copyErr == nil {
		return closeErr
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return copyErr
	}
	if written > maxPayloadBytes {
		return fmt.Errorf("member %q exceeds the per-file ceiling of %d bytes", hdr.Name, maxPayloadBytes)
	}
	return nil
}
