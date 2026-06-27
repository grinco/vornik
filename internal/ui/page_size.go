// Helpers for the ?limit=N page-size selector that appears on every
// list view in the UI. The audit page introduced the pattern
// (limit param, options 10/20/50/100, default 20); this file
// generalises it so all list pages share the same validator + the
// same shared template partial in _partials.html ("pageSizeSelector").
//
// Why a dedicated file: the validator's tight allowlist is load-
// bearing for safety — the parsed value flows directly into a
// LIMIT clause on the storage query, so a hostile ?limit=9999999
// must NOT punch through. Keeping the allowlist in one place
// means a future change (say, adding 200) lands consistently
// across every page.

package ui

import "strconv"

// DefaultPageSize is the per-page row cap when no ?limit= is
// supplied. Matches /ui/audit's default since 2026-04-30 — the
// canonical implementation that this selector pattern was lifted
// from. Bumping this would shift the default everywhere; verify
// it's still the right balance between "scannable" and "useful
// at a glance" before changing.
const DefaultPageSize = 20

// PageSizeOptions is the dropdown's option set. Order is the
// render order in the <select>; the validator accepts any value
// in this slice and rejects everything else (including the empty
// string and out-of-set integers like 75).
//
// Same set the audit page has shipped with — 10 for a fast scan,
// 100 for a deep dive, 20/50 in between. Wider ranges (200, 500)
// were intentionally rejected: the templates render every row
// server-side and the network payload + initial paint cost beyond
// 100 rows isn't worth the rare "show me everything" use case
// when CSV export already covers that need.
var PageSizeOptions = []int{10, 20, 50, 100}

// parsePageSize reads the raw ?limit= query param and returns a
// validated row cap. Falls back to DefaultPageSize on:
//   - missing / empty value (the default render)
//   - non-integer ("abc", " 20 " — whitespace is rejected)
//   - integer outside PageSizeOptions ("75", "999999999")
//
// The strict allowlist (not a range) is deliberate: it prevents a
// crafted ?limit=99999999 from forcing a multi-million-row scan
// and locks the UI's row counts to a known menu so screenshots /
// docs stay accurate. Same defensive shape as audit.go's inline
// switch — extracted so every list view inherits the discipline.
func parsePageSize(raw string) int {
	if raw == "" {
		return DefaultPageSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultPageSize
	}
	for _, ok := range PageSizeOptions {
		if n == ok {
			return n
		}
	}
	return DefaultPageSize
}

// DefaultEpochsLimit is the default number of corpus-epoch snapshots
// surfaced in the rollback picker. It matches the API ceiling (500) so
// all epochs are visible by default — operators with large snapshot
// counts must be able to roll back past the first 100 (the old cap
// that came from parsePageSize / PageSizeOptions max = 100).
const DefaultEpochsLimit = 500

// MaxEpochsLimit is the hard ceiling for epochs_limit, kept in sync
// with the GET /api/v1/projects/{id}/memory/epochs?limit= API cap so
// the two surfaces honour the same invariant.
const MaxEpochsLimit = 500

// parseEpochsLimit reads the raw ?epochs_limit= query param and
// returns a value in [1, MaxEpochsLimit]. It uses a range check (not
// the PageSizeOptions allowlist) because epoch counts can reach into
// the hundreds and the rollback picker must surface all of them —
// capping at 100 (the PageSizeOptions max) is the bug this function
// fixes. Falls back to DefaultEpochsLimit on missing / invalid /
// out-of-range input.
func parseEpochsLimit(raw string) int {
	if raw == "" {
		return DefaultEpochsLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return DefaultEpochsLimit
	}
	if n > MaxEpochsLimit {
		return DefaultEpochsLimit
	}
	return n
}
