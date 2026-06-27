package ui

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"reflect"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/idfmt"
)

// hasAdminFlag returns the truthy value of an `IsAdmin` field on
// the supplied data, or false when the field is missing. Used by
// the shared nav partial to decide whether to render the "Admin"
// link without forcing every existing data struct to grow an
// IsAdmin field — admin handlers set it explicitly, everything
// else falls through to false.
//
// Lookup is reflection-based so it works whether data is a struct,
// a struct pointer, or a map[string]any. Anything that doesn't
// expose IsAdmin returns false.
func hasAdminFlag(data any) bool {
	if data == nil {
		return false
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		f := v.FieldByName("IsAdmin")
		if !f.IsValid() {
			return false
		}
		if f.Kind() == reflect.Bool {
			return f.Bool()
		}
		return false
	case reflect.Map:
		// map[string]any path — accept any truthy bool.
		key := reflect.ValueOf("IsAdmin")
		mv := v.MapIndex(key)
		if !mv.IsValid() {
			return false
		}
		if mv.Kind() == reflect.Interface {
			mv = mv.Elem()
		}
		if mv.Kind() == reflect.Bool {
			return mv.Bool()
		}
		return false
	}
	return false
}

// hasSessionFlag returns the truthy value of an `IsSession` field on
// the supplied data, or false when missing. Used by the shared nav
// partial to decide whether to render the logout button — only
// browser-session logins can log out (api-key callers have no
// session to revoke). Same defensive reflection contract as
// hasAdminFlag so page data structs opt in by adding the field
// rather than every struct being forced to grow it.
func hasSessionFlag(data any) bool {
	return boolFieldOrFalse(data, "IsSession")
}

// boolFieldOrFalse reads a named bool field/key off data via
// reflection, returning false when absent or non-bool. Shared by the
// nav-flag helpers so their lookup logic can't drift apart.
func boolFieldOrFalse(data any, field string) bool {
	if data == nil {
		return false
	}
	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		f := v.FieldByName(field)
		if f.IsValid() && f.Kind() == reflect.Bool {
			return f.Bool()
		}
		return false
	case reflect.Map:
		mv := v.MapIndex(reflect.ValueOf(field))
		if !mv.IsValid() {
			return false
		}
		if mv.Kind() == reflect.Interface {
			mv = mv.Elem()
		}
		if mv.Kind() == reflect.Bool {
			return mv.Bool()
		}
		return false
	}
	return false
}

// indentStyle returns an inline style declaring the row indent in
// rem. Depth 0 → empty string (no style). Used by the task list to
// nest subtasks under their on-page parent. Each level is 1rem so
// the indent tracks the base font size; Tailwind classes would not
// purge-survive arbitrary depth values, so inline style is the
// portable choice.
func indentStyle(depth int) template.CSS {
	if depth <= 0 {
		return ""
	}
	if depth > 10 {
		depth = 10
	}
	return template.CSS(fmt.Sprintf("padding-left: %drem;", depth))
}

// hierarchyMeta is the template-side accessor for TaskHierarchyMeta
// that handles the "missing key" case (rendered in flat mode, or
// from a partial that wasn't decorated). Returns zero meta on miss.
// Defined here rather than as a closure in server.go so the helper
// is unit-testable.
func hierarchyMeta(m map[string]TaskHierarchyMeta, id string) TaskHierarchyMeta {
	if m == nil {
		return TaskHierarchyMeta{}
	}
	return m[id]
}

// Phase 33 of the phone-first UI redesign — small helpers shared
// across templates. Lives in its own file so they're easy to find
// and unit-test.

// shortID is the template-FuncMap entry point. Implementation lives
// in internal/idfmt so non-UI surfaces (Telegram bot, dispatcher
// tool results, CLI output) share the same compaction rules
// without duplicating the prefix table.
func shortID(id string) string {
	return idfmt.Short(id)
}

// taskSummary extracts a one-line excerpt from a task's payload
// for the task list "Summary" column. Task payloads are JSON-
// shaped — typically `{"context": {"prompt": "..."}, "args":
// {"prompt": "...", "type": "..."}}` — and the actual ask sits
// inside one of two well-known shapes.
//
// Lookup order (first hit wins, mirrors the roadmap-scoped
// priority list):
//   - args.prompt (the autonomy / chat-driven task shape)
//   - context.prompt (the v1 task shape)
//   - context.brief (older API-driven shape)
//   - args.body (email-ingested task shape — first line of body)
//
// Returns an empty string when no match — the template falls
// back to project + attempt counter as before. Capped at 140
// chars with an ellipsis for overflow so the table stays
// scannable.
//
// SAFETY: the result is plain text — callers must apply
// html-escape if rendering into HTML attributes / content.
func taskSummary(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return ""
	}
	for _, path := range [][]string{
		{"args", "prompt"},
		{"context", "prompt"},
		{"context", "brief"},
		{"args", "body"},
	} {
		if s := digString(doc, path...); s != "" {
			return summariseLine(s, 140)
		}
	}
	return ""
}

// digString walks a nested JSON object via the given path and
// returns the leaf string, or empty when any segment is absent
// or non-string.
func digString(node any, path ...string) string {
	for _, key := range path {
		obj, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		node, ok = obj[key]
		if !ok {
			return ""
		}
	}
	s, _ := node.(string)
	return s
}

// summariseLine takes a multi-line input and returns the first
// non-blank line, capped at maxLen characters with an ellipsis.
// Strips a leading slug-prefix ("project-name: ") so the actual
// ask shows in the limited horizontal real estate.
func summariseLine(s string, maxLen int) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Strip "<slug>: " prefix — autonomy feeds often namespace
		// their prompts and the slug eats the visible width.
		if i := strings.Index(trimmed, ": "); i > 0 && i < 40 {
			// Only strip when the prefix looks like a slug (no spaces).
			if !strings.ContainsAny(trimmed[:i], " \t") {
				trimmed = strings.TrimSpace(trimmed[i+2:])
			}
		}
		if len(trimmed) > maxLen {
			return trimmed[:maxLen-1] + "…"
		}
		return trimmed
	}
	return ""
}

// statusPill renders a Tailwind-styled span for a task / execution
// status. Centralised here so the colour mapping is consistent
// across every list view + detail header. The full status string
// is rendered as the label (no abbreviation) — Phase 41 a11y guarantee.
//
// Status values are matched against the conversational task lifecycle
// taxonomy (Phase 23): PENDING, QUEUED, LEASED, RUNNING,
// WAITING_FOR_CHILDREN, COMPLETED, FAILED, CANCELLED, AWAITING_INPUT,
// AWAITING_EXTERNAL, PAUSED, CLOSED. Unknown statuses fall back to
// neutral gray.
func statusPill(status string) template.HTML {
	classes := statusPillClasses(status)
	return template.HTML(fmt.Sprintf(
		`<span class="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium whitespace-nowrap %s">%s</span>`,
		classes, html.EscapeString(status)))
}

// statusPillClasses returns just the Tailwind class string. Useful
// when callers want to compose their own pill markup (e.g. with an
// icon or extra padding).
func statusPillClasses(status string) string {
	// Status pill contrast strategy is theme-specific:
	//
	//   light theme — SOLID -100 backgrounds + -800/-900 text. The
	//     translucent /15-/25 washes that looked fine on a dark
	//     theme read as ghost pills on white cards because mixing
	//     a saturated colour with white at 15% only shifts the
	//     surface by a few percent. Solid -100 hues sit clearly
	//     above the card and -800/-900 text passes WCAG AA with
	//     plenty of headroom.
	//
	//   dark theme — translucent /25 washes + -200/-300 text. On
	//     a charcoal card a solid -100 would be too bright; the
	//     wash lets the surface charcoal bleed through and the
	//     pill reads as a tinted region rather than a sticker.
	//
	// Subtlety for the gray pills: the daemon's Tailwind config
	// INVERTS the gray palette (gray-100 = darkest text in either
	// theme via CSS variables) so the standard "bg-gray-100 =
	// near-white" assumption breaks. We use bg-gray-700 / -800 to
	// get a pale gray BACKGROUND in light theme (because under
	// inversion those shade numbers now resolve to the lighter end),
	// and switch to a gray wash for dark.
	switch status {
	case "QUEUED":
		return "bg-gray-700 dark:bg-gray-500/20 text-gray-100 dark:text-gray-200"
	case "PENDING":
		return "bg-gray-800 dark:bg-gray-300/30 text-gray-200 dark:text-gray-300"
	case "LEASED":
		return "bg-brand-100 dark:bg-brand-500/25 text-brand-800 dark:text-brand-200"
	case "RUNNING":
		return "bg-brand-200 dark:bg-brand-500/35 text-brand-900 dark:text-brand-100"
	case "WAITING_FOR_CHILDREN":
		return "bg-blue-100 dark:bg-blue-500/25 text-blue-800 dark:text-blue-200"
	case "AWAITING_INPUT":
		return "bg-amber-100 dark:bg-amber-500/30 text-amber-900 dark:text-amber-200"
	case "AWAITING_EXTERNAL":
		return "bg-accent-100 dark:bg-accent-500/25 text-accent-800 dark:text-accent-200"
	case "PAUSED":
		return "bg-purple-100 dark:bg-purple-500/25 text-purple-800 dark:text-purple-200"
	case "COMPLETED":
		return "bg-green-100 dark:bg-green-500/25 text-green-800 dark:text-green-200"
	case "CLOSED":
		return "bg-green-200 dark:bg-green-500/30 text-green-900 dark:text-green-100"
	case "FAILED":
		return "bg-red-100 dark:bg-red-500/25 text-red-800 dark:text-red-200"
	case "CANCELLED":
		return "bg-gray-700 dark:bg-gray-500/20 text-gray-300 dark:text-gray-400"
	default:
		return "bg-gray-700 dark:bg-gray-500/25 text-gray-200 dark:text-gray-400"
	}
}

// renderInlineMarkdown converts a small subset of Markdown to safe
// HTML for inline rendering of operator + lead messages in the
// conversation thread. Supported:
//   - **bold**       → <strong>
//   - *italic*       → <em>
//   - `code`         → <code>
//   - ```fenced```   → <pre><code>
//   - [text](url)    → <a href="url"> (http(s) only)
//   - newlines       → <br>
//
// Everything is HTML-escaped first, then markers are post-processed
// with regex on the escaped string. URLs are length-capped + scheme-
// validated. The output is template.HTML so the template doesn't
// re-escape it.
//
// Deliberately NOT supported: headings, lists, tables, images,
// blockquote. Operators chatting about a task don't need them, and
// every additional shape grows the surface area for parser bugs.
func renderInlineMarkdown(text string) template.HTML {
	if text == "" {
		return ""
	}
	// 1. Pull out fenced code blocks so they're rendered as-is and
	//    inline patterns don't fire inside them.
	const placeholder = "\x00CODEBLOCK"
	var blocks []string
	idx := 0
	out := reFencedCode.ReplaceAllStringFunc(text, func(match string) string {
		// Strip the leading ``` and trailing ``` (with optional language).
		body := match
		body = strings.TrimPrefix(body, "```")
		body = strings.TrimSuffix(body, "```")
		// Drop optional language hint on the first line.
		if i := strings.IndexByte(body, '\n'); i >= 0 && !strings.ContainsAny(body[:i], " ") {
			body = body[i+1:]
		}
		blocks = append(blocks, body)
		marker := fmt.Sprintf("%s%d\x00", placeholder, idx)
		idx++
		return marker
	})

	// 2. HTML-escape the surviving prose (now devoid of code blocks).
	out = html.EscapeString(out)

	// 3. Run inline markers on the escaped string. Order matters:
	//    bold (**) before italic (*) so we don't match the bold
	//    asterisks twice.
	out = reMdBold.ReplaceAllString(out, `<strong>$1</strong>`)
	out = reMdItalic.ReplaceAllString(out, `<em>$1</em>`)
	out = reMdInlineCode.ReplaceAllString(out, `<code class="px-1 py-0.5 rounded bg-dark-900 text-amber-300 font-mono text-[0.85em]">$1</code>`)
	// Links — only http(s); cap label + URL length.
	//
	// Safety invariant: `out` was HTML-escaped at line 219 BEFORE
	// this regex runs, so `url` and `label` extracted here cannot
	// contain raw `"`, `<`, `>`, `&`, or `'` — any such character
	// in the user's input is already `&quot;` / `&lt;` / etc. by
	// the time the regex matches. That makes interpolation into
	// the href attribute safe.
	//
	// Defense-in-depth: also reject URLs containing whitespace
	// or control characters. Spaces in an href don't break out
	// of the attribute (they're just URL characters at HTML
	// parse time) but they signal a malformed link and may
	// confuse downstream URL parsers. Stricter is safer.
	out = reMdLink.ReplaceAllStringFunc(out, func(m string) string {
		sub := reMdLink.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		label, url := sub[1], sub[2]
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return m
		}
		if len(label) > 200 || len(url) > 500 {
			return m
		}
		// Reject any whitespace or control byte in the URL —
		// well-formed http(s) URLs never contain them
		// unencoded. Catches both literal spaces and any C0
		// control bytes that slipped through HTML escaping.
		for i := 0; i < len(url); i++ {
			c := url[i]
			if c <= 0x20 || c == 0x7f {
				return m
			}
		}
		return fmt.Sprintf(`<a href="%s" class="text-brand-700 hover:text-brand-500 dark:text-brand-300 dark:hover:text-brand-200 underline" rel="nofollow noopener" target="_blank">%s</a>`, url, label)
	})
	// Newlines → <br>. Done last so it doesn't break inline tokens.
	out = strings.ReplaceAll(out, "\n", "<br>")

	// 4. Re-inject code blocks (HTML-escaped inside <pre><code>).
	for i, body := range blocks {
		marker := fmt.Sprintf("%s%d\x00", placeholder, i)
		rendered := fmt.Sprintf(
			`<pre class="my-1 px-2 py-1.5 rounded bg-dark-700 border border-dark-600 overflow-x-auto"><code class="font-mono text-[0.85em] text-amber-800 dark:text-amber-200 whitespace-pre">%s</code></pre>`,
			html.EscapeString(body))
		out = strings.Replace(out, marker, rendered, 1)
	}
	return template.HTML(out)
}

// regex bank for renderInlineMarkdown. Compiled once at package load.
var (
	reFencedCode   = regexp.MustCompile("(?s)```[^\n]*\n.*?```|```[^`]*```")
	reMdBold       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reMdItalic     = regexp.MustCompile(`\*([^*]+)\*`)
	reMdInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reMdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

// statusDot returns a coloured dot character (•) wrapped in a span
// — handy for compact "● running" / "✓ done" leading icons in card
// headers without importing an icon library.
func statusDot(status string) template.HTML {
	// Status-dot colour is theme-aware: -600 shade on light theme
	// (visible against near-white card), -400 shade on dark theme
	// (visible against charcoal card). Tailwind dark: variants pick
	// the right one based on <html data-theme>.
	colour := "text-gray-500 dark:text-gray-400"
	switch status {
	case "RUNNING", "LEASED":
		colour = "text-brand-600 dark:text-brand-400"
	case "AWAITING_INPUT":
		colour = "text-amber-600 dark:text-amber-400"
	case "AWAITING_EXTERNAL":
		colour = "text-accent-600 dark:text-accent-400"
	case "PAUSED":
		colour = "text-purple-600 dark:text-purple-400"
	case "COMPLETED", "CLOSED":
		colour = "text-green-600 dark:text-green-400"
	case "FAILED":
		colour = "text-red-600 dark:text-red-400"
	}
	return template.HTML(fmt.Sprintf(`<span class="%s" aria-hidden="true">●</span>`, colour))
}

// refOpen reports whether a panelRef should default to open for the
// given status. openWhen is the set of statuses that force the panel
// open; an empty set means "closed by default". status is accepted as
// any so typed status values (persistence.TaskStatus / ExecutionStatus)
// work from templates without an explicit cast. %v stringifies typed
// strings identically to the template's `printf` idiom while rendering
// a nil/non-string value safely (no `%!s(<nil>)` artifact).
func refOpen(status any, openWhen ...string) bool {
	s := fmt.Sprintf("%v", status)
	for _, w := range openWhen {
		if w == s {
			return true
		}
	}
	return false
}
