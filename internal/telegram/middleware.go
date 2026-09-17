package telegram

import (
	"context"
	"strconv"
	"time"

	"vornik.io/vornik/internal/chatauth"
)

// SetIdentityShim wires the Phase-4 compatibility shim
// (oidc-identity-permissions-design §5.1/§5.3). Nil — the default — leaves
// every authorization question answered by the legacy allowlist alone, which
// is the pre-Phase-4 behaviour exactly.
//
// Wired, a LINKED identity is authorized by the resolver and carries its own
// project scope; an unlinked one keeps working off the allowlist for the
// migration window, and lands on the worklist while it does.
func (b *Bot) SetIdentityShim(s *chatauth.Shim) { b.identityShim = s }

// legacyAccess reads what the hand-maintained list says about a user, in the
// shape the shim consumes. Kept separate from IsAllowed so the legacy rule —
// including "an empty list DENIES unless allow_unlisted_users" — stays in one
// place and is not re-derived per call site.
func (b *Bot) legacyAccess(userID int64) chatauth.Legacy {
	l := chatauth.Legacy{ConfigKey: "telegram.allowed_users"}
	if len(b.config.AllowedUsers) == 0 {
		l.Allowed = b.config.AllowUnlistedUsers
		return l
	}
	ua, ok := b.config.AllowedUsers[userID]
	if !ok || !ua.Allowed {
		return l
	}
	l.Allowed = true
	if !ua.Wildcard() {
		l.Projects = ua.Projects
	}
	return l
}

// authorize runs the OR-matrix for one Telegram user.
func (b *Bot) authorize(userID int64) chatauth.Decision {
	return b.authorizeCtx(context.Background(), userID)
}

// authorizeCtx is authorize with a caller-supplied deadline. See the Slack
// twin: the configuration assistant's chat entrypoint needs a HANGING resolver
// to produce the same refusal a failing one does, rather than blocking the
// handler indefinitely (review-20260915-8717 F3).
func (b *Bot) authorizeCtx(ctx context.Context, userID int64) chatauth.Decision {
	legacy := b.legacyAccess(userID)
	if b.identityShim == nil {
		return chatauth.Decision{Allowed: legacy.Allowed, Projects: legacy.Projects}
	}
	return b.identityShim.Authorize(ctx, "telegram", strconv.FormatInt(userID, 10), legacy)
}

// IsAllowed reports whether the user may interact with the bot at
// all. Project scoping (which projects they can /project into) is a
// separate question answered by UserCanAccessProject.
//
// An EMPTY allowlist DENIES (2026-08-05). It used to admit everyone, which
// made an unconfigured bot open to anyone who found it — a Telegram bot is
// publicly addressable by username, so "no allowlist" was not a private
// default. Set telegram.allow_unlisted_users: true to keep the old
// dev-mode behaviour deliberately. Explicit Allowed:false entries are
// treated the same as missing (unauthorized).
func (b *Bot) IsAllowed(userID int64) bool {
	return b.authorize(userID).Allowed
}

// UserCanAccessProject reports whether the given user may /project
// into projectID or see it in list_projects output. Unauthorized users
// return false regardless of projectID.
//
// When no allowlist is configured at all, this follows IsAllowed: denied
// unless allow_unlisted_users opts the deployment back into dev mode.
func (b *Bot) UserCanAccessProject(userID int64, projectID string) bool {
	d := b.authorize(userID)
	if !d.Allowed {
		return false
	}
	// Nil scope means unrestricted — a wildcard legacy entry, an admin
	// principal, or dev mode. Otherwise the scope is whichever authority
	// granted: the principal's when the resolver did, the allowlist's when it
	// was the legacy list.
	if d.Projects == nil {
		return true
	}
	for _, p := range d.Projects {
		if p == "*" || p == projectID {
			return true
		}
	}
	return false
}

// OperatorChatIDsForProject returns the Telegram chat IDs (a DM chat_id
// equals the user id) of every allowed user who may access projectID —
// wildcard users always, project-scoped users only when projectID is in
// their whitelist. Used to route ownerless-task steering alerts (autonomy /
// routed sub-tasks with no originating chat) to exactly the operators with
// access to that task's project. Returns nil when no allowlist is
// configured (dev mode: no scoped recipients to fan out to).
func (b *Bot) OperatorChatIDsForProject(projectID string) []int64 {
	if len(b.config.AllowedUsers) == 0 {
		return nil
	}
	var out []int64
	for id, ua := range b.config.AllowedUsers {
		if ua.Allowed && ua.CanAccessProject(projectID) {
			out = append(out, id)
		}
	}
	return out
}

// AllowedProjectsForUser returns the project-ID whitelist for a
// dispatcher.Request. Semantics match the API's projectIDKey context
// value: nil → no restriction (dev mode or fully-trusted user);
// non-nil slice → exact-match whitelist with "*" meaning wildcard.
//
// Returning nil when the user is wildcard (["*"]) or when no allowlist
// is configured lets downstream tool code skip the check entirely for
// the common case, rather than pay for an "is it in this list of 1"
// lookup on every tool call.
func (b *Bot) AllowedProjectsForUser(userID int64) []string {
	if b.identityShim != nil {
		d := b.authorize(userID)
		if !d.Allowed {
			return []string{}
		}
		return d.Projects
	}
	if len(b.config.AllowedUsers) == 0 {
		return nil
	}
	ua, ok := b.config.AllowedUsers[userID]
	if !ok || !ua.Allowed {
		// Denied users shouldn't reach dispatcher at all; IsAllowed
		// gates earlier. This is belt-and-braces: return an empty
		// non-nil slice so any tool call is rejected structurally.
		return []string{}
	}
	if ua.Wildcard() {
		return nil
	}
	// Copy to keep callers from mutating the bot's config.
	out := make([]string, len(ua.Projects))
	copy(out, ua.Projects)
	return out
}

// CheckRateLimit checks if user has exceeded rate limit.
// Returns true if the request should be allowed, false if rate limited.
func (b *Bot) CheckRateLimit(userID int64) bool {
	// If rate limiting is disabled (0 or negative), allow all
	if b.config.RateLimit <= 0 {
		return true
	}

	b.mu.Lock()
	entry, exists := b.rateLimits[userID]
	if !exists {
		entry = &rateLimitEntry{
			count:       0,
			windowStart: time.Now(),
		}
		b.rateLimits[userID] = entry
	}
	b.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	windowDuration := time.Minute

	// Reset window if more than a minute has passed
	if now.Sub(entry.windowStart) >= windowDuration {
		entry.count = 0
		entry.windowStart = now
	}

	// Check if under the limit
	if entry.count >= b.config.RateLimit {
		return false
	}

	entry.count++
	return true
}

// GetRateLimitStatus returns the current rate limit status for a user.
func (b *Bot) GetRateLimitStatus(userID int64) (count int, resetIn time.Duration, limited bool) {
	if b.config.RateLimit <= 0 {
		return 0, 0, false
	}

	b.mu.RLock()
	entry, exists := b.rateLimits[userID]
	b.mu.RUnlock()

	if !exists {
		return 0, time.Minute, false
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	windowDuration := time.Minute

	// Calculate time until window reset
	elapsed := now.Sub(entry.windowStart)
	if elapsed >= windowDuration {
		return 0, time.Minute, false
	}

	remaining := windowDuration - elapsed
	return entry.count, remaining, entry.count >= b.config.RateLimit
}

// ClearRateLimits clears all rate limit counters.
func (b *Bot) ClearRateLimits() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rateLimits = make(map[int64]*rateLimitEntry)
}

// AddAllowedUser adds a user to the allowlist with wildcard project
// access. For finer-grained control, edit the YAML config directly.
func (b *Bot) AddAllowedUser(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.config.AllowedUsers == nil {
		b.config.AllowedUsers = make(map[int64]UserAccess)
	}
	b.config.AllowedUsers[userID] = UserAccess{Allowed: true, Projects: []string{"*"}}
}

// RemoveAllowedUser removes a user from the allowlist.
func (b *Bot) RemoveAllowedUser(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.config.AllowedUsers, userID)
}

// GetAllowedUsers returns a copy of the allowed users list.
func (b *Bot) GetAllowedUsers() []int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	users := make([]int64, 0, len(b.config.AllowedUsers))
	for userID := range b.config.AllowedUsers {
		users = append(users, userID)
	}
	return users
}
