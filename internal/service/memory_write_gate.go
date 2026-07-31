package service

import (
	"context"

	"vornik.io/vornik/internal/registry"
)

// memoryWriteGate answers the chat memory-write capability question from
// project config (chat memory-write design §5.1). It satisfies
// dispatcher.MemoryWriteGate.
//
// A channel is granted iff SOME loaded project lists it in
// memory.write_channels. Deny-by-default: no project, or no matching grant,
// → false. This is what makes a dispatcher-level durable write defensible —
// the set of writing channels is an auditable config fact, not an emergent
// property of who talked to the bot.
//
// The grant is CHANNEL-level, matching the design's containment (which is
// authorization, not project routing): the gate answers only "may this
// channel write durable memory at all", while the WRITE itself lands in the
// turn's active project. In the single-customer topology one project owns
// each channel, so channel-name scoping is exact; a future multi-project
// deployment that shares a channel-name across projects would grant the
// capability if ANY of them opted in, which is the documented caveat.
// projectLister is the narrow slice of *registry.Registry the gate needs —
// the live project set, read per call so a config reload takes effect without
// rebuilding the gate. *registry.Registry satisfies it.
type projectLister interface {
	ListProjects() []*registry.Project
}

type memoryWriteGate struct {
	projects projectLister
}

func newMemoryWriteGate(p projectLister) *memoryWriteGate {
	return &memoryWriteGate{projects: p}
}

// CanWriteMemory reports whether the named channel may write durable
// project memory. sessionID is accepted for interface conformance but not
// consulted — the grant is channel-level (§5.1). The caller
// (ToolExecutor.remember) already refuses an empty channel/session.
func (g *memoryWriteGate) CanWriteMemory(_ context.Context, channel, _ string) bool {
	if g == nil || g.projects == nil || channel == "" {
		return false
	}
	for _, p := range g.projects.ListProjects() {
		if p != nil && p.Memory.GrantsMemoryWrite(channel) {
			return true
		}
	}
	return false
}
