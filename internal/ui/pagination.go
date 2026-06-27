package ui

import (
	"net/url"
	"strconv"
)

// Reusable list pagination. The first adopter is /ui/projects/<id>/keys
// (whose one-time task keys made the list endless), but Paginator and
// its {{template "pagination"}} partial are deliberately list-agnostic
// so Executions / Tasks / any other long list can adopt them without
// reinventing the controls. The data layer still hands back a full
// (filtered) slice today; pageWindow slices it in memory. Swapping in a
// repo-level LIMIT/OFFSET later is a data-source change only — the
// Paginator shape and the partial stay identical.

const defaultPerPage = 25

// Paginator is the template-facing pagination model. It is view-only:
// the caller has already sliced the data to the current page with
// pageWindow; Paginator renders the "Showing X–Y of Z" summary and the
// prev/next controls, preserving every other query param (status
// filters, etc.) on the navigation links.
type Paginator struct {
	Page    int // 1-based, clamped into [1, Pages]
	PerPage int
	Total   int // total items across all pages (post-filter)
	Pages   int
	Start   int // 1-based index of the first item shown (0 when empty)
	End     int // 1-based index of the last item shown
	HasPrev bool
	HasNext bool
	PrevURL string
	NextURL string
}

// NewPaginator clamps page into [1, Pages], computes the visible
// window, and builds prev/next URLs from basePath + the supplied query
// values. The "page" key is overwritten; every other key is preserved
// so an active/all filter survives navigation. perPage <= 0 falls back
// to defaultPerPage. Pass the SAME page value to pageWindow so the
// slice and the controls agree.
func NewPaginator(total, page, perPage int, basePath string, query url.Values) Paginator {
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}

	p := Paginator{Page: page, PerPage: perPage, Total: total, Pages: pages}
	if total > 0 {
		p.Start = (page-1)*perPage + 1
		p.End = page * perPage
		if p.End > total {
			p.End = total
		}
	}
	p.HasPrev = page > 1
	p.HasNext = page < pages

	mk := func(n int) string {
		q := url.Values{}
		for k, v := range query {
			if k == "page" {
				continue
			}
			q[k] = append([]string(nil), v...)
		}
		q.Set("page", strconv.Itoa(n))
		return basePath + "?" + q.Encode()
	}
	if p.HasPrev {
		p.PrevURL = mk(page - 1)
	}
	if p.HasNext {
		p.NextURL = mk(page + 1)
	}
	return p
}

// pageWindow slices items to the 1-based page. An out-of-range page
// clamps to the last page's window (matching NewPaginator's clamp) so
// a stale ?page= never renders an empty page mid-list. perPage <= 0
// falls back to defaultPerPage.
func pageWindow[T any](items []T, page, perPage int) []T {
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if len(items) == 0 {
		return items
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * perPage
	if start >= len(items) {
		lastPage := (len(items) + perPage - 1) / perPage
		start = (lastPage - 1) * perPage
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// parsePageParam reads the 1-based ?page= query value, defaulting to 1
// for absent / malformed / non-positive input.
func parsePageParam(values url.Values) int {
	if n, err := strconv.Atoi(values.Get("page")); err == nil && n > 0 {
		return n
	}
	return 1
}
