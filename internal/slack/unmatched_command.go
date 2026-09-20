package slack

import (
	"sort"
	"sync"
	"time"
)

// unmatchedCommandsCap bounds the distinct slash-command literals one channel
// will track. The literal comes off a request body, so the map is keyed on
// attacker-influenceable content — behind a valid signature, which makes this
// hygiene rather than defence, but an unbounded map keyed on request content is
// not a shape this repository ships. Eight is well above the real ceiling: a
// deployment runs one Slack app per project, and the reference host's busiest
// workspace has two.
const unmatchedCommandsCap = 8

// UnmatchedSlashCommand is one literal the daemon was sent and does not answer,
// with the window over which it was seen. The window is what distinguishes a
// single fat-fingered command from an app that has been misconfigured since it
// was installed.
type UnmatchedSlashCommand struct {
	Command   string    `json:"command"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// unmatchedCommands tallies slash commands this channel received and refused.
//
// WHY THIS EXISTS. On 2026-09-15 a Slack app publishing /t800 spoke to a
// project configured with the default /vornik. Every delivery passed signature
// verification, was rejected at the command comparison, and was answered 200 —
// and the HTTP access logger records only non-2xx, so the journal held nothing
// at all. "Zero /api/v1/slack/webhook entries" read as "Slack never reached
// this daemon" when it meant the exact opposite. Five link codes expired unused
// over forty minutes.
//
// The daemon held BOTH halves of the answer at the moment of failure — the
// command it received and the command it answers — and discarded them. This
// keeps them, in memory, so `doctor` can put them side by side.
//
// In memory, deliberately: the question ("is a Slack app talking past this
// daemon right now?") is about the running configuration, and a restart that
// clears the tally is a restart after which the operator wants a fresh answer
// anyway. A durable row would invite reading a fixed mismatch as a live one.
type unmatchedCommands struct {
	mu       sync.Mutex
	cap      int
	seen     map[string]*UnmatchedSlashCommand
	overflow int
}

func newUnmatchedCommands(capacity int) *unmatchedCommands {
	if capacity <= 0 {
		capacity = unmatchedCommandsCap
	}
	return &unmatchedCommands{cap: capacity, seen: map[string]*UnmatchedSlashCommand{}}
}

// record tallies one refused literal. A literal already tracked keeps counting
// past the cap — otherwise a burst of junk freezes the count for the one
// command the operator actually needs to see, which would make the cap a way to
// hide the signal rather than to bound the map.
func (u *unmatchedCommands) record(command string, at time.Time) {
	if u == nil || command == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	if e, ok := u.seen[command]; ok {
		e.Count++
		e.LastSeen = at
		return
	}
	if len(u.seen) >= u.cap {
		u.overflow++
		return
	}
	u.seen[command] = &UnmatchedSlashCommand{
		Command:   command,
		Count:     1,
		FirstSeen: at,
		LastSeen:  at,
	}
}

// snapshot returns the tally, busiest first, so an operator reads the command
// that is actually failing rather than the alphabetically luckiest one.
func (u *unmatchedCommands) snapshot() []UnmatchedSlashCommand {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	out := make([]UnmatchedSlashCommand, 0, len(u.seen))
	for _, e := range u.seen {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Command < out[j].Command
	})
	return out
}

// overflowed reports how many distinct literals were dropped at the cap. It is
// published rather than swallowed: a tally that silently stops being complete
// is the control-that-cannot-distinguish problem in miniature.
func (u *unmatchedCommands) overflowed() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.overflow
}

// noteUnmatchedSlashCommand records a slash command this channel refused.
// Safe on a channel with no recorder.
func (c *Channel) noteUnmatchedSlashCommand(command string) {
	if c == nil || c.unmatchedCommands == nil {
		return
	}
	now := time.Now()
	if c.clock != nil {
		now = c.clock()
	}
	c.unmatchedCommands.record(command, now)
}

// UnmatchedSlashCommands reports the slash commands this channel was sent and
// does not answer. Read by the `slack_slash_command` doctor check.
func (c *Channel) UnmatchedSlashCommands() []UnmatchedSlashCommand {
	if c == nil {
		return nil
	}
	return c.unmatchedCommands.snapshot()
}

// UnmatchedSlashCommandsDropped reports distinct literals the cap discarded.
func (c *Channel) UnmatchedSlashCommandsDropped() int {
	if c == nil {
		return 0
	}
	return c.unmatchedCommands.overflowed()
}

// ConfiguredSlashCommand is the command this channel DOES answer, normalised.
// The doctor check prints it beside what arrived; the two together are the
// whole diagnosis.
func (c *Channel) ConfiguredSlashCommand() string {
	if c == nil {
		return ""
	}
	return NormaliseSlashCommand(c.cfg.SlashCommand)
}

// ProjectIDs lists the projects this channel serves, in installation order.
// The doctor check needs them because slash_command is a per-project key: a
// finding that cannot name the project is not an instruction.
func (c *Channel) ProjectIDs() []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(c.installations))
	for _, inst := range c.installations {
		id := inst.projectID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
