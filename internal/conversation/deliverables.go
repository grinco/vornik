package conversation

import (
	"fmt"
	"net/url"
	"strings"
)

// DeliverableLink is one operator-visible artifact line surfaced
// in chat replies. Each row carries a display name + an optional
// absolute download URL. Channels render the URL when set,
// otherwise they fall back to a plain-name "produced files: ..."
// summary so operators with shell access can find the file
// without a working artifact UI.
//
// Motivating incident (2026-05-17 CV demo): the writer role
// produced a 132-line deliverable.md but the dispatcher's
// follow-up chat reply only surfaced a 2-sentence summary,
// leaving the operator without a way to read the actual file
// short of `docker exec` into the agent container.
type DeliverableLink struct {
	// Name is the operator-visible filename (e.g. "deliverable.md").
	Name string

	// URL is the absolute https URL the channel renders for the
	// "Download:" link. Empty when no artifact UI is configured
	// for this deployment — channels then emit the name without
	// a clickable link.
	URL string
}

// RenderDeliverableLinks builds the "Download: <name>" block
// channels (Telegram, web chat, future Slack) append to the
// task-complete reply when the task produced operator-facing
// files. Returns "" when links is empty so callers can
// unconditionally append the result.
//
// Format chosen for readability across the three known
// renderers:
//
//	Telegram: plain text (no HTML / MarkdownV2) — clickable
//	    URLs auto-link in the Telegram client.
//	Web chat: same plain text — the UI's renderMarkdown helper
//	    auto-links bare URLs.
//	Future Slack: same shape — Slack auto-links bare URLs in
//	    plain messages too.
//
// Each non-empty URL renders as "Download: <name> — <URL>".
// Empty URLs degrade to "Download: <name>" with an explanatory
// trailer line.
func RenderDeliverableLinks(links []DeliverableLink) string {
	if len(links) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nProduced files:")
	anyURL := false
	for _, l := range links {
		name := strings.TrimSpace(l.Name)
		if name == "" {
			continue
		}
		if l.URL != "" {
			anyURL = true
			fmt.Fprintf(&sb, "\nDownload: %s — %s", name, l.URL)
		} else {
			fmt.Fprintf(&sb, "\nDownload: %s", name)
		}
	}
	if !anyURL {
		sb.WriteString("\n(no artifact UI configured — operator must have shell access to read these)")
	}
	return sb.String()
}

// BuildDeliverableLinks turns a raw (projectID, baseURL,
// filename) tuple into the link slice channels render. Used by
// the dispatcher's notify-followup path where the artifact set
// has been resolved to a list of names.
//
// projectID is included in the URL so the artifact UI can scope
// access checks per project; baseURL is the daemon's externally-
// reachable web UI prefix (e.g. "https://vornik.example.com").
// An empty baseURL emits links with URL="" — channels render the
// "shell access" fallback line.
//
// The path shape ("/ui/projects/<id>/artifacts/raw?path=<name>")
// matches the artifact-raw endpoint the UI exposes; the helper
// URL-encodes the filename so paths containing slashes (e.g.
// "out/deliverable.md") round-trip cleanly.
func BuildDeliverableLinks(baseURL, projectID string, names []string) []DeliverableLink {
	out := make([]DeliverableLink, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		link := DeliverableLink{Name: name}
		if baseURL != "" && projectID != "" {
			u := strings.TrimRight(baseURL, "/") +
				"/ui/projects/" + url.PathEscape(projectID) +
				"/artifacts/raw?path=" + url.QueryEscape(name)
			link.URL = u
		}
		out = append(out, link)
	}
	return out
}
