package projectdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Plan is what one manifest entry resolves to before anything is fetched: the
// key it needs, whether that key is already materialised, and the reason it
// cannot be used if it cannot.
//
// The doctor check and the mount path compute the SAME plan. That is the
// point of the type: a doctor that answered "is this project's cache ready"
// by a different route than the mount uses would eventually answer it for a
// key the mount never asks for.
type Plan struct {
	Entry        Entry
	Key          string
	LockfilePath string
	Materialised bool
	// Problem is non-nil when the entry cannot produce a mount at all —
	// an unreadable or un-keyable lockfile. It is NOT "the fetch has not
	// happened yet", which is Materialised=false with a nil Problem.
	Problem error
}

// Mount returns the mount this plan produces. Only meaningful once the key is
// materialised.
func (p Plan) Mount(store *Store) Mount {
	return Mount{Ecosystem: p.Entry.Ecosystem, HostPath: store.Path(p.Key)}
}

// Resolver turns a project's manifest into plans and then into mounts.
type Resolver struct {
	store    *Store
	platform string
}

// NewResolver returns a Resolver over store. An empty platform uses the
// daemon's own GOOS/GOARCH, which is the materialising host — and the
// materialising host is the one whose wheels the container will import.
func NewResolver(store *Store, platform string) *Resolver {
	if platform == "" {
		platform = Platform()
	}
	return &Resolver{store: store, platform: platform}
}

// Store is the resolver's cache.
func (r *Resolver) Store() *Store { return r.store }

// Plan reads each entry's lockfile from projectRoot and reports what it
// resolves to. It never fetches and never writes, so it is safe to call from a
// doctor check on a live deployment.
func (r *Resolver) Plan(projectRoot string, entries []Entry) []Plan {
	plans := make([]Plan, 0, len(entries))
	for _, e := range entries {
		p := Plan{Entry: e}

		lockPath, err := resolveLockfilePath(projectRoot, e.Lockfile)
		if err != nil {
			p.Problem = err
			plans = append(plans, p)
			continue
		}
		p.LockfilePath = lockPath

		content, err := os.ReadFile(p.LockfilePath)
		if err != nil {
			// A manifest naming a lockfile the project does not have is
			// a config error, and it must read as one rather than as an
			// empty dependency set.
			p.Problem = fmt.Errorf("read lockfile: %w", err)
			plans = append(plans, p)
			continue
		}
		if err := RequireHashPinned(p.LockfilePath, content); err != nil {
			p.Problem = err
			plans = append(plans, p)
			continue
		}

		p.Key = CacheKey(e.Ecosystem, content, r.platform)
		done, err := r.store.IsMaterialised(p.Key)
		if err != nil {
			p.Problem = err
			plans = append(plans, p)
			continue
		}
		p.Materialised = done
		plans = append(plans, p)
	}
	return plans
}

// Ensure materialises every plan that needs it and returns the mounts.
//
// It refuses the WHOLE set on the first failure rather than mounting the part
// that worked. A partial mount is the reviewer problem again: the agent gets
// an environment that imports some of what the project declared, and the
// missing half surfaces as an ordinary ImportError that reads like the code's
// fault rather than the provisioner's.
func (r *Resolver) Ensure(ctx context.Context, plans []Plan) ([]Mount, error) {
	mounts := make([]Mount, 0, len(plans))
	for _, p := range plans {
		if p.Problem != nil {
			return nil, fmt.Errorf("dependency %s: %w", p.Entry.Ecosystem, p.Problem)
		}
		if p.Entry.Ecosystem != EcosystemPip {
			return nil, fmt.Errorf("dependency %s: not yet materialised — slice 1 provisions pip only", p.Entry.Ecosystem)
		}
		if _, err := r.store.Materialise(ctx, p.Key, WithSiteCustomise(PipFetcher(nil, p.LockfilePath))); err != nil {
			return nil, err
		}
		mounts = append(mounts, p.Mount(r.store))
	}
	if len(mounts) == 0 {
		// No manifest, no mounts, and therefore no env injection. Nil
		// rather than an empty slice so a caller cannot accidentally
		// inject an empty PYTHONPATH.
		return nil, nil
	}
	return mounts, nil
}

// resolveLockfilePath joins a manifest's lockfile path to the project root and
// refuses anything that leaves it.
//
// Validate refuses a traversing path at registry load, so this is the SECOND
// check — deliberately, because Plan must not depend on having been called
// only on validated input. filepath.Join cleans AFTER joining, so
// Join(root, "../../../etc/passwd") resolves outside the root without the
// caller doing anything visibly wrong; a test caught exactly that here.
func resolveLockfilePath(projectRoot, lockfile string) (string, error) {
	if lockfile == "" {
		return "", fmt.Errorf("no lockfile declared")
	}
	if filepath.IsAbs(lockfile) {
		return "", fmt.Errorf("lockfile %q must be relative to the project root", lockfile)
	}
	joined := filepath.Join(projectRoot, lockfile)
	rel, err := filepath.Rel(projectRoot, joined)
	if err != nil {
		return "", fmt.Errorf("resolve lockfile %q: %w", lockfile, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("lockfile %q escapes the project tree", lockfile)
	}
	return joined, nil
}
