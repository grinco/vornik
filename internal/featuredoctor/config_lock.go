package featuredoctor

import (
	"path/filepath"
	"sync"
)

// configFileLocks maps a cleaned config-file path to its write mutex. Entries
// are never evicted — the set of config-file paths is tiny and bounded, so a
// stale *sync.Mutex for a no-longer-written path costs one pointer.
var configFileLocks sync.Map // string -> *sync.Mutex

// LockConfigFile acquires the process-wide write lock for one config file and
// returns its release func. EVERY read-modify-write transaction against a
// config file MUST hold it across the full backup→read→patch→write→validate→
// reload sequence: FileConfigWriter's temp+rename stops file CORRUPTION, not a
// LOST UPDATE (two transactions each read the old file, patch their own copy,
// and the second write clobbers the first's just-landed keys).
//
// Path-keyed: transactions targeting the SAME file serialize; transactions on
// DIFFERENT files run concurrently. Keyed on filepath.Clean(path) so equivalent
// path strings (e.g. "/x/./c.yaml" and "/x/c.yaml") share one lock.
//
// Process-local only. The multi-daemon case (a Postgres advisory lock) is
// deferred with the multi-instance work; a single daemon owns its config tree
// today.
func LockConfigFile(path string) (unlock func()) {
	key := filepath.Clean(path)
	// LoadOrStore may allocate a throwaway *sync.Mutex for racers that lose the
	// create; harmless (a pointer) and this path is admin-triggered + rare.
	v, _ := configFileLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
