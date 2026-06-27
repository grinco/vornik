package telegram

import (
	"time"
)

// IsAllowed reports whether the user may interact with the bot at
// all. Project scoping (which projects they can /project into) is a
// separate question answered by UserCanAccessProject.
//
// If no allowlist is configured (empty map), any user is allowed —
// keeps the dev/single-user path frictionless. Explicit Allowed:false
// entries are treated the same as missing (unauthorized).
func (b *Bot) IsAllowed(userID int64) bool {
	if len(b.config.AllowedUsers) == 0 {
		return true
	}
	return b.config.AllowedUsers[userID].Allowed
}

// UserCanAccessProject reports whether the given user may /project
// into projectID or see it in list_projects output. Unauthorized users
// return false regardless of projectID.
//
// When no allowlist is configured at all (dev mode), every user sees
// every project — same "no restrictions" semantics as IsAllowed.
func (b *Bot) UserCanAccessProject(userID int64, projectID string) bool {
	if len(b.config.AllowedUsers) == 0 {
		return true
	}
	ua, ok := b.config.AllowedUsers[userID]
	if !ok {
		return false
	}
	return ua.CanAccessProject(projectID)
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
