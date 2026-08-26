// Package backlogfile owns every read-modify-write of a project's
// BACKLOG.md. It is the single place that understands the marker
// grammar ([ ] pending, [?] proposed, [x] done, [!] failed) and
// serialises concurrent writers per project, so callers (the
// deposit HTTP endpoint, the autonomy manager's backlog tick) never
// race each other or the operator hand-editing the file.
package backlogfile

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

// FormatHeader is written at the top of a freshly created
// BACKLOG.md so the marker grammar is self-documenting for an
// operator opening the file in an editor.
const FormatHeader = "<!-- vornik backlog v1 — markers: [ ] pending | [?] proposed | [~] in-flight | [x] done | [!] failed -->"

// itemRE matches any of the five marker states.
//
// WIDENING THIS IS A DISPATCH CHANGE, NOT A PARSING CHANGE. PeekNext below
// returns the first item whose marker is pending (' '), and the autonomy tick
// calls PeekNext to decide what to BUILD NEXT for a `mode: backlog` project.
// So anything itemRE starts matching becomes executable work, for every
// project, at once.
//
// Concretely: this project's own https://docs.vornik.io keeps its 48 real items as
// `## [ ]` H2 headings and uses `- [ ]` only for sub-notes inside them, so
// widening itemRE to accept headings would hand the autonomy loop 48 items it
// was never meant to run. That is why the 2026-08-26 companion-backlog-deposit
// design normalises heading items to bullets on IMPORT, with its own parser,
// rather than touching this one. See
// https://docs.vornik.io §4.
//
// (This comment previously pointed at internal/autonomy/manager.go's
// backlogPendingRE as the constraint to stay compatible with. That symbol was
// deleted; selection moved here, into PeekNext. The constraint outlived its
// name — corrected 2026-08-26.)
//
// The `~` (in-flight) marker was added 2026-07-12 (LLD 2026-07-12-backlog-
// success-terminal-stamp): dispatch stamps `[~]`, and the reconciler flips
// it to `[x]` on success or `[!]` on failure — so a task is never marked done
// before it is done. The class widening is additive: older parsers that only
// know `[ ?x!]` treat a `[~]` line as a non-item (safe skip).
//
// Capture groups: 1) leading indent + bullet + spaces before `[`,
// 2) the single marker character, 3) the text after the box.
var itemRE = regexp.MustCompile(`^(\s*[-*]\s+)\[([ ?x!~])\]\s+(.*)$`)

// Item is one parsed checklist line from BACKLOG.md.
type Item struct {
	Marker byte   // ' ', '?', '~', 'x', '!'
	Text   string // text after the marker box
	Line   int    // 0-based line index in the file
}

// Store serialises all BACKLOG.md mutations per project (round-2
// F2). Each project ID gets its own *sync.Mutex, obtained lazily via
// sync.Map.LoadOrStore, so unrelated projects never contend on the
// same lock.
type Store struct {
	mus sync.Map // projectID (string) -> *sync.Mutex
}

// NewStore returns an empty Store ready for use.
func NewStore() *Store {
	return &Store{}
}

func (s *Store) lockFor(projectID string) *sync.Mutex {
	v, _ := s.mus.LoadOrStore(projectID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Append adds a new proposed item ("- [?] <itemText>") to the end of
// path, creating the file with FormatHeader if it doesn't exist yet.
// itemText must not contain a newline — flattening multi-line
// proposals into a single line is the caller's responsibility.
func (s *Store) Append(path, projectID, itemText string) error {
	if strings.Contains(itemText, "\n") {
		return fmt.Errorf("backlogfile: item text must not contain a newline: %q", itemText)
	}

	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return err
	}
	if !existed {
		content = FormatHeader + "\n\n"
	}

	content = appendLine(content, "- [?] "+itemText)

	return os.WriteFile(path, []byte(content), 0o600)
}

// Items parses every marker line in path. It returns nil, nil if the
// file does not exist — an absent BACKLOG.md is an empty backlog,
// not an error.
func (s *Store) Items(path, projectID string) ([]Item, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, nil
	}
	return parseItems(content), nil
}

// PeekNext returns the text of the first consumable ("- [ ]")
// item, without modifying the file. ok is false when no such item
// exists (including when the file is absent).
func (s *Store) PeekNext(path, projectID string) (text string, ok bool, err error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return "", false, err
	}
	if !existed {
		return "", false, nil
	}
	for _, it := range parseItems(content) {
		if it.Marker == ' ' {
			return it.Text, true, nil
		}
	}
	return "", false, nil
}

// MarkInFlight finds the first line whose marker is pending (' ') and whose
// text equals text exactly, rewrites its marker to '~' (in-flight), and
// appends " (task: <taskID>)" to the text. It returns (false, nil) when no
// line matches — e.g. the operator already hand-edited the line, or a
// concurrent caller won the race.
//
// This is the dispatch-time stamp (LLD 2026-07-12-backlog-success-terminal-
// stamp): a backlog item is stamped in-flight — NOT done — when its task is
// dispatched, and the tick reconciler later flips `[~]` → `[x]` on a
// successful terminal or `[~]` → `[!]` on failure. Stamping `[~]` (which
// PeekNext skips) is also the file-level double-dispatch interlock that
// survives a daemon restart. Same body shape as MarkConsumed with a different
// marker.
func (s *Store) MarkInFlight(path, projectID, text, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || m[2] != " " || m[3] != text {
			continue
		}
		lines[i] = m[1] + "[~] " + text + fmt.Sprintf(" (task: %s)", taskID)
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// MarkDone finds the in-flight ('~') line whose text ends with
// "(task: <taskID>)" and rewrites its marker to 'x' (done). It returns
// (false, nil) when no line matches — including on a second call for the same
// taskID (the marker is then 'x' and no longer matches the '~' search), so it
// is idempotent. Called by the tick reconciler when the dispatched task
// reached a successful terminal (LLD 2026-07-12-backlog-success-terminal-stamp).
func (s *Store) MarkDone(path, projectID, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	suffix := fmt.Sprintf("(task: %s)", taskID)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || m[2] != "~" || !strings.HasSuffix(m[3], suffix) {
			continue
		}
		lines[i] = m[1] + "[x] " + m[3]
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// MarkConsumed finds the first line whose marker is pending (' ')
// and whose text equals text exactly, rewrites its marker to 'x',
// and appends " (task: <taskID>)" to the text. It returns
// (false, nil) when no line matches — e.g. the operator already
// hand-edited the line, or it was consumed by a concurrent caller.
//
// DEPRECATED for the autonomy dispatch path (superseded by MarkInFlight per
// LLD 2026-07-12-backlog-success-terminal-stamp — dispatch stamps `[~]`, not
// `[x]`). Retained for any non-autonomy caller; the live backlog tick no
// longer uses it.
func (s *Store) MarkConsumed(path, projectID, text, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || m[2] != " " || m[3] != text {
			continue
		}
		lines[i] = m[1] + "[x] " + text + fmt.Sprintf(" (task: %s)", taskID)
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// MarkFailed finds the line whose marker is in-flight ('~') or done ('x') and
// whose text ends with "(task: <taskID>)", rewrites its marker to '!', and
// appends ", failed" inside that annotation (producing
// "(task: <taskID>, failed)"). It returns (false, nil) when no line matches —
// including on a second call for the same taskID, since the line's marker is
// then '!' and no longer matches. Idempotent.
//
// It matches BOTH markers so it handles the new in-flight path (`[~]` — a task
// that failed while dispatched) AND back-compat with items stamped `[x]` at
// dispatch by the pre-2026-07-12 code (the stranded optimistic `[x]`s the
// original reconciler was built to flip). LLD 2026-07-12-backlog-success-
// terminal-stamp §5.
func (s *Store) MarkFailed(path, projectID, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}

	suffix := fmt.Sprintf("(task: %s)", taskID)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || (m[2] != "x" && m[2] != "~") || !strings.HasSuffix(m[3], suffix) {
			continue
		}
		newText := strings.TrimSuffix(m[3], ")") + ", failed)"
		lines[i] = m[1] + "[!] " + newText
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// taskAnnotationRE strips a trailing " (task: <id>)" / " (task: <id>, failed)"
// annotation so a consumed/failed line's base item text can be matched against
// origin's pending line (which carries no annotation).
var taskAnnotationRE = regexp.MustCompile(`\s*\(task: [^)]+\)\s*$`)

func stripTaskAnnotation(s string) string {
	return strings.TrimSpace(taskAnnotationRE.ReplaceAllString(s, ""))
}

// MergeConsumedMarks returns origin's BACKLOG.md content with the local file's
// consumption marks ([x] done, [!] failed) re-applied to matching items.
//
// The workspace-refresh contract (autonomous-dev-loop follow-up): origin is
// authoritative for the item SET, text, and [ ]/[?] states — operator
// promotions and external contributions merged to main — while local is
// authoritative for which items the daemon has already dispatched. An origin
// `[ ]` item whose base text matches a local `[x]`/`[!]` item (ignoring the
// `(task: id)` annotation) is re-stamped with the local marker + annotation, so
// resetting the workspace to origin never re-runs an already-consumed item.
// Items removed from origin drop out; items new/re-opened on origin keep their
// origin marker.
//
// Matching is by base item text (the line minus its (task: id) annotation) —
// the same identity the deposit dedup and MarkConsumed use. Two items sharing
// identical text is therefore ambiguous (last consumed mark wins); in a
// human-authored backlog distinct items carry distinct text, so this doesn't
// arise in practice.
func MergeConsumedMarks(origin, local string) string {
	type mark struct {
		marker byte
		text   string // full text after the "[m] " box, incl. the (task: id) annotation
	}
	consumed := map[string]mark{}
	for _, line := range strings.Split(local, "\n") {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || (m[2] != "x" && m[2] != "!") {
			continue
		}
		if base := stripTaskAnnotation(m[3]); base != "" {
			consumed[base] = mark{marker: m[2][0], text: m[3]}
		}
	}
	if len(consumed) == 0 {
		return origin
	}
	lines := strings.Split(origin, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || m[2] != " " { // only re-stamp still-pending origin items
			continue
		}
		if cm, ok := consumed[stripTaskAnnotation(m[3])]; ok {
			lines[i] = m[1] + "[" + string(cm.marker) + "] " + cm.text
		}
	}
	return strings.Join(lines, "\n")
}

// RefreshFromOrigin reconciles the workspace BACKLOG.md with origin under the
// per-project lock. It snapshots the local consumption marks, runs gitReset
// (which overwrites the working tree — BACKLOG.md included — with origin/main),
// then re-applies the local [x]/[!] marks onto origin's version via
// MergeConsumedMarks and writes the result. Holding the lock across gitReset
// stops a concurrent deposit / MarkConsumed from racing the reset.
//
// Best-effort by contract: a nil gitReset is a no-op, and a gitReset error is
// returned with the file left as gitReset left it — the caller (autonomy tick)
// logs and proceeds rather than blocking the loop. When origin carries no
// BACKLOG.md the local file is restored so a refresh never empties the backlog.
func (s *Store) RefreshFromOrigin(path, projectID string, gitReset func() error) error {
	if gitReset == nil {
		return nil
	}
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	local, localExisted, err := readFile(path)
	if err != nil {
		return err
	}
	if err := gitReset(); err != nil {
		return err
	}
	origin, originExisted, err := readFile(path)
	if err != nil {
		return err
	}
	if !originExisted {
		if !localExisted || strings.TrimSpace(local) == "" {
			return nil
		}
		return os.WriteFile(path, []byte(local), 0o600)
	}
	merged := MergeConsumedMarks(origin, local)
	if merged == origin {
		return nil // gitReset already wrote origin's version; nothing to re-apply
	}
	return os.WriteFile(path, []byte(merged), 0o600)
}

// RevertToPending finds the consumed line ('x' or '!' marker) whose text
// carries "(task: <taskID>)", flips it back to a pending '[ ]' item, and
// strips the task annotation — so a dispatched item whose task did NOT
// succeed (failed / cancelled) returns to the queue for another attempt
// rather than staying marked done and being skipped forever. A '[x]' is
// reserved for items whose task actually completed. Returns (false, nil)
// when no matching consumed line exists.
func (s *Store) RevertToPending(path, projectID, taskID string) (bool, error) {
	mu := s.lockFor(projectID)
	mu.Lock()
	defer mu.Unlock()

	content, existed, err := readFile(path)
	if err != nil || !existed {
		return false, err
	}
	suffix := fmt.Sprintf("(task: %s)", taskID)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := itemRE.FindStringSubmatch(line)
		if m == nil || (m[2] != "x" && m[2] != "!") {
			continue
		}
		// Match the task by its annotation — [x] has "(task: id)",
		// [!] has "(task: id, failed)"; strip either form.
		base := stripTaskAnnotation(m[3])
		if !strings.HasSuffix(m[3], suffix) && !strings.Contains(m[3], "(task: "+taskID+",") {
			continue
		}
		lines[i] = m[1] + "[ ] " + base
		return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	}
	return false, nil
}

// readFile reads path, reporting existed=false (with a nil error)
// when the file is absent so callers can distinguish "empty
// backlog" from a real I/O failure.
func readFile(path string) (content string, existed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// appendLine appends line to content, ensuring exactly one newline
// separates it from prior content and that the result ends with a
// trailing newline.
func appendLine(content, line string) string {
	if content == "" {
		return line + "\n"
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + line + "\n"
}

// parseItems splits content into lines and collects every line
// matching itemRE, in file order.
func parseItems(content string) []Item {
	var items []Item
	for i, line := range strings.Split(content, "\n") {
		m := itemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		items = append(items, Item{
			Marker: m[2][0],
			Text:   m[3],
			Line:   i,
		})
	}
	return items
}
