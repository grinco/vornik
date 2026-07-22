package service

import (
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
)

// webWriteComponents lazily builds — and memoizes on the container — the two
// shared pieces of the supervised web-write feature (LLD
// 2026-07-21-supervised-web-write-actions, Components 3/5):
//
//   - the pending-write store (persistence.WebWriteRepo over the daemon's raw
//     *sql.DB pool): web_submit(preview) inserts a pending row; the /inbox
//     approve surface lists + approves it; web_submit(submit) reads it back.
//   - the token-delivery store (dispatcher.WebWriteTokenStore): the operator-
//     chat-driven v1 channel by which the authenticated inbox approve hands the
//     freshly minted approval token to the submit path daemon-side (the assistant
//     never holds the token).
//
// Both must be the SAME instances across the dispatcher Agent and the UI server,
// so they are constructed exactly once here and reused by initDispatcher and
// initHTTPServer regardless of call order.
//
// Gate (mirrors how the scraper MCP is conditionally wired today, e.g.
// container_subsystems.go's block-notify hook): the scraper is reached over the
// MCP manager, so without c.mcpManager there is no scraper to write to; without
// c.DB there is no pending-write store. In either case the seams stay nil and
// web_submit degrades to its "not configured" HARD gate. The daemon-level
// web.writes tri-state toggle is enforced inside the tool itself — it is NOT a
// wiring gate here (an operator flipping web.writes on must not require a daemon
// rebuild), matching WithWebWritesConfig always being passed.
func (c *Container) webWriteComponents() (persistence.WebWriteRepo, *dispatcher.WebWriteTokenStore) {
	if c.webWriteRepo != nil && c.webWriteTokenStore != nil {
		return c.webWriteRepo, c.webWriteTokenStore
	}
	if c.mcpManager == nil || c.DB == nil {
		return nil, nil
	}
	c.webWriteRepo = persistence.NewWebWriteRepo(c.DB)
	c.webWriteTokenStore = dispatcher.NewWebWriteTokenStore()
	return c.webWriteRepo, c.webWriteTokenStore
}
