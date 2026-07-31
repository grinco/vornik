package registry

import "testing"

// TestProjectMemory_GrantsMemoryWrite — the per-project channel opt-in
// (chat memory-write design §5.1) is deny-by-default: empty grants nobody,
// and matching is exact.
func TestProjectMemory_GrantsMemoryWrite(t *testing.T) {
	empty := ProjectMemory{}
	if empty.GrantsMemoryWrite("slack") {
		t.Error("zero-value ProjectMemory must grant nobody")
	}
	if empty.GrantsMemoryWrite("") {
		t.Error("empty channel is never granted")
	}

	m := ProjectMemory{WriteChannels: []string{"slack", "github"}}
	if !m.GrantsMemoryWrite("slack") || !m.GrantsMemoryWrite("github") {
		t.Error("listed channels must be granted")
	}
	if m.GrantsMemoryWrite("telegram") {
		t.Error("unlisted channel must not be granted")
	}
	if m.GrantsMemoryWrite("Slack") {
		t.Error("match is case-sensitive exact")
	}
}
