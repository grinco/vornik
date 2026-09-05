package registry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TreeIndex is the whole-tree roll-up the resolved-config dump serves
// (`vornikctl config show --trees`, resolved-config provenance design §4.2):
// for every registry object, the file that supplied it and the layer it came
// from; the files the loader refused, with the decoder's error; and the
// role-library files present. It is built by the SAME load the daemon runs on
// — not a re-read of the tree from the CLI — so it agrees with `workflow
// show` and `mcp tools` by construction.
type TreeIndex struct {
	// Layers are the config directories that fed the active set, in
	// precedence order (later wins per id).
	Layers   []string       `json:"layers"`
	LoadedAt time.Time      `json:"loaded_at"`
	Sources  []SourceRecord `json:"sources"`
	Rejected []RejectedFile `json:"rejected,omitempty"`
}

// SourceRecord is one object and the file that supplied it.
type SourceRecord struct {
	Kind string `json:"kind"` // project | swarm | workflow | role-library
	ID   string `json:"id"`
	// Path is relative to the layer's config directory.
	Path  string `json:"path"`
	Layer string `json:"layer"`
	// ShadowedBy names the layer whose same-id object replaced this one, for
	// an object a later layer overrode. Empty for the object in effect.
	ShadowedBy string `json:"shadowed_by,omitempty"`
}

// RejectedFile is a file the loader refused and skipped.
type RejectedFile struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Layer string `json:"layer"`
	Error string `json:"error"`
}

func newTreeIndex(layer string) *TreeIndex {
	return &TreeIndex{Layers: []string{layer}, LoadedAt: time.Now().UTC()}
}

// source records an accepted object. nil-safe so the loaders can be called
// without an index.
func (ix *TreeIndex) source(kind, id, path string) {
	if ix == nil {
		return
	}
	ix.Sources = append(ix.Sources, SourceRecord{Kind: kind, ID: id, Path: path, Layer: ix.Layers[len(ix.Layers)-1]})
}

func (ix *TreeIndex) reject(kind, path string, err error) {
	if ix == nil {
		return
	}
	ix.Rejected = append(ix.Rejected, RejectedFile{Kind: kind, Path: path, Layer: ix.Layers[len(ix.Layers)-1], Error: err.Error()})
}

// indexRoleLibrary lists the role-library files. The library is read fresh on
// every evaluation (no cache — config/watcher.go), so "which file supplied
// it" is the file on disk at index time.
func (ix *TreeIndex) indexRoleLibrary(configDir string) {
	if ix == nil {
		return
	}
	entries, err := os.ReadDir(filepath.Join(configDir, "role-library"))
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.EqualFold(name, "README.md") {
			continue
		}
		ix.source("role-library", strings.TrimSuffix(name, ".md"), filepath.Join("role-library", name))
	}
}

// merge folds a later layer into ix: later wins per (kind, id), and the
// earlier object is marked shadowed rather than dropped.
func (ix *TreeIndex) merge(layer *TreeIndex) {
	if ix == nil || layer == nil {
		return
	}
	newLayer := layer.Layers[len(layer.Layers)-1]
	ix.Layers = append(ix.Layers, newLayer)
	byKey := map[string]int{}
	for i, s := range ix.Sources {
		byKey[s.Kind+"/"+s.ID] = i
	}
	for _, s := range layer.Sources {
		if i, ok := byKey[s.Kind+"/"+s.ID]; ok && ix.Sources[i].ShadowedBy == "" {
			ix.Sources[i].ShadowedBy = newLayer
		}
		ix.Sources = append(ix.Sources, s)
	}
	ix.Rejected = append(ix.Rejected, layer.Rejected...)
	ix.LoadedAt = layer.LoadedAt
}

// sorted returns a copy with stable ordering for output.
func (ix *TreeIndex) sorted() *TreeIndex {
	if ix == nil {
		return nil
	}
	out := *ix
	out.Sources = append([]SourceRecord(nil), ix.Sources...)
	out.Rejected = append([]RejectedFile(nil), ix.Rejected...)
	sort.SliceStable(out.Sources, func(i, j int) bool {
		a, b := out.Sources[i], out.Sources[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Layer < b.Layer
	})
	sort.SliceStable(out.Rejected, func(i, j int) bool { return out.Rejected[i].Path < out.Rejected[j].Path })
	return &out
}

// TreeIndex returns the index of the ACTIVE set — the live registry the
// per-object commands read — or nil before the first load.
func (r *Registry) TreeIndex() *TreeIndex {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		return nil
	}
	return r.active.index.sorted()
}
