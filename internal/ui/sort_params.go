// Helpers for parsing ?sort=KEY&dir=DIR query params on list/detail pages
// that render sortable tables.

package ui

import (
	"net/http"
	"slices"
	"strings"
)

// sortParams parses sort/dir from the request, validating the key against
// allowed and falling back to defaultKey/defaultDir when invalid or
// missing. The returned dir is always "asc" or "desc".
func sortParams(r *http.Request, allowed []string, defaultKey, defaultDir string) (string, string) {
	q := r.URL.Query()
	key := q.Get("sort")
	ok := false
	for _, a := range allowed {
		if a == key {
			ok = true
			break
		}
	}
	if !ok {
		key = defaultKey
	}
	dir := q.Get("dir")
	if dir != "asc" && dir != "desc" {
		dir = defaultDir
	}
	return key, dir
}

// uiPathPrefix is the mount point of the UI subapp on the daemon's
// HTTP server (see service.uiSubtreeHandler). The middleware strips
// this prefix from r.URL.Path before handlers run so the mux can
// register routes like "/projects" without repeating "/ui/" on every
// pattern. URL-emitting code has to reattach the prefix or the
// browser will hit /projects/... directly and get a 404 from the
// API mux. Every template in internal/ui/templates/ hardcodes
// "/ui/..." for the same reason.
const uiPathPrefix = "/ui"

// sortBaseURL returns the request URL with sort/dir stripped, so the
// sortHeader template partial can reattach them. Preserves all other
// query params (status, project_id, limit, q, …) so clicking a column
// header doesn't drop the user's filters. The /ui prefix is reattached
// because the subtree middleware stripped it before this handler ran —
// without it the emitted href would 404.
func sortBaseURL(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del("sort")
	q.Del("dir")
	u.RawQuery = q.Encode()
	if u.Path == "" {
		u.Path = r.URL.Path
	}
	path := u.Path
	if !strings.HasPrefix(path, uiPathPrefix+"/") && path != uiPathPrefix {
		path = uiPathPrefix + path
	}
	if encoded := u.RawQuery; encoded != "" {
		return path + "?" + encoded
	}
	return path
}

// sortBy sorts items in place by a column-keyed comparator map.
//
// columns maps a sort key (the value behind ?sort=) to a comparator that
// returns the standard cmp.Compare result for two items: negative if i<j,
// 0 if equal, positive if i>j. fallback is the key used when the requested
// key isn't in the map; "asc" or "desc" controls direction. The fallback
// always sorts descending — designed for "biggest first" defaults.
//
// One generic helper replaces the three earlier ad-hoc closures
// (sortRoleSpend / sortTaskCostRows / sortTasks). Each table now declares
// its columns as a small map[string]func(a, b T) int and lets sortBy do
// the rest.
func sortBy[T any](items []T, columns map[string]func(a, b T) int, key, dir, fallback string) {
	cmpFn, ok := columns[key]
	if !ok {
		cmpFn = columns[fallback]
		dir = "desc"
	}
	slices.SortFunc(items, func(a, b T) int {
		c := cmpFn(a, b)
		if dir == "desc" {
			c = -c
		}
		return c
	})
}
