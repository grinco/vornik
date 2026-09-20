package projectdeps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CompletionMarker is written LAST inside a materialisation and fsynced, so a
// half-materialised directory is never mistaken for a complete one — the
// checkpoint-durability rule this codebase already applies to config applies
// and step results (design §5.2).
const CompletionMarker = ".vornik-deps-complete"

// Posture is the deployment's network stance for dependency fetching.
type Posture string

const (
	// PostureConnected may fetch from an index.
	PostureConnected Posture = "connected"
	// PostureAirGapped may not. Materialise refuses and names the
	// operator action rather than leaving an agent to review code it
	// could not run.
	PostureAirGapped Posture = "air-gapped"
)

// ErrAirGapped is returned when a key is absent on an air-gapped deployment.
// Callers match on it to distinguish "the operator must import a bundle" from
// "the fetch failed", which are different problems with different fixes.
var ErrAirGapped = errors.New("dependency materialisation requires a fetch, and this deployment is air-gapped")

// ErrCorruptMaterialisation is returned when the key's directory exists but
// carries no completion marker. Materialise never produces that state (it
// renames a complete staging directory into place), so it means something
// else wrote there — and removing a directory an operator may have placed by
// hand is not a decision this code makes.
var ErrCorruptMaterialisation = errors.New("dependency cache entry exists without a completion marker")

// Fetcher populates dir with an ecosystem's materialised tree. It is called
// with a STAGING directory, never the live one, so a failure leaves the cache
// untouched.
type Fetcher func(ctx context.Context, dir string) error

// Store owns the content-addressed dependency cache under a root directory.
//
// Materialisation is leader-locked by the caller (a cluster-singleton daemon
// job). The mount, by contrast, is local to whichever host runs the container,
// so on a multi-node deployment each node materialises the keys it is asked to
// mount and skips the ones already marked complete (design §5.3a) — the
// content-addressed key is what makes the per-node result identical by
// construction.
type Store struct {
	root    string
	posture Posture
	now     func() time.Time
}

// NewStore returns a Store rooted at root. It does not touch the filesystem.
func NewStore(root string, posture Posture) *Store {
	return &Store{root: root, posture: posture, now: time.Now}
}

// Root is the deps root directory.
func (s *Store) Root() string { return s.root }

// Path is where key materialises. It is not a promise the key exists.
func (s *Store) Path(key string) string { return filepath.Join(s.root, key) }

// IsMaterialised reports whether key is present AND complete. A directory
// without the marker reports false, which is the conservative answer: the
// alternative mounts a partial tree and presents the missing half as an
// import error inside an agent.
func (s *Store) IsMaterialised(key string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.Path(key), CompletionMarker))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat completion marker for %s: %w", key, err)
	}
}

// Materialise returns the directory for key, fetching it first if it is not
// already complete. It is idempotent and safe to call on every mount.
func (s *Store) Materialise(ctx context.Context, key string, fetch Fetcher) (string, error) {
	target := s.Path(key)

	done, err := s.IsMaterialised(key)
	if err != nil {
		return "", err
	}
	if done {
		return target, nil
	}

	// Present but unmarked: not a state this code produces. Refuse and
	// name the path rather than deleting it.
	if _, statErr := os.Stat(target); statErr == nil {
		return "", fmt.Errorf("%w: %s — inspect it and remove it by hand if it is stale", ErrCorruptMaterialisation, target)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", target, statErr)
	}

	if s.posture == PostureAirGapped {
		return "", fmt.Errorf("%w: run `vornikctl deps import <bundle>` to supply %s", ErrAirGapped, key)
	}

	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return "", fmt.Errorf("create deps root %s: %w", s.root, err)
	}
	staging, err := os.MkdirTemp(s.root, ".staging-")
	if err != nil {
		return "", fmt.Errorf("create staging dir under %s: %w", s.root, err)
	}
	// Remove the staging tree on every failure path. On the success path
	// the rename has already emptied it, so this is a no-op.
	defer func() { _ = os.RemoveAll(staging) }()

	if err := fetch(ctx, staging); err != nil {
		return "", fmt.Errorf("materialise %s: %w", key, err)
	}
	if err := writeCompletionMarker(filepath.Join(staging, CompletionMarker), key, s.now()); err != nil {
		return "", err
	}

	if err := os.Rename(staging, target); err != nil {
		// Another materialiser of the same key won the race. Its result
		// is byte-identical by construction (the key IS the content), so
		// a complete target is a success, not a conflict.
		if done, checkErr := s.IsMaterialised(key); checkErr == nil && done {
			return target, nil
		}
		return "", fmt.Errorf("publish %s: %w", key, err)
	}
	if err := fsyncDir(s.root); err != nil {
		return "", fmt.Errorf("fsync deps root after publishing %s: %w", key, err)
	}
	return target, nil
}

// writeCompletionMarker writes and fsyncs the marker, then fsyncs its
// directory. Both are required: a marker in the page cache is a marker that
// can outlive a crash while the tree it vouches for does not.
func writeCompletionMarker(path, key string, at time.Time) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}
	body := fmt.Sprintf("key: %s\ncompleted: %s\n", key, at.UTC().Format(time.RFC3339))
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write completion marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync completion marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close completion marker: %w", err)
	}
	return fsyncDir(filepath.Dir(path))
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
