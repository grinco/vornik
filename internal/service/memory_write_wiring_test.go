package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestEveryChannelReceiverWiresMemoryWriteConfirmations is the chat
// memory-write analogue of TestEveryChannelReceiverWiresTheAIDisclosure: the
// shared-scope acknowledgement (chat memory-write design §5.3.2 step 2) is
// stamped inside dispatcher.ChannelReceiver, which is constructed at five
// sites. If a site forgets MemoryWriteConfirmations, a human's acknowledgement
// on that channel can never advance a shared write past PROPOSED — silently.
//
// If you are here because this failed on a new channel: add
// `MemoryWriteConfirmations: c.chatMemoryConfirmations()` (or the ui option
// equivalent) to the literal.
func TestEveryChannelReceiverWiresMemoryWriteConfirmations(t *testing.T) {
	offenders := receiverLiteralsMissingField(t, "MemoryWriteConfirmations:")
	if len(offenders) > 0 {
		t.Errorf(
			"these dispatcher.ChannelReceiver construction sites do not set "+
				"MemoryWriteConfirmations, so a shared-scope memory write on those "+
				"channels can never be acknowledged: %v", offenders)
	}
}

// fakeProjectLister returns a fixed project set for the gate test.
type fakeProjectLister struct{ projects []*registry.Project }

func (f fakeProjectLister) ListProjects() []*registry.Project { return f.projects }

// TestMemoryWriteGate_DenyByDefault — the gate answers false when nothing is
// wired or no project granted the channel, and true when a loaded project
// lists the channel in memory.write_channels (§5.1). A granted channel and an
// ungranted one differ ONLY in the gate verdict, not the refusal text (that
// identity is enforced in the dispatcher's TestRemember_* tests).
func TestMemoryWriteGate_DenyByDefault(t *testing.T) {
	// Nil registry / nil gate: deny.
	var g *memoryWriteGate
	if g.CanWriteMemory(context.Background(), "slack", "sess") {
		t.Error("nil gate must deny")
	}
	if newMemoryWriteGate(nil).CanWriteMemory(context.Background(), "slack", "sess") {
		t.Error("nil lister must deny")
	}

	lister := fakeProjectLister{projects: []*registry.Project{
		{ID: "p1", Memory: registry.ProjectMemory{WriteChannels: []string{"slack"}}},
		{ID: "p2"}, // no grant
		nil,        // a nil project must not panic
	}}
	gate := newMemoryWriteGate(lister)

	if !gate.CanWriteMemory(context.Background(), "slack", "sess") {
		t.Error("a granted channel must be allowed")
	}
	if gate.CanWriteMemory(context.Background(), "telegram", "sess") {
		t.Error("an ungranted channel must be denied")
	}
	if gate.CanWriteMemory(context.Background(), "", "sess") {
		t.Error("an empty channel must be denied")
	}
}
