package config

import "sync/atomic"

// Snapshot is one resolved configuration with its provenance — what the
// daemon is running and where each value came from.
type Snapshot struct {
	Config     *Config
	Provenance *Provenance
}

// SnapshotHolder is the seam `config show` reads. Written at boot and by the
// reload activator, so the dump reflects the reload the daemon applied;
// c.Config itself is deliberately never swapped (many goroutines hold the
// pointer — container_registry.go), and the holder is what lets the API read
// the live resolution without that swap. Resolved-config provenance design
// §4.1.
type SnapshotHolder struct {
	p atomic.Pointer[Snapshot]
}

// Store publishes a snapshot. A nil config is ignored so a failed reload can
// never blank the dump.
func (h *SnapshotHolder) Store(cfg *Config, prov *Provenance) {
	if h == nil || cfg == nil {
		return
	}
	h.p.Store(&Snapshot{Config: cfg, Provenance: prov})
}

// Load returns the latest snapshot, or nil before the first Store.
func (h *SnapshotHolder) Load() *Snapshot {
	if h == nil {
		return nil
	}
	return h.p.Load()
}
