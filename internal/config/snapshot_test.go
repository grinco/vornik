package config

import "testing"

func TestSnapshotHolder_LatestWinsAndNilNeverBlanks(t *testing.T) {
	var h SnapshotHolder
	if h.Load() != nil {
		t.Fatal("empty holder must load nil")
	}
	a := DefaultConfig()
	a.Logging.Level = "a"
	h.Store(a, &Provenance{Path: "a.yaml"})
	b := DefaultConfig()
	b.Logging.Level = "b"
	h.Store(b, nil)
	if got := h.Load(); got == nil || got.Config.Logging.Level != "b" || got.Provenance != nil {
		t.Fatalf("latest store must win: %+v", got)
	}
	h.Store(nil, &Provenance{Path: "x"})
	if got := h.Load(); got.Config.Logging.Level != "b" {
		t.Fatal("a nil config must not replace the snapshot")
	}
	var nilHolder *SnapshotHolder
	nilHolder.Store(a, nil)
	if nilHolder.Load() != nil {
		t.Fatal("nil holder is inert")
	}
}
